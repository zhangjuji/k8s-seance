package autopsy

// Property tests for the classifier and the evidence builder (Requirements 2.3,
// 2.4, 3; design.md "Testing Strategy", properties P-C1..P-C6 and P-E1..P-E8).
// They use testing/quick with fixed seeds, so every run checks the same cases.

import (
	"fmt"
	"math/rand"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"testing/quick"
	"unicode/utf8"
)

// propConfig returns a deterministic quick.Config. The seed is logged so a
// failure names the generator state that produced it.
func propConfig(t *testing.T, seed int64, n int) *quick.Config {
	t.Helper()
	t.Logf("testing/quick seed=%d cases=%d", seed, n)
	return &quick.Config{MaxCount: n, Rand: rand.New(rand.NewSource(seed))}
}

func pick[T any](r *rand.Rand, xs []T) T { return xs[r.Intn(len(xs))] }

var (
	genReasons = map[string][]string{
		"running":    {""},
		"waiting":    {"", "CrashLoopBackOff", "ImagePullBackOff", "ErrImagePull", "ContainerCreating", "CreateContainerConfigError"},
		"terminated": {"", "OOMKilled", "Error", "Completed", "ContainerCannotRun"},
	}
	genExitCodes = []*int32{nil, i32p(0), i32p(1), i32p(2), i32p(137), i32p(139), i32p(143)}
	genEvents    = []EventRecord{
		{Type: "Warning", Reason: "BackOff", Message: "Back-off restarting failed container app in pod x"},
		{Type: "Normal", Reason: "BackOff", Message: "Back-off pulling image \"nope:1\""},
		{Type: "Warning", Reason: "Unhealthy", Message: "Liveness probe failed: HTTP probe failed with statuscode: 500"},
		{Type: "Warning", Reason: "Unhealthy", Message: "Readiness probe failed: connection refused"},
		{Type: "Warning", Reason: "Unhealthy", Message: "Startup probe failed: timeout"},
		{Type: "Warning", Reason: "Unhealthy", Message: "something else is unwell"},
		{Type: "Warning", Reason: "OOMKilling", Message: "Memory cgroup out of memory: Killed process 42"},
		{Type: "Warning", Reason: "Failed", Message: "Failed to pull image \"nope:1\": not found"},
		{Type: "Normal", Reason: "Pulled", Message: "Successfully pulled image"},
		{Type: "Normal", Reason: "Killing", Message: "Stopping container app"},
	}
)

func i32p(v int32) *int32 { return &v }

func genState(r *rand.Rand) ContainerState {
	kind := pick(r, []string{"running", "waiting", "terminated"})
	s := ContainerState{Kind: kind, Reason: pick(r, genReasons[kind])}
	if kind == "terminated" {
		s.ExitCode = pick(r, genExitCodes)
	}
	return s
}

// genPod is a random PodAutopsy built from the vocabulary above. It
// implements quick.Generator.
type genPod struct{ A *PodAutopsy }

func (genPod) Generate(r *rand.Rand, _ int) reflect.Value {
	a := &PodAutopsy{Namespace: "ns", Name: "p"}
	for i := range r.Intn(4) {
		c := ContainerAutopsy{
			Name:         fmt.Sprintf("c%d", i),
			Image:        "img:1",
			RestartCount: int32(r.Intn(3)),
			State:        genState(r),
		}
		if r.Intn(2) == 0 {
			ls := genState(r)
			ls.Kind = "terminated"
			ls.Reason = pick(r, genReasons["terminated"])
			ls.ExitCode = pick(r, genExitCodes)
			c.LastState = &ls
		}
		a.Containers = append(a.Containers, c)
	}
	for range r.Intn(5) {
		a.Events = append(a.Events, pick(r, genEvents))
	}
	return reflect.ValueOf(genPod{a})
}

// Oracle predicates, written from the rules in design.md, not from classifier.go.

func anyTerminated(a *PodAutopsy, f func(ContainerState) bool) bool {
	for _, c := range a.Containers {
		if c.State.Kind == "terminated" && f(c.State) {
			return true
		}
		if c.LastState != nil && c.LastState.Kind == "terminated" && f(*c.LastState) {
			return true
		}
	}
	return false
}

func anyWaiting(a *PodAutopsy, reasons ...string) bool {
	for _, c := range a.Containers {
		if c.State.Kind == "waiting" && slices.Contains(reasons, c.State.Reason) {
			return true
		}
	}
	return false
}

