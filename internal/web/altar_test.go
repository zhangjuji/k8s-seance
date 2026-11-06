package web

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/zhangjuji/k8s-seance/internal/autopsy"
	"github.com/zhangjuji/k8s-seance/internal/graveyard"
	"github.com/zhangjuji/k8s-seance/internal/llm"
)

func TestAltarTiers(t *testing.T) {
	for _, tc := range []struct {
		n    int
		want []int
	}{
		{0, nil},
		{1, []int{1}},
		{3, []int{3}},
		{4, []int{3, 1}},
		{7, []int{3, 4}},
		{8, []int{3, 4, 1}},
		{12, []int{3, 4, 5}},
		{13, []int{3, 4, 5, 1}},
		{17, []int{3, 4, 5, 5}},
		{18, []int{3, 4, 5, 5, 1}},
	} {
		stones := make([]stoneView, tc.n)
		for i := range stones {
			stones[i].Pod = fmt.Sprintf("p%02d", i)
		}
		tiers := altarTiers(stones)
		var sizes []int
		next := 0
		for i, tier := range tiers {
			sizes = append(sizes, len(tier.Stones))
			if tier.Level != i+1 {
				t.Errorf("n=%d: tier %d has level %d", tc.n, i, tier.Level)
			}
			for _, s := range tier.Stones {
				if want := fmt.Sprintf("p%02d", next); s.Pod != want {
					t.Errorf("n=%d: tier %d holds %s, want %s (order must be kept)", tc.n, i+1, s.Pod, want)
				}
				next++
			}
		}
		if fmt.Sprint(sizes) != fmt.Sprint(tc.want) {
			t.Errorf("n=%d: tier sizes %v, want %v", tc.n, sizes, tc.want)
		}
	}
}

// TestAltarTiersDoNotAlias: appending to a tier must not overwrite the next one.
func TestAltarTiersDoNotAlias(t *testing.T) {
	stones := make([]stoneView, 7)
	for i := range stones {
		stones[i].Pod = fmt.Sprint(i)
	}
	tiers := altarTiers(stones)
	_ = append(tiers[0].Stones, stoneView{Pod: "x"})
	if tiers[1].Stones[0].Pod != "3" {
		t.Errorf("tier 2 starts with %q after an append to tier 1", tiers[1].Stones[0].Pod)
	}
}

