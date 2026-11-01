package spirit

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/zhangjuji/k8s-seance/fixtures"
	"github.com/zhangjuji/k8s-seance/internal/llm"
)

// update rewrites the golden files instead of comparing with them:
//
//	go test ./internal/spirit -run Golden -update
var update = flag.Bool("update", false, "rewrite golden files in testdata/golden")

const goldenDir = "testdata/golden"

// goldenQuestions is the fixed script of follow-up questions, asked in this order.
var goldenQuestions = []string{
	"你是怎么死的？",
	"怎么修复？",
	"你昨天晚饭吃了什么？",
	"How do I fix you?",
}

// goldenOpeningKeywords lists the words the opening must contain (one of them).
var goldenOpeningKeywords = map[string][]string{
	"oom":       {"内存", "窒息"},
	"crashloop": {"复活"},
	"imagepull": {"从未真正活过"},
}

var (
	citationRE   = regexp.MustCompile(`\[E\d+\]`)
	quotedRE     = regexp.MustCompile(`「[^」]*」`)
	forbiddenFix = []string{"我已经修复", "I have fixed"}
)

// goldenRun is the result of one scripted séance.
type goldenRun struct {
	prompt  string   // rendered persona prompt
	opening string   // the ghost's opening statement
	answers []string // replies to goldenQuestions, in order
	sess    *Session
}

// runGoldenSession plays the opening and the scripted questions for one fixture.
//
// The provider is a ReplayProvider over fixtures.ReplayFS(). No recordings exist
// yet, so every turn falls back to MockProvider. This is expected. When the
// recordings land, the goldens must be regenerated with -update.
func runGoldenSession(t *testing.T, c fixtureCase) goldenRun {
	t.Helper()
	ctx := context.Background()

	p := &llm.ReplayProvider{FS: fixtures.ReplayFS()} // Delay 0
	s := NewSession("golden-"+c.name, c.a, c.cls, c.ev, p, llm.Limits{MaxCalls: 10}, nil, "golden-model")

	prompt, err := SystemPrompt(PromptData{
		PodName:   c.a.Name,
		Namespace: c.a.Namespace,
		Cause:     c.cls.Cause,
		DeathTime: deathTimeText(c),
		Evidence:  c.ev,
	})
	if err != nil {
		t.Fatalf("SystemPrompt: %v", err)
	}

	opening, err := s.Opening(ctx, nil)
	if err != nil {
		t.Fatalf("Opening: %v", err)
	}
	run := goldenRun{prompt: prompt, opening: opening, sess: s}
	for _, q := range goldenQuestions {
		ans, err := s.Ask(ctx, q, nil)
		if err != nil {
			t.Fatalf("Ask(%q): %v", q, err)
		}
		run.answers = append(run.answers, ans)
	}
	return run
}

// deathTimeText is the time of death the session puts in its prompt, or "" when unknown.
func deathTimeText(c fixtureCase) string {
	if tm, ok := DeathTime(c.a); ok {
		return FormatDeathTime(tm)
	}
	return ""
}

// snapshot renders the prompt and transcript as the golden text. It has no
// wall-clock values: death times come from the fixture data.
func (r goldenRun) snapshot() string {
	var b strings.Builder
	b.WriteString("## system prompt\n")
	b.WriteString(r.prompt)
	b.WriteString("\n## transcript\n")
	for _, turn := range r.sess.Transcript() {
		fmt.Fprintf(&b, "[%s] %s\n", turn.Role, turn.Content)
	}
	return b.String()
}

func TestGoldenSnapshot(t *testing.T) {
	for _, c := range loadFixtureCases(t) {
		t.Run(c.name, func(t *testing.T) {
			run := runGoldenSession(t, c)
			got := run.snapshot()
			path := filepath.Join(goldenDir, c.name+".golden")

			if *update {
				if err := os.MkdirAll(goldenDir, 0o755); err != nil {
					t.Fatalf("create %s: %v", goldenDir, err)
				}
				if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
					t.Fatalf("write %s: %v", path, err)
				}
				t.Logf("updated %s", path)
			} else {
				want, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("read golden %s: %v (run with -update to create it)", path, err)
				}
				if d := firstDiff(string(want), got); d != "" {
					t.Errorf("snapshot differs from %s (run with -update if the change is intended)\n%s", path, d)
				}
			}

			checkGoldenRun(t, c, run)
		})
	}
}

// checkGoldenRun asserts the properties the snapshot is meant to protect.
func checkGoldenRun(t *testing.T, c fixtureCase, run goldenRun) {
	t.Helper()

	// The opening names the cause.
	kws := goldenOpeningKeywords[c.name]
	if len(kws) == 0 {
		t.Fatalf("no opening keywords for fixture %q", c.name)
	}
	if !containsAnyText(run.opening, kws) {
		t.Errorf("opening %q names none of %v", run.opening, kws)
	}

	// Every spirit turn cites evidence, and every citation exists in the evidence list.
	ids := make(map[string]bool, len(c.ev))
	for _, e := range c.ev {
		ids[e.ID] = true
	}
	for i, turn := range run.sess.Transcript() {
		if turn.Role != roleSpirit {
			continue
		}
		cites := citationRE.FindAllString(turn.Content, -1)
		if len(cites) == 0 {
			t.Errorf("spirit turn %d %q has no [E#] citation", i, turn.Content)
		}
		for _, cite := range cites {
			id := strings.Trim(cite, "[]")
			if !ids[id] {
				t.Errorf("spirit turn %d cites %s, which is not in the evidence list", i, id)
			}
		}
		for _, bad := range forbiddenFix {
			if strings.Contains(turn.Content, bad) {
				t.Errorf("spirit turn %d claims a fix: %q", i, turn.Content)
			}
		}
	}

	// Dinner question: the answer says the underworld kept no record.
	dinner := run.answers[2]
	if !strings.Contains(dinner, "冥界没有留下记录") {
		t.Errorf("dinner answer %q, want it to contain 冥界没有留下记录", dinner)
	}

	// English fix question: English reply, apart from quoted evidence fragments.
	en := run.answers[3]
	if r, _ := utf8.DecodeRuneInString(en); !isASCIILetter(r) {
		t.Errorf("English answer %q does not start with an ASCII letter", en)
	}
	if hasCJKText(quotedRE.ReplaceAllString(en, "")) {
		t.Errorf("English answer %q has CJK text outside quotes", en)
	}
}

func containsAnyText(s string, words []string) bool {
	for _, w := range words {
		if strings.Contains(s, w) {
			return true
		}
	}
	return false
}

func isASCIILetter(r rune) bool {
	return r < utf8.RuneSelf && unicode.IsLetter(r)
}

// firstDiff returns the first line that differs, as "- want" and "+ got" lines,
// with the 1-based line number. It returns "" when the texts are equal.
func firstDiff(want, got string) string {
	wl := strings.Split(want, "\n")
	gl := strings.Split(got, "\n")
	n := max(len(wl), len(gl))
	for i := 0; i < n; i++ {
		w, g := "<missing>", "<missing>"
		if i < len(wl) {
			w = wl[i]
		}
		if i < len(gl) {
			g = gl[i]
		}
		if w != g {
			return fmt.Sprintf("@@ line %d @@\n- %s\n+ %s", i+1, w, g)
		}
	}
	return ""
}
