package llm

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/zhangjuji/k8s-seance/internal/autopsy"
)

func testEvidence() []autopsy.Evidence {
	return []autopsy.Evidence{
		{ID: "E1", Kind: "status", Text: "container app state: terminated reason=OOMKilled exitCode=137"},
		{ID: "E2", Kind: "spec", Text: "container app limits: memory=64Mi"},
		{ID: "E3", Kind: "log", Text: "container app previous log: FATAL: out of memory, allocation failed"},
		{ID: "E4", Kind: "event", Text: "event Warning BackOff: Back-off restarting failed container"},
	}
}

func userMsg(content string) []Message {
	return []Message{{Role: "user", Content: content}}
}

// collect runs Stream and returns every fragment plus the usage.
func collect(t *testing.T, ctx context.Context, p Provider, req ChatRequest) ([]string, Usage) {
	t.Helper()
	var chunks []string
	usage, err := p.Stream(ctx, req, func(s string) error {
		chunks = append(chunks, s)
		return nil
	})
	if err != nil {
		t.Fatalf("Stream: unexpected error: %v", err)
	}
	return chunks, usage
}

func TestMockStreamPerCause(t *testing.T) {
	tests := []struct {
		name   string
		cause  autopsy.CauseOfDeath
		phrase string
	}{
		{"OOMKilled", autopsy.CauseOOMKilled, "越来越重"},
		{"CrashLoopBackOff", autopsy.CauseCrashLoopBackOff, "一次次复活又倒下"},
		{"ImagePullBackOff", autopsy.CauseImagePullBackOff, "我从未真正活过"},
		{"ProbeFailure", autopsy.CauseProbeFailure, "他们以为我死了，于是真的杀了我"},
		{"Error", autopsy.CauseError, "我的进程在不该停的地方停了下来"},
		{"Unknown", autopsy.CauseUnknown, "说不清自己是怎么死的"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := ChatRequest{
				Messages: userMsg("你是怎么死的？"),
				Cause:    tt.cause,
				PodName:  "demo",
				Evidence: testEvidence(),
			}
			chunks, usage := collect(t, context.Background(), MockProvider{}, req)
			text := strings.Join(chunks, "")

			if text == "" {
				t.Fatal("empty text")
			}
			if !strings.Contains(text, "[E") {
				t.Errorf("no [E#] citation in %q", text)
			}
			if !strings.Contains(text, tt.phrase) {
				t.Errorf("voice phrase %q missing from %q", tt.phrase, text)
			}
			if len(chunks) < 2 {
				t.Errorf("expected several fragments, got %d", len(chunks))
			}
			if usage.OutputTokens != utf8.RuneCountInString(text) {
				t.Errorf("OutputTokens = %d, want %d", usage.OutputTokens, utf8.RuneCountInString(text))
			}
			if usage.InputTokens <= 0 {
				t.Errorf("InputTokens = %d, want > 0", usage.InputTokens)
			}
		})
	}
}

func TestMockStreamFragmentsConcatenateToFullText(t *testing.T) {
	req := ChatRequest{
		Messages: userMsg("hi"),
		Cause:    autopsy.CauseOOMKilled,
		Evidence: testEvidence(),
	}
	chunks, _ := collect(t, context.Background(), MockProvider{}, req)
	got := strings.Join(chunks, "")

	if want := mockText(req); got != want {
		t.Errorf("concatenated fragments differ from full text\n got: %q\nwant: %q", got, want)
	}
	for i, c := range chunks {
		if n := utf8.RuneCountInString(c); n == 0 || n > chunkRunes {
			t.Errorf("chunk %d has %d runes, want 1..%d", i, n, chunkRunes)
		}
		if !utf8.ValidString(c) {
			t.Errorf("chunk %d is not valid UTF-8: %q", i, c)
		}
	}
}

func TestMockStreamPicksEvidenceInOrder(t *testing.T) {
	req := ChatRequest{Messages: userMsg("hi"), Cause: autopsy.CauseOOMKilled, Evidence: testEvidence()}
	text := mockText(req)

	// OOM: status, then the limits spec, then the FATAL log line. The cap of 3
	// drops the Warning event.
	for _, want := range []string{"[E1]", "[E2]", "[E3]"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %s in %q", want, text)
		}
	}
	if strings.Contains(text, "[E4]") {
		t.Errorf("event should be cut by the citation cap: %q", text)
	}
	if i1, i3 := strings.Index(text, "[E1]"), strings.Index(text, "[E3]"); i1 > i3 {
		t.Errorf("status should come before log: %q", text)
	}
}

