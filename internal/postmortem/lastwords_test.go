package postmortem

import (
	"encoding/json"
	"io/fs"
	"strings"
	"testing"

	"github.com/zhangjuji/k8s-seance/fixtures"
	"github.com/zhangjuji/k8s-seance/internal/autopsy"
	"github.com/zhangjuji/k8s-seance/internal/spirit"
)

const lastWordsPod = "restless-spirit-84fc8b7c4c-52nl8"

func TestLastWordsSentenceRules(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string
	}{
		{
			name:    "self introduction with pod name is skipped",
			content: "我是 `" + lastWordsPod + "` 的亡灵。第二句话很短。",
			want:    "第二句话很短。",
		},
		{
			name:    "sentence with an evidence citation is skipped",
			content: "我死得很冤 [E1]。我的内存一点点涨满。",
			want:    "我的内存一点点涨满。",
		},
		{
			name:    "sentence with a date is skipped",
			content: "死于 2026-11-01 04:55:23 UTC。也没有别的话。",
			want:    "也没有别的话。",
		},
		{
			name:    "sentence with a clock time is skipped",
			content: "我在 04:55 倒下。剩下的我不记得了。",
			want:    "剩下的我不记得了。",
		},
		{
			name:    "sentence under 4 runes is skipped",
			content: "好。我的内存一点点涨满。",
			want:    "我的内存一点点涨满。",
		},
		{
			name:    "sentence over 60 runes is skipped",
			content: strings.Repeat("长", 61) + "。我倒下了。",
			want:    "我倒下了。",
		},
		{
			name:    "sentence of exactly 60 runes is kept",
			content: strings.Repeat("字", 59) + "。",
			want:    strings.Repeat("字", 59) + "。",
		},
		{
			name:    "newline ends a sentence without an end mark",
			content: "我从未呼吸过\n我连一行遗言都没有。",
			want:    "我从未呼吸过",
		},
		{
			name:    "markdown marks and spaces are cleaned",
			content: "我是**真的**很累，`stress`  也累。",
			want:    "我是真的很累，stress 也累。",
		},
		{
			name:    "double quotes become single quotes",
			content: "他说\"走吧\"，我没应。",
			want:    "他说'走吧'，我没应。",
		},
		{
			name:    "no qualifying sentence gives fallback",
			content: "我死于 2026-11-01 04:55:23 UTC [E1]。好。",
			want:    lastWordsFallback[autopsy.CauseOOMKilled],
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := lastWords(autopsy.CauseOOMKilled, lastWordsPod, []spirit.Turn{{Role: roleSpirit, Content: tc.content}})
			if got != tc.want {
				t.Errorf("lastWords = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestLastWordsSentenceDots covers the ASCII '.' sentence end. A '.' splits only
// when whitespace or the end of the text follows it.
func TestLastWordsSentenceDots(t *testing.T) {
	cases := []struct {
		name    string
		podName string
		content string
		want    string
	}{
		{
			name:    "English opening skips the name sentence and the citation",
			podName: "memory-glutton-x",
			content: "I am memory-glutton-x. My memory filled up until I choked. Evidence [E1].",
			want:    "My memory filled up until I choked.",
		},
		{
			name:    "ASCII dot followed by space ends a sentence",
			podName: "x",
			content: "I stopped. Then nothing came.",
			want:    "I stopped.",
		},
		{
			name:    "version number with patch part is not split",
			podName: "web-pod-7f9",
			content: "I was pulling nginx:1.27.0 when I stopped. Nothing more.",
			want:    "I was pulling nginx:1.27.0 when I stopped.",
		},
		{
			name:    "version number at the start of a sentence is not split",
			podName: "x",
			content: "1.36 refused me in the end. Fine.",
			want:    "1.36 refused me in the end.",
		},
		{
			name:    "dot inside a file name is not split",
			podName: "x",
			content: "The config in app.yaml broke me. Nothing more.",
			want:    "The config in app.yaml broke me.",
		},
		{
			name:    "dot at the end of the text ends the sentence",
			podName: "x",
			content: "I stopped breathing.",
			want:    "I stopped breathing.",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := lastWords(autopsy.CauseOOMKilled, tc.podName, []spirit.Turn{{Role: roleSpirit, Content: tc.content}})
			if got != tc.want {
				t.Errorf("lastWords = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLastWordsEmptyPodNameDoesNotRejectEverything(t *testing.T) {
	got := lastWords(autopsy.CauseOOMKilled, "", []spirit.Turn{{Role: roleSpirit, Content: "我从未呼吸过。"}})
	if got != "我从未呼吸过。" {
		t.Errorf("lastWords = %q, want the sentence", got)
	}
}

// recordedOpening is the first spirit reply of a file in fixtures/replay.
type recordedOpening struct {
	FixtureID string `json:"fixtureId"`
	Turns     []struct {
		TurnIndex int    `json:"turnIndex"`
		Cause     string `json:"cause"`
		Reply     string `json:"reply"`
	} `json:"turns"`
}

// TestLastWordsFromRecordedOpenings loads the real openings in fixtures/replay and
// checks the Last words the rule picks for each.
func TestLastWordsFromRecordedOpenings(t *testing.T) {
	want := map[string]string{
		"restless-spirit-84fc8b7c4c-52nl8": "所以不是我起不来，是我每次睁眼，都发现那张配置文件不在。",
		"lost-soul-5ff5cb8464-hjtms":       "我从未呼吸过。",
		"memory-glutton-7cc6df9966-bf87c":  "冥界没有留下记录的事，我不会替它编。",
		// English séances (task 10.4): fixture ID <pod>-en.
		"restless-spirit-84fc8b7c4c-52nl8-en": "Exit code 1.",
		"lost-soul-5ff5cb8464-hjtms-en":       "Not one restart.",
		"memory-glutton-7cc6df9966-bf87c-en":  "They say my memory grew heavy.",
	}
	entries, err := fs.ReadDir(fixtures.ReplayFS(), ".")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	seen := make(map[string]bool)
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := fs.ReadFile(fixtures.ReplayFS(), e.Name())
		if err != nil {
			t.Fatalf("ReadFile %s: %v", e.Name(), err)
		}
		var rec recordedOpening
		if err := json.Unmarshal(data, &rec); err != nil {
			t.Fatalf("decode %s: %v", e.Name(), err)
		}
		if len(rec.Turns) == 0 || rec.Turns[0].TurnIndex != 0 {
			t.Fatalf("%s: first turn is not the opening", e.Name())
		}
		w, ok := want[rec.FixtureID]
		if !ok {
			t.Errorf("%s: no expected Last words recorded in the test", rec.FixtureID)
			continue
		}
		seen[rec.FixtureID] = true
		opening := rec.Turns[0].Reply
		// The generator passes the pod name, which is the fixture ID without the
		// "-en" suffix of an English recording.
		pod := strings.TrimSuffix(rec.FixtureID, "-en")
		got := lastWords(autopsy.CauseOfDeath(rec.Turns[0].Cause), pod, []spirit.Turn{{Role: roleSpirit, Content: opening}})
		if got != w {
			t.Errorf("%s: Last words = %q, want %q", rec.FixtureID, got, w)
		}
	}
	for id := range want {
		if !seen[id] {
			t.Errorf("recording %s not found in fixtures/replay", id)
		}
	}
}

// TestLastWordsMockOpeningsUnchanged checks that the mock openings keep their
// first sentence as Last words.
func TestLastWordsMockOpeningsUnchanged(t *testing.T) {
	for _, c := range loadCases(t) {
		opening := openingLine(t, c.turns)
		want := expectedFirstSentence(opening)
		if got := lastWords(c.cls.Cause, c.a.Name, c.turns); got != want {
			t.Errorf("%s: Last words = %q, want %q", c.a.Name, got, want)
		}
	}
	const mockOOM = "我的内存一点点涨满，窒息的时候，我越来越重。"
	if got := lastWords(autopsy.CauseOOMKilled, "x", []spirit.Turn{{Role: roleSpirit, Content: mockOOM}}); got != mockOOM {
		t.Errorf("mock OOM opening: Last words = %q, want %q", got, mockOOM)
	}
}
