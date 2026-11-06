package web

import (
	"encoding/json"
	"html"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/zhangjuji/k8s-seance/fixtures"
	"github.com/zhangjuji/k8s-seance/internal/autopsy"
	"github.com/zhangjuji/k8s-seance/internal/graveyard"
	"github.com/zhangjuji/k8s-seance/internal/llm"
	"github.com/zhangjuji/k8s-seance/internal/spirit"
)

// newGraveyardServer is a fixtures-only server with mock replies and a graveyard
// file in a temporary directory.
func newGraveyardServer(t *testing.T) (*Server, *graveyard.Store) {
	t.Helper()
	st, err := graveyard.Open(filepath.Join(t.TempDir(), "graveyard.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv := newTestServer(t, Options{
		Fixtures:     NewFixtureSource(),
		FixturesOnly: true,
		Provider:     llm.MockProvider{},
		Graveyard:    st,
	})
	return srv, st
}

// graveyardList calls GET /api/graveyard and decodes it.
func graveyardList(t *testing.T, srv *Server) graveyardResponse {
	t.Helper()
	rec := get(t, srv, "/api/graveyard")
	if rec.Code != http.StatusOK {
		t.Fatalf("/api/graveyard: status = %d, body %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("/api/graveyard: Content-Type = %q", ct)
	}
	var res graveyardResponse
	decodeJSON(t, rec.Body.Bytes(), &res)
	return res
}

var lastWordsLine = regexp.MustCompile(`(?m)^> Last words: "(.*)"$`)

func TestPostmortemBuriesTombstone(t *testing.T) {
	srv, st := newGraveyardServer(t)
	if got := graveyardList(t, srv); len(got.Tombstones) != 0 {
		t.Fatalf("new graveyard has %d tombstones", len(got.Tombstones))
	}
	res := createSeance(t, srv, crashNS, crashPod)
	streamOnce(t, srv, res.ID)
	reply := postmortemJSON(t, srv, res.ID)

	list := graveyardList(t, srv).Tombstones
	if len(list) != 1 {
		t.Fatalf("tombstones = %d, want 1", len(list))
	}
	ts := list[0]
	if ts.PodName != crashPod || ts.Namespace != crashNS || ts.Cause != autopsy.CauseCrashLoopBackOff {
		t.Errorf("tombstone = %+v", ts)
	}
	if reply.TombstoneURL != "/graveyard/"+ts.ID || ts.URL != reply.TombstoneURL {
		t.Errorf("tombstoneUrl = %q, list url = %q, id = %q", reply.TombstoneURL, ts.URL, ts.ID)
	}
	if ts.PostmortemURL != "/graveyard/"+ts.ID+"/postmortem.md" {
		t.Errorf("postmortemUrl = %q", ts.PostmortemURL)
	}

	// The epitaph is the postmortem's Last words; birth and death come from the snapshot.
	m := lastWordsLine.FindStringSubmatch(reply.Markdown)
	if m == nil {
		t.Fatalf("markdown has no Last words line")
	}
	if ts.Epitaph == "" || ts.Epitaph != m[1] {
		t.Errorf("epitaph = %q, last words = %q", ts.Epitaph, m[1])
	}
	a := fixtureNamed(t, crashPod)
	if ts.BornAt == nil || !ts.BornAt.Equal(a.CreatedAt) {
		t.Errorf("bornAt = %v, want %v", ts.BornAt, a.CreatedAt)
	}
	died, ok := spirit.DeathTime(a)
	if !ok || ts.DiedAt == nil || !ts.DiedAt.Equal(died) {
		t.Errorf("diedAt = %v, want %v", ts.DiedAt, died)
	}

	// The file holds the whole postmortem.
	full, err := st.Get(ts.ID)
	if err != nil {
		t.Fatal(err)
	}
	if full.PostmortemMD != reply.Markdown {
		t.Error("stored postmortem differs from the one returned")
	}
	if full.UID != a.UID {
		t.Errorf("uid = %q, want %q", full.UID, a.UID)
	}
}

func TestRegeneratedPostmortemReplacesTombstone(t *testing.T) {
	srv, st := newGraveyardServer(t)
	res := createSeance(t, srv, crashNS, crashPod)
	streamOnce(t, srv, res.ID)
	first := postmortemJSON(t, srv, res.ID)
	ts1, err := st.Get(strings.TrimPrefix(first.TombstoneURL, "/graveyard/"))
	if err != nil {
		t.Fatal(err)
	}

	// Spend the remaining calls, so the second postmortem comes from the template.
	for range 6 {
		askJSON(t, srv, res.ID, "还有呢？")
	}
	second := postmortemJSON(t, srv, res.ID)
	if second.Mode != "template" {
		t.Fatalf("second postmortem mode = %q, want template", second.Mode)
	}
	if second.TombstoneURL != first.TombstoneURL {
		t.Errorf("tombstone moved: %q then %q", first.TombstoneURL, second.TombstoneURL)
	}
	// A second séance for the same Pod also lands on the same tombstone.
	res2 := createSeance(t, srv, crashNS, crashPod)
	third := postmortemJSON(t, srv, res2.ID)
	if third.TombstoneURL != first.TombstoneURL {
		t.Errorf("new séance made a new tombstone: %q", third.TombstoneURL)
	}

	list := graveyardList(t, srv).Tombstones
	if len(list) != 1 {
		t.Fatalf("tombstones = %d, want 1", len(list))
	}
	ts2, err := st.Get(list[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if ts2.PostmortemMD != third.Markdown || ts2.PostmortemMD == ts1.PostmortemMD {
		t.Error("tombstone does not hold the latest postmortem")
	}
}

func TestGraveyardOrderedByDeathNewestFirst(t *testing.T) {
	srv, _ := newGraveyardServer(t)
	all, err := fixtures.Load()
	if err != nil {
		t.Fatal(err)
	}
	type death struct {
		pod string
		at  time.Time
	}
	var deaths []death
	for _, a := range all {
		res := createSeance(t, srv, a.Namespace, a.Name)
		postmortemJSON(t, srv, res.ID)
		d, ok := spirit.DeathTime(a)
		if !ok {
			t.Fatalf("fixture %s has no death time", a.Name)
		}
		deaths = append(deaths, death{a.Name, d})
	}
	list := graveyardList(t, srv).Tombstones
	if len(list) != len(all) {
		t.Fatalf("tombstones = %d, want %d", len(list), len(all))
	}
	for i := 1; i < len(list); i++ {
		if list[i-1].DiedAt.Before(*list[i].DiedAt) {
			t.Errorf("tombstone %d (%s, %v) died before tombstone %d (%s, %v)",
				i-1, list[i-1].PodName, list[i-1].DiedAt, i, list[i].PodName, list[i].DiedAt)
		}
	}
	newest := deaths[0]
	for _, d := range deaths[1:] {
		if d.at.After(newest.at) {
			newest = d
		}
	}
	if list[0].PodName != newest.pod {
		t.Errorf("first tombstone = %s, want the newest death %s", list[0].PodName, newest.pod)
	}

	// The grid page shows the same order, and each stone links to its page.
	rec := get(t, srv, "/graveyard")
	if rec.Code != http.StatusOK {
		t.Fatalf("/graveyard: status = %d", rec.Code)
	}
	page := rec.Body.String()
	last := -1
	for _, ts := range list {
		i := strings.Index(page, `href="/graveyard/`+ts.ID+`"`)
		if i < 0 {
			t.Fatalf("grid lacks a link to %s", ts.ID)
		}
		if i < last {
			t.Errorf("grid order differs from the API order at %s", ts.PodName)
		}
		last = i
		for _, want := range []string{ts.PodName, string(ts.Cause), html.EscapeString(ts.Epitaph)} {
			if !strings.Contains(page, want) {
				t.Errorf("grid lacks %q", want)
			}
		}
	}
}

func TestTombstonePageShowsPostmortem(t *testing.T) {
	srv, _ := newGraveyardServer(t)
	res := createSeance(t, srv, crashNS, crashPod)
	streamOnce(t, srv, res.ID)
	reply := postmortemJSON(t, srv, res.ID)

	rec := get(t, srv, reply.TombstoneURL)
	if rec.Code != http.StatusOK {
		t.Fatalf("tombstone page: status = %d, body %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q", ct)
	}
	page := rec.Body.String()
	for _, want := range []string{
		crashPod,
		`id="pm-md"`,
		html.EscapeString(reply.Markdown),
		`href="` + reply.TombstoneURL + `/postmortem.md"`,
		`href="/graveyard"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("tombstone page lacks %q", want[:min(len(want), 80)])
		}
	}

	dl := get(t, srv, reply.TombstoneURL+"/postmortem.md")
	if dl.Code != http.StatusOK {
		t.Fatalf("download: status = %d", dl.Code)
	}
	if dl.Body.String() != reply.Markdown {
		t.Error("downloaded postmortem differs from the stored one")
	}
	if ct := dl.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/markdown") {
		t.Errorf("download Content-Type = %q", ct)
	}
	cd := dl.Header().Get("Content-Disposition")
	want := regexp.MustCompile(`^attachment; filename="postmortem-` + regexp.QuoteMeta(crashPod) + `-\d{8}\.md"$`)
	if !want.MatchString(cd) {
		t.Errorf("Content-Disposition = %q", cd)
	}
}

func TestPostmortemFragmentLinksTombstone(t *testing.T) {
	srv, _ := newGraveyardServer(t)
	res := createSeance(t, srv, crashNS, crashPod)
	rec := send(t, srv, http.MethodPost, "/api/seances/"+res.ID+"/postmortem", "", "", map[string]string{"HX-Request": "true"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	list := graveyardList(t, srv).Tombstones
	if len(list) != 1 {
		t.Fatalf("tombstones = %d, want 1", len(list))
	}
	if !strings.Contains(rec.Body.String(), `href="/graveyard/`+list[0].ID+`"`) {
		t.Errorf("fragment lacks the tombstone link: %s", rec.Body)
	}
}

func TestDownloadWithoutPostAlsoBuries(t *testing.T) {
	srv, _ := newGraveyardServer(t)
	res := createSeance(t, srv, crashNS, crashPod)
	downloadOf(t, srv, res.ID)
	if n := len(graveyardList(t, srv).Tombstones); n != 1 {
		t.Fatalf("tombstones = %d, want 1", n)
	}
	// The stored report is reused, so a second download writes nothing new.
	downloadOf(t, srv, res.ID)
	if n := len(graveyardList(t, srv).Tombstones); n != 1 {
		t.Fatalf("tombstones = %d, want 1", n)
	}
}

func TestTombstoneUnknownOrInvalidIs404(t *testing.T) {
	srv, _ := newGraveyardServer(t)
	for _, path := range []string{
		"/graveyard/0123456789ab",
		"/graveyard/0123456789ab/postmortem.md",
		"/graveyard/not-an-id",
		"/graveyard/..%2f..%2fetc",
		"/graveyard/0123456789AB",
	} {
		if rec := get(t, srv, path); rec.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", path, rec.Code)
		}
	}
	rec := send(t, srv, http.MethodGet, "/graveyard/0123456789ab", "", "", map[string]string{"Accept": "application/json"})
	if rec.Code != http.StatusNotFound || !json.Valid(rec.Body.Bytes()) {
		t.Errorf("JSON 404: status = %d body %s", rec.Code, rec.Body)
	}
}

func TestGraveyardOff(t *testing.T) {
	srv := newSeanceServer(t, llm.Limits{})
	res := createSeance(t, srv, crashNS, crashPod)
	reply := postmortemJSON(t, srv, res.ID)
	if reply.TombstoneURL != "" {
		t.Errorf("tombstoneUrl = %q with the graveyard off", reply.TombstoneURL)
	}
	if rec := get(t, srv, "/api/graveyard"); rec.Code != http.StatusNotFound {
		t.Errorf("/api/graveyard: status = %d, want 404", rec.Code)
	}
	rec := get(t, srv, "/graveyard")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "墓地未开启") {
		t.Errorf("/graveyard: status = %d, body lacks the off notice", rec.Code)
	}
	if rec := get(t, srv, "/graveyard/0123456789ab"); rec.Code != http.StatusNotFound {
		t.Errorf("tombstone page: status = %d, want 404", rec.Code)
	}
}

func TestCorruptGraveyardKeepsPostmortemAndFile(t *testing.T) {
	srv, st := newGraveyardServer(t)
	garbage := []byte("[{broken")
	if err := os.WriteFile(st.Path(), garbage, 0o600); err != nil {
		t.Fatal(err)
	}
	res := createSeance(t, srv, crashNS, crashPod)
	reply := postmortemJSON(t, srv, res.ID)
	if reply.Markdown == "" || reply.TombstoneURL != "" {
		t.Errorf("postmortem = %d bytes, tombstoneUrl = %q; want a report and no link", len(reply.Markdown), reply.TombstoneURL)
	}
	data, err := os.ReadFile(st.Path())
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(garbage) {
		t.Error("corrupt graveyard file was overwritten")
	}
	if rec := get(t, srv, "/api/graveyard"); rec.Code != http.StatusInternalServerError {
		t.Errorf("/api/graveyard: status = %d, want 500", rec.Code)
	}
	rec := get(t, srv, "/graveyard")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "读取墓地失败") {
		t.Errorf("/graveyard: status = %d, lacks the read notice", rec.Code)
	}
}

func TestGraveyardEscapesStoredText(t *testing.T) {
	srv, st := newGraveyardServer(t)
	ts, err := st.Bury(graveyard.Tombstone{
		PodName:      "evil",
		Namespace:    "n",
		Cause:        autopsy.CauseUnknown,
		Epitaph:      `<script>alert(1)</script>`,
		PostmortemMD: "# x\n<img src=x onerror=alert(2)>\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/graveyard", "/graveyard/" + ts.ID} {
		body := get(t, srv, path).Body.String()
		if strings.Contains(body, "<script>alert(1)") || strings.Contains(body, "<img src=x") {
			t.Errorf("%s renders stored text unescaped", path)
		}
	}
}

func TestEmptyGraveyardPageAndNav(t *testing.T) {
	srv, _ := newGraveyardServer(t)
	rec := get(t, srv, "/graveyard")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "墓地还是空的") {
		t.Errorf("/graveyard empty: status = %d", rec.Code)
	}
	if body := get(t, srv, "/spirits").Body.String(); !strings.Contains(body, `href="/graveyard"`) {
		t.Error("top bar lacks the graveyard link")
	}
}

// fixtureNamed returns the embedded fixture with the Pod name.
func fixtureNamed(t *testing.T, name string) *autopsy.PodAutopsy {
	t.Helper()
	all, err := fixtures.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range all {
		if a.Name == name {
			return a
		}
	}
	t.Fatalf("no fixture %s", name)
	return nil
}