func ids(items []autopsy.Evidence) []string {
	out := make([]string, 0, len(items))
	for _, e := range items {
		out = append(out, e.ID)
	}
	return out
}

func TestPickEvidenceByCause(t *testing.T) {
	ev := []autopsy.Evidence{
		{ID: "E1", Kind: "event", Text: "event Warning BackOff: Back-off pulling image"},
		{ID: "E2", Kind: "spec", Text: "container app image: busybox:nonexist"},
		{ID: "E3", Kind: "status", Text: "container app state: waiting reason=ImagePullBackOff; restarts: 0; lastState: none"},
		{ID: "E4", Kind: "log", Text: "container app current log: error: something"},
		{ID: "E5", Kind: "spec", Text: "container app limits: memory=64Mi; requests: none"},
	}
	tests := []struct {
		name  string
		cause autopsy.CauseOfDeath
		want  []string
	}{
		// Logs are not part of the ImagePull passes, so E4 is skipped.
		{"ImagePull status, image, warning", autopsy.CauseImagePullBackOff, []string{"E3", "E2", "E1"}},
		// Limits come before logs and events for OOM.
		{"OOM status, limits, logs", autopsy.CauseOOMKilled, []string{"E3", "E5", "E4"}},
		// Generic causes use status, important logs, then warnings. E4 contains
		// "error", so it is an important log and comes before the warning.
		{"Error status, log, warning", autopsy.CauseError, []string{"E3", "E4", "E1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ids(pickEvidence(tt.cause, ev))
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Errorf("picked %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPickEvidenceNeverCitesNormalEvents(t *testing.T) {
	ev := []autopsy.Evidence{
		{ID: "E1", Kind: "event", Text: "event Normal Scheduled: Successfully assigned seance-demo/pod to node"},
		{ID: "E2", Kind: "event", Text: "event Normal Pulled: Container image already present"},
	}
	for _, cause := range []autopsy.CauseOfDeath{autopsy.CauseImagePullBackOff, autopsy.CauseOOMKilled, autopsy.CauseError} {
		if got := pickEvidence(cause, ev); len(got) != 0 {
			t.Errorf("%s: picked %v, want none", cause, ids(got))
		}
	}
}

func TestPickEvidenceSkipsNoLimits(t *testing.T) {
	ev := []autopsy.Evidence{
		{ID: "E1", Kind: "status", Text: "container app state: terminated reason=OOMKilled; restarts: 1; lastState: none"},
		{ID: "E2", Kind: "spec", Text: "container app limits: none; requests: none"},
	}
	if got := ids(pickEvidence(autopsy.CauseOOMKilled, ev)); strings.Join(got, ",") != "E1" {
		t.Errorf("picked %v, want [E1]", got)
	}
}

func TestPickEvidenceSkipsRepeatedLogLine(t *testing.T) {
	ev := []autopsy.Evidence{
		{ID: "E1", Kind: "status", Text: "container app state: terminated reason=Error; restarts: 1; lastState: none"},
		{ID: "E2", Kind: "log", Text: "container app current log: FATAL: boom"},
		{ID: "E3", Kind: "log", Text: "container app previous log: FATAL: boom"},
		{ID: "E4", Kind: "log", Text: "container app previous log: FATAL: other"},
	}
	got := ids(pickEvidence(autopsy.CauseError, ev))
	if strings.Join(got, ",") != "E1,E2,E4" {
		t.Errorf("picked %v, want [E1 E2 E4]", got)
	}
}

func TestFragmentStatus(t *testing.T) {
	tests := []struct {
		name string
		text string
		want string
	}{
		{
			"lastState terminated onward",
			"container app state: waiting reason=CrashLoopBackOff; restarts: 3; lastState: terminated reason=OOMKilled exitCode=137",
			"lastState: terminated reason=OOMKilled exitCode=137",
		},
		{
			"reason onward when no terminated lastState",
			"container app state: waiting reason=ImagePullBackOff; restarts: 0; lastState: none",
			"reason=ImagePullBackOff; restarts: 0; lastState: none",
		},
		{
			"whole text when no marker",
			"container app state: running; restarts: 0; lastState: none",
			"container app state: running; restarts: 0; lastState: none",
		},
		{
			"long reason is capped",
			"container app state: waiting reason=" + strings.Repeat("x", 80),
			"reason=" + strings.Repeat("x", 60-len("reason=")) + "…",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := fragment(autopsy.Evidence{Kind: "status", Text: tt.text})
			if got != tt.want {
				t.Errorf("fragment = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFragmentLogStripsLabel(t *testing.T) {
	tests := []struct {
		name string
		text string
		want string
	}{
		{"current", "container app current log: panic: boom", "panic: boom"},
		{"previous", "container app previous log: FATAL: out of memory", "FATAL: out of memory"},
		{"no label kept", "plain line", "plain line"},
		{"long line capped", "container app current log: " + strings.Repeat("y", 70), strings.Repeat("y", 60) + "…"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := fragment(autopsy.Evidence{Kind: "log", Text: tt.text}); got != tt.want {
				t.Errorf("fragment = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestMockStreamNoEvidence(t *testing.T) {
	tests := []struct {
		name     string
		messages []Message
	}{
		{"opening", userMsg("你是谁？")},
		{"follow-up", []Message{
			{Role: "user", Content: "你是谁？"},
			{Role: "assistant", Content: "……"},
			{Role: "user", Content: "为什么死的？"},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := ChatRequest{Messages: tt.messages, Cause: autopsy.CauseError}
			chunks, _ := collect(t, context.Background(), MockProvider{}, req)
			text := strings.Join(chunks, "")
			if !strings.Contains(text, "冥界没有留下记录") {
				t.Errorf("want 冥界没有留下记录, got %q", text)
			}
			if strings.Contains(text, "[E") {
				t.Errorf("no evidence to cite, got %q", text)
			}
		})
	}
}

func TestMockStreamFollowUp(t *testing.T) {
	req := ChatRequest{
		Messages: []Message{
			{Role: "user", Content: "你是谁？"},
			{Role: "assistant", Content: "我是一个亡灵。"},
			{Role: "user", Content: "怎么修？"},
		},
		Cause:    autopsy.CauseOOMKilled,
		Evidence: testEvidence(),
	}
	chunks, _ := collect(t, context.Background(), MockProvider{}, req)
	text := strings.Join(chunks, "")

	if !strings.HasPrefix(text, followUpLine) {
		t.Errorf("follow-up should start with %q, got %q", followUpLine, text)
	}
	if !strings.Contains(text, "[E1]") {
		t.Errorf("follow-up should cite evidence, got %q", text)
	}
	if strings.Contains(text, "证据在此") {
		t.Errorf("follow-up should not repeat the opening line: %q", text)
	}
}

func TestMockStreamCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	called := false
	_, err := MockProvider{}.Stream(ctx, ChatRequest{Cause: autopsy.CauseError, Evidence: testEvidence()}, func(string) error {
		called = true
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if called {
		t.Error("onDelta should not run on a cancelled context")
	}
}

func TestMockStreamCancelDuringDelay(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	calls := 0
	p := MockProvider{Delay: time.Hour}
	_, err := p.Stream(ctx, ChatRequest{Cause: autopsy.CauseError, Evidence: testEvidence()}, func(string) error {
		calls++
		cancel() // the next delay must end at once
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if calls != 1 {
		t.Errorf("onDelta calls = %d, want 1", calls)
	}
}

func TestMockStreamOnDeltaErrorPropagates(t *testing.T) {
	sentinel := errors.New("client gone")

	calls := 0
	_, err := MockProvider{}.Stream(context.Background(), ChatRequest{Cause: autopsy.CauseError, Evidence: testEvidence()}, func(string) error {
		calls++
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want sentinel", err)
	}
	if calls != 1 {
		t.Errorf("onDelta calls = %d, want 1 (stop at first error)", calls)
	}
}

func TestMockStreamNilOnDelta(t *testing.T) {
	if _, err := (MockProvider{}).Stream(context.Background(), ChatRequest{}, nil); err == nil {
		t.Error("want error for nil onDelta")
	}
}
