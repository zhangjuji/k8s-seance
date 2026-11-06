package llm

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
)

// Default per-séance limits from design.md (llm/budget.go).
const (
	defaultMaxCalls                  = 6
	defaultMaxInputTokens            = 6000
	defaultMaxTotalInputTokens       = 30000
	defaultMaxOutputTokens           = 400
	defaultMaxPostmortemOutputTokens = 1500
)

// EnvMaxCalls overrides Limits.MaxCalls when it holds a positive integer.
const EnvMaxCalls = "SEANCE_BUDGET_MAX_CALLS"

// EnvMaxTotalInputTokens overrides Limits.MaxTotalInputTokens when it holds a
// positive integer.
const EnvMaxTotalInputTokens = "SEANCE_BUDGET_MAX_TOTAL_INPUT_TOKENS"

// HistoryRounds is how many user/assistant rounds TrimHistory keeps.
const HistoryRounds = 4

// ExhaustedLine is the ghost's line once the call budget is spent, in Chinese.
const ExhaustedLine = "仪式之力已耗尽，亡灵沉入黑暗……（本次降灵会的 LLM 调用额度已用完，你仍可以生成复盘。）"

// ExhaustedLineEN is ExhaustedLine for an English séance.
const ExhaustedLineEN = "The ritual's power is spent, and the spirit sinks back into the dark... (This séance has used all of its LLM calls. You can still generate the postmortem.)"

// ExhaustedLineFor returns the exhausted line for a séance language. Anything
// other than LangEN gets the Chinese line.
func ExhaustedLineFor(lang string) string {
	if langOrDefault(lang) == LangEN {
		return ExhaustedLineEN
	}
	return ExhaustedLine
}

// IsExhaustedLine reports whether s is the exhausted line in any language.
func IsExhaustedLine(s string) bool {
	return s == ExhaustedLine || s == ExhaustedLineEN
}

// ErrBudgetExhausted is returned by Budget.Stream once MaxCalls calls have been
// used, or once the séance's total input would pass MaxTotalInputTokens.
var ErrBudgetExhausted = errors.New("llm: budget exhausted")

// ErrInputTooLarge is returned when the input still exceeds MaxInputTokens after
// older history has been trimmed. The inner provider is not called and the call is not counted.
var ErrInputTooLarge = errors.New("llm: input exceeds per-call token limit")

// Limits caps one séance. A zero or negative field in NewBudget falls back to its default.
//
// MaxOutputTokens applies to every call except the postmortem call, which is
// identified by TurnIndex == PostmortemTurnIndex and capped by
// MaxPostmortemOutputTokens instead. Both count as one call against MaxCalls.
//
// MaxInputTokens caps one call. MaxTotalInputTokens caps the estimated input of
// all calls of the séance together (spec §7.8: at most 30k per séance).
type Limits struct {
	MaxCalls                  int
	MaxInputTokens            int
	MaxTotalInputTokens       int
	MaxOutputTokens           int
	MaxPostmortemOutputTokens int
}

// DefaultLimits returns 6 calls, 6000 input tokens per call, 30000 input tokens
// per séance, 400 output tokens per chat call and 1500 output tokens for the
// postmortem call.
func DefaultLimits() Limits {
	return Limits{
		MaxCalls:                  defaultMaxCalls,
		MaxInputTokens:            defaultMaxInputTokens,
		MaxTotalInputTokens:       defaultMaxTotalInputTokens,
		MaxOutputTokens:           defaultMaxOutputTokens,
		MaxPostmortemOutputTokens: defaultMaxPostmortemOutputTokens,
	}
}

// LimitsFromEnv returns DefaultLimits with MaxCalls set from
// SEANCE_BUDGET_MAX_CALLS and MaxTotalInputTokens from
// SEANCE_BUDGET_MAX_TOTAL_INPUT_TOKENS. Values that are not positive integers
// are ignored.
func LimitsFromEnv() Limits {
	l := DefaultLimits()
	if n, ok := positiveEnv(EnvMaxCalls); ok {
		l.MaxCalls = n
	}
	if n, ok := positiveEnv(EnvMaxTotalInputTokens); ok {
		l.MaxTotalInputTokens = n
	}
	return l
}

// positiveEnv reads a positive integer from the environment variable name.
func positiveEnv(name string) (int, bool) {
	n, err := strconv.Atoi(strings.TrimSpace(os.Getenv(name)))
	return n, err == nil && n > 0
}

