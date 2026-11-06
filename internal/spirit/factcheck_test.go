package spirit

import (
	"context"
	"encoding/json"
	"io/fs"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zhangjuji/k8s-seance/fixtures"
	"github.com/zhangjuji/k8s-seance/internal/autopsy"
	"github.com/zhangjuji/k8s-seance/internal/llm"
)

// factEvidence is a small OOM evidence list for the table tests.
func factEvidence() []autopsy.Evidence {
	died := time.Date(2026, 11, 1, 4, 55, 23, 0, time.UTC)
	return []autopsy.Evidence{
		{ID: "E1", Kind: "status", Text: "container stress state: waiting reason=CrashLoopBackOff; restarts: 8; lastState: terminated reason=OOMKilled exitCode=137", Timestamp: &died},
		{ID: "E2", Kind: "spec", Text: "container stress limits: cpu=100m, memory=64Mi; requests: cpu=10m, memory=32Mi"},
		{ID: "E3", Kind: "event", Text: "event Warning BackOff: Back-off restarting failed container stress (count 21)"},
	}
}

const factDeath = "2026-11-01 04:55:23 UTC"

// claims returns kind:claim for each finding, for compact comparison.
func claims(fs []Finding) []string {
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, f.Kind+":"+f.Claim)
	}
	return out
}

func TestFactCheck(t *testing.T) {
	tests := []struct {
		name  string
		reply string
		want  []string
	}{
		{"all true zh", "我死于 OOMKilled，exitCode 137，重启 8 次 [E1]。上限只有 64Mi [E2]，死亡时间 2026-11-01 04:55:23 UTC。", nil},
		{"all true en", "I restarted eight times [E1]. Exit code: 137. My limit was 64Mi [E2]. I died at 2026-11-01T04:55Z.", nil},
		{"unknown citation", "证据在此 [E1][E9]。", []string{"citation:[E9]"}},
		{"citation list", "See [E2, E7].", []string{"citation:[E7]"}},
		{"repeated claim reported once", "[E9] again [E9]. And [E9].", []string{"citation:[E9]"}},
		{"wrong memory", "我的上限只有 256Mi [E2]。", []string{"memory:256Mi"}},
		{"64M is not 64Mi", "My limit was 64M.", []string{"memory:64M"}},
		{"MiB equals Mi", "My limit was 64 MiB.", nil},
		{"cpu and durations are not memory", "cpu=100m, back-off 5m0s.", nil},
		{"advice may propose memory zh", "建议把 memory limit 调到 128Mi [E2]。", nil},
		{"advice may propose memory en", "Consider raising the limit to 256Mi.", nil},
		{"advice still checks citations", "建议调到 128Mi [E9]。", []string{"citation:[E9]"}},
		{"advice still checks exit code", "建议排查 exit code 2 的来源。", []string{"exitCode:exit code 2"}},
		{"advice still checks restarts", "Consider that I restarted 3 times.", []string{"restarts:restarted 3 times"}},
		{"advice still checks time", "建议查看 2026-11-01 05:30 之后的日志。", []string{"time:2026-11-01 05:30"}},
		{"narrative memory is checked", "我的内存被提高到 128Mi 后死了。", []string{"memory:128Mi"}},
		{"wrong exit code zh", "退出码 1。", []string{"exitCode:退出码 1"}},
		{"wrong exit code en", "I exited with code 2.", []string{"exitCode:exited with code 2"}},
		{"wrong restarts zh", "我重启了 3 次。", []string{"restarts:重启了 3 次"}},
		{"wrong restarts zh numeral", "我重启了十二次。", []string{"restarts:重启了十二次"}},
		{"restarts zh numeral true", "我重启了八次。", nil},
		{"restart count field", "restarts: 4", []string{"restarts:restarts: 4"}},
		{"restarts en words", "twenty-one restarts later [E3].", nil},
		{"not one restart", "Not one restart.", []string{"restarts:Not one restart"}},
		{"wrong time", "我死于 2026-11-01 06:00:00 UTC。", []string{"time:2026-11-01 06:00:00 UTC"}},
		{"offset time converted", "I died at 2026-11-01 12:55 +08:00.", nil},
		{"seconds ignored", "I died at 2026-11-01 04:55:59 UTC.", nil},
		{"quoted evidence not rechecked", "证据：「requests: cpu=10m, memory=32M…」[E2]。", nil},
		{"quoted invention is checked", "他说「memory=512Mi」。", []string{"memory:512Mi"}},
		{"versions and decimals stay whole", "镜像 nginx:1.27 和 1.5 倍。", nil},
		{"no facts", "冥界没有留下记录。", nil},
		{"several findings", "我重启了 3 次 [E5]，上限 1Gi。", []string{"citation:[E5]", "memory:1Gi", "restarts:重启了 3 次"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := claims(FactCheck(tt.reply, factEvidence(), factDeath))
			if len(got) == 0 && len(tt.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("FactCheck(%q) = %q, want %q", tt.reply, got, tt.want)
			}
		})
	}
}

func TestFactCheckSentenceAndKnown(t *testing.T) {
	got := FactCheck("一切正常。我死于 2026-11-01 04:55 UTC。", factEvidence()[1:])
	if len(got) != 1 || got[0].Kind != FindingTime || got[0].Sentence != "我死于 2026-11-01 04:55 UTC。" {
		t.Fatalf("without the death time: %+v", got)
	}
	if got := FactCheck("我死于 2026-11-01 04:55 UTC。", factEvidence()[1:], factDeath); len(got) != 0 {
		t.Errorf("the known death time should back the claim: %+v", got)
	}
}

