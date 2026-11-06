package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/zhangjuji/k8s-seance/internal/autopsy"
	"github.com/zhangjuji/k8s-seance/internal/llm"
)

const (
	// maxRequestBytes bounds the body of the séance POST endpoints.
	maxRequestBytes = 16 << 10
	// maxQuestionRunes bounds one question.
	maxQuestionRunes = 1000
	// pingInterval is how often an open stream gets a comment line, so proxies keep it alive.
	pingInterval = 15 * time.Second
)

// registerSeanceRoutes adds the séance routes to mux.
func (s *Server) registerSeanceRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/seances", s.handleCreateSeance)
	mux.HandleFunc("GET /api/seances/{id}", s.handleGetSeance)
	mux.HandleFunc("GET /api/seances/{id}/stream", s.handleStream)
	mux.HandleFunc("POST /api/seances/{id}/messages", s.handleMessage)
	mux.HandleFunc("POST /api/seances/{id}/postmortem", s.handlePostmortem)
	mux.HandleFunc("GET /api/seances/{id}/postmortem.md", s.handlePostmortemDownload)
	mux.HandleFunc("GET /seances/{id}", s.handleSeancePage)
}

// createRequest is the body of POST /api/seances. Form posts use the same field names.
type createRequest struct {
	NS  string `json:"ns"`
	Pod string `json:"pod"`
	Src string `json:"src"`
	// Lang is the séance language, "zh" or "en". Empty means the interface
	// language of the request.
	Lang string `json:"lang"`
}

// createResponse is the 201 body of POST /api/seances.
type createResponse struct {
	ID        string `json:"id"`
	Pod       string `json:"pod"`
	Cause     string `json:"cause"`
	Lang      string `json:"lang"`
	StreamURL string `json:"streamUrl"`
	PageURL   string `json:"pageUrl"`
}

// messageReply is the 200 body of POST /api/seances/{id}/messages for JSON clients.
type messageReply struct {
	Index           int    `json:"index"`
	Reply           string `json:"reply"`
	Exhausted       bool   `json:"exhausted"`
	BudgetUsed      int    `json:"budgetUsed"`
	BudgetRemaining int    `json:"budgetRemaining"`
	// Doubts are the fact-check findings of the reply, [] when none.
	Doubts []doubtItem `json:"doubts"`
}

// seanceDetail is the body of GET /api/seances/{id}.
type seanceDetail struct {
	ID              string           `json:"id"`
	Pod             string           `json:"pod"`
	NS              string           `json:"ns"`
	Cause           string           `json:"cause"`
	Lang            string           `json:"lang"`
	Transcript      []transcriptItem `json:"transcript"`
	BudgetUsed      int              `json:"budgetUsed"`
	BudgetRemaining int              `json:"budgetRemaining"`
	Exhausted       bool             `json:"exhausted"`
	Evidence        []evidenceItem   `json:"evidence"`
}

type transcriptItem struct {
	Index   int         `json:"index"`
	Role    string      `json:"role"`
	Content string      `json:"content"`
	Doubts  []doubtItem `json:"doubts"`
}

type evidenceItem struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	Text string `json:"text"`
}

