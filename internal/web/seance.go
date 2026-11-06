package web

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/zhangjuji/k8s-seance/internal/autopsy"
	"github.com/zhangjuji/k8s-seance/internal/llm"
	"github.com/zhangjuji/k8s-seance/internal/postmortem"
	"github.com/zhangjuji/k8s-seance/internal/spirit"
)

const (
	// seanceIdleTTL is how long a séance may go unused. The next create drops older ones.
	seanceIdleTTL = 2 * time.Hour
	// turnTimeout bounds one LLM turn: the opening or one question.
	turnTimeout = 2 * time.Minute
	// subscriberBuffer is how many events a stream may queue. Deltas are dropped when it is full.
	subscriberBuffer = 256
)

// Turn roles. They are the values of turnPayload.Role and turnRecord.Role.
const (
	roleSpirit = "spirit"
	roleUser   = "user"
)

// SSE event names sent on the séance stream.
const (
	eventTurn      = "turn"
	eventDelta     = "delta"
	eventExhausted = "exhausted"
	eventError     = "error"
)

// evidenceRef matches an evidence reference such as [E3] in a reply.
var evidenceRef = regexp.MustCompile(`\[E\d+\]`)

// seanceStore holds the séances by id. The map and every seance.lastSeen are
// guarded by mu.
type seanceStore struct {
	mu  sync.Mutex
	now func() time.Time
	m   map[string]*seance
}

func newSeanceStore() *seanceStore {
	return &seanceStore{now: time.Now, m: make(map[string]*seance)}
}

// add stores se and first drops every séance that has been idle longer than seanceIdleTTL.
func (st *seanceStore) add(se *seance) {
	st.mu.Lock()
	defer st.mu.Unlock()
	now := st.now()
	for id, old := range st.m {
		if now.Sub(old.lastSeen) > seanceIdleTTL {
			delete(st.m, id)
		}
	}
	se.lastSeen = now
	st.m[se.id] = se
}

// get returns the séance with id and marks it as used. An expired séance is removed and reported as missing.
func (st *seanceStore) get(id string) *seance {
	st.mu.Lock()
	defer st.mu.Unlock()
	se, ok := st.m[id]
	if !ok {
		return nil
	}
	now := st.now()
	if now.Sub(se.lastSeen) > seanceIdleTTL {
		delete(st.m, id)
		return nil
	}
	se.lastSeen = now
	return se
}

