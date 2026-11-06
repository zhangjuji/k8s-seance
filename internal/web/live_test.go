package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhangjuji/k8s-seance/internal/autopsy"
	"github.com/zhangjuji/k8s-seance/internal/llm"
	"k8s.io/client-go/kubernetes/fake"
)

// The cluster source must offer the live stream; the fixtures must not.
var _ Watcher = ClusterSource{}

func sp(name string, cause autopsy.CauseOfDeath, restarts int32) autopsy.SpiritSummary {
	return autopsy.SpiritSummary{Namespace: "ns1", Name: name, Cause: cause, RestartCount: restarts}
}

func TestDiffSpirits(t *testing.T) {
	prev := []autopsy.SpiritSummary{
		sp("same", autopsy.CauseOOMKilled, 2),
		sp("restarted", autopsy.CauseCrashLoopBackOff, 3),
		sp("recaused", autopsy.CauseError, 1),
		sp("fewer", autopsy.CauseError, 5),
		sp("gone", autopsy.CauseError, 1),
	}
	cur := []autopsy.SpiritSummary{
		sp("new", autopsy.CauseImagePullBackOff, 0),
		sp("same", autopsy.CauseOOMKilled, 2),
		sp("restarted", autopsy.CauseCrashLoopBackOff, 4),
		sp("recaused", autopsy.CauseOOMKilled, 1),
		sp("fewer", autopsy.CauseError, 0),
	}
	arrived, worse := diffSpirits(prev, cur)
	if len(arrived) != 1 || arrived[0].Name != "new" {
		t.Errorf("arrived = %+v, want only new", arrived)
	}
	if len(worse) != 2 {
		t.Fatalf("worse = %+v, want restarted and recaused", worse)
	}
	if w := worse[0]; w.Name != "restarted" || w.FromRestarts != 3 || w.Restarts != 4 {
		t.Errorf("worse[0] = %+v", w)
	}
	if w := worse[1]; w.Name != "recaused" || w.FromCause != "Error" || w.Cause != "OOMKilled" {
		t.Errorf("worse[1] = %+v", w)
	}

	// The same name in another namespace is a different spirit.
	other := sp("same", autopsy.CauseOOMKilled, 2)
	other.Namespace = "ns2"
	if a, _ := diffSpirits(prev, []autopsy.SpiritSummary{other}); len(a) != 1 {
		t.Errorf("other namespace: arrived = %+v, want 1", a)
	}
}

func TestMassGrave(t *testing.T) {
	t0 := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	var m massGrave
	d := func(at time.Time, cause, pod string) death { return death{at: at, cause: cause, pod: pod} }

	if got := m.add(t0, []death{d(t0, "OOMKilled", "a")}); len(got) != 0 {
		t.Fatalf("one death: %+v", got)
	}
	// The same Pod dying again is not a mass grave.
	if got := m.add(t0.Add(time.Second), []death{d(t0.Add(time.Second), "OOMKilled", "a")}); len(got) != 0 {
		t.Fatalf("same pod twice: %+v", got)
	}
	// A different cause does not join the group.
	if got := m.add(t0.Add(2*time.Second), []death{d(t0.Add(2*time.Second), "Error", "x")}); len(got) != 0 {
		t.Fatalf("other cause: %+v", got)
	}
	got := m.add(t0.Add(10*time.Second), []death{d(t0.Add(10*time.Second), "OOMKilled", "b")})
	if len(got) != 1 || got[0].Cause != "OOMKilled" || strings.Join(got[0].Pods, ",") != "a,b" {
		t.Fatalf("two pods: %+v", got)
	}
	// No change: not reported again.
	if got := m.add(t0.Add(11*time.Second), nil); len(got) != 0 {
		t.Fatalf("repeat: %+v", got)
	}
	// The group grows: reported again with all three.
	got = m.add(t0.Add(20*time.Second), []death{d(t0.Add(20*time.Second), "OOMKilled", "c")})
	if len(got) != 1 || strings.Join(got[0].Pods, ",") != "a,b,c" {
		t.Fatalf("three pods: %+v", got)
	}
	// 61 s after the last death everything has left the window; a lone new death is no grave.
	late := t0.Add(81 * time.Second)
	if got := m.add(late, []death{d(late, "OOMKilled", "d")}); len(got) != 0 {
		t.Fatalf("outside window: %+v", got)
	}
	// A fresh pair reports again after the old group left.
	if got := m.add(late.Add(time.Second), []death{d(late.Add(time.Second), "OOMKilled", "e")}); len(got) != 1 {
		t.Fatalf("new pair: %+v", got)
	}
}

