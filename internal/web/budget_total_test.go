package web

import (
	"testing"

	"github.com/zhangjuji/k8s-seance/internal/llm"
)

// A full séance under the default limits (opening, 4 follow-ups and a
// postmortem, 6 calls) stays within 30000 input tokens and reports its input.
func TestSeanceInputStaysWithinTotalCap(t *testing.T) {
	srv := newSeanceServer(t, llm.Limits{})
	res := createSeance(t, srv, crashNS, crashPod)
	streamOnce(t, srv, res.ID)

	prev := detailOf(t, srv, res.ID).InputTokensUsed
	if prev <= 0 {
		t.Fatalf("inputTokensUsed after the opening = %d, want > 0", prev)
	}
	for i, q := range []string{"怎么修复？", "谁删了你？", "你的退出码是多少？", "你重启了几次？"} {
		var reply messageReply
		decodeJSON(t, askJSON(t, srv, res.ID, q).Body.Bytes(), &reply)
		if reply.Exhausted {
			t.Fatalf("question %d: exhausted, want an answer", i+1)
		}
		if reply.InputTokensMax != 30000 {
			t.Errorf("question %d: inputTokensMax = %d, want 30000", i+1, reply.InputTokensMax)
		}
		if reply.InputTokensUsed <= prev || reply.InputTokensUsed > 30000 {
			t.Errorf("question %d: inputTokensUsed = %d, want in (%d, 30000]", i+1, reply.InputTokensUsed, prev)
		}
		prev = reply.InputTokensUsed
	}
	pm := postmortemJSON(t, srv, res.ID)
	if pm.Mode != "llm" {
		t.Errorf("postmortem mode = %q reason = %q, want llm", pm.Mode, pm.Reason)
	}
	d := detailOf(t, srv, res.ID)
	if d.BudgetUsed != 6 || d.InputTokensUsed > 30000 || d.InputTokensUsed <= prev {
		t.Errorf("detail: budgetUsed=%d inputTokensUsed=%d (before postmortem %d), want 6 calls and at most 30000",
			d.BudgetUsed, d.InputTokensUsed, prev)
	}
}

// When the séance's total input is spent before its calls are, the next
// question gets the exhausted line and the postmortem falls back to the rule
// template with budget_exhausted.
func TestSeanceTotalInputCapExhausts(t *testing.T) {
	probe := newSeanceServer(t, llm.Limits{})
	p := createSeance(t, probe, crashNS, crashPod)
	streamOnce(t, probe, p.ID)
	opening := detailOf(t, probe, p.ID).InputTokensUsed

	srv := newSeanceServer(t, llm.Limits{MaxTotalInputTokens: opening + 1})
	res := createSeance(t, srv, crashNS, crashPod)
	streamOnce(t, srv, res.ID)
	if d := detailOf(t, srv, res.ID); d.Exhausted || d.BudgetUsed != 1 || d.InputTokensUsed != opening {
		t.Fatalf("after opening: exhausted=%v used=%d input=%d, want false/1/%d", d.Exhausted, d.BudgetUsed, d.InputTokensUsed, opening)
	}

	var reply messageReply
	decodeJSON(t, askJSON(t, srv, res.ID, "怎么修复？").Body.Bytes(), &reply)
	if !reply.Exhausted || reply.Reply != llm.ExhaustedLine {
		t.Fatalf("reply = %q exhausted=%v, want the exhausted line", reply.Reply, reply.Exhausted)
	}
	if reply.BudgetUsed != 1 || reply.BudgetRemaining != 0 || reply.InputTokensUsed != opening || reply.InputTokensMax != opening+1 {
		t.Errorf("reply budget: used=%d remaining=%d input=%d/%d, want 1/0/%d/%d",
			reply.BudgetUsed, reply.BudgetRemaining, reply.InputTokensUsed, reply.InputTokensMax, opening, opening+1)
	}

	pm := postmortemJSON(t, srv, res.ID)
	if pm.Mode != "template" || pm.Reason != "budget_exhausted" {
		t.Errorf("postmortem mode = %q reason = %q, want template and budget_exhausted", pm.Mode, pm.Reason)
	}
	if d := detailOf(t, srv, res.ID); !d.Exhausted || d.BudgetUsed != 1 {
		t.Errorf("detail: exhausted=%v used=%d, want true and 1", d.Exhausted, d.BudgetUsed)
	}
}
