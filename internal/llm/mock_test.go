package llm

import (
	"context"
	"errors"
	"regexp"
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

func TestMockOpeningMentionsDeathTime(t *testing.T) {
	tests := []struct {
		name      string
		deathTime string
		want      string
	}{
		{"known time is stated", "2026-10-31 20:15:03 UTC", "我死于 2026-10-31 20:15:03 UTC"},
		{"unknown time is not stated", "", "我死于"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := ChatRequest{
				Messages:  userMsg("请做开场陈述。"),
				Cause:     autopsy.CauseOOMKilled,
				Evidence:  testEvidence(),
				Purpose:   PurposeOpening,
				DeathTime: tt.deathTime,
			}
			text := mockText(req)
			if tt.deathTime == "" {
				if strings.Contains(text, tt.want) {
					t.Errorf("text %q should not mention a death time", text)
				}
				return
			}
			if !strings.Contains(text, tt.want) {
				t.Errorf("text %q, want it to contain %q", text, tt.want)
			}
			if !strings.Contains(text, "[E") {
				t.Errorf("opening %q has no [E#]", text)
			}
		})
	}
}

func TestMockChatReplies(t *testing.T) {
	tests := []struct {
		name      string
		cause     autopsy.CauseOfDeath
		question  string
		deathTime string
		contains  []string
		absent    []string
		cites     int // exact number of [E#] citations, 0 means at least one
	}{
		{
			name: "zh fix on OOM mentions memory limit", cause: autopsy.CauseOOMKilled, question: "怎么修？",
			contains: []string{"memory limit", "[E1]"}, absent: []string{"我死于"},
		},
		{
			name: "en fix on CrashLoop reads previous logs", cause: autopsy.CauseCrashLoopBackOff, question: "How do I solve this?",
			contains: []string{"kubectl logs --previous", "Evidence:"},
		},
		{
			name: "fix advice never claims an action", cause: autopsy.CauseProbeFailure, question: "怎么办",
			contains: []string{"探针", "initialDelaySeconds"}, absent: []string{"我已经"},
		},
		{
			name: "cause question restates cause with two citations", cause: autopsy.CauseOOMKilled, question: "为什么死的？",
			contains: []string{"我的死因是 OOMKilled"}, cites: 2,
		},
		{
			name: "English cause question", cause: autopsy.CauseOOMKilled, question: "How did you die?",
			contains: []string{"My cause of death is OOMKilled"}, cites: 2,
		},
		{
			name: "time question adds death time", cause: autopsy.CauseOOMKilled, question: "什么时候死的？",
			deathTime: "2026-10-31 20:15:03 UTC", contains: []string{"我死于 2026-10-31 20:15:03 UTC"},
		},
		{
			name: "time question without known time", cause: autopsy.CauseOOMKilled, question: "什么时候死的？",
			absent: []string{"我死于"},
		},
		{
			name: "unrelated zh question", cause: autopsy.CauseOOMKilled, question: "你昨天晚饭吃了什么？",
			contains: []string{"冥界没有留下记录。", "[E"},
		},
		{
			name: "unrelated en question is English", cause: autopsy.CauseOOMKilled, question: "What is your favourite colour?",
			contains: []string{"The underworld kept no record of that.", "[E"}, absent: []string{"冥界"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := ChatRequest{
				Messages:  []Message{{Role: "user", Content: tt.question}},
				Cause:     tt.cause,
				Evidence:  testEvidence(),
				Purpose:   PurposeChat,
				DeathTime: tt.deathTime,
			}
			text := mockText(req)
			for _, s := range tt.contains {
				if !strings.Contains(text, s) {
					t.Errorf("reply %q, want it to contain %q", text, s)
				}
			}
			for _, s := range tt.absent {
				if strings.Contains(text, s) {
					t.Errorf("reply %q, want it to not contain %q", text, s)
				}
			}
			if n := strings.Count(text, "[E"); n == 0 || (tt.cites != 0 && n != tt.cites) {
				t.Errorf("reply %q has %d citations, want %d (0 means at least one)", text, n, tt.cites)
			}
		})
	}
}

