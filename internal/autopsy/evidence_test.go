package autopsy

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// evCont builds a container with a running-free terminated state and given logs.
func evCont(name string, logs, previous []string) ContainerAutopsy {
	code := int32(137)
	return ContainerAutopsy{
		Name:            name,
		Image:           "busybox:1.36",
		State:           ContainerState{Kind: "terminated", Reason: "OOMKilled", ExitCode: &code},
		LogTail:         logs,
		PreviousLogTail: previous,
	}
}

func evPod(containers ...ContainerAutopsy) *PodAutopsy {
	return &PodAutopsy{Name: "demo", Containers: containers}
}

func textsOf(items []Evidence) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.Text
	}
	return out
}

func kindsOf(items []Evidence) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.Kind
	}
	return out
}

func mustBuild(t *testing.T, a *PodAutopsy, opts EvidenceOptions) []Evidence {
	t.Helper()
	items, err := BuildEvidence(a, opts)
	if err != nil {
		t.Fatalf("BuildEvidence: unexpected error: %v", err)
	}
	return items
}

func TestBuildEvidenceNumbering(t *testing.T) {
	a := evPod(
		evCont("app", []string{"start", "error x"}, nil),
		evCont("sidecar", []string{"ok"}, []string{"prev"}),
	)
	a.Events = []EventRecord{{Type: "Warning", Reason: "BackOff", Message: "restarting", Count: 3}}

	items := mustBuild(t, a, EvidenceOptions{})
	if len(items) == 0 {
		t.Fatal("expected evidence items, got none")
	}
	for i, it := range items {
		want := fmt.Sprintf("E%d", i+1)
		if it.ID != want {
			t.Errorf("item %d: ID = %q, want %q", i, it.ID, want)
		}
	}
}

func TestBuildEvidenceOrder(t *testing.T) {
	a := evPod(evCont("app", []string{"hello"}, []string{"bye"}))
	a.Events = []EventRecord{{Type: "Warning", Reason: "Killing", Message: "stop", Count: 1}}

	got := kindsOf(mustBuild(t, a, EvidenceOptions{}))
	want := []string{"status", "spec", "spec", "event", "log", "log"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("kinds = %v, want %v", got, want)
	}
}

func TestBuildEvidenceLogSelection(t *testing.T) {
	seq := func(prefix string, n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("%s%d", prefix, i)
		}
		return out
	}

	tests := []struct {
		name    string
		logs    []string
		opts    EvidenceOptions
		wantLog []string // expected log item texts, without the "container app current log: " prefix
	}{
		{
			name:    "consecutive duplicates collapse to one",
			logs:    []string{"a", "a", "a", "b", "b", "a"},
			wantLog: []string{"a", "b", "a"},
		},
		{
			name:    "non-consecutive duplicates are kept",
			logs:    []string{"a", "b", "a"},
			wantLog: []string{"a", "b", "a"},
		},
		{
			name:    "TailLines keeps only the last lines",
			logs:    []string{"ERROR early", "x1", "x2", "x3", "x4", "x5", "x6", "x7"},
			opts:    EvidenceOptions{TailLines: 5},
			wantLog: []string{"x3", "x4", "x5", "x6", "x7"},
		},
		{
			name:    "TailLines applies after dedupe",
			logs:    []string{"x", "x", "x", "y", "z"},
			opts:    EvidenceOptions{TailLines: 2},
			wantLog: []string{"y", "z"},
		},
		{
			name:    "error line earlier than the last five is kept",
			logs:    append([]string{"panic: nil map"}, seq("ok", 9)...),
			wantLog: []string{"panic: nil map", "ok4", "ok5", "ok6", "ok7", "ok8"},
		},
		{
			name:    "keyword match is case-insensitive",
			logs:    append([]string{"Fatal: disk gone", "Exception in thread"}, seq("ok", 6)...),
			wantLog: []string{"Fatal: disk gone", "Exception in thread", "ok1", "ok2", "ok3", "ok4", "ok5"},
		},
		{
			name:    "error line in the last five is not duplicated",
			logs:    []string{"a", "b", "c", "d", "error here"},
			wantLog: []string{"a", "b", "c", "d", "error here"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			items := mustBuild(t, evPod(evCont("app", tt.logs, []string{})), tt.opts)
			var got []string
			for _, it := range items {
				if it.Kind != "log" {
					continue
				}
				got = append(got, strings.TrimPrefix(it.Text, "container app current log: "))
			}
			if strings.Join(got, "|") != strings.Join(tt.wantLog, "|") {
				t.Errorf("log lines = %q, want %q", got, tt.wantLog)
			}
		})
	}
}

