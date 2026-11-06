package spirit

import (
	"context"
	"encoding/json"
	"io/fs"
	"strings"
	"testing"

	"github.com/zhangjuji/k8s-seance/fixtures"
	"github.com/zhangjuji/k8s-seance/internal/llm"
)

// hasCJKRune reports whether s has a Han character or CJK punctuation.
func hasCJKRune(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool {
		return (r >= 0x4E00 && r <= 0x9FFF) || (r >= 0x3000 && r <= 0x303F) || (r >= 0xFF00 && r <= 0xFFEF)
	})
}

func newLangSession(c fixtureCase, lang string, p llm.Provider, limits llm.Limits, cache *llm.OpeningCache) *Session {
	return NewSessionIn(lang, "sess-"+lang+"-"+c.name, c.a, c.cls, c.ev, p, limits, cache, "test-model")
}

// TestSystemPromptLanguageRule checks that the Chinese prompt is unchanged for
// Lang "" and "zh", and that the English prompt is the Chinese one plus the rule.
func TestSystemPromptLanguageRule(t *testing.T) {
	for _, c := range loadFixtureCases(t) {
		d := PromptData{PodName: c.a.Name, Namespace: c.a.Namespace, Cause: c.cls.Cause, DeathTime: c.deathStr, Evidence: c.ev}
		zh, err := SystemPrompt(d)
		if err != nil {
			t.Fatal(err)
		}
		d.Lang = llm.LangZH
		if got, _ := SystemPrompt(d); got != zh {
			t.Errorf("%s: Lang zh prompt differs from the default prompt", c.name)
		}
		if strings.Contains(zh, "## Language") {
			t.Errorf("%s: Chinese prompt has the English rule", c.name)
		}
		d.Lang = llm.LangEN
		en, _ := SystemPrompt(d)
		if !strings.HasPrefix(en, zh) {
			t.Fatalf("%s: English prompt does not start with the Chinese prompt", c.name)
		}
		rule := strings.TrimPrefix(en, zh)
		if !strings.HasPrefix(rule, "\n## Language\n") || !strings.Contains(rule, "held in English") {
			t.Errorf("%s: English rule = %q", c.name, rule)
		}
	}
}

func TestFixtureID(t *testing.T) {
	tests := []struct{ pod, lang, want string }{
		{"ghost", llm.LangZH, "ghost"},
		{"ghost", "", "ghost"},
		{"ghost", llm.LangEN, "ghost-en"},
		{"ghost", "en-US", "ghost-en"},
		{"", llm.LangEN, ""},
	}
	for _, tt := range tests {
		if got := FixtureID(tt.pod, tt.lang); got != tt.want {
			t.Errorf("FixtureID(%q, %q) = %q, want %q", tt.pod, tt.lang, got, tt.want)
		}
	}
}