func anyProbeEvent(a *PodAutopsy) bool {
	for _, e := range a.Events {
		m := strings.ToLower(e.Message)
		if e.Reason == "Unhealthy" && (strings.Contains(m, "liveness probe") ||
			strings.Contains(m, "readiness probe") || strings.Contains(m, "startup probe")) {
			return true
		}
	}
	return false
}

func anyOOMEvent(a *PodAutopsy) bool {
	for _, e := range a.Events {
		if strings.Contains(strings.ToLower(e.Reason+" "+e.Message), "oom") {
			return true
		}
	}
	return false
}

// anyCrashLoop: a CrashLoopBackOff waiting reason, or a restarted container
// plus a BackOff event about restarting a failed container (kind's shape).
func anyCrashLoop(a *PodAutopsy) bool {
	if anyWaiting(a, "CrashLoopBackOff") {
		return true
	}
	restarted := slices.ContainsFunc(a.Containers, func(c ContainerAutopsy) bool { return c.RestartCount > 0 })
	return restarted && slices.ContainsFunc(a.Events, func(e EventRecord) bool {
		return e.Reason == "BackOff" && strings.HasPrefix(e.Message, "Back-off restarting failed container")
	})
}

var primaryCauses = []CauseOfDeath{CauseOOMKilled, CauseImagePullBackOff, CauseCrashLoopBackOff}

// P-C1: Classify is total and deterministic, and its cause is one of the six.
func TestPropClassifyTotalDeterministic(t *testing.T) {
	valid := []CauseOfDeath{CauseOOMKilled, CauseCrashLoopBackOff, CauseImagePullBackOff,
		CauseError, CauseProbeFailure, CauseUnknown}
	f := func(g genPod) bool {
		c1, c2 := Classify(g.A), Classify(g.A)
		return slices.Contains(valid, c1.Cause) && reflect.DeepEqual(c1, c2)
	}
	if err := quick.Check(f, propConfig(t, 101, 3000)); err != nil {
		t.Fatal(err)
	}
	if got := Classify(nil).Cause; got != CauseUnknown {
		t.Fatalf("Classify(nil) = %s, want Unknown", got)
	}
}

// P-C2: rule order. An OOMKilled reason wins; otherwise an image-pull waiting
// reason wins; exit 137 is OOM only together with an OOM event; with no
// primary cause a probe event gives ProbeFailure, then a non-zero exit gives
// Error, else Unknown.
func TestPropClassifyRuleOrder(t *testing.T) {
	f := func(g genPod) bool {
		a := g.A
		got := Classify(a).Cause
		oomReason := anyTerminated(a, func(s ContainerState) bool { return s.Reason == "OOMKilled" })
		exit137 := anyTerminated(a, func(s ContainerState) bool { return s.ExitCode != nil && *s.ExitCode == 137 })
		oom := oomReason || (exit137 && anyOOMEvent(a))
		switch {
		case oom:
			return got == CauseOOMKilled
		case anyWaiting(a, "ImagePullBackOff", "ErrImagePull"):
			return got == CauseImagePullBackOff
		case anyCrashLoop(a):
			return got == CauseCrashLoopBackOff
		case anyProbeEvent(a):
			return got == CauseProbeFailure
		case anyTerminated(a, func(s ContainerState) bool { return s.ExitCode != nil && *s.ExitCode != 0 }):
			return got == CauseError
		default:
			return got == CauseUnknown
		}
	}
	if err := quick.Check(f, propConfig(t, 102, 3000)); err != nil {
		t.Fatal(err)
	}
}

// P-C3: Secondary is empty or exactly [ProbeFailure], and it is set exactly
// when the cause is primary and a probe event exists.
func TestPropClassifySecondary(t *testing.T) {
	f := func(g genPod) bool {
		c := Classify(g.A)
		want := slices.Contains(primaryCauses, c.Cause) && anyProbeEvent(g.A)
		if !want {
			return len(c.Secondary) == 0
		}
		return slices.Equal(c.Secondary, []CauseOfDeath{CauseProbeFailure})
	}
	if err := quick.Check(f, propConfig(t, 103, 3000)); err != nil {
		t.Fatal(err)
	}
}