// Budget wraps a Provider and enforces Limits. It is safe for concurrent use.
// Calls are counted when they are made, including calls that fail, and also for
// mock and replay providers, so the limits can be tested without spending money.
type Budget struct {
	inner  Provider
	limits Limits

	mu     sync.Mutex
	used   int
	totals Usage
	// inputUsed is the input counted against MaxTotalInputTokens: for each call,
	// the larger of the estimate and the input tokens the provider reported.
	inputUsed int
	// drained is set once a call was refused because the séance's total input
	// was spent. No call is made after that.
	drained bool
}

var _ Provider = (*Budget)(nil)

// NewBudget wraps p with l. Zero or negative fields of l are replaced by the defaults.
func NewBudget(p Provider, l Limits) *Budget {
	d := DefaultLimits()
	if l.MaxCalls <= 0 {
		l.MaxCalls = d.MaxCalls
	}
	if l.MaxInputTokens <= 0 {
		l.MaxInputTokens = d.MaxInputTokens
	}
	if l.MaxTotalInputTokens <= 0 {
		l.MaxTotalInputTokens = d.MaxTotalInputTokens
	}
	if l.MaxOutputTokens <= 0 {
		l.MaxOutputTokens = d.MaxOutputTokens
	}
	if l.MaxPostmortemOutputTokens <= 0 {
		l.MaxPostmortemOutputTokens = d.MaxPostmortemOutputTokens
	}
	return &Budget{inner: p, limits: l}
}

// Stream implements Provider. When the budget is spent it emits
// ExhaustedLineFor(req.Lang) as a single fragment and returns
// ErrBudgetExhausted without calling the inner provider.
// Otherwise it trims the request to the input limit of the call (the smaller of
// MaxInputTokens and what is left of MaxTotalInputTokens), clamps MaxTokens to
// the output limit for the call, counts the call and its input, and delegates.
// A request that fits MaxInputTokens but not the rest of the séance's total
// input spends the budget: the call is refused as exhausted. If onDelta fails,
// that error is returned.
func (b *Budget) Stream(ctx context.Context, req ChatRequest, onDelta func(string) error) (Usage, error) {
	if b.Exhausted() {
		return b.exhaustedReply(req.Lang, onDelta)
	}
	prepared, estimate, err := b.prepare(req)
	if errors.Is(err, errTotalInputSpent) {
		b.mu.Lock()
		b.drained = true
		b.mu.Unlock()
		return b.exhaustedReply(req.Lang, onDelta)
	}
	if err != nil {
		return Usage{}, err
	}
	if !b.claim(estimate) {
		// Another call used the last slot, or the rest of the input, while this
		// one was preparing.
		return b.exhaustedReply(req.Lang, onDelta)
	}

	usage, err := b.inner.Stream(ctx, prepared, onDelta)

	b.mu.Lock()
	b.totals.InputTokens += usage.InputTokens
	b.totals.OutputTokens += usage.OutputTokens
	if usage.InputTokens > estimate {
		b.inputUsed += usage.InputTokens - estimate
	}
	b.mu.Unlock()
	return usage, err
}

func (b *Budget) exhaustedReply(lang string, onDelta func(string) error) (Usage, error) {
	if onDelta != nil {
		if err := onDelta(ExhaustedLineFor(lang)); err != nil {
			return Usage{}, err
		}
	}
	return Usage{}, ErrBudgetExhausted
}

// errTotalInputSpent is returned by prepare when a request fits the per-call
// limit but not what is left of the séance's total input.
var errTotalInputSpent = errors.New("llm: séance input budget spent")

// prepare trims history until the estimated input fits the call's input limit
// and clamps MaxTokens to the output limit of the call. It returns the prepared
// request and its estimated input. It returns ErrInputTooLarge when the
// evidence and the last user message alone do not fit MaxInputTokens, and
// errTotalInputSpent when they fit that but not the rest of the total.
func (b *Budget) prepare(req ChatRequest) (ChatRequest, int, error) {
	limit := min(b.limits.MaxInputTokens, b.InputRemaining())
	msgs := trimToFit(req, limit)
	req.Messages = msgs
	est := estimateTokens(req)
	if est > limit {
		if est > b.limits.MaxInputTokens {
			return ChatRequest{}, 0, ErrInputTooLarge
		}
		return ChatRequest{}, 0, errTotalInputSpent
	}
	out := b.limits.MaxOutputTokens
	if req.TurnIndex == PostmortemTurnIndex {
		out = b.limits.MaxPostmortemOutputTokens
	}
	if req.MaxTokens <= 0 || req.MaxTokens > out {
		req.MaxTokens = out
	}
	return req, est, nil
}