func TestMockEnglishRepliesUseEnglishPunctuation(t *testing.T) {
	cjkPunct := []string{"，", "。", "：", "「", "」"}
	// englishCite matches one straight-quoted citation such as "..."[E1].
	englishCite := regexp.MustCompile(`"[^"]*"\[E\d\]`)
	tests := []struct {
		name     string
		cause    autopsy.CauseOfDeath
		question string
		cites    []string           // expected citations joined exactly as they appear
		evidence []autopsy.Evidence // nil means testEvidence()
	}{
		{"fix on OOM", autopsy.CauseOOMKilled, "How do I fix this?", []string{`"reason=OOMKilled exitCode=137"[E1]`}, nil},
		{"cause question", autopsy.CauseOOMKilled, "How did you die?", []string{`"reason=OOMKilled exitCode=137"[E1]`, `"container app limits: memory=64Mi"[E2]`}, nil},
		{"cause question on Error", autopsy.CauseError, "Why did you die?", []string{`"reason=OOMKilled exitCode=137"[E1]`, `"FATAL: out of memory, allocation failed"[E3]`}, nil},
		{"other question", autopsy.CauseOOMKilled, "What is your favourite colour?", []string{`"reason=OOMKilled exitCode=137"[E1]`}, nil},
		// The evidence text has inner double quotes. They must come out as single quotes.
		{"inner double quote in evidence", autopsy.CauseOOMKilled, "How do I fix this?",
			[]string{`"reason=OOMKilled exitCode=137 message='cgroup limit'"[E1]`},
			[]autopsy.Evidence{{ID: "E1", Kind: "status", Text: `container app state: terminated reason=OOMKilled exitCode=137 message="cgroup limit"`}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev := tt.evidence
			if ev == nil {
				ev = testEvidence()
			}
			req := ChatRequest{
				Messages: userMsg(tt.question),
				Cause:    tt.cause,
				Evidence: ev,
				Purpose:  PurposeChat,
			}
			text := mockText(req)
			if n := strings.Count(text, `"`); n != 2*len(tt.cites) {
				t.Errorf("English reply %q has %d straight quotes, want %d (one pair per citation)", text, n, 2*len(tt.cites))
			}
			for _, p := range cjkPunct {
				if strings.Contains(text, p) {
					t.Errorf("English reply %q contains CJK punctuation %q", text, p)
				}
			}
			found := englishCite.FindAllString(text, -1)
			if len(found) != len(tt.cites) {
				t.Fatalf("citations %q, want %q in %q", found, tt.cites, text)
			}
			for i, c := range tt.cites {
				if found[i] != c {
					t.Errorf("citation %d = %q, want %q", i, found[i], c)
				}
			}
			if len(tt.cites) == 2 {
				want := tt.cites[0] + ", " + tt.cites[1]
				if !strings.Contains(text, want) {
					t.Errorf("reply %q, want citations joined as %q", text, want)
				}
			}
		})
	}
}

func TestMockChatUsesLanguageOfLastQuestion(t *testing.T) {
	req := ChatRequest{
		Messages: []Message{
			{Role: "user", Content: "How did you die?"},
			{Role: "assistant", Content: "My cause of death is OOMKilled."},
			{Role: "user", Content: "怎么修？"},
		},
		Cause:    autopsy.CauseOOMKilled,
		Evidence: testEvidence(),
		Purpose:  PurposeChat,
	}
	if text := mockText(req); !strings.Contains(text, "内存") {
		t.Errorf("reply %q, want Chinese because the last question is Chinese", text)
	}
}

func TestMockPostmortemPurposeHasFourBlocks(t *testing.T) {
	causes := []autopsy.CauseOfDeath{
		autopsy.CauseOOMKilled, autopsy.CauseCrashLoopBackOff, autopsy.CauseImagePullBackOff,
		autopsy.CauseError, autopsy.CauseProbeFailure, autopsy.CauseUnknown,
	}
	for _, c := range causes {
		req := ChatRequest{
			Messages:  userMsg("write the postmortem"),
			Cause:     c,
			Evidence:  testEvidence(),
			Purpose:   PurposePostmortem,
			DeathTime: "2026-10-31 20:15:03 UTC",
		}
		text := mockText(req)
		for _, h := range []string{"### SUMMARY\n", "### ROOT_CAUSE\n", "### REMEDIATION\n", "### ACTION_ITEMS\n"} {
			if strings.Count(text, h) != 1 {
				t.Errorf("%s: header %q appears %d times, want 1\n%s", c, h, strings.Count(text, h), text)
			}
		}
		if !strings.Contains(text, "[E") {
			t.Errorf("%s: no [E#] citation in %q", c, text)
		}
		if n := strings.Count(text, "- [ ] "); n != 3 {
			t.Errorf("%s: %d action items, want 3", c, n)
		}
		if strings.Contains(text, "我") {
			t.Errorf("%s: postmortem uses the spirit voice: %q", c, text)
		}
	}
}