// handleCreateSeance collects the Pod, classifies it and starts a séance. JSON
// clients get 201 with the URLs. Form posts (the 召唤 button) get a 303 to the page.
func (s *Server) handleCreateSeance(w http.ResponseWriter, r *http.Request) {
	req, err := readCreateRequest(w, r)
	if err != nil {
		s.fail(w, r, http.StatusBadRequest, err.Error())
		return
	}
	ns := req.NS
	if ns == "" {
		ns = s.opts.DefaultNamespace
	}
	if len(ns) > 63 || !namespacePattern.MatchString(ns) {
		s.fail(w, r, http.StatusBadRequest, errInvalidNamespace.Error())
		return
	}
	if req.Pod == "" {
		s.fail(w, r, http.StatusBadRequest, "pod is required")
		return
	}
	lang := s.requestLang(r)
	if req.Lang != "" {
		if lang = llm.NormalizeLang(req.Lang); lang == "" {
			s.fail(w, r, http.StatusBadRequest, "lang must be zh or en")
			return
		}
	}

	src, err := s.sourceNamed(req.Src)
	var down *clusterDownError
	switch {
	case errors.Is(err, errUnknownSource):
		s.fail(w, r, http.StatusBadRequest, err.Error())
		return
	case errors.As(err, &down):
		s.fail(w, r, http.StatusServiceUnavailable, down.message(s.requestLang(r)))
		return
	case err != nil:
		s.fail(w, r, http.StatusInternalServerError, err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	a, err := src.Autopsy(ctx, ns, req.Pod)
	switch {
	case errors.Is(err, ErrNotFound):
		s.fail(w, r, http.StatusNotFound, fmt.Sprintf("no spirit %s/%s", ns, req.Pod))
		return
	case err != nil:
		s.opts.Logger.Printf("collect %s/%s from %s: %v", ns, req.Pod, src.Name(), err)
		s.fail(w, r, http.StatusServiceUnavailable, trf(s.requestLang(r), "read_spirits_failed", err.Error()))
		return
	}

	cls := autopsy.Classify(a)
	ev, err := autopsy.BuildEvidence(a, autopsy.EvidenceOptions{})
	if err != nil {
		s.opts.Logger.Printf("build evidence for %s/%s: %v", a.Namespace, a.Name, err)
		s.fail(w, r, http.StatusInternalServerError, "build evidence failed")
		return
	}
	id, err := newSeanceID()
	if err != nil {
		s.opts.Logger.Printf("%v", err)
		s.fail(w, r, http.StatusInternalServerError, "create séance failed")
		return
	}
	se := s.newSeance(id, src.Name(), lang, a, cls, ev)
	s.seances.add(se)

	res := createResponse{
		ID:        id,
		Pod:       a.Name,
		Cause:     string(cls.Cause),
		Lang:      se.lang,
		StreamURL: seanceStreamURL(id),
		PageURL:   seancePageURL(id),
	}
	if wantsJSON(r) {
		writeJSON(w, http.StatusCreated, res)
		return
	}
	http.Redirect(w, r, res.PageURL, http.StatusSeeOther)
}

// handleGetSeance returns the séance state as JSON.
func (s *Server) handleGetSeance(w http.ResponseWriter, r *http.Request) {
	se := s.seanceFor(w, r)
	if se == nil {
		return
	}
	turns := se.turnsSnapshot()
	transcript := make([]transcriptItem, 0, len(turns))
	for _, t := range turns {
		transcript = append(transcript, transcriptItem{Index: t.Index, Role: t.Role, Content: t.Content, Doubts: t.Doubts})
	}
	evidence := make([]evidenceItem, 0, len(se.evidence))
	for _, e := range se.evidence {
		evidence = append(evidence, evidenceItem{ID: e.ID, Kind: e.Kind, Text: e.Text})
	}
	used, left := se.sess.BudgetUsed(), se.sess.BudgetRemaining()
	writeJSON(w, http.StatusOK, seanceDetail{
		ID:              se.id,
		Pod:             se.pod,
		NS:              se.ns,
		Cause:           string(se.cause),
		Lang:            se.lang,
		Transcript:      transcript,
		BudgetUsed:      used,
		BudgetRemaining: left,
		Exhausted:       left == 0,
		Evidence:        evidence,
	})
}

// handleStream is the SSE endpoint. It first replays every completed turn, then
// starts the opening if it has not been produced, then sends live events until
// the client leaves. With ?once=1 it closes after the opening is available.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	se := s.seanceFor(w, r)
	if se == nil {
		return
	}
	once := r.URL.Query().Get("once") == "1"

	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)

	// Subscribe before writing anything, so no event between the replay and the live loop is lost.
	sub, replay, opened := se.subscribe()
	defer se.unsubscribe(sub)

	for _, t := range replay {
		ev := sseEvent{name: eventTurn, index: t.Index, data: mustJSON(t.payload())}
		if err := writeEvent(w, ev); err != nil {
			return
		}
	}
	if err := rc.Flush(); err != nil {
		return
	}
	if opened && once {
		return
	}
	if !opened {
		se.beginOpening()
	}

	ping := time.NewTicker(pingInterval)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
			if err := rc.Flush(); err != nil {
				return
			}
		case ev := <-sub.ch:
			if err := writeEvent(w, ev); err != nil {
				return
			}
			if err := rc.Flush(); err != nil {
				return
			}
			if once && ((ev.name == eventTurn && ev.index == 0) || ev.name == eventError) {
				return
			}
		}
	}
}

