package web

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhangjuji/k8s-seance/fixtures"
	"github.com/zhangjuji/k8s-seance/internal/graveyard"
	"github.com/zhangjuji/k8s-seance/internal/llm"
)

// getWith sends a GET with extra headers and cookies.
func getWith(t *testing.T, srv *Server, target string, headers map[string]string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// hanOutside reports whether html has a CJK ideograph outside the language
// switch label, which is "中文" on English pages.
func hanOutside(html string) bool {
	return hasHan(strings.ReplaceAll(html, catalog[llm.LangEN]["lang_switch"], ""))
}

func TestCatalogKeysMatch(t *testing.T) {
	zh, en := catalog[llm.LangZH], catalog[llm.LangEN]
	for k := range zh {
		if _, ok := en[k]; !ok {
			t.Errorf("key %q missing in en", k)
		}
	}
	for k, v := range en {
		if _, ok := zh[k]; !ok {
			t.Errorf("key %q missing in zh", k)
		}
		if k != "lang_switch" && hasHan(v) {
			t.Errorf("en %q has Chinese text: %q", k, v)
		}
		if strings.Count(v, "%") != strings.Count(zh[k], "%") {
			t.Errorf("key %q: zh and en have different format verbs", k)
		}
	}
}

func TestAcceptLanguage(t *testing.T) {
	tests := map[string]string{
		"en-US,en;q=0.9":               llm.LangEN,
		"zh-CN,zh;q=0.9,en;q=0.8":      llm.LangZH,
		"fr-FR,fr;q=0.9":               llm.LangEN, // neither zh nor en
		"*":                            llm.LangEN,
		"fr,en;q=0.5,zh;q=0.4":         llm.LangEN,
		"fr,zh;q=0.8,en;q=0.5":         llm.LangZH,
		"en;q=0.3,zh-TW;q=0.7":         llm.LangZH, // q wins over order
		"en;q=0, zh":                   llm.LangZH, // q=0 refuses en
		"ja, EN-gb ; q=0.8 , zh;q=0.1": llm.LangEN,
	}
	for h, want := range tests {
		if got := acceptLanguage(h); got != want {
			t.Errorf("acceptLanguage(%q) = %q, want %q", h, got, want)
		}
	}
}

// TestUILanguagePrecedence checks ?lang= > cookie > -lang > Accept-Language >
// Chinese, and that ?lang= sets the cookie.
func TestUILanguagePrecedence(t *testing.T) {
	srv := newTestServer(t, Options{FixturesOnly: true})
	fixed := newTestServer(t, Options{FixturesOnly: true, Lang: "EN"})
	enCookie := &http.Cookie{Name: langCookie, Value: "en"}
	zhCookie := &http.Cookie{Name: langCookie, Value: "zh"}
	acceptEN := map[string]string{"Accept-Language": "en-US,en;q=0.9"}
	acceptZH := map[string]string{"Accept-Language": "zh-CN"}
	acceptFR := map[string]string{"Accept-Language": "fr-FR"}

	tests := []struct {
		name    string
		srv     *Server
		target  string
		headers map[string]string
		cookies []*http.Cookie
		want    string
	}{
		{"no header is Chinese", srv, "/spirits", nil, nil, llm.LangZH},
		{"Accept-Language en", srv, "/spirits", acceptEN, nil, llm.LangEN},
		{"Accept-Language zh", srv, "/spirits", acceptZH, nil, llm.LangZH},
		{"Accept-Language without zh or en", srv, "/spirits", acceptFR, nil, llm.LangEN},
		{"cookie beats header", srv, "/spirits", acceptEN, []*http.Cookie{zhCookie}, llm.LangZH},
		{"query beats cookie", srv, "/spirits?lang=en", nil, []*http.Cookie{zhCookie}, llm.LangEN},
		{"bad query is ignored", srv, "/spirits?lang=fr", nil, []*http.Cookie{enCookie}, llm.LangEN},
		{"-lang beats header", fixed, "/spirits", acceptZH, nil, llm.LangEN},
		{"cookie beats -lang", fixed, "/spirits", nil, []*http.Cookie{zhCookie}, llm.LangZH},
		{"query beats -lang", fixed, "/spirits?lang=zh", nil, nil, llm.LangZH},
	}
	for _, tt := range tests {
		rec := getWith(t, tt.srv, tt.target, tt.headers, tt.cookies...)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d", tt.name, rec.Code)
		}
		wantAttr := fmt.Sprintf(`<html lang="%s">`, tr(tt.want, "html_lang"))
		if !strings.Contains(rec.Body.String(), wantAttr) {
			t.Errorf("%s: page is not %s", tt.name, tt.want)
		}
	}

	rec := getWith(t, srv, "/spirits?lang=en&src=fixtures", nil)
	var set *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == langCookie {
			set = c
		}
	}
	if set == nil || set.Value != "en" || set.Path != "/" || !set.HttpOnly {
		t.Errorf("?lang=en cookie = %+v", set)
	}
	if rec := getWith(t, srv, "/spirits", acceptEN); len(rec.Result().Cookies()) != 0 {
		t.Error("a page without ?lang= set a cookie")
	}
}