// P-C4: the cause and secondary causes do not depend on the order of
// containers or events.
func TestPropClassifyOrderInvariant(t *testing.T) {
	r := rand.New(rand.NewSource(104))
	f := func(g genPod) bool {
		b := *g.A
		b.Containers = slices.Clone(g.A.Containers)
		b.Events = slices.Clone(g.A.Events)
		r.Shuffle(len(b.Containers), func(i, j int) { b.Containers[i], b.Containers[j] = b.Containers[j], b.Containers[i] })
		r.Shuffle(len(b.Events), func(i, j int) { b.Events[i], b.Events[j] = b.Events[j], b.Events[i] })
		x, y := Classify(g.A), Classify(&b)
		return x.Cause == y.Cause && slices.Equal(x.Secondary, y.Secondary)
	}
	if err := quick.Check(f, propConfig(t, 104, 3000)); err != nil {
		t.Fatal(err)
	}
}

// P-C5: LastExitCode is nil exactly when no terminated state has a non-zero
// exit code; otherwise it is the first one, state before lastState, in
// container order.
func TestPropClassifyLastExitCode(t *testing.T) {
	f := func(g genPod) bool {
		var want *int32
		for _, c := range g.A.Containers {
			for _, s := range []*ContainerState{&c.State, c.LastState} {
				if want == nil && s != nil && s.Kind == "terminated" && s.ExitCode != nil && *s.ExitCode != 0 {
					want = s.ExitCode
				}
			}
		}
		got := Classify(g.A).LastExitCode
		if want == nil || got == nil {
			return want == nil && got == nil
		}
		return *got == *want
	}
	if err := quick.Check(f, propConfig(t, 105, 3000)); err != nil {
		t.Fatal(err)
	}
}

// P-C6: exit 137 alone is not OOM. Removing every OOM event and OOMKilled
// reason from a pod with exit 137 never yields OOMKilled.
func TestPropClassify137WithoutOOMEvidence(t *testing.T) {
	f := func(g genPod) bool {
		a := g.A
		for i := range a.Containers {
			a.Containers[i].State.Reason = strings.ReplaceAll(a.Containers[i].State.Reason, "OOMKilled", "Error")
			if ls := a.Containers[i].LastState; ls != nil {
				ls.Reason = strings.ReplaceAll(ls.Reason, "OOMKilled", "Error")
				ls.ExitCode = i32p(137)
			}
		}
		a.Events = slices.DeleteFunc(a.Events, func(e EventRecord) bool {
			return strings.Contains(strings.ToLower(e.Reason+" "+e.Message), "oom")
		})
		return Classify(a).Cause != CauseOOMKilled
	}
	if err := quick.Check(f, propConfig(t, 106, 3000)); err != nil {
		t.Fatal(err)
	}
}

// --- evidence ---

var genLogWords = []string{"starting", "listening on :8080", "GET /healthz 200", "ERROR db down",
	"panic: nil map", "retrying", "fatal: config missing", "ok", "Killed", "done", "exception in thread"}

func genLog(r *rand.Rand) []string {
	var out []string
	for range r.Intn(40) {
		line := pick(r, genLogWords)
		for range r.Intn(3) + 1 { // runs of duplicates
			out = append(out, line)
		}
	}
	return out
}

// genEvidencePod adds logs and resources to genPod and picks options.
type genEvidencePod struct {
	A    *PodAutopsy
	Opts EvidenceOptions
}

func (genEvidencePod) Generate(r *rand.Rand, size int) reflect.Value {
	a := genPod{}.Generate(r, size).Interface().(genPod).A
	for i := range a.Containers {
		c := &a.Containers[i]
		c.Limits = map[string]string{"memory": "64Mi", "cpu": "100m"}
		c.LogTail = genLog(r)
		if r.Intn(3) > 0 {
			c.PreviousLogTail = genLog(r)
		}
	}
	opts := EvidenceOptions{TailLines: r.Intn(12) - 1, MaxChars: r.Intn(3000) - 100}
	return reflect.ValueOf(genEvidencePod{a, opts})
}

func effective(o EvidenceOptions) EvidenceOptions {
	if o.TailLines <= 0 {
		o.TailLines = 200
	}
	if o.MaxChars <= 0 {
		o.MaxChars = 24000
	}
	return o
}

