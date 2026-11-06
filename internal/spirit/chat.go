package spirit

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/zhangjuji/k8s-seance/internal/autopsy"
	"github.com/zhangjuji/k8s-seance/internal/llm"
)

const (
	// openingRequest is the user message that asks the ghost for its opening statement.
	openingRequest = "请做开场陈述。"
	// openingRequestEN is openingRequest for an English séance.
	openingRequestEN = "Please give your opening statement."
	// englishFixtureSuffix is added to the pod name to form the replay key of an
	// English séance, so its recordings live in fixtures/replay/<pod>-en.json.
	englishFixtureSuffix = "-en"
	// maxReplyTokens is the output limit requested for each spirit reply.
	maxReplyTokens = 400
	// roleSpirit and roleUser are the Turn.Role values.
	roleSpirit = "spirit"
	roleUser   = "user"
)

// Turn is one line of a séance transcript. Role is "spirit" or "user".
type Turn struct {
	Role    string
	Content string
	At      time.Time
}

// Session is one séance: a ghost bound to one Pod snapshot, with its own call
// budget and transcript. It is safe for concurrent use. Turns (the opening and
// each question) run one at a time, and the accessors can be called during a turn.
type Session struct {
	ID       string
	Autopsy  *autopsy.PodAutopsy
	Class    autopsy.Classification
	Evidence []autopsy.Evidence
	Model    string
	// Lang is the séance language, llm.LangZH or llm.LangEN. It is fixed when
	// the session starts.
	Lang string

	budget    *llm.Budget
	cache     *llm.OpeningCache
	system    string
	promptErr error
	deathTime string

	// turnMu serializes turns, so two requests cannot both spend the opening.
	turnMu sync.Mutex

	// mu guards the fields below.
	mu         sync.Mutex
	opening    string
	hasOpening bool
	transcript []Turn
	next       int // index of the next follow-up, starting at 1
}

// NewSession starts a Chinese séance for the Pod a. It is NewSessionIn with llm.LangZH.
func NewSession(id string, a *autopsy.PodAutopsy, cls autopsy.Classification, ev []autopsy.Evidence, p llm.Provider, limits llm.Limits, cache *llm.OpeningCache, model string) *Session {
	return NewSessionIn(llm.LangZH, id, a, cls, ev, p, limits, cache, model)
}

// NewSessionIn starts a séance for the Pod a in lang. A lang that
// llm.NormalizeLang does not recognize means llm.LangZH. Calls go through a
// Budget built from p and limits. cache may be nil, in which case openings are
// never reused. model and lang are part of the opening cache key.
//
// An English séance sends an English opening request, adds an English language
// rule to the system prompt and uses <pod>-en as its replay key. A Chinese
// séance is unchanged.
func NewSessionIn(lang, id string, a *autopsy.PodAutopsy, cls autopsy.Classification, ev []autopsy.Evidence, p llm.Provider, limits llm.Limits, cache *llm.OpeningCache, model string) *Session {
	if lang = llm.NormalizeLang(lang); lang == "" {
		lang = llm.LangZH
	}
	s := &Session{
		ID:       id,
		Autopsy:  a,
		Class:    cls,
		Evidence: ev,
		Model:    model,
		Lang:     lang,
		budget:   llm.NewBudget(p, limits),
		cache:    cache,
		next:     1,
	}
	if t, ok := DeathTime(a); ok {
		s.deathTime = FormatDeathTime(t)
	}

	var name, namespace string
	if a != nil {
		name, namespace = a.Name, a.Namespace
	}
	s.system, s.promptErr = SystemPrompt(PromptData{
		PodName:   name,
		Namespace: namespace,
		Cause:     cls.Cause,
		DeathTime: s.deathTime,
		Evidence:  ev,
		Lang:      lang,
	})
	return s
}

// Opening returns the ghost's opening statement. When the cache already has an
// opening for this Pod's UID, the highest RestartCount and the model, the cached
// text is used and no budget is spent. Otherwise the opening is generated. The
// text is sent to onDelta as it arrives, and a second call returns the stored
// opening without a new call. onDelta may be nil.
func (s *Session) Opening(ctx context.Context, onDelta func(string) error) (string, error) {
	s.turnMu.Lock()
	defer s.turnMu.Unlock()

	s.mu.Lock()
	if s.hasOpening {
		text := s.opening
		s.mu.Unlock()
		return text, emit(onDelta, text)
	}
	s.mu.Unlock()

	key := s.cacheKey()
	if s.cache != nil {
		if text, ok := s.cache.Get(key); ok {
			s.finishOpening(text, false)
			return text, emit(onDelta, text)
		}
	}

	if s.promptErr != nil {
		return "", s.promptErr
	}
	messages := []llm.Message{
		{Role: "system", Content: s.system},
		{Role: "user", Content: s.openingRequest()},
	}
	text, exhausted, err := s.generate(ctx, s.request(messages, llm.PurposeOpening, 0), onDelta)
	if err != nil {
		return "", err
	}
	s.finishOpening(text, exhausted)
	if !exhausted && s.cache != nil {
		s.cache.Put(key, text)
	}
	return text, nil
}