func TestBuildEvidenceLabelsLogSources(t *testing.T) {
	items := mustBuild(t, evPod(evCont("app", []string{"now"}, []string{"then"})), EvidenceOptions{})
	want := []string{"container app current log: now", "container app previous log: then"}
	var got []string
	for _, it := range items {
		if it.Kind == "log" {
			got = append(got, it.Text)
		}
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("log items = %q, want %q", got, want)
	}
}

func TestBuildEvidenceRedaction(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"key=value password", "password=hunter2", "password=[REDACTED]"},
		{"key: value token", "token: abc", "token: [REDACTED]"},
		{"bearer header", "Authorization: Bearer xyz", "Authorization: Bearer [REDACTED]"},
		{"aws access key", "key AKIAABCDEFGHIJKLMNOP used", "key [REDACTED] used"},
		{"secret with quotes", `secret="s3cr3t"`, "secret=[REDACTED]"},
		{"api_key", "api_key=sk-123", "api_key=[REDACTED]"},
		{"apikey mixed case", "ApiKey: zzz", "ApiKey: [REDACTED]"},
		{"passwd", "passwd=pw", "passwd=[REDACTED]"},
		{"no secret stays intact", "connected to db on port 5432", "connected to db on port 5432"},
		{"word containing secret is untouched", "secretary: bob", "secretary: bob"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := redact(tt.in); got != tt.want {
				t.Errorf("redact(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestBuildEvidenceRedactsEveryItem(t *testing.T) {
	a := evPod(evCont("app", []string{"login password=hunter2"}, nil))
	a.Containers[0].State.Message = "token: abc"
	a.Events = []EventRecord{{Type: "Warning", Reason: "Failed", Message: "Bearer xyz", Count: 1}}

	for _, it := range mustBuild(t, a, EvidenceOptions{}) {
		for _, secret := range []string{"hunter2", "abc", "xyz"} {
			if strings.Contains(it.Text, secret) {
				t.Errorf("item %s leaks %q: %q", it.ID, secret, it.Text)
			}
		}
	}
}

func TestBuildEvidenceMissingPreviousLogs(t *testing.T) {
	tests := []struct {
		name     string
		previous []string
		wantNote bool
	}{
		{"nil previous logs record no record", nil, true},
		{"empty previous logs are a real record", []string{}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			items := mustBuild(t, evPod(evCont("app", []string{"x"}, tt.previous)), EvidenceOptions{})
			found := false
			for _, it := range items {
				if it.Text == "container app previous container logs: no record" {
					found = true
					if it.Kind != "log" {
						t.Errorf("no-record item kind = %q, want log", it.Kind)
					}
				}
			}
			if found != tt.wantNote {
				t.Errorf("no-record item present = %v, want %v; items = %q", found, tt.wantNote, textsOf(items))
			}
		})
	}
}

func TestBuildEvidenceMaxChars(t *testing.T) {
	logs := make([]string, 50)
	for i := range logs {
		logs[i] = fmt.Sprintf("line %02d with some padding text", i)
	}
	a := evPod(evCont("app", logs, nil))

	tests := []struct {
		name     string
		maxChars int
	}{
		{"tiny budget", 120},
		{"medium budget", 600},
		{"large budget", 100000},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			items := mustBuild(t, a, EvidenceOptions{MaxChars: tt.maxChars})
			total := 0
			for i, it := range items {
				total += len(it.Text)
				if it.ID != fmt.Sprintf("E%d", i+1) {
					t.Errorf("item %d: ID = %q, numbering not contiguous", i, it.ID)
				}
			}
			if total > tt.maxChars {
				t.Errorf("total text = %d chars, exceeds MaxChars %d", total, tt.maxChars)
			}
		})
	}
}

func TestBuildEvidenceMaxCharsKeepsStatusFirst(t *testing.T) {
	logs := make([]string, 100)
	for i := range logs {
		logs[i] = fmt.Sprintf("log line number %03d", i)
	}
	a := evPod(evCont("app", logs, nil))

	items := mustBuild(t, a, EvidenceOptions{MaxChars: 200})
	if len(items) == 0 {
		t.Fatal("expected some items within budget")
	}
	if items[0].Kind != "status" {
		t.Errorf("first item kind = %q, want status", items[0].Kind)
	}
	for _, it := range items {
		if it.Kind == "log" && strings.Contains(it.Text, "number 099") {
			t.Errorf("last log line should have been cut by MaxChars: %q", it.Text)
		}
	}
}

