package llm

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/fstest"
	"unicode"

	"github.com/zhangjuji/k8s-seance/internal/autopsy"
)

// hasCJKText reports whether s has a CJK character or CJK punctuation.
func hasCJKText(s string) bool {
	for _, r := range s {
		if hasCJK(string(r)) || strings.ContainsRune("，。！？：；「」（）、…", r) || unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}

func TestNormalizeLang(t *testing.T) {
	tests := map[string]string{
		"en": LangEN, "EN": LangEN, " en-US ": LangEN, "en_GB": LangEN,
		"zh": LangZH, "zh-CN": LangZH, "zh_Hans": LangZH, "ZH-tw": LangZH,
		"": "", "fr": "", "english": "", "e": "", "*": "",
	}
	for in, want := range tests {
		if got := NormalizeLang(in); got != want {
			t.Errorf("NormalizeLang(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestExhaustedLineFor(t *testing.T) {
	if got := ExhaustedLineFor(LangEN); got != ExhaustedLineEN {
		t.Errorf("en: got %q", got)
	}
	for _, l := range []string{"", LangZH, "fr"} {
		if got := ExhaustedLineFor(l); got != ExhaustedLine {
			t.Errorf("%q: got %q, want the Chinese line", l, got)
		}
	}
	if hasCJKText(ExhaustedLineEN) {
		t.Errorf("ExhaustedLineEN has CJK text: %q", ExhaustedLineEN)
	}
	if !IsExhaustedLine(ExhaustedLine) || !IsExhaustedLine(ExhaustedLineEN) || IsExhaustedLine("hello") {
		t.Error("IsExhaustedLine does not recognize exactly the two lines")
	}
}

// TestBudgetExhaustedFollowsLang checks that a spent budget answers in the
// request language and makes no call.
func TestBudgetExhaustedFollowsLang(t *testing.T) {
	for _, tt := range []struct{ lang, want string }{
		{LangEN, ExhaustedLineEN},
		{LangZH, ExhaustedLine},
		{"", ExhaustedLine},
	} {
		b := NewBudget(MockProvider{}, Limits{MaxCalls: 1})
		req := ChatRequest{Messages: userMsg("hi"), Evidence: testEvidence(), Purpose: PurposeChat, Lang: tt.lang}
		if _, err := b.Stream(context.Background(), req, func(string) error { return nil }); err != nil {
			t.Fatalf("first call: %v", err)
		}
		var got []string
		_, err := b.Stream(context.Background(), req, func(s string) error { got = append(got, s); return nil })
		if !errors.Is(err, ErrBudgetExhausted) {
			t.Fatalf("lang %q: err = %v, want ErrBudgetExhausted", tt.lang, err)
		}
		if len(got) != 1 || got[0] != tt.want {
			t.Errorf("lang %q: fragments = %q, want [%q]", tt.lang, got, tt.want)
		}
		if b.Used() != 1 {
			t.Errorf("lang %q: used = %d, want 1", tt.lang, b.Used())
		}
	}
}

func TestOpeningKeyIncludesLang(t *testing.T) {
	var c OpeningCache
	zh := OpeningKey{PodUID: "u", Model: "m", Lang: LangZH}
	en := OpeningKey{PodUID: "u", Model: "m", Lang: LangEN}
	c.Put(zh, "中文开场")
	if _, ok := c.Get(en); ok {
		t.Fatal("English key hit the Chinese opening")
	}
	c.Put(en, "English opening")
	if got, _ := c.Get(zh); got != "中文开场" {
		t.Errorf("zh opening = %q", got)
	}
	if got, _ := c.Get(en); got != "English opening" {
		t.Errorf("en opening = %q", got)
	}
}

// TestMockOpeningEnglish checks the English mock opening for every cause: no CJK
// text, at least one citation, and the time of death when it is known.
func TestMockOpeningEnglish(t *testing.T) {
	causes := []autopsy.CauseOfDeath{
		autopsy.CauseOOMKilled, autopsy.CauseCrashLoopBackOff, autopsy.CauseImagePullBackOff,
		autopsy.CauseProbeFailure, autopsy.CauseError, autopsy.CauseUnknown, "",
	}
	for _, cause := range causes {
		req := ChatRequest{
			Messages:  userMsg("Please give your opening statement."),
			Cause:     cause,
			Evidence:  testEvidence(),
			Purpose:   PurposeOpening,
			DeathTime: "2026-11-01 04:55:23 UTC",
			Lang:      LangEN,
		}
		chunks, _ := collect(t, context.Background(), MockProvider{}, req)
		text := strings.Join(chunks, "")
		if hasCJKText(text) {
			t.Errorf("%s: English opening has CJK text: %q", cause, text)
		}
		if !strings.Contains(text, "[E1]") {
			t.Errorf("%s: English opening cites nothing: %q", cause, text)
		}
		if !strings.Contains(text, "I died at 2026-11-01 04:55:23 UTC.") {
			t.Errorf("%s: English opening has no time of death: %q", cause, text)
		}
		if !strings.HasPrefix(text, voiceForEN(cause)) {
			t.Errorf("%s: English opening does not start with the voice: %q", cause, text)
		}
	}

	// No evidence and no time: the voice and the fixed English no-record line.
	req := ChatRequest{Cause: autopsy.CauseOOMKilled, Purpose: PurposeOpening, Lang: LangEN, Messages: userMsg("x")}
	chunks, _ := collect(t, context.Background(), MockProvider{}, req)
	if got, want := strings.Join(chunks, ""), voiceLinesEN[autopsy.CauseOOMKilled]+" The underworld kept no record of that."; got != want {
		t.Errorf("empty English opening = %q, want %q", got, want)
	}
}

// TestMockOpeningChineseUnchanged checks that Lang "" and "zh" give the same
// Chinese opening as before.
func TestMockOpeningChineseUnchanged(t *testing.T) {
	base := ChatRequest{Messages: userMsg("请做开场陈述。"), Cause: autopsy.CauseOOMKilled, Evidence: testEvidence(), Purpose: PurposeOpening}
	want := openingText(base)
	if !strings.HasPrefix(want, voiceLines[autopsy.CauseOOMKilled]) {
		t.Fatalf("Chinese opening = %q", want)
	}
	zh := base
	zh.Lang = LangZH
	if got := openingText(zh); got != want {
		t.Errorf("Lang zh opening = %q, want %q", got, want)
	}
}

// TestReplayEnglishFixture checks that an English request with the -en replay
// key reads its own file, and falls back to the English mock without one.
func TestReplayEnglishFixture(t *testing.T) {
	rec := `{"fixtureId":"ghost-en","turns":[{"turnIndex":0,"cause":"OOMKilled","lastUser":"Please give your opening statement.","reply":"I choked on 64Mi [E2].","usage":{"inputTokens":1,"outputTokens":2}}]}`
	zhRec := `{"fixtureId":"ghost","turns":[{"turnIndex":0,"cause":"OOMKilled","lastUser":"请做开场陈述。","reply":"我窒息了 [E2]。","usage":{"inputTokens":1,"outputTokens":2}}]}`
	p := &ReplayProvider{FS: fstest.MapFS{
		"ghost-en.json": {Data: []byte(rec)},
		"ghost.json":    {Data: []byte(zhRec)},
	}}
	req := ChatRequest{Messages: userMsg("Please give your opening statement."), Cause: autopsy.CauseOOMKilled, Evidence: testEvidence(), Purpose: PurposeOpening, Lang: LangEN}

	req.FixtureID = "ghost-en"
	chunks, _ := collect(t, context.Background(), p, req)
	if got := strings.Join(chunks, ""); got != "I choked on 64Mi [E2]." {
		t.Errorf("English replay = %q", got)
	}

	req.FixtureID = "nobody-en"
	chunks, _ = collect(t, context.Background(), p, req)
	if got := strings.Join(chunks, ""); hasCJKText(got) || !strings.HasPrefix(got, voiceLinesEN[autopsy.CauseOOMKilled]) {
		t.Errorf("missing English recording: got %q, want the English mock opening", got)
	}
}
