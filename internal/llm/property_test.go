package llm

// Property tests for the per-séance budget and history clipping
// (Requirement 6.2, 6.3; design.md "Testing Strategy", properties P-B1..P-B5
// and P-H1..P-H4). They use testing/quick with fixed seeds.

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/quick"
	"unicode/utf8"

	"github.com/zhangjuji/k8s-seance/internal/autopsy"
)

func propCfg(t *testing.T, seed int64, n int) *quick.Config {
	t.Helper()
	t.Logf("testing/quick seed=%d cases=%d", seed, n)
	return &quick.Config{MaxCount: n, Rand: rand.New(rand.NewSource(seed))}
}

// propTokens is the documented estimate, ceil(runes/2) over message contents
// and evidence texts, written again here as the oracle.
func propTokens(msgs []Message, ev []autopsy.Evidence) int {
	n := 0
	for _, m := range msgs {
		n += utf8.RuneCountInString(m.Content)
	}
	for _, e := range ev {
		n += utf8.RuneCountInString(e.Text)
	}
	return (n + 1) / 2
}

var propRoles = []string{"system", "user", "assistant", "assistant", "user"}

func propText(r *rand.Rand, maxRunes int) string {
	alphabet := []rune("abcdefgh 亡灵证据[E1]")
	n := r.Intn(maxRunes + 1)
	b := make([]rune, n)
	for i := range b {
		b[i] = alphabet[r.Intn(len(alphabet))]
	}
	return string(b)
}

// propMessages is a random history. Each message has a unique id prefix so
// subsequence checks are exact.
type propMessages []Message

func (propMessages) Generate(r *rand.Rand, _ int) reflect.Value {
	n := r.Intn(14)
	out := make(propMessages, n)
	for i := range out {
		out[i] = Message{Role: propRoles[r.Intn(len(propRoles))], Content: fmt.Sprintf("#%d %s", i, propText(r, 60))}
	}
	return reflect.ValueOf(out)
}

func isSubsequence(sub, full []Message) bool {
	j := 0
	for _, m := range full {
		if j < len(sub) && sub[j] == m {
			j++
		}
	}
	return j == len(sub)
}

func filterRole(msgs []Message, keep func(string) bool) []Message {
	var out []Message
	for _, m := range msgs {
		if keep(m.Role) {
			out = append(out, m)
		}
	}
	return out
}

func notSystem(r string) bool { return r != "system" }

// P-H1: TrimHistory returns a subsequence in the original order that keeps
// every system message, and its non-system messages are a suffix of the
// input's non-system messages.
func TestPropTrimHistoryShape(t *testing.T) {
	f := func(msgs propMessages, rounds int8) bool {
		in := []Message(msgs)
		out := TrimHistory(in, int(rounds%8))
		if !isSubsequence(out, in) {
			return false
		}
		if !slices.Equal(filterRole(out, func(r string) bool { return r == "system" }),
			filterRole(in, func(r string) bool { return r == "system" })) {
			return false
		}
		ns, all := filterRole(out, notSystem), filterRole(in, notSystem)
		return slices.Equal(ns, all[len(all)-len(ns):])
	}
	if err := quick.Check(f, propCfg(t, 401, 5000)); err != nil {
		t.Fatal(err)
	}
}

// P-H2: the output has min(max(rounds,0), users) user messages. When some
// rounds were dropped, the first kept non-system message is a user message.
func TestPropTrimHistoryRounds(t *testing.T) {
	f := func(msgs propMessages, rounds int8) bool {
		in := []Message(msgs)
		r := int(rounds % 8)
		out := TrimHistory(in, r)
		isUser := func(role string) bool { return role == "user" }
		users := len(filterRole(in, isUser))
		if len(filterRole(out, isUser)) != min(max(r, 0), users) {
			return false
		}
		ns := filterRole(out, notSystem)
		if users > max(r, 0) && len(ns) > 0 && ns[0].Role != "user" {
			return false
		}
		if users < r && len(ns) != len(filterRole(in, notSystem)) {
			return false // fewer rounds than asked: keep every non-system message
		}
		return true
	}
	if err := quick.Check(f, propCfg(t, 402, 5000)); err != nil {
		t.Fatal(err)
	}
}

// P-H3: TrimHistory is idempotent and does not modify or alias its input.
func TestPropTrimHistoryPure(t *testing.T) {
	f := func(msgs propMessages, rounds int8) bool {
		in := []Message(msgs)
		before := slices.Clone(in)
		r := int(rounds % 8)
		out := TrimHistory(in, r)
		if !slices.Equal(in, before) || !slices.Equal(TrimHistory(out, r), out) {
			return false
		}
		if len(out) > 0 {
			out[0].Content = "changed"
			if !slices.Equal(in, before) {
				return false
			}
		}
		return true
	}
	if err := quick.Check(f, propCfg(t, 403, 5000)); err != nil {
		t.Fatal(err)
	}
}

// --- budget ---

// propInner records every request it receives and reports a random input
// count, sometimes larger than the estimate.
type propInner struct {
	r        *rand.Rand
	reqs     []ChatRequest
	reported int // InputTokens of the most recent call
	fail     bool
}