// handleMessage asks the spirit one question. The reply is also sent to the stream
// subscribers. htmx requests get the turn fragments, JSON clients get messageReply,
// and plain form posts get a 303 back to the page.
func (s *Server) handleMessage(w http.ResponseWriter, r *http.Request) {
	se := s.seanceFor(w, r)
	if se == nil {
		return
	}
	text, err := readMessageText(w, r)
	if err != nil {
		s.fail(w, r, http.StatusBadRequest, err.Error())
		return
	}
	text = strings.TrimSpace(text)
	if text == "" {
		s.fail(w, r, http.StatusBadRequest, "text is required")
		return
	}
	if utf8.RuneCountInString(text) > maxQuestionRunes {
		s.fail(w, r, http.StatusBadRequest, fmt.Sprintf("text is longer than %d characters", maxQuestionRunes))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), turnTimeout)
	defer cancel()
	if err := se.ensureOpening(ctx); err != nil {
		s.opts.Logger.Printf("opening for séance %s: %v", se.id, err)
		s.fail(w, r, http.StatusBadGateway, tr(s.requestLang(r), "no_answer"))
		return
	}
	res, err := se.ask(ctx, text)
	if err != nil {
		s.opts.Logger.Printf("question in séance %s: %v", se.id, err)
		s.fail(w, r, http.StatusBadGateway, tr(s.requestLang(r), "no_answer"))
		return
	}

	switch {
	case r.Header.Get("HX-Request") != "":
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, string(res.user.HTML)+res.reply.HTML)
	case wantsJSON(r):
		used, left := se.sess.BudgetUsed(), se.sess.BudgetRemaining()
		writeJSON(w, http.StatusOK, messageReply{
			Index:           res.reply.Index,
			Reply:           res.reply.Content,
			Exhausted:       res.exhausted,
			BudgetUsed:      used,
			BudgetRemaining: left,
			Doubts:          res.reply.Doubts,
		})
	default:
		http.Redirect(w, r, seancePageURL(se.id), http.StatusSeeOther)
	}
}

// postmortemReply is the 200 body of POST /api/seances/{id}/postmortem for JSON clients.
type postmortemReply struct {
	Markdown    string `json:"markdown"`
	Mode        string `json:"mode"`
	Reason      string `json:"reason"`
	DownloadURL string `json:"downloadUrl"`
	// TombstoneURL is the graveyard page of the tombstone this postmortem was
	// stored as. It is empty when the graveyard is off or the write failed.
	TombstoneURL string `json:"tombstoneUrl,omitempty"`
}

// postmortemView is the data of templates/postmortem.html.
type postmortemView struct {
	Markdown    string
	Mode        string
	Reason      string
	DownloadURL string
	// TombstoneURL links to the tombstone page. Empty means no link.
	TombstoneURL string
}

// handlePostmortem generates the postmortem again and stores it. JSON clients get
// postmortemReply, htmx requests get the panel fragment, and other clients get the
// Markdown as text/markdown.
func (s *Server) handlePostmortem(w http.ResponseWriter, r *http.Request) {
	se := s.seanceFor(w, r)
	if se == nil {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), turnTimeout)
	defer cancel()
	doc, err := se.postmortemFor(ctx, true)
	if err != nil {
		s.opts.Logger.Printf("postmortem for séance %s: %v", se.id, err)
		s.fail(w, r, http.StatusGatewayTimeout, tr(s.requestLang(r), "pm_timeout"))
		return
	}

	switch {
	case wantsJSON(r):
		writeJSON(w, http.StatusOK, postmortemReply{
			Markdown:     doc.markdown,
			Mode:         doc.mode,
			Reason:       doc.reason,
			DownloadURL:  seanceDownloadURL(se.id),
			TombstoneURL: tombstoneURL(doc.tombID),
		})
	case r.Header.Get("HX-Request") != "":
		var buf bytes.Buffer
		v := postmortemView{Markdown: doc.markdown, Mode: doc.mode, Reason: doc.reason, DownloadURL: seanceDownloadURL(se.id), TombstoneURL: tombstoneURL(doc.tombID)}
		if err := s.set(s.requestLang(r)).seance.ExecuteTemplate(&buf, "postmortem", v); err != nil {
			s.opts.Logger.Printf("render postmortem %s: %v", se.id, err)
			http.Error(w, "render failed", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(buf.Bytes())
	default:
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		_, _ = io.WriteString(w, doc.markdown)
	}
}

// handlePostmortemDownload returns the stored postmortem as a .md attachment. It
// generates one first when none exists.
func (s *Server) handlePostmortemDownload(w http.ResponseWriter, r *http.Request) {
	se := s.seanceFor(w, r)
	if se == nil {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), turnTimeout)
	defer cancel()
	doc, err := se.postmortemFor(ctx, false)
	if err != nil {
		s.opts.Logger.Printf("postmortem for séance %s: %v", se.id, err)
		s.fail(w, r, http.StatusGatewayTimeout, tr(s.requestLang(r), "pm_timeout"))
		return
	}
	name := "postmortem-" + fileSafe(se.pod) + "-" + doc.at.Format("20060102") + ".md"
	h := w.Header()
	h.Set("Content-Type", "text/markdown; charset=utf-8")
	h.Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	_, _ = io.WriteString(w, doc.markdown)
}