// Ask answers one question from the user. It streams the reply to onDelta and
// returns the full text. When the budget is spent, the reply is
// llm.ExhaustedLineFor(s.Lang) and the error is nil. The question must not be empty.
func (s *Session) Ask(ctx context.Context, question string, onDelta func(string) error) (string, error) {
	question = strings.TrimSpace(question)
	if question == "" {
		return "", errors.New("spirit: empty question")
	}
	if s.promptErr != nil {
		return "", s.promptErr
	}

	s.turnMu.Lock()
	defer s.turnMu.Unlock()

	s.mu.Lock()
	turn := s.next
	s.next++
	history := s.historyLocked()
	s.transcript = append(s.transcript, Turn{Role: roleUser, Content: question, At: time.Now()})
	s.mu.Unlock()

	messages := make([]llm.Message, 0, 2+2*llm.HistoryRounds)
	messages = append(messages, llm.Message{Role: "system", Content: s.system})
	messages = append(messages, llm.TrimHistory(history, llm.HistoryRounds)...)
	messages = append(messages, llm.Message{Role: "user", Content: question})

	text, _, err := s.generate(ctx, s.request(messages, llm.PurposeChat, turn), onDelta)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		// Drop the question so the transcript does not hold a question with no answer.
		s.transcript = s.transcript[:len(s.transcript)-1]
		return "", err
	}
	s.transcript = append(s.transcript, Turn{Role: roleSpirit, Content: text, At: time.Now()})
	return text, nil
}

// Provider returns the budget-wrapped provider of this session. Calls made through
// it count against the séance budget, so the postmortem call uses the same limit.
func (s *Session) Provider() llm.Provider {
	return s.budget
}

// Transcript returns a copy of the turns so far, oldest first.
func (s *Session) Transcript() []Turn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Turn(nil), s.transcript...)
}

// BudgetUsed returns the number of LLM calls this session has made.
func (s *Session) BudgetUsed() int {
	return s.budget.Used()
}

// BudgetRemaining returns how many LLM calls this session can still make.
func (s *Session) BudgetRemaining() int {
	return s.budget.Remaining()
}

// Exhausted reports whether this session has no calls left.
func (s *Session) Exhausted() bool {
	return s.budget.Exhausted()
}

// generate streams one reply through the budget. exhausted is true when the budget
// refused the call. The text is then llm.ExhaustedLineFor(s.Lang), which the
// budget has already sent to onDelta, and the error is nil.
func (s *Session) generate(ctx context.Context, req llm.ChatRequest, onDelta func(string) error) (text string, exhausted bool, err error) {
	var sb strings.Builder
	_, err = s.budget.Stream(ctx, req, func(part string) error {
		sb.WriteString(part)
		return emit(onDelta, part)
	})
	switch {
	case errors.Is(err, llm.ErrBudgetExhausted):
		return llm.ExhaustedLineFor(s.Lang), true, nil
	case err != nil:
		return "", false, err
	}
	return sb.String(), false, nil
}

// request builds the provider request for one turn of this session.
func (s *Session) request(messages []llm.Message, purpose string, turn int) llm.ChatRequest {
	var name string
	if s.Autopsy != nil {
		name = s.Autopsy.Name
	}
	return llm.ChatRequest{
		Messages:  messages,
		Cause:     s.Class.Cause,
		PodName:   name,
		Evidence:  s.Evidence,
		MaxTokens: maxReplyTokens,
		FixtureID: FixtureID(name, s.Lang),
		TurnIndex: turn,
		Purpose:   purpose,
		DeathTime: s.deathTime,
		Lang:      s.Lang,
	}
}

// FixtureID returns the replay key of a séance with the Pod podName in lang:
// the pod name for Chinese, and the pod name with "-en" for English. An empty
// pod name stays empty, so replay is not attempted.
func FixtureID(podName, lang string) string {
	if podName == "" || llm.NormalizeLang(lang) != llm.LangEN {
		return podName
	}
	return podName + englishFixtureSuffix
}

// openingRequest returns the user message that asks for the opening in the
// séance language.
func (s *Session) openingRequest() string {
	if s.Lang == llm.LangEN {
		return openingRequestEN
	}
	return openingRequest
}

// cacheKey returns the opening cache key: the Pod UID, the highest container
// RestartCount, the model and the séance language.
func (s *Session) cacheKey() llm.OpeningKey {
	k := llm.OpeningKey{Model: s.Model, Lang: s.Lang}
	if s.Autopsy == nil {
		return k
	}
	k.PodUID = s.Autopsy.UID
	for _, c := range s.Autopsy.Containers {
		if c.RestartCount > k.RestartCount {
			k.RestartCount = c.RestartCount
		}
	}
	return k
}

// finishOpening stores the opening text and adds it to the transcript. A refused
// opening (exhausted) is recorded but not kept as the session opening, so a later
// call can try again.
func (s *Session) finishOpening(text string, exhausted bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.transcript = append(s.transcript, Turn{Role: roleSpirit, Content: text, At: time.Now()})
	if !exhausted {
		s.opening = text
		s.hasOpening = true
	}
}

// historyLocked converts the transcript to chat messages. The caller must hold mu.
func (s *Session) historyLocked() []llm.Message {
	out := make([]llm.Message, 0, len(s.transcript))
	for _, t := range s.transcript {
		role := "assistant"
		if t.Role == roleUser {
			role = "user"
		}
		out = append(out, llm.Message{Role: role, Content: t.Content})
	}
	return out
}

// emit sends text to onDelta when it is set.
func emit(onDelta func(string) error, text string) error {
	if onDelta == nil {
		return nil
	}
	return onDelta(text)
}