// buryAt stores a tombstone that died at diedAt.
func buryAt(t *testing.T, st *graveyard.Store, ns, pod string, cause autopsy.CauseOfDeath, diedAt time.Time) graveyard.Tombstone {
	t.Helper()
	ts, err := st.Bury(graveyard.Tombstone{
		Namespace: ns, PodName: pod, UID: "uid-" + pod, Cause: cause,
		DiedAt: diedAt, BornAt: diedAt.Add(-time.Hour),
		Epitaph: "last words of " + pod, PostmortemMD: "# Postmortem " + pod + "\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

// tierOf returns the HTML of altar tier level, up to the next tier.
func tierOf(t *testing.T, page string, level int) string {
	t.Helper()
	start := strings.Index(page, fmt.Sprintf(`class="altar-tier altar-tier-%d"`, level))
	if start < 0 {
		t.Fatalf("no altar tier %d", level)
	}
	rest := page[start+1:]
	if end := strings.Index(rest, `class="altar-tier altar-tier-`); end >= 0 {
		rest = rest[:end]
	}
	return rest
}

func TestAltarPageNewestOnTop(t *testing.T) {
	srv, st := newGraveyardServer(t)
	base := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	// p0 died first, p4 last. The top tier must hold p4, p3, p2.
	for i := range 5 {
		buryAt(t, st, "ns", fmt.Sprintf("pod-%d", i), autopsy.CauseOOMKilled, base.Add(time.Duration(i)*time.Minute))
	}
	rec := get(t, srv, "/graveyard")
	if rec.Code != http.StatusOK {
		t.Fatalf("/graveyard: status = %d", rec.Code)
	}
	page := rec.Body.String()
	top, second := tierOf(t, page, 1), tierOf(t, page, 2)
	for _, p := range []string{"pod-4", "pod-3", "pod-2"} {
		if !strings.Contains(top, p) {
			t.Errorf("top tier lacks %s", p)
		}
	}
	for _, p := range []string{"pod-1", "pod-0"} {
		if !strings.Contains(second, p) || strings.Contains(top, p) {
			t.Errorf("%s is not on the second tier only", p)
		}
	}
	if strings.Index(top, "pod-4") > strings.Index(top, "pod-2") {
		t.Error("the most recent death is not first on the top tier")
	}
	if strings.Contains(page, "altar-tier-3") {
		t.Error("5 photos made a third tier")
	}
	for _, want := range []string{
		`<h1>祭坛 · Ofrenda</h1>`,
		`class="papel-picado"`,
		`class="altar-step"`,
		`class="altar-candle altar-candle-left"`,
		`class="altar-candle altar-candle-right"`,
		`class="petal-path"`,
		`class="photo-frame"`,
		`portrait-badge-oomkilled`,
		`aria-label="祭台第 1 层"`,
		`href="/graveyard/`,
		`>祭坛</a>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("altar page lacks %q", want)
		}
	}
	// Each tier is one step with two candles.
	if n := strings.Count(page, `class="altar-step"`); n != 2 {
		t.Errorf("altar steps = %d, want 2", n)
	}
	// Pure CSS: no images are drawn.
	if strings.Contains(page, "<img") || strings.Contains(page, "<svg") || strings.Contains(page, "<canvas") {
		t.Error("altar page uses an image element")
	}
}

func TestAltarPageEnglish(t *testing.T) {
	srv, st := newGraveyardServer(t)
	buryAt(t, st, "ns", "pod-a", autopsy.CauseCrashLoopBackOff, time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC))
	page := getWith(t, srv, "/graveyard?lang=en", nil).Body.String()
	for _, want := range []string{"<h1>Ofrenda</h1>", `aria-label="Altar tier 1"`, ">Ofrenda</a>", "pod-a", "Born: ", "Died: "} {
		if !strings.Contains(page, want) {
			t.Errorf("English altar lacks %q", want)
		}
	}
	if hanOutside(page) {
		t.Error("English altar has Chinese text")
	}
}

// TestAltarStyleIsPureCSS: the stylesheet draws every altar piece itself, with
// no image files and no third-party names.
func TestAltarStyleIsPureCSS(t *testing.T) {
	srv := newTestServer(t, Options{FixturesOnly: true})
	css := get(t, srv, "/static/style.css").Body.String()
	for _, want := range []string{".papel-picado", ".papel", "clip-path", ".altar-step", ".altar-candle", ".altar-flame", ".petal-path", ".photo-frame", ".photo-portrait", "tr.memory-enshrined", "--marigold"} {
		if !strings.Contains(css, want) {
			t.Errorf("style.css lacks %q", want)
		}
	}
	if regexp.MustCompile(`url\(`).MatchString(css) {
		t.Error("style.css loads an image or font with url()")
	}
	lower := strings.ToLower(css)
	for _, banned := range []string{"coco", "pixar", "disney", "miguel", "héctor", "hector"} {
		if strings.Contains(lower, banned) {
			t.Errorf("style.css mentions %q", banned)
		}
	}
}

func TestPhotoPageAltarStyle(t *testing.T) {
	srv, st := newGraveyardServer(t)
	ts := buryAt(t, st, "ns", "pod-a", autopsy.CauseImagePullBackOff, time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC))
	page := get(t, srv, tombstoneURL(ts.ID)).Body.String()
	for _, want := range []string{
		`class="ofrenda-page ofrenda-single"`,
		`class="papel-picado"`,
		`class="photo photo-single"`,
		`portrait-badge-imagepullbackoff`,
		`<h1 class="photo-name">pod-a</h1>`,
		`class="altar-step"`,
		`class="petal-path"`,
		"不会被遗忘",
		"← 回到祭坛",
		"<title>祭坛上的 pod-a",
		`id="pm-md"`,
		`href="` + tombstoneDownloadURL(ts.ID) + `"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("photo page lacks %q", want)
		}
	}
}

// TestPostmortemLinkSaysEnshrined checks the wording of Requirement 15.3: the
// postmortem panel link and the route.
func TestPostmortemLinkSaysEnshrined(t *testing.T) {
	srv, _ := newGraveyardServer(t)
	res := createSeance(t, srv, crashNS, crashPod)
	rec := send(t, srv, http.MethodPost, "/api/seances/"+res.ID+"/postmortem", "", "", map[string]string{"HX-Request": "true"})
	body := rec.Body.String()
	if !regexp.MustCompile(`<a class="pm-tomb" href="/graveyard/[0-9a-f]{12}">已供上祭坛，它不会被遗忘</a>`).MatchString(body) {
		t.Errorf("postmortem panel link is not the ofrenda link:\n%s", body)
	}
}

// enshrinedCluster is a fake cluster where "kept" (enshrined) fades in 10
// minutes and "fresh" has 59 minutes left.
func enshrinedCluster(t *testing.T, enshrine ...string) (*Server, map[string]graveyard.Tombstone) {
	t.Helper()
	now := time.Date(2026, 11, 6, 6, 0, 0, 0, time.UTC)
	ns := "seance-demo"
	cluster := clusterWith(
		spiritPod(ns, "kept", "CrashLoopBackOff", 3),
		spiritPod(ns, "fresh", "CrashLoopBackOff", 1),
		spiritPod(ns, "fading", "CrashLoopBackOff", 2),
		warningEvent(ns, "kept", "e1", "Warning", "BackOff", now.Add(-50*time.Minute)),
		warningEvent(ns, "fresh", "e2", "Warning", "BackOff", now.Add(-time.Minute)),
		warningEvent(ns, "fading", "e3", "Warning", "BackOff", now.Add(-57*time.Minute)),
	)
	cluster.Now = func() time.Time { return now }
	st, err := graveyard.Open(filepath.Join(t.TempDir(), "graveyard.json"))
	if err != nil {
		t.Fatal(err)
	}
	stones := map[string]graveyard.Tombstone{}
	for _, pod := range enshrine {
		stones[pod] = buryAt(t, st, ns, pod, autopsy.CauseCrashLoopBackOff, now.Add(-time.Hour))
	}
	// A tombstone with the same Pod name in another namespace must not count.
	buryAt(t, st, "elsewhere", "fresh", autopsy.CauseCrashLoopBackOff, now.Add(-time.Hour))
	return newTestServer(t, Options{Cluster: cluster, DefaultNamespace: ns, Graveyard: st}), stones
}

func rowIn(t *testing.T, body, pod string) string {
	t.Helper()
	i := strings.Index(body, `<tr data-pod="`+pod+`"`)
	if i < 0 {
		t.Fatalf("no row for %s", pod)
	}
	end := strings.Index(body[i:], "</tr>")
	return body[i : i+end]
}

const bannerShown = `id="memory-banner" class="banner memory-banner" role="alert">`
const bannerHidden = `id="memory-banner" class="banner memory-banner" role="alert" hidden>`

func TestEnshrinedRowDoesNotFadeOrRaiseBanner(t *testing.T) {
	// "fading" (3 minutes left) is enshrined too, so nothing on the list may raise the banner.
	srv, stones := enshrinedCluster(t, "kept", "fading")
	body := get(t, srv, "/spirits").Body.String()

	kept := rowIn(t, body, "kept")
	for _, want := range []string{
		`data-enshrined`,
		`class="memory-enshrined"`,
		`href="` + tombstoneURL(stones["kept"].ID) + `"`,
		"🌼 已供奉",
		`data-memory-left="600"`,
	} {
		if !strings.Contains(kept, want) {
			t.Errorf("enshrined row lacks %q:\n%s", want, kept)
		}
	}
	for _, unwanted := range []string{"memory-fading", "memory-vanishing", "data-memory-main", "data-memory-banner", "data-memory-scope"} {
		if strings.Contains(kept, unwanted) {
			t.Errorf("enshrined row has %q:\n%s", unwanted, kept)
		}
	}
	if !strings.Contains(body, bannerHidden) {
		t.Error("the banner shows although every fading spirit is enshrined")
	}
	// "fresh" has a namesake tombstone in another namespace only.
	fresh := rowIn(t, body, "fresh")
	if strings.Contains(fresh, "data-enshrined") || strings.Contains(fresh, "已供奉") {
		t.Errorf("a tombstone in another namespace enshrined fresh:\n%s", fresh)
	}
	if !strings.Contains(fresh, "data-memory-banner") {
		t.Errorf("a row that is not enshrined lost its countdown hooks:\n%s", fresh)
	}
}

func TestBannerStillShownForSpiritNotEnshrined(t *testing.T) {
	srv, _ := enshrinedCluster(t, "kept")
	body := get(t, srv, "/spirits").Body.String()
	if !strings.Contains(body, bannerShown) {
		t.Error("the banner is hidden although fading (3 min, not enshrined) is on the list")
	}
	if row := rowIn(t, body, "fading"); !strings.Contains(row, `class="memory-fading memory-vanishing"`) || strings.Contains(row, "data-enshrined") {
		t.Errorf("fading row:\n%s", row)
	}
}

func TestEnshrinedRowEnglish(t *testing.T) {
	srv, _ := enshrinedCluster(t, "kept")
	body := getWith(t, srv, "/spirits?lang=en", nil).Body.String()
	if !strings.Contains(rowIn(t, body, "kept"), "🌼 Enshrined") {
		t.Error("English enshrined label missing")
	}
}

// TestEnshrinedWithoutGraveyard: with the graveyard off or unreadable nothing
// is enshrined and the list still renders and fades as before.
func TestEnshrinedWithoutGraveyard(t *testing.T) {
	now := time.Date(2026, 11, 6, 6, 0, 0, 0, time.UTC)
	ns := "seance-demo"
	cluster := clusterWith(
		spiritPod(ns, "kept", "CrashLoopBackOff", 3),
		warningEvent(ns, "kept", "e1", "Warning", "BackOff", now.Add(-50*time.Minute)),
	)
	cluster.Now = func() time.Time { return now }

	path := filepath.Join(t.TempDir(), "graveyard.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	broken, err := graveyard.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for name, opts := range map[string]Options{
		"off":     {Cluster: cluster, DefaultNamespace: ns},
		"corrupt": {Cluster: cluster, DefaultNamespace: ns, Graveyard: broken},
	} {
		srv := newTestServer(t, opts)
		rec := get(t, srv, "/spirits")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d", name, rec.Code)
		}
		body := rec.Body.String()
		if row := rowIn(t, body, "kept"); strings.Contains(row, "data-enshrined") || !strings.Contains(row, `class="memory-fading"`) {
			t.Errorf("%s: row:\n%s", name, row)
		}
		if !strings.Contains(body, bannerShown) {
			t.Errorf("%s: banner hidden", name)
		}
	}
}

// TestFixtureListEnshrinedAfterPostmortem walks the sample-mode path: generate
// a postmortem, and the list row of that spirit is enshrined.
func TestFixtureListEnshrinedAfterPostmortem(t *testing.T) {
	srv, _ := newGraveyardServer(t)
	if strings.Contains(get(t, srv, "/spirits").Body.String(), "data-enshrined") {
		t.Fatal("a row is enshrined before any postmortem")
	}
	res := createSeance(t, srv, crashNS, crashPod)
	streamOnce(t, srv, res.ID)
	reply := postmortemJSON(t, srv, res.ID)

	body := get(t, srv, "/spirits").Body.String()
	row := rowIn(t, body, crashPod)
	if !strings.Contains(row, "data-enshrined") || !strings.Contains(row, `href="`+reply.TombstoneURL+`"`) {
		t.Errorf("row after the postmortem:\n%s", row)
	}
	if other := rowIn(t, body, lostPod); strings.Contains(other, "data-enshrined") {
		t.Error("a spirit without a postmortem is enshrined")
	}
}

// TestLiveRowsEnshrined: the summoning bell rows are rendered with the altar
// too, and putting a spirit on the altar changes the rows signature so the
// next refresh is sent.
func TestLiveRowsEnshrined(t *testing.T) {
	fades := time.Date(2026, 11, 6, 7, 0, 0, 0, time.UTC)
	spirits := []autopsy.SpiritSummary{{
		Namespace: "ns1", Name: "kept", Cause: autopsy.CauseCrashLoopBackOff, RestartCount: 2,
		Memory: &autopsy.Memory{Key: &autopsy.FadingEvent{Reason: "BackOff", FadesAt: fades}, LeftSeconds: 300},
	}}
	if rowsSignature(llm.LangZH, spirits, nil) == rowsSignature(llm.LangZH, spirits, enshrined{"ns1/kept": "abcdefabcdef"}) {
		t.Error("enshrining a spirit does not change the rows signature")
	}

	st, err := graveyard.Open(filepath.Join(t.TempDir(), "graveyard.json"))
	if err != nil {
		t.Fatal(err)
	}
	stone := buryAt(t, st, "ns1", "kept", autopsy.CauseCrashLoopBackOff, fades.Add(-2*time.Hour))
	src := newLiveSource(listResult{spirits: spirits})
	srv := newTestServer(t, Options{Cluster: src, Graveyard: st})
	events := openBell(t, srv, "/api/spirits/stream?ns=ns1&lang=zh")
	nextEvent(t, events, eventReady)
	ev := rowsOfEvent(t, nextEvent(t, events, eventRows))
	for _, want := range []string{"data-enshrined", "🌼 已供奉", `href="` + tombstoneURL(stone.ID) + `"`} {
		if !strings.Contains(ev.HTML, want) {
			t.Errorf("live rows lack %q:\n%s", want, ev.HTML)
		}
	}
	if strings.Contains(ev.HTML, "data-memory-banner") || strings.Contains(ev.HTML, "memory-vanishing") {
		t.Errorf("live enshrined row still fades:\n%s", ev.HTML)
	}
}

func TestMemoryScriptSkipsEnshrined(t *testing.T) {
	srv := newTestServer(t, Options{FixturesOnly: true})
	js := get(t, srv, "/static/memory.js").Body.String()
	if !strings.Contains(js, "data-enshrined") {
		t.Error("memory.js does not know enshrined rows")
	}
	// The check must come before the fade states are set.
	if i, j := strings.Index(js, "if (enshrined(el))"), strings.Index(js, "setStates(el, on);"); i < 0 || j < 0 || i > j {
		t.Error("memory.js sets fade states before skipping enshrined elements")
	}
}

func TestEnshrinedOfFirstWins(t *testing.T) {
	e := enshrinedOf([]graveyard.Tombstone{
		{ID: "aaaaaaaaaaaa", Namespace: "ns", PodName: "p"},
		{ID: "bbbbbbbbbbbb", Namespace: "ns", PodName: "p"},
		{ID: "cccccccccccc", Namespace: "other", PodName: "p"},
	})
	if id, ok := e.lookup("ns", "p"); !ok || id != "aaaaaaaaaaaa" {
		t.Errorf("lookup ns/p = %q %v, want the most recent death first", id, ok)
	}
	if _, ok := e.lookup("ns", "q"); ok {
		t.Error("lookup of an unknown pod succeeded")
	}
	var none enshrined
	if _, ok := none.lookup("ns", "p"); ok {
		t.Error("nil set enshrines")
	}
}