func TestMassHintsPerCause(t *testing.T) {
	causes := []autopsy.CauseOfDeath{
		autopsy.CauseOOMKilled, autopsy.CauseCrashLoopBackOff, autopsy.CauseImagePullBackOff,
		autopsy.CauseProbeFailure, autopsy.CauseError, autopsy.CauseUnknown, "",
	}
	for _, lang := range uiLangs {
		seen := map[string]bool{}
		for _, c := range causes {
			key := massHintKey(string(c))
			hint := tr(lang, key)
			if hint == key {
				t.Errorf("%s: no hint for %q (%s)", lang, c, key)
			}
			seen[hint] = true
		}
		if len(seen) != 6 {
			t.Errorf("%s: %d distinct hints, want 6", lang, len(seen))
		}
	}
	if !strings.Contains(tr(llm.LangZH, "mass_hint_oomkilled"), "limit") {
		t.Error("OOM hint should point at the memory limit")
	}
}

// liveSource is a Source with a Watcher whose lists and signals the test drives.
type liveSource struct {
	mu      sync.Mutex
	lists   []listResult
	changes chan struct{}
	trouble func(error)
	watched chan struct{}
}

type listResult struct {
	spirits []autopsy.SpiritSummary
	err     error
}

func newLiveSource(lists ...listResult) *liveSource {
	return &liveSource{lists: lists, changes: make(chan struct{}, 1), watched: make(chan struct{})}
}

func (l *liveSource) Name() string { return clusterSource }

func (l *liveSource) List(context.Context, string) ([]autopsy.SpiritSummary, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	r := l.lists[0]
	if len(l.lists) > 1 {
		l.lists = l.lists[1:]
	}
	return r.spirits, r.err
}

func (l *liveSource) Autopsy(context.Context, string, string) (*autopsy.PodAutopsy, error) {
	return nil, ErrNotFound
}

func (l *liveSource) Watch(ctx context.Context, _ string, trouble func(error)) <-chan struct{} {
	l.mu.Lock()
	l.trouble = trouble
	l.mu.Unlock()
	close(l.watched)
	out := make(chan struct{})
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case <-l.changes:
				select {
				case out <- struct{}{}:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out
}

func (l *liveSource) poke() { l.changes <- struct{}{} }

// openBell starts the stream on a test HTTP server and returns its events.
func openBell(t *testing.T, srv *Server, target string) <-chan sseMsg {
	t.Helper()
	return openBellDebounce(t, srv, target, 5*time.Millisecond)
}

func openBellDebounce(t *testing.T, srv *Server, target string, debounce time.Duration) <-chan sseMsg {
	t.Helper()
	srv.bellDebounce = debounce
	ts := httptest.NewServer(srv.Handler())
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+target, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", target, err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d", target, resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q", ct)
	}
	out := make(chan sseMsg, 16)
	go streamSSE(ctx, resp.Body, out)
	t.Cleanup(func() {
		cancel()
		resp.Body.Close()
		ts.Close()
	})
	return out
}

func nextEvent(t *testing.T, ch <-chan sseMsg, want string) sseMsg {
	t.Helper()
	select {
	case m := <-ch:
		if m.Name != want {
			t.Fatalf("event %q (%s), want %q", m.Name, m.Data, want)
		}
		return m
	case <-time.After(3 * time.Second):
		t.Fatalf("no %q event", want)
		return sseMsg{}
	}
}