// fileSafe keeps ASCII letters, digits and '-', and replaces every other rune with '-'.
func fileSafe(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-':
			return r
		}
		return '-'
	}, s)
}

// handleSeancePage renders the séance page. Turns recorded so far are included in
// the HTML, so the page also works without JavaScript.
func (s *Server) handleSeancePage(w http.ResponseWriter, r *http.Request) {
	se := s.seanceFor(w, r)
	if se == nil {
		return
	}
	var buf bytes.Buffer
	lang := s.pageLang(w, r)
	if err := s.set(lang).seance.ExecuteTemplate(&buf, "layout", se.pageData(r, lang)); err != nil {
		s.opts.Logger.Printf("render séance page %s: %v", se.id, err)
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(buf.Bytes())
}

// seanceFor looks up the séance named in the path. An unknown or expired id gets a 404.
func (s *Server) seanceFor(w http.ResponseWriter, r *http.Request) *seance {
	se := s.seances.get(r.PathValue("id"))
	if se == nil {
		s.fail(w, r, http.StatusNotFound, "no such séance, or it has expired")
	}
	return se
}

// fail writes an error as JSON for JSON clients and as plain text otherwise.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, status int, msg string) {
	if wantsJSON(r) {
		writeJSON(w, status, apiError{Error: msg})
		return
	}
	http.Error(w, msg, status)
}

// readCreateRequest reads the create body from JSON or from a form.
func readCreateRequest(w http.ResponseWriter, r *http.Request) (createRequest, error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	var req createRequest
	if isJSONRequest(r) {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			return req, fmt.Errorf("invalid JSON body: %w", err)
		}
	} else {
		if err := r.ParseForm(); err != nil {
			return req, fmt.Errorf("invalid form body: %w", err)
		}
		req.NS = r.PostForm.Get("ns")
		req.Pod = r.PostForm.Get("pod")
		req.Src = r.PostForm.Get("src")
		req.Lang = r.PostForm.Get("lang")
	}
	req.NS = strings.TrimSpace(req.NS)
	req.Pod = strings.TrimSpace(req.Pod)
	req.Lang = strings.TrimSpace(req.Lang)
	return req, nil
}

// readMessageText reads the question from a JSON {text} body or a form field.
func readMessageText(w http.ResponseWriter, r *http.Request) (string, error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	if isJSONRequest(r) {
		var body struct {
			Text string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			return "", fmt.Errorf("invalid JSON body: %w", err)
		}
		return body.Text, nil
	}
	if err := r.ParseForm(); err != nil {
		return "", fmt.Errorf("invalid form body: %w", err)
	}
	return r.PostForm.Get("text"), nil
}

// isJSONRequest reports whether the body is application/json.
func isJSONRequest(r *http.Request) bool {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return err == nil && mt == "application/json"
}

// wantsJSON reports whether the client sent a JSON body or asked for JSON.
func wantsJSON(r *http.Request) bool {
	return isJSONRequest(r) || strings.Contains(r.Header.Get("Accept"), "application/json")
}

// writeEvent writes one SSE event. The data is a single line, because the JSON encoder never emits a newline.
func writeEvent(w io.Writer, ev sseEvent) error {
	_, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.name, ev.data)
	return err
}
