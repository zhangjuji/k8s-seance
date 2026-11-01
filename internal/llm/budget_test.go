package llm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/zhangjuji/k8s-seance/internal/autopsy"
)

// budgetCounter counts calls and records the last request it received.
type budgetCounter struct {
	calls   atomic.Int32
	mu      sync.Mutex
	lastReq ChatRequest
	usage   Usage
	err     error
}

func (f *budgetCounter) Stream(_ context.Context, req ChatRequest, onDelta func(string) error) (Usage, error) {
	f.calls.Add(1)
	f.mu.Lock()
	f.lastReq = req
	f.mu.Unlock()
	if f.err != nil {
		return Usage{}, f.err
	}
	if err := onDelta("ok"); err != nil {
		return Usage{}, err
	}
	return f.usage, nil
}

func (f *budgetCounter) last() ChatRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastReq
}

func budgetNoop(string) error { return nil }

func budgetCollect(t *testing.T, b *Budget, req ChatRequest) (string, Usage, error) {
	t.Helper()
	var sb strings.Builder
	u, err := b.Stream(context.Background(), req, func(s string) error {
		sb.WriteString(s)
		return nil
	})
	return sb.String(), u, err
}

func budgetReq() ChatRequest {
	return ChatRequest{Messages: []Message{{Role: "system", Content: "sys"}, {Role: "user", Content: "hi"}}}
}

func TestBudgetSeventhCallIsExhausted(t *testing.T) {
	fake := &budgetCounter{usage: Usage{InputTokens: 3, OutputTokens: 2}}
	b := NewBudget(fake, DefaultLimits())

	for i := 1; i <= 6; i++ {
		if _, _, err := budgetCollect(t, b, budgetReq()); err != nil {
			t.Fatalf("call %d: unexpected error %v", i, err)
		}
	}
	if !b.Exhausted() || b.Remaining() != 0 || b.Used() != 6 {
		t.Fatalf("after 6 calls: used=%d remaining=%d exhausted=%v", b.Used(), b.Remaining(), b.Exhausted())
	}

	before := fake.calls.Load()
	text, u, err := budgetCollect(t, b, budgetReq())
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("7th call error = %v, want ErrBudgetExhausted", err)
	}
	if text != ExhaustedLine {
		t.Errorf("7th call text = %q, want ExhaustedLine", text)
	}
	if u != (Usage{}) {
		t.Errorf("7th call usage = %+v, want zero", u)
	}
	if fake.calls.Load() != before {
		t.Errorf("inner provider was called on exhausted budget")
	}
	if b.Used() != 6 {
		t.Errorf("Used() = %d after refused call, want 6", b.Used())
	}
}

func TestBudgetExhaustedEmitsLineOnce(t *testing.T) {
	b := NewBudget(&budgetCounter{}, Limits{MaxCalls: 1})
	if _, _, err := budgetCollect(t, b, budgetReq()); err != nil {
		t.Fatal(err)
	}
	var fragments []string
	_, err := b.Stream(context.Background(), budgetReq(), func(s string) error {
		fragments = append(fragments, s)
		return nil
	})
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("err = %v", err)
	}
	if len(fragments) != 1 || fragments[0] != ExhaustedLine {
		t.Errorf("fragments = %q, want single ExhaustedLine", fragments)
	}
}

func TestBudgetExhaustedStopsOnDeltaError(t *testing.T) {
	stop := errors.New("client gone")
	b := NewBudget(&budgetCounter{}, Limits{MaxCalls: 1})
	_, _, _ = budgetCollect(t, b, budgetReq())
	_, err := b.Stream(context.Background(), budgetReq(), func(string) error { return stop })
	if !errors.Is(err, stop) {
		t.Errorf("err = %v, want the onDelta error", err)
	}
}

