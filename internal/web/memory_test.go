package web

import (
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zhangjuji/k8s-seance/internal/autopsy"
	"github.com/zhangjuji/k8s-seance/internal/llm"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const lostPod = "lost-soul-5ff5cb8464-hjtms"

var memoryLeftAttr = regexp.MustCompile(`data-memory-left="(-?\d+)"`)

func TestMemoryLabels(t *testing.T) {
	tests := []struct {
		left    int64
		class   string
		zh, en  string
		fading  bool
		comment string
	}{
		{3600, "", "1 小时后遗忘", "forgotten in 1 h", false, "an hour"},
		{3900, "", "1 小时 5 分钟后遗忘", "forgotten in 1 h 5 min", false, "hours and minutes"},
		{15 * 60, "", "15 分钟后遗忘", "forgotten in 15 min", false, "exactly 15 minutes is not fading"},
		{15*60 - 1, "memory-fading", "14 分钟后遗忘", "forgotten in 14 min", true, "just below 15 minutes"},
		{5 * 60, "memory-fading", "5 分钟后遗忘", "forgotten in 5 min", true, "exactly 5 minutes"},
		{299, "memory-fading memory-vanishing", "4 分钟后遗忘", "forgotten in 4 min", true, "below 5 minutes"},
		{59, "memory-fading memory-vanishing", "不到 1 分钟后遗忘", "forgotten in under 1 min", true, "under a minute"},
		{0, "memory-gone", "已被遗忘", "Forgotten", false, "zero"},
		{-30, "memory-gone", "已被遗忘", "Forgotten", false, "past"},
	}
	for _, tc := range tests {
		if got := memoryClass(tc.left); got != tc.class {
			t.Errorf("%s: memoryClass(%d) = %q, want %q", tc.comment, tc.left, got, tc.class)
		}
		if got := memoryText(llm.LangZH, tc.left); got != tc.zh {
			t.Errorf("%s: zh text = %q, want %q", tc.comment, got, tc.zh)
		}
		if got := memoryText(llm.LangEN, tc.left); got != tc.en {
			t.Errorf("%s: en text = %q, want %q", tc.comment, got, tc.en)
		}
		if got := memoryFading(tc.left); got != tc.fading {
			t.Errorf("%s: memoryFading(%d) = %v", tc.comment, tc.left, got)
		}
	}
}

// TestFixtureListMemoryColumn checks that sample mode counts from the recording
// time, shows a countdown per spirit and says how it is counted.
func TestFixtureListMemoryColumn(t *testing.T) {
	srv := newTestServer(t, Options{FixturesOnly: true})
	body := get(t, srv, "/spirits").Body.String()
	for _, want := range []string{
		"<th>记忆</th>",
		tr(llm.LangZH, "memory_fixture_note"),
		`id="memory-i18n"`,
		`data-left="{t}后遗忘"`,
		`<script src="/static/memory.js" defer></script>`,
		`id="memory-banner" class="banner memory-banner" role="alert" hidden`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("fixture list does not contain %q", want)
		}
	}
	matches := memoryLeftAttr.FindAllStringSubmatch(body, -1)
	if len(matches) != 3 {
		t.Fatalf("countdowns = %d, want 3", len(matches))
	}
	lost := fixtureNamed(t, lostPod)
	want := autopsy.Remember(lost, 0, lost.CollectedAt).LeftSeconds
	if !strings.Contains(body, fmt.Sprintf(`data-memory-left="%d" title="Failed"`, want)) {
		t.Errorf("lost-soul countdown %d (Failed) not on the page", want)
	}
	for _, m := range matches {
		if n, _ := strconv.ParseInt(m[1], 10, 64); n <= 0 || n > 3600 {
			t.Errorf("fixture countdown %d is not within the hour; sample mode must count from collectedAt", n)
		}
	}
}