func TestNewRejectsUnknownLang(t *testing.T) {
	if _, err := New(Options{Lang: "fr"}); err == nil {
		t.Error("New with Lang fr: no error")
	}
}

// TestEnglishListPage checks the English spirit list: English text only, the
// switch link back to Chinese with the other query parameters, and a summon
// form that carries lang=en.
func TestEnglishListPage(t *testing.T) {
	srv := newTestServer(t, Options{FixturesOnly: true})
	rec := getWith(t, srv, "/spirits?src=fixtures&lang=en", nil)
	html := rec.Body.String()
	for _, want := range []string{
		`<html lang="en">`, "Restless spirits", "Sample mode: the spirits come from the built-in fixtures",
		"all 3 spirits are listed", ">Summon</button>", `name="lang" value="en"`, "Cause of death",
		`href="/spirits?lang=zh&amp;src=fixtures"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("English list page has no %q", want)
		}
	}
	if hanOutside(html) {
		t.Error("English list page has Chinese text outside the language switch")
	}

	zh := get(t, srv, "/spirits?src=fixtures").Body.String()
	for _, want := range []string{`<html lang="zh-CN">`, "<h1>亡灵列表</h1>", ">召唤</button>", `name="lang" value="zh"`, ">English</a>"} {
		if !strings.Contains(zh, want) {
			t.Errorf("Chinese list page has no %q", want)
		}
	}
}

func TestEnglishClusterDownNotice(t *testing.T) {
	srv := newTestServer(t, Options{})
	html := getWith(t, srv, "/spirits?lang=en", nil).Body.String()
	if !strings.Contains(html, "Cannot reach the cluster: no cluster connection is configured") || hanOutside(html) {
		t.Errorf("English cluster notice missing or not English")
	}
	if zh := get(t, srv, "/spirits").Body.String(); !strings.Contains(zh, "连不上集群：未配置集群连接") {
		t.Error("Chinese cluster notice changed")
	}
}

// createSeanceLang starts a séance through the JSON API with extra headers and
// an optional lang field.
func createSeanceLang(t *testing.T, srv *Server, lang string, headers map[string]string) createResponse {
	t.Helper()
	body := fmt.Sprintf(`{"ns":%q,"pod":%q,"src":"fixtures","lang":%q}`, crashNS, crashPod, lang)
	rec := send(t, srv, http.MethodPost, "/api/seances", "application/json", body, headers)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: status = %d, body %s", rec.Code, rec.Body)
	}
	var res createResponse
	decodeJSON(t, rec.Body.Bytes(), &res)
	return res
}

func TestSeanceLanguageAtCreation(t *testing.T) {
	srv := newSeanceServer(t, llm.Limits{})
	tests := []struct {
		name    string
		lang    string
		headers map[string]string
		want    string
	}{
		{"default", "", nil, llm.LangZH},
		{"lang field", "en", nil, llm.LangEN},
		{"interface language", "", map[string]string{"Accept-Language": "en"}, llm.LangEN},
		{"field beats interface", "zh", map[string]string{"Accept-Language": "en"}, llm.LangZH},
	}
	for _, tt := range tests {
		res := createSeanceLang(t, srv, tt.lang, tt.headers)
		if res.Lang != tt.want {
			t.Errorf("%s: lang = %q, want %q", tt.name, res.Lang, tt.want)
		}
		if d := detailOf(t, srv, res.ID); d.Lang != tt.want {
			t.Errorf("%s: detail lang = %q", tt.name, d.Lang)
		}
	}

	body := fmt.Sprintf(`{"ns":%q,"pod":%q,"src":"fixtures","lang":"fr"}`, crashNS, crashPod)
	if rec := send(t, srv, http.MethodPost, "/api/seances", "application/json", body, nil); rec.Code != http.StatusBadRequest {
		t.Errorf("lang fr: status = %d, want 400", rec.Code)
	}

	// The summon form of the English list page.
	form := "ns=" + crashNS + "&pod=" + crashPod + "&src=fixtures&lang=en"
	rec := send(t, srv, http.MethodPost, "/api/seances", "application/x-www-form-urlencoded", form, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("form create: status = %d", rec.Code)
	}
	id := strings.TrimPrefix(rec.Header().Get("Location"), "/seances/")
	if d := detailOf(t, srv, id); d.Lang != llm.LangEN {
		t.Errorf("form create: lang = %q, want en", d.Lang)
	}
}

// TestEnglishSeanceFlow runs an English séance on the mock: English opening,
// English turn labels, English quick questions, English answers and the
// English exhausted line.
func TestEnglishSeanceFlow(t *testing.T) {
	srv := newSeanceServer(t, llm.Limits{MaxCalls: 2})
	res := createSeanceLang(t, srv, "en", nil)

	turns := turnsOf(t, streamOnce(t, srv, res.ID))
	if len(turns) != 1 || hasHan(turns[0].Content) || !strings.Contains(turns[0].Content, "[E") {
		t.Fatalf("English opening turns = %+v", turns)
	}
	if !strings.Contains(turns[0].HTML, `<span class="who">Spirit</span>`) {
		t.Errorf("opening fragment label: %s", turns[0].HTML)
	}

	page := getWith(t, srv, res.PageURL+"?lang=en", nil).Body.String()
	for _, want := range []string{"How did you die?", "How do I fix it?", "Ritual power: 1 / 2 calls used", "Time of death (UTC):", ">Ask</button>"} {
		if !strings.Contains(page, want) {
			t.Errorf("English séance page has no %q", want)
		}
	}
	if strings.Contains(page, "lang-notice") {
		t.Error("English page of an English séance shows the language notice")
	}
	if hanOutside(page) {
		t.Error("English séance page has Chinese text outside the language switch")
	}

	var fix messageReply
	decodeJSON(t, askJSON(t, srv, res.ID, "How do I fix it?").Body.Bytes(), &fix)
	if hasHan(fix.Reply) || !strings.Contains(fix.Reply, "[E") {
		t.Errorf("English fix reply = %q", fix.Reply)
	}

	var out messageReply
	decodeJSON(t, askJSON(t, srv, res.ID, "How did you die?").Body.Bytes(), &out)
	if !out.Exhausted || out.Reply != llm.ExhaustedLineEN {
		t.Errorf("exhausted reply = %+v, want the English line", out)
	}
}

// TestLanguageMismatchNotice checks the notice shown when the séance language
// differs from the interface language.
func TestLanguageMismatchNotice(t *testing.T) {
	srv := newSeanceServer(t, llm.Limits{})
	en := createSeanceLang(t, srv, "en", nil)
	zh := createSeanceLang(t, srv, "zh", nil)

	page := getWith(t, srv, en.PageURL+"?lang=zh", nil).Body.String()
	if !strings.Contains(page, "这场降灵会用英文进行。切换语言只影响界面。") {
		t.Error("Chinese page of an English séance has no notice")
	}
	if !strings.Contains(page, "How did you die?") {
		t.Error("quick questions do not follow the séance language")
	}
	page = getWith(t, srv, zh.PageURL+"?lang=en", nil).Body.String()
	if !strings.Contains(page, "This séance is held in Chinese. Switching the language only changes the interface.") {
		t.Error("English page of a Chinese séance has no notice")
	}
	if !strings.Contains(page, "你是怎么死的？") {
		t.Error("quick questions do not follow the séance language")
	}
	if strings.Contains(get(t, srv, zh.PageURL).Body.String(), "lang-notice") {
		t.Error("Chinese page of a Chinese séance shows the notice")
	}
}

// TestEnglishPostmortemAndGraveyard checks the postmortem panel and the
// graveyard pages in English.
func TestEnglishPostmortemAndGraveyard(t *testing.T) {
	store, err := graveyard.Open(filepath.Join(t.TempDir(), "graveyard.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv := newTestServer(t, Options{FixturesOnly: true, Provider: llm.MockProvider{}, Graveyard: store})
	res := createSeanceLang(t, srv, "en", nil)
	streamOnce(t, srv, res.ID)

	rec := send(t, srv, http.MethodPost, "/api/seances/"+res.ID+"/postmortem", "", "", map[string]string{"HX-Request": "true", "Accept-Language": "en"})
	panel := rec.Body.String()
	for _, want := range []string{`aria-label="Postmortem"`, ">Copy</button>", "Download .md", "On the ofrenda. It will not be forgotten", `"Copied"`} {
		if !strings.Contains(panel, want) {
			t.Errorf("English postmortem panel has no %q", want)
		}
	}
	if hasHan(panel) {
		t.Error("English postmortem panel has Chinese text")
	}

	grid := getWith(t, srv, "/graveyard?lang=en", nil).Body.String()
	for _, want := range []string{"<h1>Ofrenda</h1>", "Born: ", "Died: ", crashPod} {
		if !strings.Contains(grid, want) {
			t.Errorf("English graveyard has no %q", want)
		}
	}
	if hanOutside(grid) {
		t.Error("English graveyard has Chinese text")
	}
	all, _ := store.List()
	if len(all) != 1 {
		t.Fatalf("tombstones = %d", len(all))
	}
	tomb := getWith(t, srv, "/graveyard/"+all[0].ID+"?lang=en", nil).Body.String()
	for _, want := range []string{crashPod + " on the ofrenda", "← Back to the ofrenda", "<h2>Postmortem</h2>"} {
		if !strings.Contains(tomb, want) {
			t.Errorf("English tombstone page has no %q", want)
		}
	}

	off := newTestServer(t, Options{FixturesOnly: true})
	if html := getWith(t, off, "/graveyard?lang=en", nil).Body.String(); !strings.Contains(html, "The ofrenda is off") {
		t.Error("English graveyard-off notice missing")
	}
}

// TestEnglishSeanceReplay checks that an English séance on the replay provider
// streams the recorded English opening, and a Chinese séance its own recording.
func TestEnglishSeanceReplay(t *testing.T) {
	srv := newTestServer(t, Options{
		FixturesOnly: true,
		Provider:     &llm.ReplayProvider{FS: fixtures.ReplayFS()},
	})
	open := func(lang string) string {
		res := createSeanceLang(t, srv, lang, nil)
		turns := turnsOf(t, streamOnce(t, srv, res.ID))
		if len(turns) != 1 {
			t.Fatalf("%s: turns = %d", lang, len(turns))
		}
		return turns[0].Content
	}
	en := open(llm.LangEN)
	if hasHan(en) || !strings.Contains(en, "FATAL: config /etc/app/config.yaml not found") {
		t.Errorf("English replay opening = %q", en)
	}
	if zh := open(llm.LangZH); !hasHan(zh) {
		t.Errorf("Chinese replay opening = %q", zh)
	}
}