// newSeanceID returns 16 random bytes as 32 hex characters.
func newSeanceID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("random séance id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// seance is one live séance: a spirit.Session with the turns recorded so far and
// the stream subscribers that get them.
type seance struct {
	id     string
	source string
	// lang is the séance language, fixed at creation. Turn fragments and the
	// spirit's messages use it. The page around them uses the request language.
	lang     string
	ns, pod  string
	cause    autopsy.CauseOfDeath
	died     string
	sess     *spirit.Session
	evidence []autopsy.Evidence
	evIDs    map[string]bool
	// eventTTL and now drive the forgetting countdown of the page (Requirement 14).
	eventTTL time.Duration
	now      func() time.Time
	tmpl     *template.Template
	logf     func(format string, args ...any)
	// bury stores a tombstone for a new postmortem and returns its ID, or "" when
	// none was stored. It is nil when the graveyard is off.
	bury func(res postmortem.Result) string

	// lastSeen is guarded by seanceStore.mu.
	lastSeen time.Time

	// askMu serializes questions, so the transcript and the turn list stay in the same order.
	askMu sync.Mutex

	// mu guards the fields below.
	mu          sync.Mutex
	turns       []turnRecord // turn i has Index i; turn 0 is the opening
	openingDone bool
	openRun     *openingRun // the opening in progress, or nil
	subs        map[*subscriber]struct{}
	pm          *postmortemDoc // the last generated postmortem, or nil
}

// postmortemDoc is a generated postmortem and how it was made.
type postmortemDoc struct {
	markdown string
	mode     string
	reason   string
	at       time.Time
	// tombID is the graveyard tombstone written for this postmortem, or "".
	tombID string
}

// postmortemFor returns the stored postmortem. It generates one when none exists
// or when regen is true. The call takes askMu, so it does not overlap a question.
func (se *seance) postmortemFor(ctx context.Context, regen bool) (postmortemDoc, error) {
	se.askMu.Lock()
	defer se.askMu.Unlock()
	if !regen {
		se.mu.Lock()
		doc := se.pm
		se.mu.Unlock()
		if doc != nil {
			return *doc, nil
		}
	}
	in := postmortem.Input{
		Autopsy:    se.sess.Autopsy,
		Class:      se.sess.Class,
		Evidence:   se.sess.Evidence,
		Transcript: se.sess.Transcript(),
	}
	res, err := postmortem.Generate(ctx, in, se.sess.Provider())
	if err != nil {
		return postmortemDoc{}, err
	}
	doc := postmortemDoc{markdown: res.Markdown, mode: res.Mode, reason: res.Reason, at: time.Now().UTC()}
	if se.bury != nil {
		doc.tombID = se.bury(res)
	}
	se.mu.Lock()
	se.pm = &doc
	se.mu.Unlock()
	return doc, nil
}

// openingRun is one attempt at the opening. done is closed when it ends, and err
// is set before that.
type openingRun struct {
	done chan struct{}
	err  error
}

// turnRecord is a completed turn. HTML is the rendered turn fragment. Doubts
// are the fact-check findings of a spirit turn, and empty for a user turn.
type turnRecord struct {
	Index   int
	Role    string
	Content string
	HTML    string
	Doubts  []doubtItem
}

// payload returns the data of the turn event for t.
func (t turnRecord) payload() turnPayload {
	return turnPayload{Index: t.Index, Role: t.Role, Content: t.Content, HTML: t.HTML, Doubts: t.Doubts}
}

// doubtItem is one claim of the spirit that the evidence does not back up
// (Requirement 11). Message describes it in the séance language.
type doubtItem struct {
	Kind     string `json:"kind"`
	Claim    string `json:"claim"`
	Sentence string `json:"sentence"`
	Message  string `json:"message"`
}

// doubtsFor fact-checks a spirit reply. The exhausted line is not a reply, so
// it is not checked. The result is never nil, so JSON shows [] for no doubts.
func (se *seance) doubtsFor(role, content string) []doubtItem {
	out := []doubtItem{}
	if role != roleSpirit || llm.IsExhaustedLine(content) {
		return out
	}
	for _, f := range se.sess.FactCheck(content) {
		out = append(out, doubtItem{
			Kind:     f.Kind,
			Claim:    f.Claim,
			Sentence: f.Sentence,
			Message:  trf(se.lang, "doubt_"+f.Kind, f.Claim),
		})
	}
	return out
}

// subscriber is one open stream. Senders stop waiting for it once gone is closed.
type subscriber struct {
	ch   chan sseEvent
	gone chan struct{}
}

// sseEvent is one event ready to write. index is set for turn events only.
type sseEvent struct {
	name  string
	index int
	data  []byte
}

// turnPayload is the data of an `event: turn` line. Doubts is [] when the
// fact check found nothing, which does not mean the reply is verified.
type turnPayload struct {
	Index   int         `json:"index"`
	Role    string      `json:"role"`
	Content string      `json:"content"`
	HTML    string      `json:"html"`
	Doubts  []doubtItem `json:"doubts"`
}

// deltaPayload is the data of an `event: delta` line.
type deltaPayload struct {
	Index int    `json:"index"`
	Text  string `json:"text"`
}

// newSeance wraps a new session for the Pod a. The session uses the server's
// provider, limits and opening cache, so each séance has its own call budget.
// lang is the séance language.
func (s *Server) newSeance(id, source, lang string, a *autopsy.PodAutopsy, cls autopsy.Classification, ev []autopsy.Evidence) *seance {
	lang = uiLang(lang)
	se := &seance{
		id:       id,
		source:   source,
		lang:     lang,
		ns:       a.Namespace,
		pod:      a.Name,
		cause:    cls.Cause,
		evidence: ev,
		evIDs:    make(map[string]bool, len(ev)),
		eventTTL: s.opts.EventTTL,
		now:      s.now,
		tmpl:     s.set(lang).seance,
		logf:     s.opts.Logger.Printf,
		subs:     make(map[*subscriber]struct{}),
	}
	if t, ok := spirit.DeathTime(a); ok {
		se.died = formatTime(&t)
	} else {
		se.died = formatTime(nil)
	}
	for _, e := range ev {
		se.evIDs[e.ID] = true
	}
	if s.opts.Graveyard != nil {
		se.bury = func(res postmortem.Result) string { return s.buryTombstone(a, cls, res) }
	}
	se.sess = spirit.NewSessionIn(lang, id, a, cls, ev, s.opts.Provider, s.opts.Limits, s.cache, s.opts.Model)
	return se
}

// subscribe registers a stream. It returns the turns completed so far, and whether
// the opening has finished. The registration and the snapshot happen under one
// lock, so every turn is seen exactly once: in the snapshot or as a live event.
func (se *seance) subscribe() (*subscriber, []turnRecord, bool) {
	se.mu.Lock()
	defer se.mu.Unlock()
	sub := &subscriber{ch: make(chan sseEvent, subscriberBuffer), gone: make(chan struct{})}
	se.subs[sub] = struct{}{}
	snap := append([]turnRecord(nil), se.turns...)
	return sub, snap, se.openingDone
}

// unsubscribe removes sub. It must be called once per subscribe.
func (se *seance) unsubscribe(sub *subscriber) {
	se.mu.Lock()
	delete(se.subs, sub)
	se.mu.Unlock()
	close(sub.gone)
}

// turnsSnapshot returns a copy of the completed turns.
func (se *seance) turnsSnapshot() []turnRecord {
	se.mu.Lock()
	defer se.mu.Unlock()
	return append([]turnRecord(nil), se.turns...)
}

// subsLocked copies the subscriber list. The caller must hold mu.
func (se *seance) subsLocked() []*subscriber {
	out := make([]*subscriber, 0, len(se.subs))
	for sub := range se.subs {
		out = append(out, sub)
	}
	return out
}

// beginOpening starts the opening if it has not been produced and is not already
// running. It returns the run to wait on, or nil when the opening is done.
func (se *seance) beginOpening() *openingRun {
	se.mu.Lock()
	defer se.mu.Unlock()
	if se.openingDone {
		return nil
	}
	if se.openRun == nil {
		run := &openingRun{done: make(chan struct{})}
		se.openRun = run
		go se.runOpening(run)
	}
	return se.openRun
}

// ensureOpening makes sure the opening exists, starting it if needed, and waits for it.
func (se *seance) ensureOpening(ctx context.Context) error {
	run := se.beginOpening()
	if run == nil {
		return nil
	}
	select {
	case <-run.done:
		return run.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// runOpening generates the opening and records it as turn 0. Deltas go to the
// subscribers while it streams. On failure the error is logged, and the stream
// gets a generic error event so the browser does not see provider details.
func (se *seance) runOpening(run *openingRun) {
	ctx, cancel := context.WithTimeout(context.Background(), turnTimeout)
	defer cancel()
	text, err := se.sess.Opening(ctx, func(part string) error {
		se.sendDelta(0, part)
		return nil
	})
	if err != nil {
		se.logf("opening for %s/%s: %v", se.ns, se.pod, err)
		se.mu.Lock()
		se.openRun = nil
		se.mu.Unlock()
		run.err = err
		se.broadcast(sseEvent{name: eventError, data: mustJSON(map[string]string{"message": tr(se.lang, "no_answer")})})
		close(run.done)
		return
	}
	se.addTurn(roleSpirit, text, se.sess.BudgetUsed(), se.sess.BudgetRemaining())
	close(run.done)
}

// askResult is what one question produced: the user's turn and the spirit's reply.
type askResult struct {
	user, reply turnRecord
	exhausted   bool
}

// ask records one question and its reply. Calls are serialized per séance.
// When the budget is spent, the reply is llm.ExhaustedLineFor(se.lang) and
// exhausted is true.
// The opening must exist before ask is called.
func (se *seance) ask(ctx context.Context, question string) (askResult, error) {
	se.askMu.Lock()
	defer se.askMu.Unlock()

	se.mu.Lock()
	replyIndex := len(se.turns) + 1 // the question takes len(turns), the reply comes after it
	se.mu.Unlock()

	text, err := se.sess.Ask(ctx, question, func(part string) error {
		se.sendDelta(replyIndex, part)
		return nil
	})
	if err != nil {
		return askResult{}, err
	}
	used, left := se.sess.BudgetUsed(), se.sess.BudgetRemaining()
	line := llm.ExhaustedLineFor(se.lang)
	res := askResult{exhausted: text == line}
	res.user = se.addTurn(roleUser, strings.TrimSpace(question), used, left)
	if res.exhausted {
		se.broadcast(sseEvent{name: eventExhausted, data: mustJSON(map[string]string{"message": line})})
	}
	res.reply = se.addTurn(roleSpirit, text, used, left)
	return res, nil
}

// addTurn records a completed turn and sends it to every subscriber. Turn 0 is the
// opening, so it also marks the opening as done.
func (se *seance) addTurn(role, content string, used, left int) turnRecord {
	se.mu.Lock()
	rec := turnRecord{
		Index:   len(se.turns),
		Role:    role,
		Content: content,
		Doubts:  se.doubtsFor(role, content),
	}
	rec.HTML = se.renderTurn(rec.Index, role, content, rec.Doubts, used, left)
	se.turns = append(se.turns, rec)
	if rec.Index == 0 {
		se.openingDone = true
		se.openRun = nil
	}
	subs := se.subsLocked()
	se.mu.Unlock()

	ev := sseEvent{
		name:  eventTurn,
		index: rec.Index,
		data:  mustJSON(rec.payload()),
	}
	for _, sub := range subs {
		sub.deliver(ev)
	}
	return rec
}

// sendDelta sends one fragment of the reply at index to the subscribers. A stream
// whose buffer is full misses the fragment. The turn event at the end still
// carries the whole text.
func (se *seance) sendDelta(index int, text string) {
	se.mu.Lock()
	subs := se.subsLocked()
	se.mu.Unlock()
	if len(subs) == 0 {
		return
	}
	ev := sseEvent{name: eventDelta, data: mustJSON(deltaPayload{Index: index, Text: text})}
	for _, sub := range subs {
		sub.offer(ev)
	}
}

// broadcast sends ev to every subscriber and waits for each one to take it.
func (se *seance) broadcast(ev sseEvent) {
	se.mu.Lock()
	subs := se.subsLocked()
	se.mu.Unlock()
	for _, sub := range subs {
		sub.deliver(ev)
	}
}

// deliver sends ev, or gives up when the subscriber has gone.
func (sub *subscriber) deliver(ev sseEvent) {
	select {
	case sub.ch <- ev:
	case <-sub.gone:
	}
}

// offer sends ev if the buffer has room, and drops it otherwise.
func (sub *subscriber) offer(ev sseEvent) {
	select {
	case sub.ch <- ev:
	default:
	}
}

// turnView is the data of templates/turn.html.
type turnView struct {
	Index  int
	Role   string
	Who    string
	Body   template.HTML
	Advice bool
	// Doubts are listed under the reply as "the spirit may be lying".
	Doubts []doubtItem
	// Verdict is where the spirit board's planchette goes when the turn ends:
	// "yes", "no" or "" (Requirement 17).
	Verdict string
	Used    int
	Left    int
}

// renderTurn builds the HTML fragment of one turn in the séance language. The
// text is escaped first, and then each [E#] that names a known evidence item
// becomes a link to its card. Spirit replies also get **bold** and `code`.
func (se *seance) renderTurn(index int, role, content string, doubts []doubtItem, used, left int) string {
	v := turnView{
		Index:   index,
		Role:    role,
		Who:     tr(se.lang, "who_user"),
		Body:    se.linkEvidence(content),
		Advice:  role == roleSpirit && isAdvice(content),
		Doubts:  doubts,
		Verdict: turnVerdict(role, llm.IsExhaustedLine(content), doubts),
		Used:    used,
		Left:    left,
	}
	if role == roleSpirit {
		v.Who = tr(se.lang, "who_spirit")
		v.Body = se.renderReply(content)
	}
	var buf bytes.Buffer
	if err := se.tmpl.ExecuteTemplate(&buf, "turn", v); err != nil {
		se.logf("render turn %d of %s: %v", index, se.id, err)
		return ""
	}
	return buf.String()
}

// linkEvidence escapes text and turns each [E#] into a link to #ev-E#.
func (se *seance) linkEvidence(text string) template.HTML {
	return template.HTML(se.linkRefs(template.HTMLEscapeString(text)))
}

// renderReply escapes a spirit reply, renders **bold** and `code` with
// markdownLite, and links each [E#] outside code spans.
func (se *seance) renderReply(text string) template.HTML {
	return template.HTML(markdownLite(template.HTMLEscapeString(text), se.linkRefs))
}

// linkRefs turns each [E#] of escaped text that names a known evidence item
// into a link to its card.
func (se *seance) linkRefs(escaped string) string {
	return evidenceRef.ReplaceAllStringFunc(escaped, func(ref string) string {
		id := ref[1 : len(ref)-1]
		if !se.evIDs[id] {
			return ref
		}
		return `<a class="ev-link" href="#ev-` + id + `">` + ref + `</a>`
	})
}

// adviceMarkers are the words that mark a reply as a fix suggestion.
var adviceMarkers = []string{"建议", "可以考虑", "Consider"}

// isAdvice reports whether a reply contains a fix suggestion.
func isAdvice(text string) bool {
	for _, m := range adviceMarkers {
		if strings.Contains(text, m) {
			return true
		}
	}
	return false
}

// seancePage is the data of templates/seance.html.
type seancePage struct {
	UI    chrome
	Title string
	// Lang is the séance language. LangNotice is set when it differs from the
	// interface language.
	Lang       string
	LangNotice string
	// SpiritWho labels the typing indicator, like the spirit turns.
	SpiritWho string
	// Quick are the one-click questions, in the séance language.
	Quick       []quickQuestion
	ID          string
	Pod         string
	BadgeClass  string
	Cause       string
	Died        string
	StreamURL   string
	MessagesURL string
	Used        int
	Left        int
	Total       int
	Exhausted   bool
	Turns       []template.HTML
	Evidence    []evidenceCard
	// Memory is the forgetting panel (Requirement 14).
	Memory memoryPanel
	// Lineage is the family and rebirths panel (Requirement 16).
	Lineage lineagePanel
	// Board is the spirit board (Requirement 17).
	Board         spiritBoard
	PostmortemURL string
	// SpellURL is the resurrection spell panel (Requirement 13).
	SpellURL string
}

// quickQuestion is one one-click question. Vals is the hx-vals JSON.
type quickQuestion struct {
	Text string
	Vals string
}

// quickQuestions returns the one-click questions in lang.
func quickQuestions(lang string) []quickQuestion {
	keys := []string{"q_how_died", "q_fix"}
	out := make([]quickQuestion, 0, len(keys))
	for _, k := range keys {
		text := tr(lang, k)
		out = append(out, quickQuestion{Text: text, Vals: string(mustJSON(map[string]string{"text": text}))})
	}
	return out
}

// pageData builds the data for the séance page in the interface language ui.
// It includes the turns recorded so far, so the page works without the stream.
func (se *seance) pageData(r *http.Request, ui string) seancePage {
	used, left := se.sess.BudgetUsed(), se.sess.BudgetRemaining()
	turns := se.turnsSnapshot()
	html := make([]template.HTML, 0, len(turns))
	for _, t := range turns {
		html = append(html, template.HTML(t.HTML))
	}
	memory, cards := buildMemory(ui, se.sess.Autopsy, se.evidence, se.eventTTL, se.now())
	p := seancePage{
		UI:            chromeFor(r, ui),
		Lang:          se.lang,
		SpiritWho:     tr(se.lang, "who_spirit"),
		Quick:         quickQuestions(se.lang),
		Title:         se.pod,
		ID:            se.id,
		Pod:           se.pod,
		BadgeClass:    badgeClass(se.cause),
		Cause:         string(se.cause),
		Died:          se.died,
		StreamURL:     seanceStreamURL(se.id),
		MessagesURL:   seanceMessagesURL(se.id),
		Used:          used,
		Left:          left,
		Total:         used + left,
		Exhausted:     left == 0,
		Turns:         html,
		Evidence:      cards,
		Memory:        memory,
		Lineage:       buildLineage(ui, se.sess.Autopsy, se.evidence),
		Board:         buildBoard(se.evidence),
		PostmortemURL: seancePostmortemURL(se.id),
		SpellURL:      seanceSpellURL(se.id),
	}
	if se.lang != ui {
		p.LangNotice = trf(ui, "lang_mismatch", tr(ui, "lang_name_"+se.lang))
	}
	return p
}

// mustJSON encodes v as one line of JSON, without HTML escaping, so that html
// fragments stay readable in the stream. The payloads are plain structs, so
// encoding cannot fail.
func mustJSON(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return bytes.TrimRight(buf.Bytes(), "\n")
}

func seanceStreamURL(id string) string   { return "/api/seances/" + id + "/stream" }
func seanceMessagesURL(id string) string { return "/api/seances/" + id + "/messages" }
func seancePostmortemURL(id string) string {
	return "/api/seances/" + id + "/postmortem"
}
func seanceDownloadURL(id string) string {
	return "/api/seances/" + id + "/postmortem.md"
}
func seancePageURL(id string) string { return "/seances/" + id }