func TestEventTTLOption(t *testing.T) {
	if _, err := New(Options{EventTTL: -time.Minute}); err == nil {
		t.Error("New accepted a negative event TTL")
	}
	srv := newTestServer(t, Options{FixturesOnly: true, EventTTL: 2 * time.Hour})
	var res spiritsResponse
	decodeJSON(t, get(t, srv, "/api/spirits").Body.Bytes(), &res)
	lost := fixtureNamed(t, lostPod)
	want := autopsy.Remember(lost, 2*time.Hour, lost.CollectedAt).LeftSeconds
	for _, sp := range res.Spirits {
		if sp.Memory == nil {
			t.Fatalf("%s: no memory in /api/spirits", sp.Name)
		}
		if sp.Memory.TTLSeconds != 7200 {
			t.Errorf("%s: ttlSeconds = %d, want 7200", sp.Name, sp.Memory.TTLSeconds)
		}
		if sp.Name == lostPod && sp.Memory.LeftSeconds != want {
			t.Errorf("lost-soul leftSeconds = %d, want %d", sp.Memory.LeftSeconds, want)
		}
	}
	if want <= 3600 {
		t.Errorf("a 2h TTL should leave more than an hour, got %d", want)
	}
}

func warningEvent(ns, pod, name, typ, reason string, last time.Time) *corev1.Event {
	return &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Namespace: ns, Name: name},
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Namespace: ns, Name: pod},
		Type:           typ,
		Reason:         reason,
		Message:        reason + " happened",
		Count:          1,
		FirstTimestamp: metav1.NewTime(last),
		LastTimestamp:  metav1.NewTime(last),
	}
}

// TestClusterListFadesAndBanner uses a fake cluster and a fixed clock: one spirit
// fades in 10 minutes, one in 3, one is forgotten, one has no Warning event.
func TestClusterListFadesAndBanner(t *testing.T) {
	now := time.Date(2026, 11, 6, 6, 0, 0, 0, time.UTC)
	ns := "seance-demo"
	cluster := clusterWith(
		spiritPod(ns, "fading", "CrashLoopBackOff", 3),
		spiritPod(ns, "vanishing", "CrashLoopBackOff", 3),
		spiritPod(ns, "gone", "CrashLoopBackOff", 3),
		spiritPod(ns, "silent", "CrashLoopBackOff", 3),
		warningEvent(ns, "fading", "e1", "Warning", "BackOff", now.Add(-50*time.Minute)),
		warningEvent(ns, "vanishing", "e2", "Warning", "BackOff", now.Add(-57*time.Minute)),
		warningEvent(ns, "gone", "e3", "Warning", "BackOff", now.Add(-2*time.Hour)),
		warningEvent(ns, "silent", "e4", "Normal", "Pulled", now.Add(-time.Minute)),
	)
	cluster.Now = func() time.Time { return now }
	srv := newTestServer(t, Options{Cluster: cluster, DefaultNamespace: ns})

	body := get(t, srv, "/spirits").Body.String()
	rowOf := func(pod string) string {
		i := strings.Index(body, `<tr data-pod="`+pod+`"`)
		if i < 0 {
			t.Fatalf("no row for %s", pod)
		}
		end := strings.Index(body[i:], "</tr>")
		return body[i : i+end]
	}
	for pod, want := range map[string][]string{
		"fading":    {`class="memory-fading"`, `data-memory-left="600"`, "10 分钟后遗忘"},
		"vanishing": {`class="memory-fading memory-vanishing"`, `data-memory-left="180"`, "3 分钟后遗忘"},
		"gone":      {`class="memory-gone"`, `data-memory-left="-3600"`, "已被遗忘"},
		"silent":    {"无事件记录"},
	} {
		row := rowOf(pod)
		for _, w := range want {
			if !strings.Contains(row, w) {
				t.Errorf("row %s does not contain %q:\n%s", pod, w, row)
			}
		}
	}
	if strings.Contains(rowOf("silent"), "data-memory-left") {
		t.Error("a spirit without Warning events has a countdown")
	}
	if !strings.Contains(body, `id="memory-banner" class="banner memory-banner" role="alert">`) {
		t.Error("the memory banner is not shown for a spirit fading in 10 minutes")
	}
	if strings.Contains(body, tr(llm.LangZH, "memory_fixture_note")) {
		t.Error("cluster list shows the sample-mode note")
	}

	var res spiritsResponse
	decodeJSON(t, get(t, srv, "/api/spirits").Body.Bytes(), &res)
	for _, sp := range res.Spirits {
		if sp.Name == "fading" && (sp.Memory == nil || sp.Memory.LeftSeconds != 600 || sp.Memory.Key.Reason != "BackOff") {
			t.Errorf("fading memory = %+v", sp.Memory)
		}
		if sp.Name == "silent" && (sp.Memory == nil || sp.Memory.Key != nil) {
			t.Errorf("silent memory = %+v, want no key", sp.Memory)
		}
	}
}