func TestFactCheckNoEvidence(t *testing.T) {
	got := claims(FactCheck("exitCode=0 [E1]", nil))
	want := []string{"citation:[E1]", "exitCode:exitCode=0"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestParseNumber(t *testing.T) {
	tests := map[string]int{"8": 8, "eight": 8, "Twenty-one": 21, "no": 0, "not one": 0, "八": 8, "十": 10, "十二": 12, "六十六": 66, "两": 2}
	for in, want := range tests {
		if got, ok := parseNumber(in); !ok || got != want {
			t.Errorf("parseNumber(%q) = %d, %v; want %d", in, got, ok, want)
		}
	}
	for _, in := range []string{"the", "many", "not two", "一二", "十十", "twenty-twenty"} {
		if _, ok := parseNumber(in); ok {
			t.Errorf("parseNumber(%q) should fail", in)
		}
	}
}

// replayFile is the part of a fixtures/replay file the test needs.
type replayFile struct {
	FixtureID string `json:"fixtureId"`
	Turns     []struct {
		TurnIndex int    `json:"turnIndex"`
		Reply     string `json:"reply"`
	} `json:"turns"`
}

// TestFactCheckRecordedRepliesHaveNoFindings guards Requirement 11.4: the
// recorded real replies (openings and follow-ups) must not raise a doubt.
func TestFactCheckRecordedRepliesHaveNoFindings(t *testing.T) {
	byPod := make(map[string]fixtureCase)
	for _, c := range loadFixtureCases(t) {
		byPod[c.a.Name] = c
	}
	files, err := fs.Glob(fixtures.ReplayFS(), "*.json")
	if err != nil || len(files) == 0 {
		t.Fatalf("no replay files: %v", err)
	}
	checked := 0
	for _, name := range files {
		data, err := fs.ReadFile(fixtures.ReplayFS(), name)
		if err != nil {
			t.Fatal(err)
		}
		var rf replayFile
		if err := json.Unmarshal(data, &rf); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		c, ok := byPod[strings.TrimSuffix(rf.FixtureID, englishFixtureSuffix)]
		if !ok {
			t.Fatalf("%s: no fixture for %s", name, rf.FixtureID)
		}
		for _, turn := range rf.Turns {
			if turn.TurnIndex >= 100 {
				continue // the postmortem is not a spirit reply
			}
			checked++
			if got := FactCheck(turn.Reply, c.ev, c.deathStr); len(got) != 0 {
				t.Errorf("%s turn %d: false findings %+v", name, turn.TurnIndex, got)
			}
		}
	}
	if checked < 8 {
		t.Errorf("checked %d recorded replies, want at least 8", checked)
	}
}

// TestFactCheckMockRepliesHaveNoFindings guards Requirement 11.4 for the mock:
// every opening and every kind of follow-up, in both languages.
func TestFactCheckMockRepliesHaveNoFindings(t *testing.T) {
	questions := []string{
		"你是怎么死的？", "怎么修复？", "你什么时候死的？", "谁删了你？", "看看日志",
		"How did you die?", "How do I fix it?", "When did you die?", "Who deleted you?",
	}
	cases := loadFixtureCases(t)
	// A Pod with no evidence gets the "no record" lines.
	empty := fixtureCase{name: "empty", a: &autopsy.PodAutopsy{Name: "empty"}, cls: autopsy.Classification{Cause: autopsy.CauseUnknown}}
	// Other causes, with the OOM evidence relabelled.
	oom := caseByName(t, cases, "oom")
	for _, cause := range []autopsy.CauseOfDeath{autopsy.CauseProbeFailure, autopsy.CauseError, autopsy.CauseUnknown} {
		c := oom
		c.name = string(cause)
		c.cls.Cause = cause
		cases = append(cases, c)
	}
	cases = append(cases, empty)

	for _, c := range cases {
		for _, lang := range []string{llm.LangZH, llm.LangEN} {
			s := NewSessionIn(lang, "mock-"+c.name, c.a, c.cls, c.ev, llm.MockProvider{}, llm.Limits{MaxCalls: 20}, nil, "m")
			replies := []string{}
			text, err := s.Opening(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			replies = append(replies, text)
			for _, q := range questions {
				replies = append(replies, askText(t, s, q))
			}
			replies = append(replies, llm.ExhaustedLineFor(lang))
			for _, r := range replies {
				if got := s.FactCheck(r); len(got) != 0 {
					t.Errorf("%s/%s: false findings in %q: %+v", c.name, lang, r, got)
				}
			}
		}
	}
}

// TestFactCheckTamperedRecording shows the recording test is not vacuous: the
// recorded OOM openings with their facts changed raise one finding per change.
func TestFactCheckTamperedRecording(t *testing.T) {
	c := caseByName(t, loadFixtureCases(t), "oom")
	for _, name := range []string{"memory-glutton-7cc6df9966-bf87c.json", "memory-glutton-7cc6df9966-bf87c-en.json"} {
		data, err := fs.ReadFile(fixtures.ReplayFS(), name)
		if err != nil {
			t.Fatal(err)
		}
		var rf replayFile
		if err := json.Unmarshal(data, &rf); err != nil {
			t.Fatal(err)
		}
		opening := rf.Turns[0].Reply
		tampered := strings.NewReplacer(
			"64Mi", "256Mi",
			"exitCode 137", "exitCode 139",
			"[E3]", "[E30]",
			"04:55:23", "05:15:23",
		).Replace(opening)
		got := FactCheck(tampered, c.ev, c.deathStr)
		kinds := make(map[string]bool)
		for _, f := range got {
			kinds[f.Kind] = true
		}
		for _, k := range []string{FindingMemory, FindingExitCode, FindingCitation, FindingTime} {
			if !kinds[k] {
				t.Errorf("%s: tampered opening has no %s finding: %+v", name, k, got)
			}
		}
	}
}