func TestBudgetMaxTokensClamp(t *testing.T) {
	tests := []struct {
		name string
		in   int
		want int
	}{
		{"zero becomes limit", 0, 400},
		{"above limit is clamped", 1000, 400},
		{"below limit is kept", 200, 200},
		{"negative becomes limit", -5, 400},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &budgetCounter{}
			b := NewBudget(fake, DefaultLimits())
			req := budgetReq()
			req.MaxTokens = tt.in
			if _, _, err := budgetCollect(t, b, req); err != nil {
				t.Fatal(err)
			}
			if got := fake.last().MaxTokens; got != tt.want {
				t.Errorf("MaxTokens = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestBudgetDoesNotMutateCallerRequest(t *testing.T) {
	fake := &budgetCounter{}
	b := NewBudget(fake, DefaultLimits())
	req := budgetReq()
	req.MaxTokens = 0
	if _, _, err := budgetCollect(t, b, req); err != nil {
		t.Fatal(err)
	}
	if req.MaxTokens != 0 {
		t.Errorf("caller MaxTokens changed to %d", req.MaxTokens)
	}
}

func TestBudgetInputTrimmingKeepsLastUser(t *testing.T) {
	big := strings.Repeat("字", 3000) // 3000 runes, about 1500 tokens each
	msgs := []Message{
		{Role: "system", Content: "persona"},
		{Role: "user", Content: "old question " + big},
		{Role: "assistant", Content: "old answer " + big},
		{Role: "user", Content: "old question 2 " + big},
		{Role: "assistant", Content: "old answer 2 " + big},
		{Role: "user", Content: "the latest question"},
	}
	fake := &budgetCounter{}
	b := NewBudget(fake, Limits{MaxCalls: 3, MaxInputTokens: 2000, MaxOutputTokens: 400})

	if _, _, err := budgetCollect(t, b, ChatRequest{Messages: msgs}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := fake.last().Messages
	if got[0].Role != "system" || got[0].Content != "persona" {
		t.Errorf("system message not kept: %+v", got)
	}
	if last := got[len(got)-1]; last.Content != "the latest question" {
		t.Errorf("last message = %+v, want the latest user question", last)
	}
	if n := estimateTokens(ChatRequest{Messages: got}); n > 2000 {
		t.Errorf("trimmed estimate = %d, want <= 2000", n)
	}
	if len(msgs) != 6 {
		t.Errorf("caller slice length changed")
	}
}

func TestBudgetInputTooLargeIsNotCounted(t *testing.T) {
	fake := &budgetCounter{}
	b := NewBudget(fake, Limits{MaxCalls: 3, MaxInputTokens: 10, MaxOutputTokens: 400})
	req := ChatRequest{Messages: []Message{{Role: "user", Content: strings.Repeat("x", 100)}}}

	_, _, err := budgetCollect(t, b, req)
	if !errors.Is(err, ErrInputTooLarge) {
		t.Fatalf("err = %v, want ErrInputTooLarge", err)
	}
	if fake.calls.Load() != 0 || b.Used() != 0 {
		t.Errorf("inner calls=%d used=%d, want 0 and 0", fake.calls.Load(), b.Used())
	}
}

func TestBudgetEvidenceCountsTowardInput(t *testing.T) {
	fake := &budgetCounter{}
	b := NewBudget(fake, Limits{MaxCalls: 3, MaxInputTokens: 10, MaxOutputTokens: 400})
	req := ChatRequest{
		Messages: []Message{{Role: "user", Content: "hi"}},
		Evidence: []autopsy.Evidence{{ID: "E1", Text: strings.Repeat("e", 100)}},
	}
	if _, _, err := budgetCollect(t, b, req); !errors.Is(err, ErrInputTooLarge) {
		t.Fatalf("err = %v, want ErrInputTooLarge from evidence alone", err)
	}
}

func TestBudgetErrorsStillCount(t *testing.T) {
	boom := errors.New("upstream 500")
	fake := &budgetCounter{err: boom}
	b := NewBudget(fake, Limits{MaxCalls: 2})

	for i := 0; i < 2; i++ {
		if _, _, err := budgetCollect(t, b, budgetReq()); !errors.Is(err, boom) {
			t.Fatalf("call %d: err = %v, want upstream error", i, err)
		}
	}
	if b.Used() != 2 || !b.Exhausted() {
		t.Errorf("used=%d exhausted=%v, want 2 and true", b.Used(), b.Exhausted())
	}
	if _, _, err := budgetCollect(t, b, budgetReq()); !errors.Is(err, ErrBudgetExhausted) {
		t.Errorf("err = %v, want ErrBudgetExhausted", err)
	}
}

func TestBudgetCountsMockAndReplayProviders(t *testing.T) {
	tests := []struct {
		name string
		p    Provider
	}{
		{"mock", MockProvider{}},
		{"replay", fakeReplayLike{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := NewBudget(tt.p, Limits{MaxCalls: 1})
			if _, _, err := budgetCollect(t, b, budgetReq()); err != nil {
				t.Fatal(err)
			}
			if b.Used() != 1 {
				t.Errorf("Used() = %d, want 1", b.Used())
			}
		})
	}
}

// fakeReplayLike stands in for ReplayProvider, which lands in task 4.4.
type fakeReplayLike struct{}

func (fakeReplayLike) Stream(_ context.Context, _ ChatRequest, onDelta func(string) error) (Usage, error) {
	return Usage{InputTokens: 1, OutputTokens: 1}, onDelta("replayed")
}

func TestBudgetTotalsAccumulate(t *testing.T) {
	fake := &budgetCounter{usage: Usage{InputTokens: 7, OutputTokens: 3}}
	b := NewBudget(fake, DefaultLimits())
	for i := 0; i < 3; i++ {
		if _, _, err := budgetCollect(t, b, budgetReq()); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := b.Totals(), (Usage{InputTokens: 21, OutputTokens: 9}); got != want {
		t.Errorf("Totals() = %+v, want %+v", got, want)
	}
}

func TestBudgetZeroFieldsFallBackToDefaults(t *testing.T) {
	fake := &budgetCounter{}
	b := NewBudget(fake, Limits{})
	if b.Remaining() != 6 {
		t.Errorf("Remaining() = %d, want default 6", b.Remaining())
	}
	req := budgetReq()
	req.MaxTokens = 1000
	if _, _, err := budgetCollect(t, b, req); err != nil {
		t.Fatal(err)
	}
	if got := fake.last().MaxTokens; got != 400 {
		t.Errorf("MaxTokens = %d, want default 400", got)
	}
}

func TestLimitsFromEnv(t *testing.T) {
	tests := []struct {
		name string
		env  string
		want int
	}{
		{"unset keeps default", "", 6},
		{"positive overrides", "3", 3},
		{"whitespace is trimmed", "  9 ", 9},
		{"zero is ignored", "0", 6},
		{"negative is ignored", "-2", 6},
		{"non-number is ignored", "many", 6},
		{"float is ignored", "2.5", 6},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(EnvMaxCalls, tt.env)
			l := LimitsFromEnv()
			if l.MaxCalls != tt.want {
				t.Errorf("MaxCalls = %d, want %d", l.MaxCalls, tt.want)
			}
			if l.MaxInputTokens != 6000 || l.MaxOutputTokens != 400 {
				t.Errorf("other limits changed: %+v", l)
			}
		})
	}
}

func TestBudgetConcurrentCallsRespectLimit(t *testing.T) {
	const max = 5
	const workers = 40
	fake := &budgetCounter{usage: Usage{InputTokens: 1, OutputTokens: 1}}
	b := NewBudget(fake, Limits{MaxCalls: max})

	var wg sync.WaitGroup
	var ok, refused atomic.Int32
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := b.Stream(context.Background(), budgetReq(), budgetNoop)
			switch {
			case err == nil:
				ok.Add(1)
			case errors.Is(err, ErrBudgetExhausted):
				refused.Add(1)
			default:
				t.Errorf("worker %d: unexpected error %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	if ok.Load() != max || refused.Load() != workers-max {
		t.Errorf("ok=%d refused=%d, want %d and %d", ok.Load(), refused.Load(), max, workers-max)
	}
	if fake.calls.Load() != max {
		t.Errorf("inner calls = %d, want %d", fake.calls.Load(), max)
	}
	if got := b.Totals(); got.InputTokens != max {
		t.Errorf("Totals().InputTokens = %d, want %d", got.InputTokens, max)
	}
}

func TestOpeningCache(t *testing.T) {
	var c OpeningCache
	k := OpeningKey{PodUID: "uid-1", RestartCount: 0, Model: "m"}

	if _, ok := c.Get(k); ok {
		t.Fatal("empty cache returned a hit")
	}
	c.Put(k, "opening")
	if got, ok := c.Get(k); !ok || got != "opening" {
		t.Fatalf("Get = %q, %v; want opening, true", got, ok)
	}

	misses := []OpeningKey{
		{PodUID: "uid-2", RestartCount: 0, Model: "m"},
		{PodUID: "uid-1", RestartCount: 1, Model: "m"},
		{PodUID: "uid-1", RestartCount: 0, Model: "other"},
	}
	for _, m := range misses {
		if _, ok := c.Get(m); ok {
			t.Errorf("key %+v: unexpected hit", m)
		}
	}

	c.Put(k, "replaced")
	if got, _ := c.Get(k); got != "replaced" {
		t.Errorf("after Put, Get = %q, want replaced", got)
	}
}

func TestOpeningCacheConcurrent(t *testing.T) {
	var c OpeningCache
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			k := OpeningKey{PodUID: fmt.Sprint(i % 4)}
			c.Put(k, "x")
			c.Get(k)
		}(i)
	}
	wg.Wait()
}

func TestTrimHistory(t *testing.T) {
	round := func(n int) []Message {
		return []Message{
			{Role: "user", Content: fmt.Sprintf("q%d", n)},
			{Role: "assistant", Content: fmt.Sprintf("a%d", n)},
		}
	}
	six := []Message{{Role: "system", Content: "persona"}}
	for i := 1; i <= 6; i++ {
		six = append(six, round(i)...)
	}

	tests := []struct {
		name   string
		msgs   []Message
		rounds int
		want   []string
	}{
		{
			name:   "six rounds trimmed to four with system kept",
			msgs:   six,
			rounds: HistoryRounds,
			want:   []string{"persona", "q3", "a3", "q4", "a4", "q5", "a5", "q6", "a6"},
		},
		{
			name:   "fewer rounds than limit keeps all",
			msgs:   append([]Message{{Role: "system", Content: "persona"}}, round(1)...),
			rounds: HistoryRounds,
			want:   []string{"persona", "q1", "a1"},
		},
		{
			name:   "zero rounds keeps only system",
			msgs:   six,
			rounds: 0,
			want:   []string{"persona"},
		},
		{
			name:   "trailing unanswered question counts as a round",
			msgs:   append(append([]Message{}, six[:1]...), append(round(1), Message{Role: "user", Content: "q2"})...),
			rounds: 1,
			want:   []string{"persona", "q2"},
		},
		{
			name:   "empty input",
			msgs:   nil,
			rounds: HistoryRounds,
			want:   nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := TrimHistory(tt.msgs, tt.rounds)
			var contents []string
			for _, m := range got {
				contents = append(contents, m.Content)
			}
			if fmt.Sprint(contents) != fmt.Sprint(tt.want) {
				t.Errorf("TrimHistory = %v, want %v", contents, tt.want)
			}
		})
	}
}