// TestEnglishSessionRequests checks what an English séance sends: the English
// opening request, the -en replay key, Lang en and the English prompt rule.
func TestEnglishSessionRequests(t *testing.T) {
	c := caseByName(t, loadFixtureCases(t), "oom")
	p := newMockProvider()
	s := newLangSession(c, llm.LangEN, p, bigLimits(), nil)
	if s.Lang != llm.LangEN {
		t.Fatalf("Lang = %q", s.Lang)
	}
	opening, err := s.Opening(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if hasCJKRune(opening) || !strings.Contains(opening, "[E") {
		t.Errorf("English mock opening = %q", opening)
	}
	req := p.request(0)
	if req.FixtureID != c.a.Name+"-en" || req.Lang != llm.LangEN || req.Purpose != llm.PurposeOpening {
		t.Errorf("opening request: fixture %q lang %q purpose %q", req.FixtureID, req.Lang, req.Purpose)
	}
	if last := req.Messages[len(req.Messages)-1]; last.Role != "user" || last.Content != "Please give your opening statement." {
		t.Errorf("opening user message = %+v", last)
	}
	if sys := req.Messages[0]; sys.Role != "system" || !strings.Contains(sys.Content, "## Language") {
		t.Error("English system prompt has no language rule")
	}

	if _, err := s.Ask(context.Background(), "How do I fix it?", nil); err != nil {
		t.Fatal(err)
	}
	if req := p.request(1); req.FixtureID != c.a.Name+"-en" || req.Lang != llm.LangEN || req.TurnIndex != 1 {
		t.Errorf("follow-up request: fixture %q lang %q turn %d", req.FixtureID, req.Lang, req.TurnIndex)
	}
}

// TestChineseSessionRequestsUnchanged checks that NewSession and an unknown
// language both give the Chinese séance.
func TestChineseSessionRequestsUnchanged(t *testing.T) {
	c := caseByName(t, loadFixtureCases(t), "crashloop")
	for _, lang := range []string{"zh", "fr", ""} {
		p := newMockProvider()
		s := newLangSession(c, lang, p, bigLimits(), nil)
		if _, err := s.Opening(context.Background(), nil); err != nil {
			t.Fatal(err)
		}
		req := p.request(0)
		if s.Lang != llm.LangZH || req.FixtureID != c.a.Name || req.Lang != llm.LangZH {
			t.Errorf("lang %q: session %q, fixture %q, request lang %q", lang, s.Lang, req.FixtureID, req.Lang)
		}
		if last := req.Messages[len(req.Messages)-1]; last.Content != openingRequest {
			t.Errorf("lang %q: opening request = %q", lang, last.Content)
		}
	}
}

// TestEnglishExhaustedLine checks that an English séance gets the English line
// once its calls are spent.
func TestEnglishExhaustedLine(t *testing.T) {
	c := caseByName(t, loadFixtureCases(t), "imagepull")
	s := newLangSession(c, llm.LangEN, newMockProvider(), llm.Limits{MaxCalls: 1}, nil)
	if _, err := s.Opening(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	var streamed strings.Builder
	text, err := s.Ask(context.Background(), "Why?", func(p string) error { streamed.WriteString(p); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if text != llm.ExhaustedLineEN || streamed.String() != llm.ExhaustedLineEN {
		t.Errorf("exhausted reply = %q, streamed %q", text, streamed.String())
	}
}

// TestOpeningCacheSeparatesLanguages checks that a Chinese and an English
// séance of the same Pod do not share a cached opening.
func TestOpeningCacheSeparatesLanguages(t *testing.T) {
	c := caseByName(t, loadFixtureCases(t), "oom")
	cache := &llm.OpeningCache{}
	p := newMockProvider()
	zh, _ := newLangSession(c, llm.LangZH, p, bigLimits(), cache).Opening(context.Background(), nil)
	en, _ := newLangSession(c, llm.LangEN, p, bigLimits(), cache).Opening(context.Background(), nil)
	if p.callCount() != 2 {
		t.Errorf("calls = %d, want 2 (one per language)", p.callCount())
	}
	if zh == en || !hasCJKRune(zh) || hasCJKRune(en) {
		t.Errorf("zh %q / en %q", zh, en)
	}
	again, _ := newLangSession(c, llm.LangEN, p, bigLimits(), cache).Opening(context.Background(), nil)
	if again != en || p.callCount() != 2 {
		t.Errorf("second English opening: %q, calls %d (want cache hit)", again, p.callCount())
	}
}

// TestEnglishReplayRecordings plays the recorded English openings in
// fixtures/replay/<pod>-en.json. Each fixture has one, it is English, it cites
// evidence that exists, and the Chinese séance still replays its own file.
// A follow-up without a recording falls back to the English mock.
func TestEnglishReplayRecordings(t *testing.T) {
	for _, c := range loadFixtureCases(t) {
		ids := make(map[string]bool, len(c.ev))
		for _, e := range c.ev {
			ids["["+e.ID+"]"] = true
		}
		replay := &countingProvider{inner: &llm.ReplayProvider{FS: fixtures.ReplayFS(), Fallback: llm.MockProvider{}}}

		en := newLangSession(c, llm.LangEN, replay, bigLimits(), nil)
		opening, err := en.Opening(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		recorded := recordedReply(t, c.a.Name+"-en", 0)
		if opening != recorded {
			t.Errorf("%s: English opening is not the recording", c.name)
		}
		if hasCJKRune(opening) {
			t.Errorf("%s: English recording has Chinese text: %q", c.name, opening)
		}
		refs := citationRE.FindAllString(opening, -1)
		if len(refs) == 0 {
			t.Errorf("%s: English recording cites nothing", c.name)
		}
		for _, r := range refs {
			if !ids[r] {
				t.Errorf("%s: English recording cites unknown evidence %s", c.name, r)
			}
		}
		if !strings.Contains(opening, c.deathStr) {
			t.Errorf("%s: English recording has no time of death %q", c.name, c.deathStr)
		}

		answer, err := en.Ask(context.Background(), "How did you die?", nil)
		if err != nil {
			t.Fatal(err)
		}
		if hasCJKRune(answer) || !strings.HasPrefix(answer, "My cause of death is ") {
			t.Errorf("%s: unrecorded English follow-up = %q, want the English mock", c.name, answer)
		}

		zh := newLangSession(c, llm.LangZH, replay, bigLimits(), nil)
		zhOpening, err := zh.Opening(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if zhOpening != recordedReply(t, c.a.Name, 0) {
			t.Errorf("%s: Chinese opening is not the Chinese recording", c.name)
		}
	}
}

// recordedReply reads the reply of one turn from fixtures/replay/<id>.json.
func recordedReply(t *testing.T, id string, turn int) string {
	t.Helper()
	data, err := fs.ReadFile(fixtures.ReplayFS(), id+".json")
	if err != nil {
		t.Fatalf("read recording %s: %v", id, err)
	}
	var rec struct {
		FixtureID string `json:"fixtureId"`
		Turns     []struct {
			TurnIndex int    `json:"turnIndex"`
			LastUser  string `json:"lastUser"`
			Reply     string `json:"reply"`
		} `json:"turns"`
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatalf("decode recording %s: %v", id, err)
	}
	if rec.FixtureID != id {
		t.Fatalf("recording %s has fixtureId %q", id, rec.FixtureID)
	}
	for _, tr := range rec.Turns {
		if tr.TurnIndex == turn {
			if turn == 0 && strings.HasSuffix(id, "-en") && tr.LastUser != openingRequestEN {
				t.Errorf("recording %s: opening lastUser = %q", id, tr.LastUser)
			}
			return tr.Reply
		}
	}
	t.Fatalf("recording %s has no turn %d", id, turn)
	return ""
}
