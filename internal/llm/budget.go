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
	defaultMaxCalls        = 6
	defaultMaxInputTokens  = 6000
	defaultMaxOutputTokens = 400
)

// EnvMaxCalls overrides Limits.MaxCalls when it holds a positive integer.
const EnvMaxCalls = "SEANCE_BUDGET_MAX_CALLS"

// HistoryRounds is how many user/assistant rounds TrimHistory keeps.
const HistoryRounds = 4

// ExhaustedLine is the ghost's line once the call budget is spent.
const ExhaustedLine = "仪式之力已耗尽，亡灵沉入黑暗……（本次降灵会的 LLM 调用额度已用完，你仍可以生成复盘。）"

// ErrBudgetExhausted is returned by Budget.Stream once MaxCalls calls have been used.
var ErrBudgetExhausted = errors.New("llm: budget exhausted")

// ErrInputTooLarge is returned when the input still exceeds MaxInputTokens after
// older history has been trimmed. The inner provider is not called and the call is not counted.
var ErrInputTooLarge = errors.New("llm: input exceeds per-call token limit")

// Limits caps one séance. A zero or negative field in NewBudget falls back to its default.
type Limits struct {
	MaxCalls        int
	MaxInputTokens  int
	MaxOutputTokens int
}

// DefaultLimits returns 6 calls, 6000 input tokens and 400 output tokens per call.
func DefaultLimits() Limits {
	return Limits{
		MaxCalls:        defaultMaxCalls,
		MaxInputTokens:  defaultMaxInputTokens,
		MaxOutputTokens: defaultMaxOutputTokens,
	}
}

// LimitsFromEnv returns DefaultLimits with MaxCalls set from SEANCE_BUDGET_MAX_CALLS.
// Values that are not positive integers are ignored.
func LimitsFromEnv() Limits {
	l := DefaultLimits()
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv(EnvMaxCalls))); err == nil && n > 0 {
		l.MaxCalls = n
	}
	return l
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
	if l.MaxOutputTokens <= 0 {
		l.MaxOutputTokens = d.MaxOutputTokens
	}
	return &Budget{inner: p, limits: l}
}

// Stream implements Provider. When the budget is spent it emits ExhaustedLine as a
// single fragment and returns ErrBudgetExhausted without calling the inner provider.
// Otherwise it trims the request to the input limit, clamps MaxTokens to the output
// limit, counts the call and delegates. If onDelta fails, that error is returned.
func (b *Budget) Stream(ctx context.Context, req ChatRequest, onDelta func(string) error) (Usage, error) {
	if b.Exhausted() {
		return b.exhaustedReply(onDelta)
	}
	prepared, err := b.prepare(req)
	if err != nil {
		return Usage{}, err
	}
	if !b.claim() {
		// Another call used the last slot while this one was preparing.
		return b.exhaustedReply(onDelta)
	}

	usage, err := b.inner.Stream(ctx, prepared, onDelta)

	b.mu.Lock()
	b.totals.InputTokens += usage.InputTokens
	b.totals.OutputTokens += usage.OutputTokens
	b.mu.Unlock()
	return usage, err
}

func (b *Budget) exhaustedReply(onDelta func(string) error) (Usage, error) {
	if onDelta != nil {
		if err := onDelta(ExhaustedLine); err != nil {
			return Usage{}, err
		}
	}
	return Usage{}, ErrBudgetExhausted
}

// prepare trims history until the estimated input fits MaxInputTokens and clamps
// MaxTokens. It returns ErrInputTooLarge when the evidence and the last user message
// alone do not fit.
func (b *Budget) prepare(req ChatRequest) (ChatRequest, error) {
	msgs := trimToFit(req, b.limits.MaxInputTokens)
	req.Messages = msgs
	if estimateTokens(req) > b.limits.MaxInputTokens {
		return ChatRequest{}, ErrInputTooLarge
	}
	if req.MaxTokens <= 0 || req.MaxTokens > b.limits.MaxOutputTokens {
		req.MaxTokens = b.limits.MaxOutputTokens
	}
	return req, nil
}

// claim reserves one call slot. It returns false when the budget is spent.
func (b *Budget) claim() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.used >= b.limits.MaxCalls {
		return false
	}
	b.used++
	return true
}

// Used returns the number of calls counted so far.
func (b *Budget) Used() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used
}

// Remaining returns how many calls are left. It is never negative.
func (b *Budget) Remaining() int {
	b.mu.Lock()
	defer b.mu.Unlock()
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
// so a new life gets a new opening.
type OpeningKey struct {
	PodUID       string
	RestartCount int32
	Model        string
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
