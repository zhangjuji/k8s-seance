package spirit

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"unicode"

	"github.com/zhangjuji/k8s-seance/fixtures"
	"github.com/zhangjuji/k8s-seance/internal/autopsy"
	"github.com/zhangjuji/k8s-seance/internal/llm"
)

// fixtureCase is one recorded Pod with its classification and evidence.
type fixtureCase struct {
	name     string
	a        *autopsy.PodAutopsy
	cls      autopsy.Classification
	ev       []autopsy.Evidence
	deathStr string
	keywords []string // the opening must contain one of these
}

// loadFixtureCases loads the three recorded Pods in fixtures order.
func loadFixtureCases(t *testing.T) []fixtureCase {
	t.Helper()
	pods, err := fixtures.Load()
	if err != nil {
		t.Fatalf("fixtures.Load: %v", err)
	}
	if len(pods) != 3 {
		t.Fatalf("fixtures.Load returned %d pods, want 3", len(pods))
	}
	want := []struct {
		name     string
		cause    autopsy.CauseOfDeath
		deathStr string
		keywords []string
	}{
		{"oom", autopsy.CauseOOMKilled, "2026-11-01 04:55:23 UTC", []string{"OOM", "内存"}},
		{"crashloop", autopsy.CauseCrashLoopBackOff, "2026-11-01 04:55:14 UTC", []string{"复活", "CrashLoop"}},
		{"imagepull", autopsy.CauseImagePullBackOff, "2026-11-01 04:54:22 UTC", []string{"从未真正活过", "镜像"}},
	}
	out := make([]fixtureCase, 0, len(pods))
	for i, a := range pods {
		ev, err := autopsy.BuildEvidence(a, autopsy.EvidenceOptions{})
		if err != nil {
			t.Fatalf("BuildEvidence(%s): %v", a.Name, err)
		}
		cls := autopsy.Classify(a)
		if cls.Cause != want[i].cause {
			t.Fatalf("fixture %s classified as %s, want %s", a.Name, cls.Cause, want[i].cause)
		}
		out = append(out, fixtureCase{
			name:     want[i].name,
			a:        a,
			cls:      cls,
			ev:       ev,
			deathStr: want[i].deathStr,
			keywords: want[i].keywords,
		})
	}
	return out
}

func caseByName(t *testing.T, cases []fixtureCase, name string) fixtureCase {
	t.Helper()
	for _, c := range cases {
		if c.name == name {
			return c
		}
	}
	t.Fatalf("no fixture case %q", name)
	return fixtureCase{}
}

// countingProvider counts calls and records each request the inner provider gets.
type countingProvider struct {
	inner llm.Provider

	mu    sync.Mutex
	calls int
	reqs  []llm.ChatRequest
}

func (p *countingProvider) Stream(ctx context.Context, req llm.ChatRequest, onDelta func(string) error) (llm.Usage, error) {
	p.mu.Lock()
	p.calls++
	p.reqs = append(p.reqs, req)
	p.mu.Unlock()
	return p.inner.Stream(ctx, req, onDelta)
}

func (p *countingProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func (p *countingProvider) request(i int) llm.ChatRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.reqs[i]
}

func newMockProvider() *countingProvider {
	return &countingProvider{inner: llm.MockProvider{}}
}

func bigLimits() llm.Limits {
	return llm.Limits{MaxCalls: 10}
}

func newCaseSession(c fixtureCase, p llm.Provider, limits llm.Limits, cache *llm.OpeningCache) *Session {
	return NewSession("sess-"+c.name, c.a, c.cls, c.ev, p, limits, cache, "test-model")
}

// askText runs Ask and fails the test on error.
func askText(t *testing.T, s *Session, q string) string {
	t.Helper()
	text, err := s.Ask(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("Ask(%q): %v", q, err)
	}
	return text
}

func hasCJKText(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}

func TestOpeningNamesCauseAndCitesEvidence(t *testing.T) {
	for _, c := range loadFixtureCases(t) {
		t.Run(c.name, func(t *testing.T) {
			s := newCaseSession(c, newMockProvider(), bigLimits(), nil)
			text, err := s.Opening(context.Background(), nil)
			if err != nil {
				t.Fatalf("Opening: %v", err)
			}
			found := false
			for _, kw := range c.keywords {
				if strings.Contains(text, kw) {
					found = true
				}
			}
			if !found {
				t.Errorf("opening %q names none of %v", text, c.keywords)
			}
			if !strings.Contains(text, "[E") {
				t.Errorf("opening %q has no [E#] citation", text)
			}
			if !strings.Contains(text, "我死于 "+c.deathStr) {
				t.Errorf("opening %q lacks time of death %q", text, c.deathStr)
			}
		})
	}
}