// P-E1: IDs are E1..En without gaps, and the total text is at most MaxChars bytes.
func TestPropEvidenceNumberingAndBudget(t *testing.T) {
	f := func(g genEvidencePod) bool {
		ev, err := BuildEvidence(g.A, g.Opts)
		if err != nil {
			return false
		}
		total := 0
		for i, e := range ev {
			if e.ID != fmt.Sprintf("E%d", i+1) {
				return false
			}
			total += len(e.Text)
		}
		return total <= effective(g.Opts).MaxChars
	}
	if err := quick.Check(f, propConfig(t, 201, 1500)); err != nil {
		t.Fatal(err)
	}
}

// P-E2: truncation only cuts the tail. The evidence for a smaller MaxChars is a
// prefix of the evidence for a larger one, and kinds appear in the order
// status/spec, event, lineage, log.
func TestPropEvidenceTruncationIsPrefix(t *testing.T) {
	rank := map[string]int{"status": 0, "spec": 0, "event": 1, "lineage": 2, "log": 3}
	f := func(g genEvidencePod, cut uint16) bool {
		full, _ := BuildEvidence(g.A, EvidenceOptions{TailLines: g.Opts.TailLines, MaxChars: 1 << 20})
		small, _ := BuildEvidence(g.A, EvidenceOptions{TailLines: g.Opts.TailLines, MaxChars: int(cut) + 1})
		if len(small) > len(full) || !reflect.DeepEqual(small, full[:len(small)]) {
			return false
		}
		for i := 1; i < len(full); i++ {
			if rank[full[i].Kind] < rank[full[i-1].Kind] {
				return false
			}
		}
		return true
	}
	if err := quick.Check(f, propConfig(t, 202, 1000)); err != nil {
		t.Fatal(err)
	}
}

// P-E3: without truncation, each container has 1 status and 2 spec items, and
// each event has 1 event item.
func TestPropEvidenceCompleteWhenRoomy(t *testing.T) {
	f := func(g genEvidencePod) bool {
		ev, _ := BuildEvidence(g.A, EvidenceOptions{TailLines: g.Opts.TailLines, MaxChars: 1 << 20})
		count := map[string]int{}
		for _, e := range ev {
			count[e.Kind]++
		}
		n := len(g.A.Containers)
		return count["status"] == n && count["spec"] == 2*n && count["event"] == len(g.A.Events)
	}
	if err := quick.Check(f, propConfig(t, 203, 1000)); err != nil {
		t.Fatal(err)
	}
}

// P-E4: collapseDuplicates leaves no two equal neighbours, is idempotent, and
// keeps one line per run of equal lines, in order.
func TestPropCollapseDuplicates(t *testing.T) {
	f := func(seed int64) bool {
		r := rand.New(rand.NewSource(seed))
		in := genLog(r)
		out := collapseDuplicates(in)
		for i := 1; i < len(out); i++ {
			if out[i] == out[i-1] {
				return false
			}
		}
		var runs []string
		for i, l := range in {
			if i == 0 || l != in[i-1] {
				runs = append(runs, l)
			}
		}
		return slices.Equal(out, runs) && slices.Equal(collapseDuplicates(out), out)
	}
	if err := quick.Check(f, propConfig(t, 204, 3000)); err != nil {
		t.Fatal(err)
	}
}

// P-E5: selectLogLines returns at most tail lines, in order, all from the last
// tail collapsed lines. The last min(5, n) of those are always kept, and every
// other kept line matches the error pattern.
func TestPropSelectLogLines(t *testing.T) {
	f := func(seed int64, tail uint8) bool {
		r := rand.New(rand.NewSource(seed))
		in := genLog(r)
		n := int(tail%30) + 1
		window := collapseDuplicates(in)
		if len(window) > n {
			window = window[len(window)-n:]
		}
		out := selectLogLines(in, n)
		if len(out) > n {
			return false
		}
		// Exactly the window positions that are among the last 5 or match the
		// error pattern, in order.
		var want []string
		for i, l := range window {
			if i >= len(window)-5 || ImportantLine(l) {
				want = append(want, l)
			}
		}
		return slices.Equal(out, want)
	}
	if err := quick.Check(f, propConfig(t, 205, 3000)); err != nil {
		t.Fatal(err)
	}
}

// --- redaction ---

var secretAlphabet = []rune("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~+/")

func genSecret(r *rand.Rand) string {
	b := []rune("Zq9")
	for range r.Intn(20) + 8 {
		b = append(b, pick(r, secretAlphabet))
	}
	return string(b)
}