func (p *propInner) Stream(_ context.Context, req ChatRequest, onDelta func(string) error) (Usage, error) {
	p.reqs = append(p.reqs, req)
	est := propTokens(req.Messages, req.Evidence)
	u := Usage{InputTokens: est + p.r.Intn(3)*p.r.Intn(40) - p.r.Intn(5), OutputTokens: 5}
	p.reported = u.InputTokens
	if onDelta != nil {
		_ = onDelta("ok")
	}
	if p.fail && p.r.Intn(4) == 0 {
		return u, errors.New("inner failed")
	}
	return u, nil
}

// propScenario is a random set of limits plus a random sequence of requests.
type propScenario struct {
	Limits Limits
	Reqs   []ChatRequest
	Seed   int64
	Fail   bool
}

func (propScenario) Generate(r *rand.Rand, size int) reflect.Value {
	s := propScenario{
		Limits: Limits{
			MaxCalls:                  r.Intn(8) - 1,
			MaxInputTokens:            r.Intn(400) - 20,
			MaxTotalInputTokens:       r.Intn(1500) - 50,
			MaxOutputTokens:           r.Intn(500) - 50,
			MaxPostmortemOutputTokens: r.Intn(2000) - 100,
		},
		Seed: r.Int63(),
		Fail: r.Intn(2) == 0,
	}
	for range r.Intn(12) {
		msgs := []Message(propMessages{}.Generate(r, size).Interface().(propMessages))
		if r.Intn(3) > 0 {
			msgs = append(msgs, Message{Role: "user", Content: "last " + propText(r, 30)})
		}
		var ev []autopsy.Evidence
		for i := range r.Intn(4) {
			ev = append(ev, autopsy.Evidence{ID: fmt.Sprintf("E%d", i+1), Text: propText(r, 120)})
		}
		turn := r.Intn(4)
		if r.Intn(5) == 0 {
			turn = PostmortemTurnIndex
		}
		lang := LangZH
		if r.Intn(2) == 0 {
			lang = LangEN
		}
		s.Reqs = append(s.Reqs, ChatRequest{Messages: msgs, Evidence: ev, TurnIndex: turn,
			MaxTokens: r.Intn(3000) - 100, Lang: lang})
	}
	return reflect.ValueOf(s)
}

// P-B6: under concurrent calls the inner provider is still called at most
// MaxCalls times, and the forwarded estimates stay within MaxTotalInputTokens.
func TestPropBudgetConcurrent(t *testing.T) {
	f := func(calls8, workers8 uint8, seed int64) bool {
		maxCalls := int(calls8%6) + 1
		workers := int(workers8%24) + 2
		var mu sync.Mutex
		forwarded, sumEst := 0, 0
		inner := providerFunc(func(req ChatRequest) {
			mu.Lock()
			forwarded++
			sumEst += propTokens(req.Messages, req.Evidence)
			mu.Unlock()
		})
		b := NewBudget(inner, Limits{MaxCalls: maxCalls, MaxTotalInputTokens: 200 + int(uint64(seed)%300)})
		start := make(chan struct{})
		var wg sync.WaitGroup
		for w := range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				req := ChatRequest{Messages: []Message{{Role: "user", Content: fmt.Sprintf("q%d %s", w, strings.Repeat("x", 40))}}}
				_, _ = b.Stream(context.Background(), req, nil)
			}()
		}
		close(start)
		wg.Wait()
		return forwarded <= maxCalls && b.Used() == forwarded && sumEst <= b.Limits().MaxTotalInputTokens
	}
	if err := quick.Check(f, propCfg(t, 502, 400)); err != nil {
		t.Fatal(err)
	}
}

// providerFunc is a Provider that calls fn and replies "ok". It yields first,
// so concurrent callers overlap inside Budget.Stream.
type providerFunc func(ChatRequest)

func (p providerFunc) Stream(_ context.Context, req ChatRequest, onDelta func(string) error) (Usage, error) {
	p(req)
	runtime.Gosched()
	if onDelta != nil {
		_ = onDelta("ok")
	}
	return Usage{}, nil
}

func lastUserIndex(msgs []Message) int {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			return i
		}
	}
	return -1
}