func TestOpeningTwiceSpendsOneCall(t *testing.T) {
	c := loadFixtureCases(t)[0]
	p := newMockProvider()
	s := newCaseSession(c, p, bigLimits(), nil)

	var streamed []string
	first, err := s.Opening(context.Background(), func(d string) error {
		streamed = append(streamed, d)
		return nil
	})
	if err != nil {
		t.Fatalf("first Opening: %v", err)
	}
	second, err := s.Opening(context.Background(), nil)
	if err != nil {
		t.Fatalf("second Opening: %v", err)
	}
	if second != first {
		t.Errorf("second opening = %q, want stored %q", second, first)
	}
	if got := p.callCount(); got != 1 {
		t.Errorf("provider calls = %d, want 1", got)
	}
	if got := s.BudgetUsed(); got != 1 {
		t.Errorf("BudgetUsed = %d, want 1", got)
	}
	if strings.Join(streamed, "") != first {
		t.Errorf("streamed deltas = %q, want the opening", strings.Join(streamed, ""))
	}
}

func TestOpeningCacheSkipsBudget(t *testing.T) {
	c := loadFixtureCases(t)[1] // crashloop
	cache := &llm.OpeningCache{}
	p := newMockProvider()

	first := newCaseSession(c, p, bigLimits(), cache)
	text1, err := first.Opening(context.Background(), nil)
	if err != nil {
		t.Fatalf("first session Opening: %v", err)
	}
	if p.callCount() != 1 {
		t.Fatalf("first session calls = %d, want 1", p.callCount())
	}

	second := NewSession("second", c.a, c.cls, c.ev, p, bigLimits(), cache, "test-model")
	text2, err := second.Opening(context.Background(), nil)
	if err != nil {
		t.Fatalf("second session Opening: %v", err)
	}
	if text2 != text1 {
		t.Errorf("cached opening = %q, want %q", text2, text1)
	}
	if got := p.callCount(); got != 1 {
		t.Errorf("provider calls after cache hit = %d, want still 1", got)
	}
	if got := second.BudgetUsed(); got != 0 {
		t.Errorf("second session BudgetUsed = %d, want 0", got)
	}
	if n := len(second.Transcript()); n != 1 || second.Transcript()[0].Role != "spirit" {
		t.Errorf("second transcript = %+v, want one spirit turn", second.Transcript())
	}

	// A different model is a different key, so the cache does not answer.
	third := NewSession("third", c.a, c.cls, c.ev, p, bigLimits(), cache, "other-model")
	if _, err := third.Opening(context.Background(), nil); err != nil {
		t.Fatalf("third session Opening: %v", err)
	}
	if got := p.callCount(); got != 2 {
		t.Errorf("provider calls for other model = %d, want 2", got)
	}
}

func TestOpeningCacheKeyUsesRestartCount(t *testing.T) {
	c := loadFixtureCases(t)[0]
	cache := &llm.OpeningCache{}
	p := newMockProvider()
	newCaseSession(c, p, bigLimits(), cache).Opening(context.Background(), nil)

	restarted := *c.a
	restarted.Containers = append([]autopsy.ContainerAutopsy(nil), c.a.Containers...)
	restarted.Containers[0].RestartCount++
	s := NewSession("restarted", &restarted, c.cls, c.ev, p, bigLimits(), cache, "test-model")
	if _, err := s.Opening(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if got := p.callCount(); got != 2 {
		t.Errorf("provider calls after restart = %d, want 2 (new life, new opening)", got)
	}
}

func TestFixFollowUpZhOOMMentionsMemoryLimit(t *testing.T) {
	c := caseByName(t, loadFixtureCases(t), "oom")
	s := newCaseSession(c, newMockProvider(), bigLimits(), nil)
	text := askText(t, s, "怎么修？")
	if !strings.Contains(text, "memory limit") {
		t.Errorf("fix answer %q does not mention memory limit", text)
	}
	if !strings.Contains(text, "[E") {
		t.Errorf("fix answer %q has no [E#] citation", text)
	}
	if !strings.HasSuffix(strings.TrimRight(text, "。"), "]") {
		t.Errorf("fix answer %q should end with a citation", text)
	}
}

func TestFixFollowUpEnImagePullIsEnglish(t *testing.T) {
	c := caseByName(t, loadFixtureCases(t), "imagepull")
	s := newCaseSession(c, newMockProvider(), bigLimits(), nil)
	text := askText(t, s, "How do I fix this?")
	if hasCJKText(text) {
		t.Errorf("English question got a Chinese answer: %q", text)
	}
	if !strings.Contains(text, "tag") {
		t.Errorf("fix answer %q does not mention the image tag", text)
	}
	if !strings.Contains(text, "[E") {
		t.Errorf("fix answer %q has no [E#] citation", text)
	}
}

func TestOutOfEvidenceQuestion(t *testing.T) {
	c := caseByName(t, loadFixtureCases(t), "oom")
	tests := []struct {
		name     string
		question string
		want     string
	}{
		{"zh", "你昨天晚饭吃了什么？", "冥界没有留下记录"},
		{"en", "What is your favourite colour?", "kept no record"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newCaseSession(c, newMockProvider(), bigLimits(), nil)
			text := askText(t, s, tt.question)
			if !strings.Contains(text, tt.want) {
				t.Errorf("answer %q, want it to contain %q", text, tt.want)
			}
			if !strings.Contains(text, "[E") {
				t.Errorf("answer %q has no [E#] citation", text)
			}
		})
	}
}