func rowsOfEvent(t *testing.T, m sseMsg) rowsEvent {
	t.Helper()
	var ev rowsEvent
	if err := json.Unmarshal([]byte(m.Data), &ev); err != nil {
		t.Fatalf("rows data: %v (%s)", err, m.Data)
	}
	if ev.Arrived == nil || ev.Worse == nil || ev.Mass == nil {
		t.Errorf("rows lists must be [] not null: %s", m.Data)
	}
	return ev
}

func TestSpiritStreamBaselineArrivedWorseMassTrouble(t *testing.T) {
	a1 := sp("a", autopsy.CauseOOMKilled, 1)
	a2 := sp("a", autopsy.CauseOOMKilled, 2)
	b0 := sp("b", autopsy.CauseOOMKilled, 0)
	src := newLiveSource(
		listResult{spirits: []autopsy.SpiritSummary{a1}},
		listResult{spirits: []autopsy.SpiritSummary{a1, b0}},
		listResult{spirits: []autopsy.SpiritSummary{a2, b0}},
		listResult{err: errors.New("apiserver went away")},
		listResult{spirits: []autopsy.SpiritSummary{a2, b0}},
	)
	srv := newTestServer(t, Options{Cluster: src})
	events := openBell(t, srv, "/api/spirits/stream?ns=ns1&lang=en")

	ready := nextEvent(t, events, eventReady)
	if !strings.Contains(ready.Data, `"namespace":"ns1"`) || !strings.Contains(ready.Data, catalog[llm.LangEN]["bell_ready"]) {
		t.Errorf("ready = %s", ready.Data)
	}
	// The baseline: rows, but no notices for spirits that died before the page opened.
	base := rowsOfEvent(t, nextEvent(t, events, eventRows))
	if base.Count != 1 || len(base.Arrived)+len(base.Worse)+len(base.Mass) != 0 {
		t.Errorf("baseline = %+v", base)
	}
	if !strings.Contains(base.HTML, `data-pod="a"`) || !strings.Contains(base.HTML, `value="cluster"`) || !strings.Contains(base.HTML, `value="en"`) {
		t.Errorf("baseline html = %s", base.HTML)
	}

	src.poke()
	ev := rowsOfEvent(t, nextEvent(t, events, eventRows))
	if len(ev.Arrived) != 1 || ev.Arrived[0].Name != "b" || ev.Arrived[0].Cause != "OOMKilled" || ev.Arrived[0].Restarts != 0 {
		t.Fatalf("arrived = %+v", ev.Arrived)
	}
	if want := "New spirit: b, cause OOMKilled, restarts: 0."; ev.Arrived[0].Text != want {
		t.Errorf("arrived text = %q, want %q", ev.Arrived[0].Text, want)
	}
	if len(ev.Mass) != 0 {
		t.Errorf("mass after one new death = %+v", ev.Mass)
	}

	src.poke()
	ev = rowsOfEvent(t, nextEvent(t, events, eventRows))
	if len(ev.Worse) != 1 || ev.Worse[0].FromRestarts != 1 || ev.Worse[0].Restarts != 2 {
		t.Fatalf("worse = %+v", ev.Worse)
	}
	if !strings.Contains(ev.Worse[0].Text, "1 → 2") {
		t.Errorf("worse text = %q", ev.Worse[0].Text)
	}
	if len(ev.Mass) != 1 || strings.Join(ev.Mass[0].Pods, ",") != "a,b" ||
		ev.Mass[0].Hint != catalog[llm.LangEN]["mass_hint_oomkilled"] || !strings.Contains(ev.Mass[0].Title, "Mass grave: 2 Pods") {
		t.Fatalf("mass = %+v", ev.Mass)
	}

	// A failed list is a trouble event, and the stream stays open.
	src.poke()
	tr := nextEvent(t, events, eventTrouble)
	if !strings.Contains(tr.Data, catalog[llm.LangEN]["bell_trouble"]) || !strings.Contains(tr.Data, "apiserver went away") {
		t.Errorf("trouble = %s", tr.Data)
	}
	// After a trouble the next good list is sent even though nothing changed.
	src.poke()
	ev = rowsOfEvent(t, nextEvent(t, events, eventRows))
	if ev.Count != 2 || len(ev.Arrived)+len(ev.Worse)+len(ev.Mass) != 0 {
		t.Errorf("recovered rows = %+v", ev)
	}

	// The watch breaking is a trouble event as well.
	src.mu.Lock()
	trouble := src.trouble
	src.mu.Unlock()
	trouble(errors.New("watch closed"))
	nextEvent(t, events, eventTrouble)

	// Nothing changed and nothing is wrong: no event at all.
	src.mu.Lock()
	src.lists = []listResult{{spirits: []autopsy.SpiritSummary{a2, b0}}}
	src.mu.Unlock()
	src.poke() // recovery after the watch trouble: rows again
	nextEvent(t, events, eventRows)
	src.poke()
	select {
	case m := <-events:
		t.Errorf("unexpected event %q: %s", m.Name, m.Data)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestSpiritStreamDebounces(t *testing.T) {
	src := newLiveSource(
		listResult{spirits: nil},
		listResult{spirits: []autopsy.SpiritSummary{sp("a", autopsy.CauseError, 1)}},
		listResult{spirits: []autopsy.SpiritSummary{sp("a", autopsy.CauseError, 1), sp("b", autopsy.CauseError, 1)}},
	)
	srv := newTestServer(t, Options{Cluster: src})
	events := openBellDebounce(t, srv, "/api/spirits/stream?ns=ns1", 150*time.Millisecond)
	nextEvent(t, events, eventReady)
	nextEvent(t, events, eventRows)
	// Three quick signals make one list.
	for i := 0; i < 3; i++ {
		src.poke()
		time.Sleep(10 * time.Millisecond)
	}
	ev := rowsOfEvent(t, nextEvent(t, events, eventRows))
	if len(ev.Arrived) != 1 || ev.Arrived[0].Name != "a" {
		t.Fatalf("arrived = %+v", ev.Arrived)
	}
	if !strings.Contains(ev.Arrived[0].Text, "新亡灵：a") {
		t.Errorf("default language text = %q", ev.Arrived[0].Text)
	}
	select {
	case m := <-events:
		t.Errorf("one burst gave a second event %q", m.Name)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestSpiritStreamRefusals(t *testing.T) {
	live := newLiveSource(listResult{})
	cases := []struct {
		name   string
		opts   Options
		target string
		status int
	}{
		{"fixtures src", Options{Cluster: live}, "/api/spirits/stream?src=fixtures", http.StatusNotFound},
		{"fixtures only", Options{Cluster: live, FixturesOnly: true}, "/api/spirits/stream", http.StatusNotFound},
		{"cluster down", Options{ClusterErr: errors.New("no kubeconfig")}, "/api/spirits/stream", http.StatusServiceUnavailable},
		{"bad namespace", Options{Cluster: live}, "/api/spirits/stream?ns=Bad_NS", http.StatusBadRequest},
		{"bad src", Options{Cluster: live}, "/api/spirits/stream?src=nope", http.StatusBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := newTestServer(t, c.opts)
			rec := get(t, srv, c.target)
			if rec.Code != c.status {
				t.Errorf("status = %d, want %d (%s)", rec.Code, c.status, rec.Body)
			}
			if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				t.Errorf("Content-Type = %q", ct)
			}
		})
	}
}

// TestSpiritStreamWithFakeCluster runs the real ClusterSource and k8s.WatchPods
// against a fake clientset: a Pod that starts crash looping shows up as arrived.
func TestSpiritStreamWithFakeCluster(t *testing.T) {
	cs := fake.NewClientset(spiritPod("ns1", "old-ghost", "CrashLoopBackOff", 3))
	srv := newTestServer(t, Options{Cluster: ClusterSource{Client: cs}})
	events := openBell(t, srv, "/api/spirits/stream?ns=ns1&lang=en")
	nextEvent(t, events, eventReady)
	base := rowsOfEvent(t, nextEvent(t, events, eventRows))
	if base.Count != 1 || !strings.Contains(base.HTML, "old-ghost") {
		t.Fatalf("baseline = %+v", base)
	}

	deadline := time.Now().Add(3 * time.Second)
	for !hasWatch(cs) {
		if time.Now().After(deadline) {
			t.Fatal("the stream never opened a Pod watch")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// The test adds the Pod straight into the fake tracker; the server code only reads.
	if err := cs.Tracker().Add(spiritPod("ns1", "new-ghost", "ImagePullBackOff", 0)); err != nil {
		t.Fatal(err)
	}
	ev := rowsOfEvent(t, nextEvent(t, events, eventRows))
	if len(ev.Arrived) != 1 || ev.Arrived[0].Name != "new-ghost" || ev.Arrived[0].Cause != "ImagePullBackOff" {
		t.Fatalf("arrived = %+v", ev.Arrived)
	}
	for _, a := range cs.Actions() {
		switch a.GetVerb() {
		case "list", "watch", "get":
		default:
			t.Errorf("non-read action %s %s", a.GetVerb(), a.GetResource().Resource)
		}
	}
}

func hasWatch(cs *fake.Clientset) bool {
	for _, a := range cs.Actions() {
		if a.GetVerb() == "watch" && a.GetResource().Resource == "pods" {
			return true
		}
	}
	return false
}

func TestListPageLiveMode(t *testing.T) {
	srv := newTestServer(t, Options{Cluster: clusterWith(spiritPod("default", "ghost", "CrashLoopBackOff", 2))})
	html := get(t, srv, "/spirits").Body.String()
	for _, want := range []string{
		`sse-connect="/api/spirits/stream?ns=default&amp;lang=zh"`,
		`hx-trigger="sse:ready, sse:rows, sse:trouble"`,
		`id="bell-sound" class="bell-sound" aria-pressed="false"`,
		catalog[llm.LangZH]["bell_sound_off"],
		`id="spirit-rows"`,
		`data-pod="ghost"`,
		`class="candle"`,
		`/static/sse.js`,
		`AudioContext`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("live list page lacks %q", want)
		}
	}
	// In live mode the namespace switch is a full page load.
	if strings.Contains(html, `hx-get="/spirits"`) {
		t.Error("live list page still swaps the namespace with htmx")
	}
	// No audio file is loaded.
	for _, bad := range []string{"<audio", ".mp3", ".wav", ".ogg"} {
		if strings.Contains(html, bad) {
			t.Errorf("live list page loads audio: %q", bad)
		}
	}

	en := get(t, srv, "/spirits?lang=en").Body.String()
	if !strings.Contains(en, `lang=en"`) || hanOutside(en) {
		t.Error("English live list page: wrong stream language or Chinese text")
	}
}

func TestListPageLiveModeEmptyNamespace(t *testing.T) {
	srv := newTestServer(t, Options{Cluster: clusterWith()})
	html := get(t, srv, "/spirits").Body.String()
	for _, want := range []string{`id="spirit-table" hidden`, `id="no-spirits"`, `sse-connect=`} {
		if !strings.Contains(html, want) {
			t.Errorf("empty live page lacks %q", want)
		}
	}
}

func TestListPageFixturesHasNoStream(t *testing.T) {
	srv := newTestServer(t, Options{FixturesOnly: true})
	html := get(t, srv, "/spirits").Body.String()
	for _, bad := range []string{"sse-connect", "bell-root", "/api/spirits/stream"} {
		if strings.Contains(html, bad) {
			t.Errorf("fixture list page contains %q", bad)
		}
	}
	if !strings.Contains(html, `data-pod="memory-glutton-7cc6df9966-bf87c"`) {
		t.Error("fixture rows missing")
	}
}

func TestStyleHasBellMotionRules(t *testing.T) {
	srv := newTestServer(t, Options{})
	css := get(t, srv, "/static/style.css").Body.String()
	for _, want := range []string{".candle", "tr.fresh", "@keyframes row-flicker", "@keyframes bell-arrive", ".mass-grave"} {
		if !strings.Contains(css, want) {
			t.Errorf("style.css lacks %q", want)
		}
	}
	i := strings.Index(css, "@media (prefers-reduced-motion: reduce)")
	if i < 0 || !strings.Contains(css[i:], "animation: none !important") {
		t.Error("reduced motion does not switch animations off")
	}
}