// claim reserves one call slot and input tokens for it. It returns false when
// the budget is spent or the input no longer fits.
func (b *Budget) claim(input int) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.drained || b.used >= b.limits.MaxCalls {
		return false
	}
	if b.inputUsed+input > b.limits.MaxTotalInputTokens {
		b.drained = true
		return false
	}
	b.used++
	b.inputUsed += input
	return true
}

// InputUsed returns the input tokens counted against MaxTotalInputTokens.
func (b *Budget) InputUsed() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.inputUsed
}

// InputRemaining returns how much of MaxTotalInputTokens is left. It is never negative.
func (b *Budget) InputRemaining() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return max(0, b.limits.MaxTotalInputTokens-b.inputUsed)
}

// Used returns the number of calls counted so far.
func (b *Budget) Used() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used
}

// Remaining returns how many calls are left. It is never negative, and it is
// 0 once a call was refused because the séance's total input was spent.
func (b *Budget) Remaining() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.drained {
		return 0
	}
	return max(0, b.limits.MaxCalls-b.used)
}

// Exhausted reports whether no calls are left.
func (b *Budget) Exhausted() bool {
	return b.Remaining() == 0
}

// Totals returns the token usage the inner provider reported, summed over all calls.
func (b *Budget) Totals() Usage {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.totals
}

// estimateTokens approximates the input size as ceil(runes / 2) over all message
// contents and evidence texts. Roles and the request's other fields are not counted.
// The estimate is deliberately rough and errs on the high side for CJK text.
func estimateTokens(req ChatRequest) int {
	runes := 0
	for _, m := range req.Messages {
		runes += utf8.RuneCountInString(m.Content)
	}
	for _, e := range req.Evidence {
		runes += utf8.RuneCountInString(e.Text)
	}
	return (runes + 1) / 2
}

// trimToFit drops the oldest non-system messages until the estimate is within
// limit. The last user message is never dropped. It returns a new slice and does
// not modify req.Messages.
func trimToFit(req ChatRequest, limit int) []Message {
	msgs := append([]Message(nil), req.Messages...)
	for estimateTokens(ChatRequest{Messages: msgs, Evidence: req.Evidence}) > limit {
		idx := droppableIndex(msgs)
		if idx < 0 {
			break
		}
		msgs = append(msgs[:idx], msgs[idx+1:]...)
	}
	return msgs
}

// droppableIndex returns the index of the oldest non-system message that comes
// before the last user message, or -1 if there is none.
func droppableIndex(msgs []Message) int {
	lastUser := -1
	for i, m := range msgs {
		if m.Role == "user" {
			lastUser = i
		}
	}
	for i, m := range msgs {
		if lastUser >= 0 && i >= lastUser {
			return -1
		}
		if m.Role != "system" {
			return i
		}
	}
	return -1
}

// OpeningKey identifies a cached opening line. Restarting a Pod changes RestartCount,
// so a new life gets a new opening. Lang keeps the Chinese and English openings apart.
type OpeningKey struct {
	PodUID       string
	RestartCount int32
	Model        string
	Lang         string
}

// OpeningCache stores opening lines by key. A cached opening is replayed by the caller
// without spending budget. The caller is the séance session code (task 5.2).
// The zero value is ready to use and the cache is safe for concurrent use.
type OpeningCache struct {
	mu sync.Mutex
	m  map[OpeningKey]string
}

// Get returns the cached opening for k, if any.
func (c *OpeningCache) Get(k OpeningKey) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	text, ok := c.m[k]
	return text, ok
}

// Put stores the opening text for k, replacing any earlier entry.
func (c *OpeningCache) Put(k OpeningKey, text string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = make(map[OpeningKey]string)
	}
	c.m[k] = text
}

// TrimHistory keeps every system message and only the last `rounds` user/assistant
// rounds. A round starts at a user message and includes the assistant reply after it.
// If there are fewer rounds than requested, all non-system messages are kept.
// The result keeps the original order and is a new slice.
func TrimHistory(msgs []Message, rounds int) []Message {
	if rounds < 0 {
		rounds = 0
	}
	// Find where the kept rounds start: the rounds-th user message from the end.
	start := len(msgs)
	users := 0
	for i := len(msgs) - 1; i >= 0 && users < rounds; i-- {
		if msgs[i].Role == "user" {
			users++
			start = i
		}
	}
	if users < rounds {
		// Fewer user messages than rounds: keep everything from the beginning.
		start = 0
	}

	out := make([]Message, 0, len(msgs))
	for i, m := range msgs {
		if m.Role == "system" || i >= start {
			out = append(out, m)
		}
	}
	return out
}