func TestBannerHiddenWhenNothingFades(t *testing.T) {
	now := time.Date(2026, 11, 6, 6, 0, 0, 0, time.UTC)
	ns := "seance-demo"
	cluster := clusterWith(
		spiritPod(ns, "fresh", "CrashLoopBackOff", 1),
		warningEvent(ns, "fresh", "e1", "Warning", "BackOff", now.Add(-time.Minute)),
	)
	cluster.Now = func() time.Time { return now }
	srv := newTestServer(t, Options{Cluster: cluster, DefaultNamespace: ns})
	body := get(t, srv, "/spirits").Body.String()
	if !strings.Contains(body, `id="memory-banner" class="banner memory-banner" role="alert" hidden`) {
		t.Error("banner shown although the only spirit has 59 minutes left")
	}
}

// TestRowsSignatureIgnoresSecondsLeft: the live list is not resent only because
// time passed, but it is resent when a Warning event was refreshed.
func TestRowsSignatureIgnoresSecondsLeft(t *testing.T) {
	fades := time.Date(2026, 11, 6, 7, 0, 0, 0, time.UTC)
	mk := func(left int64, at time.Time) []autopsy.SpiritSummary {
		return []autopsy.SpiritSummary{{
			Namespace: "ns", Name: "p", Cause: autopsy.CauseCrashLoopBackOff, RestartCount: 2,
			Memory: &autopsy.Memory{Key: &autopsy.FadingEvent{Reason: "BackOff", FadesAt: at}, LeftSeconds: left},
		}}
	}
	a := rowsSignature(llm.LangZH, mk(1200, fades))
	if b := rowsSignature(llm.LangZH, mk(1100, fades)); a != b {
		t.Errorf("signature changed with the seconds left only:\n%s\n%s", a, b)
	}
	if c := rowsSignature(llm.LangZH, mk(1200, fades.Add(time.Minute))); a == c {
		t.Error("signature did not change when the key memory moved")
	}
}

