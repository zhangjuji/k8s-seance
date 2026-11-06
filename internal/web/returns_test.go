package web

import (
	"html"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhangjuji/k8s-seance/internal/autopsy"
	"github.com/zhangjuji/k8s-seance/internal/graveyard"
)

// Offerings and returns (Requirement 18).

// familyStore is a graveyard with one tombstone: ns1/web-a of Deployment/web,
// buried now. Deaths in the tests are relative to the real clock, because the
// store stamps the burial with it.
func familyStore(t *testing.T) (*graveyard.Store, graveyard.Tombstone) {
	t.Helper()
	st, err := graveyard.Open(filepath.Join(t.TempDir(), "graveyard.json"))
	if err != nil {
		t.Fatal(err)
	}
	tomb, err := st.Bury(graveyard.Tombstone{
		Namespace: "ns1", PodName: "web-a", UID: "uid-web-a", Cause: autopsy.CauseCrashLoopBackOff,
		DiedAt: time.Now().Add(-10 * time.Minute), Family: "Deployment/web",
		Epitaph: "bye", PostmortemMD: "# web-a\n",
		Offerings: []graveyard.Offering{
			{Kind: graveyard.OfferingDeath, Text: "container app: CrashLoopBackOff, exit code 1", Evidence: "E1"},
			{Kind: graveyard.OfferingLastWords, Text: "FATAL <b>config</b> missing", Evidence: "E9"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return st, tomb
}

func deadSpirit(name, family string, restarts int32, died time.Time) autopsy.SpiritSummary {
	return autopsy.SpiritSummary{
		Namespace: "ns1", Name: name, Cause: autopsy.CauseCrashLoopBackOff,
		RestartCount: restarts, LastDeath: &died, Family: family,
	}
}

func TestListRecordsReturnsOfSelfAndFamily(t *testing.T) {
	st, tomb := familyStore(t)
	later := time.Now().Add(time.Minute)
	spirits := []autopsy.SpiritSummary{
		deadSpirit("web-a", "Deployment/web", 4, later),                        // the enshrined Pod died again
		deadSpirit("web-b", "Deployment/web", 1, later.Add(time.Second)),       // a new Pod of the family died
		deadSpirit("web-old", "Deployment/web", 9, time.Now().Add(-time.Hour)), // died before the burial
		deadSpirit("api-x", "Deployment/api", 2, later),                        // another family
		deadSpirit("bare", "", 2, later),                                       // no family at all
	}
	src := newLiveSource(listResult{spirits: spirits})
	srv := newTestServer(t, Options{Cluster: src, DefaultNamespace: "ns1", Graveyard: st})

	for round := 0; round < 2; round++ {
		body := get(t, srv, "/spirits").Body.String()
		link := `href="` + tombstoneURL(tomb.ID) + `"`
		if row := rowIn(t, body, "web-a"); !strings.Contains(row, "🕯️ 第 1 次归来") || !strings.Contains(row, `class="return-mark" `+link) {
			t.Errorf("round %d: web-a row:\n%s", round, row)
		}
		if row := rowIn(t, body, "web-b"); !strings.Contains(row, "🕯️ 第 2 次归来") || !strings.Contains(row, link) {
			t.Errorf("round %d: web-b row:\n%s", round, row)
		}
		// A family member is not enshrined itself: it keeps its countdown hooks.
		if row := rowIn(t, body, "web-b"); strings.Contains(row, "data-enshrined") {
			t.Errorf("round %d: family member is enshrined:\n%s", round, row)
		}
		for _, pod := range []string{"web-old", "api-x", "bare"} {
			if row := rowIn(t, body, pod); strings.Contains(row, "return-mark") {
				t.Errorf("round %d: %s has a return mark:\n%s", round, pod, row)
			}
		}
	}
	// Listing again does not count the same deaths again.
	stored, err := st.Get(tomb.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.Returns) != 2 || stored.Returns[0].Pod != "web-a" || stored.Returns[1].Pod != "web-b" {
		t.Fatalf("returns = %+v, want web-a then web-b", stored.Returns)
	}

	// A new death of the enshrined Pod is another return. web-b dying again is not.
	src.lists = []listResult{{spirits: []autopsy.SpiritSummary{
		deadSpirit("web-a", "Deployment/web", 5, later.Add(time.Minute)),
		deadSpirit("web-b", "Deployment/web", 2, later.Add(time.Minute)),
	}}}
	body := get(t, srv, "/spirits").Body.String()
	if row := rowIn(t, body, "web-a"); !strings.Contains(row, "🕯️ 第 3 次归来") {
		t.Errorf("web-a after a new death:\n%s", row)
	}
	if row := rowIn(t, body, "web-b"); !strings.Contains(row, "🕯️ 第 2 次归来") {
		t.Errorf("web-b keeps its return:\n%s", row)
	}
	if stored, _ := st.Get(tomb.ID); len(stored.Returns) != 3 {
		t.Errorf("returns = %d, want 3", len(stored.Returns))
	}

	// The JSON API carries the family of each spirit.
	if b := get(t, srv, "/api/spirits").Body.String(); !strings.Contains(b, `"family":"Deployment/web"`) {
		t.Errorf("/api/spirits lacks the family: %s", b)
	}
}

func TestBellAnnouncesReturnsInsteadOfDiedAgain(t *testing.T) {
	st, tomb := familyStore(t)
	before := time.Now().Add(-5 * time.Minute)
	later := time.Now().Add(time.Minute)
	src := newLiveSource(
		listResult{spirits: []autopsy.SpiritSummary{deadSpirit("web-a", "Deployment/web", 1, before)}},
		listResult{spirits: []autopsy.SpiritSummary{
			deadSpirit("web-a", "Deployment/web", 2, later),
			deadSpirit("web-b", "Deployment/web", 0, later),
			deadSpirit("api-x", "Deployment/api", 1, later),
		}},
		listResult{spirits: []autopsy.SpiritSummary{
			deadSpirit("web-a", "Deployment/web", 2, later),
			deadSpirit("web-b", "Deployment/web", 1, later.Add(time.Minute)),
			deadSpirit("api-x", "Deployment/api", 1, later),
		}},
	)
	srv := newTestServer(t, Options{Cluster: src, Graveyard: st})
	events := openBell(t, srv, "/api/spirits/stream?ns=ns1&lang=zh")
	nextEvent(t, events, eventReady)
	base := rowsOfEvent(t, nextEvent(t, events, eventRows))
	if len(base.Returned) != 0 || strings.Contains(base.HTML, "return-mark") {
		t.Fatalf("baseline announces a return: %+v", base.Returned)
	}

	src.poke()
	ev := rowsOfEvent(t, nextEvent(t, events, eventRows))
	if len(ev.Returned) != 2 {
		t.Fatalf("returned = %+v, want web-a and web-b", ev.Returned)
	}
	self, fam := ev.Returned[0], ev.Returned[1]
	if self.Name != "web-a" || !self.Self || self.N != 1 || self.URL != tombstoneURL(tomb.ID) || self.Restarts != 2 {
		t.Errorf("self return = %+v", self)
	}
	if fam.Name != "web-b" || fam.Self || fam.N != 2 || fam.TombPod != "web-a" {
		t.Errorf("family return = %+v", fam)
	}
	for _, r := range ev.Returned {
		if !strings.Contains(r.Text, "之前的修复可能没有生效") {
			t.Errorf("return text %q lacks the warning", r.Text)
		}
	}
	if !strings.Contains(fam.Text, "Deployment/web") || !strings.Contains(fam.Text, "web-a") {
		t.Errorf("family text = %q", fam.Text)
	}
	// The returns replace the ordinary notices; the other family still arrives.
	if len(ev.Worse) != 0 {
		t.Errorf("worse = %+v, want none: the return replaces it", ev.Worse)
	}
	if len(ev.Arrived) != 1 || ev.Arrived[0].Name != "api-x" {
		t.Errorf("arrived = %+v, want only api-x", ev.Arrived)
	}
	if !strings.Contains(ev.HTML, "🕯️ 第 1 次归来") || !strings.Contains(ev.HTML, "🕯️ 第 2 次归来") {
		t.Errorf("live rows lack the return marks:\n%s", ev.HTML)
	}

	// A family member returns once: its next death is an ordinary died-again notice.
	src.poke()
	ev = rowsOfEvent(t, nextEvent(t, events, eventRows))
	if len(ev.Returned) != 0 || len(ev.Worse) != 1 || ev.Worse[0].Name != "web-b" {
		t.Errorf("second death of web-b: returned %+v, worse %+v", ev.Returned, ev.Worse)
	}
}

func TestBellReturnEnglishAndScript(t *testing.T) {
	st, _ := familyStore(t)
	later := time.Now().Add(time.Minute)
	src := newLiveSource(
		listResult{spirits: []autopsy.SpiritSummary{}},
		listResult{spirits: []autopsy.SpiritSummary{deadSpirit("web-b", "Deployment/web", 0, later)}},
	)
	srv := newTestServer(t, Options{Cluster: src, Graveyard: st, DefaultNamespace: "ns1"})
	events := openBell(t, srv, "/api/spirits/stream?ns=ns1&lang=en")
	nextEvent(t, events, eventReady)
	nextEvent(t, events, eventRows)
	src.poke()
	ev := rowsOfEvent(t, nextEvent(t, events, eventRows))
	if len(ev.Returned) != 1 || !strings.Contains(ev.Returned[0].Text, "Return #1") || !strings.Contains(ev.Returned[0].Text, "earlier fix may not have worked") {
		t.Errorf("English return = %+v", ev.Returned)
	}
	if !strings.Contains(ev.HTML, "🕯️ Return #1") {
		t.Errorf("English rows lack the mark:\n%s", ev.HTML)
	}

	// The page script shows returns as their own notice and rings twice.
	page := get(t, srv, "/spirits?ns=ns1").Body.String()
	for _, want := range []string{"d.returned", "notice(r, 'returned')", "ring(2)", "bell-photo"} {
		if !strings.Contains(page, want) {
			t.Errorf("list page script lacks %q", want)
		}
	}
}

func TestAltarCandlesAndPhotoPageOfferings(t *testing.T) {
	st, tomb := familyStore(t)
	for i := 0; i < 12; i++ {
		pod := "web-" + string(rune('b'+i))
		if _, added, err := st.Return(tomb.ID, graveyard.Return{Pod: pod, DiedAt: time.Now().Add(time.Duration(i+1) * time.Minute), Cause: autopsy.CauseOOMKilled}); err != nil || !added {
			t.Fatalf("return %s: %v %v", pod, added, err)
		}
	}
	srv := newTestServer(t, Options{FixturesOnly: true, Graveyard: st})

	altar := get(t, srv, "/graveyard").Body.String()
	if n := strings.Count(altar, `class="return-candle"`); n != maxCandles {
		t.Errorf("altar candles = %d, want %d", n, maxCandles)
	}
	for _, want := range []string{"归来 12 次", "死状", "遗言", html.EscapeString("FATAL <b>config</b> missing")} {
		if !strings.Contains(altar, want) {
			t.Errorf("altar lacks %q", want)
		}
	}
	if strings.Contains(altar, "<b>config</b>") {
		t.Error("altar renders offering text as HTML")
	}

	photo := get(t, srv, tombstoneURL(tomb.ID)).Body.String()
	for _, want := range []string{
		"它已经归来 12 次：之前的修复可能没有生效。",
		"家族：Deployment/web",
		`class="offering offering-death"`,
		"当时的证据 [E1]",
		"当时的证据 [E9]",
		"web-m 卒于",
	} {
		if !strings.Contains(photo, want) {
			t.Errorf("photo page lacks %q", want)
		}
	}
	if n := strings.Count(photo, "卒于"); n != maxReturnLog {
		t.Errorf("photo page lists %d returns, want the latest %d", n, maxReturnLog)
	}

	en := get(t, srv, tombstoneURL(tomb.ID)+"?lang=en").Body.String()
	for _, want := range []string{"It has returned 12 times: the earlier fix may not have worked.", "Offerings", "How it died", "evidence [E1] at the time"} {
		if !strings.Contains(en, want) {
			t.Errorf("English photo page lacks %q", want)
		}
	}

	res := graveyardList(t, srv)
	if len(res.Tombstones) != 1 {
		t.Fatalf("tombstones = %d", len(res.Tombstones))
	}
	item := res.Tombstones[0]
	if item.Family != "Deployment/web" || item.Returns != 12 || len(item.ReturnLog) != 12 || len(item.Offerings) != 2 || item.Offerings[0].Evidence != "E1" {
		t.Errorf("api item = %+v", item)
	}
}

func TestPhotoWithoutReturnsOrOfferings(t *testing.T) {
	st, err := graveyard.Open(filepath.Join(t.TempDir(), "graveyard.json"))
	if err != nil {
		t.Fatal(err)
	}
	tomb := buryAt(t, st, "ns1", "quiet", autopsy.CauseError, time.Now().Add(-time.Hour))
	srv := newTestServer(t, Options{FixturesOnly: true, Graveyard: st})
	photo := get(t, srv, tombstoneURL(tomb.ID)).Body.String()
	if strings.Contains(photo, "return-banner") || strings.Contains(photo, "return-candle") {
		t.Error("a tombstone without returns shows the return warning or candles")
	}
	if !strings.Contains(photo, "这块墓碑没有供品。") {
		t.Error("photo page does not say there are no offerings")
	}
	if b := get(t, srv, "/api/graveyard").Body.String(); !strings.Contains(b, `"offerings":[]`) || !strings.Contains(b, `"returns":0`) || !strings.Contains(b, `"returnLog":[]`) {
		t.Errorf("api without offerings: %s", b)
	}
}

// TestPostmortemKeepsOfferingsAndSampleModeNeverReturns walks sample mode: the
// postmortem stores the family and the offerings from the séance evidence,
// listing never records a return (Requirement 18.5), and generating the
// postmortem again clears the returns (Requirement 18.4).
func TestPostmortemKeepsOfferingsAndSampleModeNeverReturns(t *testing.T) {
	srv, st := newGraveyardServer(t)
	res := createSeance(t, srv, crashNS, crashPod)
	streamOnce(t, srv, res.ID)
	postmortemJSON(t, srv, res.ID)

	list := graveyardList(t, srv).Tombstones
	if len(list) != 1 {
		t.Fatalf("tombstones = %d", len(list))
	}
	tomb, err := st.Get(list[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if tomb.Family != "Deployment/restless-spirit" {
		t.Errorf("family = %q", tomb.Family)
	}
	ev, err := autopsy.BuildEvidence(fixtureNamed(t, crashPod), autopsy.EvidenceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]string{}
	for _, e := range ev {
		byID[e.ID] = e.Text
	}
	var kinds []string
	for _, o := range tomb.Offerings {
		kinds = append(kinds, o.Kind)
		if byID[o.Evidence] == "" {
			t.Errorf("offering %s cites %q, not in the séance evidence", o.Kind, o.Evidence)
		}
	}
	if got := strings.Join(kinds, ","); got != "death,last_words,warning" {
		t.Errorf("offerings = %s, want death,last_words,warning", got)
	}

	for i := 0; i < 2; i++ {
		body := get(t, srv, "/spirits").Body.String()
		if strings.Contains(body, "return-mark") {
			t.Fatal("sample mode shows a return")
		}
	}
	if again, _ := st.Get(tomb.ID); len(again.Returns) != 0 {
		t.Fatalf("sample mode recorded returns: %+v", again.Returns)
	}

	// A return recorded by hand is cleared by the next postmortem.
	if _, added, err := st.Return(tomb.ID, graveyard.Return{Pod: crashPod, DiedAt: time.Now().Add(time.Minute)}); err != nil || !added {
		t.Fatalf("manual return: %v %v", added, err)
	}
	postmortemJSON(t, srv, res.ID)
	if again, _ := st.Get(tomb.ID); len(again.Returns) != 0 || len(again.Offerings) != 3 {
		t.Errorf("after a new postmortem: returns %d, offerings %d", len(again.Returns), len(again.Offerings))
	}
}