func TestBuildEvidenceStatusText(t *testing.T) {
	finished := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	code := int32(137)
	zero := int32(0)
	a := evPod(ContainerAutopsy{
		Name:         "app",
		Image:        "polinux/stress",
		RestartCount: 4,
		State:        ContainerState{Kind: "waiting", Reason: "CrashLoopBackOff"},
		LastState:    &ContainerState{Kind: "terminated", Reason: "OOMKilled", ExitCode: &code, FinishedAt: &finished},
		Limits:       map[string]string{"memory": "64Mi", "cpu": "500m"},
		Requests:     map[string]string{},
	})
	a.Containers = append(a.Containers, ContainerAutopsy{
		Name:  "init",
		Image: "busybox",
		State: ContainerState{Kind: "terminated", Reason: "Completed", ExitCode: &zero},
	})

	items := mustBuild(t, a, EvidenceOptions{})
	want := []string{
		"container app state: waiting reason=CrashLoopBackOff; restarts: 4; lastState: terminated reason=OOMKilled exitCode=137",
		"container app image: polinux/stress",
		"container app limits: cpu=500m, memory=64Mi; requests: none",
		"container init state: terminated reason=Completed exitCode=0; restarts: 0; lastState: none",
		"container init image: busybox",
		"container init limits: none; requests: none",
	}
	got := textsOf(items)
	for i := range want {
		if i >= len(got) || got[i] != want[i] {
			t.Errorf("item %d = %q, want %q", i, safeIndex(got, i), want[i])
		}
	}
	if items[0].Timestamp != nil {
		t.Errorf("status timestamp should be empty for waiting state, got %v", items[0].Timestamp)
	}
}

func TestBuildEvidenceTimestamps(t *testing.T) {
	first := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	last := first.Add(time.Minute)
	a := evPod(evCont("app", nil, nil))
	a.Events = []EventRecord{
		{Type: "Warning", Reason: "BackOff", Message: "m", Count: 2, FirstTimestamp: &first, LastTimestamp: &last},
		{Type: "Normal", Reason: "Pulled", Message: "m", Count: 1, FirstTimestamp: &first},
	}

	items := mustBuild(t, a, EvidenceOptions{})
	var events []Evidence
	for _, it := range items {
		if it.Kind == "event" {
			events = append(events, it)
		}
	}
	if len(events) != 2 {
		t.Fatalf("got %d event items, want 2", len(events))
	}
	if events[0].Timestamp == nil || !events[0].Timestamp.Equal(last) {
		t.Errorf("event 0 timestamp = %v, want last timestamp %v", events[0].Timestamp, last)
	}
	if events[1].Timestamp == nil || !events[1].Timestamp.Equal(first) {
		t.Errorf("event 1 timestamp = %v, want first timestamp %v", events[1].Timestamp, first)
	}
	if want := "event Warning BackOff: m (count 2)"; events[0].Text != want {
		t.Errorf("event text = %q, want %q", events[0].Text, want)
	}
}

func TestBuildEvidenceNilAutopsy(t *testing.T) {
	if _, err := BuildEvidence(nil, EvidenceOptions{}); err == nil {
		t.Error("BuildEvidence(nil) returned no error")
	}
}

func TestBuildEvidenceZeroOptionsUseDefaults(t *testing.T) {
	logs := make([]string, 300)
	for i := range logs {
		logs[i] = fmt.Sprintf("row %d", i)
	}
	a := evPod(evCont("app", logs, nil))

	def := mustBuild(t, a, EvidenceOptions{TailLines: defaultTailLines, MaxChars: defaultMaxChars})
	zero := mustBuild(t, a, EvidenceOptions{})
	if strings.Join(textsOf(def), "\n") != strings.Join(textsOf(zero), "\n") {
		t.Error("zero options differ from explicit defaults")
	}
	for _, it := range zero {
		if it.Text == "container app current log: row 0" {
			t.Error("default TailLines should drop row 0 of 300 lines")
		}
	}
}

func safeIndex(s []string, i int) string {
	if i < len(s) {
		return s[i]
	}
	return "<missing>"
}