func TestSeanceMemoryPanelFixture(t *testing.T) {
	srv := newSeanceServer(t, llm.Limits{})
	created := createSeance(t, srv, crashNS, lostPod)
	html := get(t, srv, created.PageURL).Body.String()

	lost := fixtureNamed(t, lostPod)
	ev, err := autopsy.BuildEvidence(lost, autopsy.EvidenceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	m := autopsy.Remember(lost, 0, lost.CollectedAt)
	keyID := autopsy.EventEvidenceID(ev, lost.Events[m.Key.Index])
	asOf := lost.CollectedAt
	for _, want := range []string{
		`id="memory-panel"`,
		"<h2>遗忘倒计时</h2>",
		`<a class="ev-link" href="#ev-` + keyID + `">[` + keyID + `]</a> <code>Failed</code>`,
		"Kubernetes 删除它的时间（UTC）：2026-11-01 05:42:03 UTC",
		fmt.Sprintf(`data-memory-main data-memory-left="%d"`, m.LeftSeconds),
		"全部事件消散的时间（UTC）：" + formatTime(m.AllFadeAt),
		tr(llm.LangZH, "memory_pod_gone"),
		trf(llm.LangZH, "memory_fixture_asof", formatTime(&asOf)),
		`class="memory-keep" hx-post="` + seancePostmortemURL(created.ID) + `" hx-target="#postmortem"`,
		`id="memory-i18n"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("séance page does not contain %q", want)
		}
	}
	if strings.Contains(html, "CrashLoopBackOff：--previous") {
		t.Error("image pull spirit got the --previous warning")
	}
	// Each event evidence card has its own countdown.
	var events int
	for _, e := range ev {
		if e.Kind == "event" {
			events++
			want := fmt.Sprintf(`<details id="ev-%s"><summary data-memory-scope`, e.ID)
			if !strings.Contains(html, want) {
				t.Errorf("event card %s has no countdown", e.ID)
			}
		}
	}
	if n := strings.Count(html, "<summary data-memory-scope"); n != events {
		t.Errorf("evidence countdowns = %d, want %d (one per event card)", n, events)
	}

	d := detailOf(t, srv, created.ID)
	if d.Memory.Key == nil || d.Memory.LeftSeconds != m.LeftSeconds || len(d.RestartsSoon) != 0 {
		t.Errorf("detail memory = %+v, restartsSoon %v", d.Memory, d.RestartsSoon)
	}
}

func TestSeanceMemoryPanelCrashLoop(t *testing.T) {
	srv := newSeanceServer(t, llm.Limits{})
	created := createSeance(t, srv, crashNS, crashPod)
	html := get(t, srv, created.PageURL).Body.String()
	if want := trf(llm.LangZH, "memory_previous", "app"); !strings.Contains(html, want) {
		t.Errorf("crashloop page does not warn about the --previous log: %q", want)
	}
	d := detailOf(t, srv, created.ID)
	if len(d.RestartsSoon) != 1 || d.RestartsSoon[0] != "app" {
		t.Errorf("restartsSoon = %v, want [app]", d.RestartsSoon)
	}
}

// TestSeanceMemoryClusterClock: a cluster séance counts from the server clock,
// and a spirit without Warning events says so.
func TestSeanceMemoryClusterClock(t *testing.T) {
	now := time.Date(2026, 11, 6, 6, 0, 0, 0, time.UTC)
	last := now.Add(-56 * time.Minute)
	a := &autopsy.PodAutopsy{
		Namespace: crashNS, Name: "quiet", Source: "cluster", CollectedAt: now,
		Containers: []autopsy.ContainerAutopsy{{Name: "app", State: autopsy.ContainerState{Kind: "waiting", Reason: "CrashLoopBackOff"}}},
		Events:     []autopsy.EventRecord{{Type: "Warning", Reason: "BackOff", Message: "Back-off", Count: 4, LastTimestamp: &last}},
	}
	srv := newTestServer(t, Options{Cluster: fixedSource{a: a}, Provider: llm.MockProvider{}, Lang: llm.LangEN})
	srv.now = func() time.Time { return now }
	rec := send(t, srv, http.MethodPost, "/api/seances", "application/json", `{"ns":"`+crashNS+`","pod":"quiet"}`, nil)
	var created createResponse
	decodeJSON(t, rec.Body.Bytes(), &created)
	html := get(t, srv, created.PageURL).Body.String()
	for _, want := range []string{
		"<h2>Fading memory</h2>",
		`<section id="memory-panel" class="memory-panel memory-fading memory-vanishing"`,
		`data-memory-main data-memory-left="240">forgotten in 4 min</span>`,
		"Container app is in CrashLoopBackOff.",
		"Keep the memory: write the postmortem",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("cluster séance page does not contain %q", want)
		}
	}
	if strings.Contains(html, "Sample mode: the countdown") {
		t.Error("cluster séance shows the sample-mode note")
	}

	a2 := *a
	a2.Name = "silent"
	a2.Events = nil
	srv2 := newTestServer(t, Options{Cluster: fixedSource{a: &a2}, Provider: llm.MockProvider{}, Lang: llm.LangEN})
	rec = send(t, srv2, http.MethodPost, "/api/seances", "application/json", `{"ns":"`+crashNS+`","pod":"silent"}`, nil)
	decodeJSON(t, rec.Body.Bytes(), &created)
	html = get(t, srv2, created.PageURL).Body.String()
	if !strings.Contains(html, `<p class="memory-none">No event record</p>`) {
		t.Error("séance without Warning events does not say so")
	}
}

func TestMemoryScriptServed(t *testing.T) {
	srv := newTestServer(t, Options{FixturesOnly: true})
	rec := get(t, srv, "/static/memory.js")
	if rec.Code != http.StatusOK {
		t.Fatalf("memory.js: status = %d", rec.Code)
	}
	js := rec.Body.String()
	for _, want := range []string{"data-memory-left", "memory-fading", "memory-vanishing", "memory-gone", "memory-banner", "10000", "window.seanceMemory"} {
		if !strings.Contains(js, want) {
			t.Errorf("memory.js does not contain %q", want)
		}
	}
	css := get(t, srv, "/static/style.css").Body.String()
	for _, want := range []string{"tr.memory-fading", "@keyframes memory-flicker", ".memory-panel"} {
		if !strings.Contains(css, want) {
			t.Errorf("style.css does not contain %q", want)
		}
	}
}