// secretTemplates are the shapes the design names: token, password, secret,
// api_key, Bearer and AWS access keys. %s is the secret value; the key name
// before it must stay visible.
var secretTemplates = []struct{ format, key string }{
	{"token=%s", "token="},
	{"TOKEN: %s", "TOKEN: "},
	{"password = %s", "password = "},
	{"db passwd:%s", "passwd:"},
	{`secret="%s"`, "secret="},
	{"api_key='%s'", "api_key="},
	{"apikey=%s&page=2", "apikey="},
	{"access_token=%s", "access_token="},
	{"Authorization: Bearer %s", "Authorization: Bearer "},
	{"bearer %s", "bearer "},
	{"token: Bearer %s", "token: "},
	{"password: bearer %s", "password: "},
}

var genFiller = []string{"", "pod started ", "level=info ", "GET /x 200 ", "retrying; ", "msg=\"boom\" "}

// P-E6: every templated secret value disappears after Redact, the key name
// stays, and [REDACTED] appears.
func TestPropRedactRemovesSecrets(t *testing.T) {
	f := func(seed int64) bool {
		r := rand.New(rand.NewSource(seed))
		tpl := pick(r, secretTemplates)
		v := genSecret(r)
		in := pick(r, genFiller) + fmt.Sprintf(tpl.format, v) + " " + pick(r, genFiller)
		out := Redact(in)
		if strings.Contains(out, v) || strings.Contains(out, v[:8]) || !strings.Contains(out, redacted) ||
			!strings.Contains(out, tpl.key) {
			t.Logf("in=%q out=%q", in, out)
			return false
		}
		return true
	}
	if err := quick.Check(f, propConfig(t, 301, 3000)); err != nil {
		t.Fatal(err)
	}
}

// P-E7: AWS access key IDs are removed wherever they appear.
func TestPropRedactAWSKey(t *testing.T) {
	upper := []rune("ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789")
	f := func(seed int64) bool {
		r := rand.New(rand.NewSource(seed))
		k := []rune("AKIA")
		for range 16 {
			k = append(k, pick(r, upper))
		}
		in := pick(r, genFiller) + "key " + string(k) + " " + pick(r, genFiller)
		return !strings.Contains(Redact(in), string(k))
	}
	if err := quick.Check(f, propConfig(t, 302, 2000)); err != nil {
		t.Fatal(err)
	}
}

// P-E8: Redact is idempotent and keeps valid UTF-8, text without a secret
// keyword, "bearer" or "AKIA" is unchanged, and every evidence text is
// already a fixed point of Redact.
func TestPropRedactIdempotentAndQuiet(t *testing.T) {
	keyword := regexp.MustCompile(`(?i)api_key|apikey|token|password|passwd|secret|bearer|AKIA`)
	words := []string{"pod", "died", "exit", "137", "memory", "oom", "=", ":", " ", "key", "pass", "数据库", "值", "\"", "'"}
	f := func(seed int64) bool {
		r := rand.New(rand.NewSource(seed))
		var b strings.Builder
		for range r.Intn(30) {
			b.WriteString(pick(r, words))
		}
		plain := b.String()
		if keyword.MatchString(plain) {
			return true
		}
		if Redact(plain) != plain {
			return false
		}
		tpl := pick(r, secretTemplates)
		mixed := plain + fmt.Sprintf(tpl.format, genSecret(r))
		once := Redact(mixed)
		return Redact(once) == once && utf8.ValidString(once)
	}
	if err := quick.Check(f, propConfig(t, 303, 3000)); err != nil {
		t.Fatal(err)
	}

	g := func(p genEvidencePod, seed int64) bool {
		r := rand.New(rand.NewSource(seed))
		var secrets []string
		for i := range p.A.Containers {
			v := genSecret(r)
			secrets = append(secrets, v)
			c := &p.A.Containers[i]
			c.LogTail = append(c.LogTail, fmt.Sprintf(pick(r, secretTemplates).format, v))
		}
		ev, _ := BuildEvidence(p.A, EvidenceOptions{TailLines: 50, MaxChars: 1 << 20})
		for _, e := range ev {
			if Redact(e.Text) != e.Text {
				return false
			}
			for _, s := range secrets {
				if strings.Contains(e.Text, s) {
					t.Logf("leak %q in %q", s, e.Text)
					return false
				}
			}
		}
		return true
	}
	if err := quick.Check(g, propConfig(t, 304, 800)); err != nil {
		t.Fatal(err)
	}
}