func TestEnglishQuestionGetsEnglishReply(t *testing.T) {
	for _, c := range loadFixtureCases(t) {
		t.Run(c.name, func(t *testing.T) {
			s := newCaseSession(c, newMockProvider(), bigLimits(), nil)
			text := askText(t, s, "How did you die?")
			if hasCJKText(text) {
				t.Errorf("English question got a Chinese answer: %q", text)
			}
			if !strings.Contains(text, "My cause of death is "+string(c.cls.Cause)) {
				t.Errorf("answer %q does not restate the cause %s", text, c.cls.Cause)
			}
			if n := strings.Count(text, "[E"); n != 2 {
				t.Errorf("answer %q has %d citations, want 2", text, n)
			}
		})
	}
}

func TestTimeQuestionIncludesDeathTime(t *testing.T) {
	c := caseByName(t, loadFixtureCases(t), "crashloop")
	s := newCaseSession(c, newMockProvider(), bigLimits(), nil)
	text := askText(t, s, "你什么时候死的？")
	if !strings.Contains(text, "我死于 "+c.deathStr) {
		t.Errorf("time answer %q lacks %q", text, c.deathStr)
	}
}

func TestAskBudgetExhausted(t *testing.T) {
	c := caseByName(t, loadFixtureCases(t), "oom")
	p := newMockProvider()
	s := newCaseSession(c, p, llm.Limits{MaxCalls: 3}, nil)

	if _, err := s.Opening(context.Background(), nil); err != nil {
		t.Fatalf("Opening: %v", err)
	}
	for i := 1; i <= 2; i++ {
		text, err := s.Ask(context.Background(), "为什么死？", nil)
		if err != nil {
			t.Fatalf("ask %d: %v", i, err)
		}
		if text == llm.ExhaustedLine || !strings.Contains(text, "[E") {
			t.Errorf("ask %d = %q, want a cited answer", i, text)
		}
	}
	if !s.Exhausted() || s.BudgetRemaining() != 0 {
		t.Fatalf("after opening and 2 asks (3 calls of 3): exhausted=%v remaining=%d, want true and 0", s.Exhausted(), s.BudgetRemaining())
	}

	var streamed []string
	text, err := s.Ask(context.Background(), "为什么死？", func(d string) error {
		streamed = append(streamed, d)
		return nil
	})
	if err != nil {
		t.Fatalf("exhausted ask returned error %v, want nil", err)
	}
	if text != llm.ExhaustedLine {
		t.Errorf("exhausted ask = %q, want ExhaustedLine", text)
	}
	if strings.Join(streamed, "") != llm.ExhaustedLine {
		t.Errorf("streamed = %q, want ExhaustedLine", strings.Join(streamed, ""))
	}
	if !s.Exhausted() || s.BudgetRemaining() != 0 || s.BudgetUsed() != 3 {
		t.Errorf("exhausted=%v remaining=%d used=%d, want true 0 3", s.Exhausted(), s.BudgetRemaining(), s.BudgetUsed())
	}
	if got := p.callCount(); got != 3 {
		t.Errorf("provider calls = %d, want 3", got)
	}
	tr := s.Transcript()
	if last := tr[len(tr)-1]; last.Role != "spirit" || last.Content != llm.ExhaustedLine {
		t.Errorf("last turn = %+v, want spirit ExhaustedLine", last)
	}
}

