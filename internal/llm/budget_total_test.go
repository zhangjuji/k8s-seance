package llm

import (
	"errors"
	"strings"
	"testing"
)

// sized returns a request whose estimated input is tokens (2 runes per token).
func sized(tokens int) ChatRequest {
	return ChatRequest{Messages: []Message{{Role: "user", Content: strings.Repeat("x", 2*tokens)}}}
}

func TestDefaultLimitsCapTotalInputAt30k(t *testing.T) {
	l := DefaultLimits()
	if l.MaxTotalInputTokens != 30000 || l.MaxInputTokens != 6000 {
		t.Fatalf("limits = %+v, want 30000 total and 6000 per call", l)
	}
	if NewBudget(&budgetCounter{}, Limits{}).limits.MaxTotalInputTokens != 30000 {
		t.Fatal("zero MaxTotalInputTokens does not fall back to 30000")
	}
}

func TestBudgetTotalInputCapRefusesAsExhausted(t *testing.T) {
	fake := &budgetCounter{}
	b := NewBudget(fake, Limits{MaxCalls: 10, MaxInputTokens: 6000, MaxTotalInputTokens: 30000})
	// Five calls of 5900 tokens use 29500 of 30000.
	for i := range 5 {
		if _, _, err := budgetCollect(t, b, sized(5900)); err != nil {
			t.Fatalf("call %d: %v", i+1, err)
		}
	}
	if got := b.InputUsed(); got != 29500 {
		t.Fatalf("InputUsed = %d, want 29500", got)
	}
	if got := b.InputRemaining(); got != 500 {
		t.Fatalf("InputRemaining = %d, want 500", got)
	}
	// The sixth call fits the per-call 6000 but not the 500 left.
	text, _, err := budgetCollect(t, b, sized(600))
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("err = %v, want ErrBudgetExhausted", err)
	}
	if text != ExhaustedLine {
		t.Errorf("text = %q, want the exhausted line", text)
	}
	if fake.calls.Load() != 5 || b.Used() != 5 {
		t.Errorf("inner calls=%d used=%d, want 5 and 5", fake.calls.Load(), b.Used())
	}
	if !b.Exhausted() || b.Remaining() != 0 {
		t.Errorf("exhausted=%v remaining=%d, want true and 0 once the input is spent", b.Exhausted(), b.Remaining())
	}
	// Even a tiny request is refused afterwards.
	if _, _, err := budgetCollect(t, b, sized(1)); !errors.Is(err, ErrBudgetExhausted) {
		t.Errorf("tiny call after drain: err = %v, want ErrBudgetExhausted", err)
	}
	if fake.calls.Load() != 5 {
		t.Errorf("inner calls = %d after drain, want 5", fake.calls.Load())
	}
}

func TestBudgetPerCallLimitStillWins(t *testing.T) {
	fake := &budgetCounter{}
	b := NewBudget(fake, Limits{MaxCalls: 3, MaxInputTokens: 6000, MaxTotalInputTokens: 30000})
	if _, _, err := budgetCollect(t, b, sized(6001)); !errors.Is(err, ErrInputTooLarge) {
		t.Fatalf("err = %v, want ErrInputTooLarge for a call over 6000", err)
	}
	if b.Used() != 0 || b.InputUsed() != 0 || b.Exhausted() {
		t.Errorf("used=%d input=%d exhausted=%v, want an uncounted refusal", b.Used(), b.InputUsed(), b.Exhausted())
	}
}

func TestBudgetTotalInputTrimsHistoryFirst(t *testing.T) {
	fake := &budgetCounter{}
	b := NewBudget(fake, Limits{MaxCalls: 5, MaxInputTokens: 6000, MaxTotalInputTokens: 6000})
	if _, _, err := budgetCollect(t, b, sized(5000)); err != nil {
		t.Fatal(err)
	}
	// 1000 tokens are left. Old history is dropped so the question still fits.
	req := ChatRequest{Messages: []Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: strings.Repeat("o", 3000)},
		{Role: "assistant", Content: strings.Repeat("a", 3000)},
		{Role: "user", Content: "now"},
	}}
	if _, _, err := budgetCollect(t, b, req); err != nil {
		t.Fatalf("err = %v, want the trimmed call to go through", err)
	}
	got := fake.last().Messages
	if len(got) != 2 || got[1].Content != "now" {
		t.Errorf("messages = %+v, want system and the last question", got)
	}
}

func TestBudgetCountsReportedInputWhenLarger(t *testing.T) {
	fake := &budgetCounter{usage: Usage{InputTokens: 9000}}
	b := NewBudget(fake, Limits{MaxCalls: 5, MaxTotalInputTokens: 30000})
	if _, _, err := budgetCollect(t, b, sized(10)); err != nil {
		t.Fatal(err)
	}
	if got := b.InputUsed(); got != 9000 {
		t.Errorf("InputUsed = %d, want the reported 9000", got)
	}
}

func TestLimitsFromEnvTotalInput(t *testing.T) {
	for _, tt := range []struct {
		env  string
		want int
	}{{"", 30000}, {"12000", 12000}, {" 45000 ", 45000}, {"0", 30000}, {"-1", 30000}, {"lots", 30000}} {
		t.Setenv(EnvMaxTotalInputTokens, tt.env)
		if got := LimitsFromEnv().MaxTotalInputTokens; got != tt.want {
			t.Errorf("%s=%q: MaxTotalInputTokens = %d, want %d", EnvMaxTotalInputTokens, tt.env, got, tt.want)
		}
	}
}