// P-B1..P-B5, checked on every call of every random scenario:
//
//	B1 the inner provider is called at most MaxCalls times, Used() equals the
//	   number of inner calls, and Used()+Remaining() == MaxCalls until the
//	   séance's input is spent (then Remaining() == 0);
//	B2 every forwarded request is within MaxInputTokens, and the forwarded
//	   estimates sum to at most MaxTotalInputTokens; InputUsed() is the sum of
//	   max(estimate, reported input) over the calls;
//	B3 forwarded MaxTokens is in [1, limit], where the limit is
//	   MaxPostmortemOutputTokens for TurnIndex 100 and MaxOutputTokens otherwise,
//	   and equals the caller's value when that is already in range;
//	B4 trimming keeps the evidence, every system message and the last user
//	   message, keeps the order, and drops the oldest droppable messages first;
//	B5 ErrInputTooLarge is not counted; once ErrBudgetExhausted is returned,
//	   every later call returns it too, emits ExhaustedLineFor(lang) and does not
//	   reach the inner provider.
func TestPropBudget(t *testing.T) {
	// seen counts outcomes, so the test fails if the generator stops reaching one.
	seen := map[string]int{}
	f := func(s propScenario) bool {
		inner := &propInner{r: rand.New(rand.NewSource(s.Seed)), fail: s.Fail}
		b := NewBudget(inner, s.Limits)
		lim := b.Limits()
		if lim.MaxCalls <= 0 || lim.MaxInputTokens <= 0 || lim.MaxTotalInputTokens <= 0 ||
			lim.MaxOutputTokens <= 0 || lim.MaxPostmortemOutputTokens <= 0 {
			return false
		}
		exhausted := false
		sumEst, accounted := 0, 0
		for i, req := range s.Reqs {
			usedBefore, callsBefore := b.Used(), len(inner.reqs)
			var deltas []string
			_, err := b.Stream(context.Background(), req, func(d string) error {
				deltas = append(deltas, d)
				return nil
			})
			called := len(inner.reqs) > callsBefore
			switch {
			case errors.Is(err, ErrBudgetExhausted):
				if called || !slices.Equal(deltas, []string{ExhaustedLineFor(req.Lang)}) {
					t.Logf("call %d: exhausted but inner called or wrong line %q", i, deltas)
					return false
				}
				exhausted = true
				seen["exhausted"]++
			case errors.Is(err, ErrInputTooLarge):
				seen["too large"]++
				if called || b.Used() != usedBefore {
					t.Logf("call %d: ErrInputTooLarge was counted", i)
					return false
				}
			default:
				if exhausted || !called {
					t.Logf("call %d: call made after exhaustion (exhausted=%v called=%v err=%v)", i, exhausted, called, err)
					return false
				}
				got := inner.reqs[len(inner.reqs)-1]
				est := propTokens(got.Messages, got.Evidence)
				if est > lim.MaxInputTokens {
					t.Logf("call %d: estimate %d > per-call %d", i, est, lim.MaxInputTokens)
					return false
				}
				sumEst += est
				seen["forwarded"]++
				if len(got.Messages) < len(req.Messages) {
					seen["trimmed"]++
				}
				if got.MaxTokens != req.MaxTokens {
					seen["clamped"]++
				}
				if req.TurnIndex == PostmortemTurnIndex {
					seen["postmortem"]++
				}
				accounted += max(est, inner.reported)
				out := lim.MaxOutputTokens
				if req.TurnIndex == PostmortemTurnIndex {
					out = lim.MaxPostmortemOutputTokens
				}
				if got.MaxTokens < 1 || got.MaxTokens > out ||
					(req.MaxTokens >= 1 && req.MaxTokens <= out && got.MaxTokens != req.MaxTokens) {
					t.Logf("call %d: MaxTokens %d (asked %d, limit %d)", i, got.MaxTokens, req.MaxTokens, out)
					return false
				}
				if !reflect.DeepEqual(got.Evidence, req.Evidence) || !isSubsequence(got.Messages, req.Messages) ||
					!slices.Equal(filterRole(got.Messages, func(r string) bool { return r == "system" }),
						filterRole(req.Messages, func(r string) bool { return r == "system" })) {
					t.Logf("call %d: evidence, order or system messages changed", i)
					return false
				}
				if lu := lastUserIndex(req.Messages); lu >= 0 {
					if !slices.Contains(got.Messages, req.Messages[lu]) {
						t.Logf("call %d: last user message dropped", i)
						return false
					}
					// Oldest first: the kept droppable messages before the last
					// user message are a suffix of the original ones.
					orig := filterRole(req.Messages[:lu], notSystem)
					gl := lastUserIndex(got.Messages)
					kept := filterRole(got.Messages[:gl], notSystem)
					if !slices.Equal(kept, orig[len(orig)-len(kept):]) {
						t.Logf("call %d: dropped a newer message before an older one", i)
						return false
					}
				}
			}
			if len(inner.reqs) > lim.MaxCalls || b.Used() != len(inner.reqs) {
				return false
			}
			if b.Remaining() != 0 && b.Used()+b.Remaining() != lim.MaxCalls {
				return false
			}
			if exhausted && !b.Exhausted() {
				return false
			}
		}
		if sumEst > lim.MaxTotalInputTokens || b.InputUsed() != accounted || b.InputRemaining() < 0 {
			t.Logf("sumEst=%d accounted=%d InputUsed=%d max=%d", sumEst, accounted, b.InputUsed(), lim.MaxTotalInputTokens)
			return false
		}
		return true
	}
	if err := quick.Check(f, propCfg(t, 501, 3000)); err != nil {
		t.Fatal(err)
	}
	t.Logf("outcomes: %v", seen)
	for _, k := range []string{"exhausted", "too large", "forwarded", "trimmed", "clamped", "postmortem"} {
		if seen[k] == 0 {
			t.Errorf("generator never produced outcome %q", k)
		}
	}
}