func TestHistoryTrimmedInRequest(t *testing.T) {
	c := caseByName(t, loadFixtureCases(t), "oom")
	p := newMockProvider()
	s := newCaseSession(c, p, bigLimits(), nil)

	if _, err := s.Opening(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 6; i++ {
		askText(t, s, "q"+string(rune('0'+i)))
	}
	askText(t, s, "q7")

	req := p.request(p.callCount() - 1)
	msgs := req.Messages
	if len(msgs) < 2 || msgs[0].Role != "system" {
		t.Fatalf("messages = %+v, want system first", msgs)
	}
	if last := msgs[len(msgs)-1]; last.Role != "user" || last.Content != "q7" {
		t.Errorf("last message = %+v, want the new question q7", last)
	}
	middle := msgs[1 : len(msgs)-1]
	if len(middle) > 2*llm.HistoryRounds {
		t.Errorf("history has %d messages, want at most %d", len(middle), 2*llm.HistoryRounds)
	}
	if len(middle) != 2*llm.HistoryRounds {
		t.Fatalf("history has %d messages, want exactly %d", len(middle), 2*llm.HistoryRounds)
	}
	if middle[0].Role != "user" || middle[0].Content != "q3" {
		t.Errorf("oldest kept message = %+v, want user q3", middle[0])
	}
	if last := middle[len(middle)-1]; last.Role != "assistant" {
		t.Errorf("newest history message = %+v, want assistant", last)
	}
	for i, m := range req.Messages {
		if m.Role == "system" && i != 0 {
			t.Errorf("system message at index %d", i)
		}
	}
}

func TestTurnIndexesAndPurpose(t *testing.T) {
	c := caseByName(t, loadFixtureCases(t), "imagepull")
	p := newMockProvider()
	s := newCaseSession(c, p, bigLimits(), nil)
	if _, err := s.Opening(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	askText(t, s, "how do i fix it")
	askText(t, s, "why")

	open := p.request(0)
	if open.Purpose != llm.PurposeOpening || open.TurnIndex != 0 || open.FixtureID != c.a.Name || open.DeathTime != c.deathStr {
		t.Errorf("opening request = purpose %q turn %d fixture %q death %q", open.Purpose, open.TurnIndex, open.FixtureID, open.DeathTime)
	}
	for i := 1; i <= 2; i++ {
		req := p.request(i)
		if req.Purpose != llm.PurposeChat || req.TurnIndex != i {
			t.Errorf("request %d: purpose %q turn %d, want chat turn %d", i, req.Purpose, req.TurnIndex, i)
		}
	}
}

func TestAskEmptyQuestion(t *testing.T) {
	c := caseByName(t, loadFixtureCases(t), "oom")
	p := newMockProvider()
	s := newCaseSession(c, p, bigLimits(), nil)
	for _, q := range []string{"", "   \n"} {
		if _, err := s.Ask(context.Background(), q, nil); err == nil {
			t.Errorf("Ask(%q): want error", q)
		}
	}
	if p.callCount() != 0 || len(s.Transcript()) != 0 {
		t.Errorf("empty question changed state: calls=%d turns=%d", p.callCount(), len(s.Transcript()))
	}
}

func TestAskProviderErrorDropsQuestion(t *testing.T) {
	c := caseByName(t, loadFixtureCases(t), "oom")
	boom := errors.New("upstream down")
	s := newCaseSession(c, &countingProvider{inner: errProvider{boom}}, bigLimits(), nil)
	if _, err := s.Ask(context.Background(), "为什么死？", nil); !errors.Is(err, boom) {
		t.Fatalf("Ask error = %v, want upstream error", err)
	}
	if n := len(s.Transcript()); n != 0 {
		t.Errorf("transcript has %d turns after failed ask, want 0", n)
	}
}

type errProvider struct{ err error }

func (p errProvider) Stream(context.Context, llm.ChatRequest, func(string) error) (llm.Usage, error) {
	return llm.Usage{}, p.err
}

func TestTranscriptIsCopy(t *testing.T) {
	c := caseByName(t, loadFixtureCases(t), "oom")
	s := newCaseSession(c, newMockProvider(), bigLimits(), nil)
	askText(t, s, "你是谁？")
	tr := s.Transcript()
	tr[0].Content = "changed"
	if s.Transcript()[0].Content == "changed" {
		t.Error("Transcript returned the internal slice")
	}
	if got := s.Transcript(); len(got) != 2 || got[0].Role != "user" || got[1].Role != "spirit" {
		t.Errorf("transcript = %+v, want user then spirit", got)
	}
}

func TestConcurrentAsksRespectBudget(t *testing.T) {
	c := caseByName(t, loadFixtureCases(t), "oom")
	p := newMockProvider()
	s := newCaseSession(c, p, llm.Limits{MaxCalls: 3}, nil)

	const workers = 8
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Ask(context.Background(), "为什么死？", nil); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("ask: %v", err)
	}
	if got := p.callCount(); got != 3 {
		t.Errorf("provider calls = %d, want 3", got)
	}
	if got := len(s.Transcript()); got != 2*workers {
		t.Errorf("transcript turns = %d, want %d", got, 2*workers)
	}
}
