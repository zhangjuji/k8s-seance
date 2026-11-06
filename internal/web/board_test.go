package web

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/zhangjuji/k8s-seance/internal/autopsy"
	"github.com/zhangjuji/k8s-seance/internal/llm"
)

func runeIDs(n int) []string {
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("E%d", i+1)
	}
	return ids
}

func TestPlaceRunes(t *testing.T) {
	if got := placeRunes(nil); got != nil {
		t.Errorf("placeRunes(nil) = %v, want nil", got)
	}
	one := placeRunes([]string{"E1"})
	if len(one) != 1 || one[0].Left != 50 || one[0].Top != 30 {
		t.Errorf("one rune = %+v, want the top of the arc (50, 30)", one)
	}

	for _, n := range []int{2, 5, 10, 11, 20, 31} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			ids := runeIDs(n)
			got := placeRunes(ids)
			if len(got) != n {
				t.Fatalf("runes = %d, want %d", len(got), n)
			}
			seen := make(map[[2]float64]string)
			for i, r := range got {
				if r.ID != ids[i] {
					t.Errorf("rune %d = %s, want %s (order kept)", i, r.ID, ids[i])
				}
				// Inside the board, clear of YES/NO (top 13 %) and the number row (top 79 %).
				if r.Left < 5 || r.Left > 95 || r.Top < 25 || r.Top > 70 {
					t.Errorf("rune %s at (%v, %v) is outside the rune area", r.ID, r.Left, r.Top)
				}
				p := [2]float64{r.Left, r.Top}
				if other, dup := seen[p]; dup {
					t.Errorf("runes %s and %s share position %v", other, r.ID, p)
				}
				seen[p] = r.ID
			}
			if n <= boardArcMax {
				// One arc: left to right.
				for i := 1; i < n; i++ {
					if got[i].Left <= got[i-1].Left {
						t.Errorf("rune %s is not right of %s", got[i].ID, got[i-1].ID)
					}
				}
				return
			}
			// Two arcs: the outer one takes the first half, rounded up, and the
			// inner one sits lower in the middle.
			outer := (n + 1) / 2
			midOuter, midInner := got[outer/2], got[outer+(n-outer)/2]
			if midInner.Top <= midOuter.Top {
				t.Errorf("inner arc (top %v) is not below the outer arc (top %v)", midInner.Top, midOuter.Top)
			}
		})
	}
}

func TestBuildBoard(t *testing.T) {
	ev := []autopsy.Evidence{{ID: "E1", Kind: "status"}, {ID: "E2", Kind: "event"}}
	b := buildBoard(ev)
	if len(b.Runes) != 2 || b.Runes[0].Kind != "status" || b.Runes[1].Kind != "event" {
		t.Errorf("runes = %+v", b.Runes)
	}
	if strings.Join(b.Digits, "") != "1234567890" {
		t.Errorf("digits = %v", b.Digits)
	}
}

func TestMarkdownLite(t *testing.T) {
	link := func(s string) string { return strings.ReplaceAll(s, "[E1]", `<a href="#ev-E1">[E1]</a>`) }
	cases := []struct{ name, in, want string }{
		{"bold", "a **b** c", "a <strong>b</strong> c"},
		{"code", "run `kubectl get pods`", "run <code>kubectl get pods</code>"},
		{"both", "**x** and `y`", "<strong>x</strong> and <code>y</code>"},
		{"escaped tag in bold", "**<b>x</b>**", "<strong>&lt;b&gt;x&lt;/b&gt;</strong>"},
		{"escaped script in code", "`<script>alert(1)</script>`", "<code>&lt;script&gt;alert(1)&lt;/script&gt;</code>"},
		{"quote in code", "`\" onmouseover=\"x`", "<code>&#34; onmouseover=&#34;x</code>"},
		{"unclosed", "**open and `half", "**open and `half"},
		{"no newline span", "**a\nb**", "**a\nb**"},
		{"link outside code", "see [E1] `[E1]`", `see <a href="#ev-E1">[E1]</a> <code>[E1]</code>`},
		{"bold not inside code", "`**x**`", "<code>**x**</code>"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			escaped := htmlEscape(c.in)
			if got := markdownLite(escaped, link); got != c.want {
				t.Errorf("markdownLite(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func htmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "'", "&#39;", "<", "&lt;", ">", "&gt;", `"`, "&#34;")
	return r.Replace(s)
}

func TestTurnVerdict(t *testing.T) {
	doubt := []doubtItem{{Kind: "citation"}}
	cases := []struct {
		role      string
		exhausted bool
		doubts    []doubtItem
		want      string
	}{
		{roleSpirit, false, nil, verdictYes},
		{roleSpirit, false, doubt, verdictNo},
		{roleSpirit, true, nil, ""},
		{roleUser, false, nil, ""},
	}
	for _, c := range cases {
		if got := turnVerdict(c.role, c.exhausted, c.doubts); got != c.want {
			t.Errorf("turnVerdict(%s, %v, %d doubts) = %q, want %q", c.role, c.exhausted, len(c.doubts), got, c.want)
		}
	}
}

// TestSeancePageBoard: the séance page has a foldable spirit board whose runes
// are the evidence IDs, with YES/NO/GOODBYE and the number row.
func TestSeancePageBoard(t *testing.T) {
	for _, lang := range []string{llm.LangZH, llm.LangEN} {
		t.Run(lang, func(t *testing.T) {
			srv := newTestServer(t, Options{Fixtures: NewFixtureSource(), FixturesOnly: true, Provider: llm.MockProvider{}, Lang: lang})
			created := createSeance(t, srv, crashNS, crashPod)
			html := get(t, srv, created.PageURL).Body.String()
			for _, want := range []string{
				`<details id="board-wrap" class="board-wrap" open>`,
				"<h2>" + tr(lang, "board_title") + "</h2>",
				`data-rune="yes">` + tr(lang, "board_yes") + `<`,
				`data-rune="no">` + tr(lang, "board_no") + `<`,
				`data-rune="bye">` + tr(lang, "board_bye") + `<`,
				`<div class="board-digits" aria-hidden="true"><span>1</span>`,
				`<span>0</span></div>`,
				`id="planchette" class="planchette" aria-hidden="true"`,
				`id="board-status" class="board-status" role="status"`,
				// The script hooks: deltas, the verdict of a turn and the postmortem.
				`'sse:delta'`, `data-verdict`, `push('bye')`, `STEP_MS = 750`,
			} {
				if !strings.Contains(html, want) {
					t.Errorf("séance page does not contain %q", want)
				}
			}
			if strings.Contains(strings.ToLower(html), "ouija") {
				t.Error("séance page uses the Ouija trademark")
			}
			d := detailOf(t, srv, created.ID)
			runes := regexp.MustCompile(`<a class="rune ev-link" href="#ev-(E\d+)" data-rune="(E\d+)" title="[a-z]+" style="left: [\d.]+%; top: [\d.]+%">(E\d+)</a>`).FindAllStringSubmatch(html, -1)
			if len(runes) != len(d.Evidence) || len(runes) == 0 {
				t.Fatalf("runes = %d, want one per evidence item (%d)", len(runes), len(d.Evidence))
			}
			for i, m := range runes {
				id := d.Evidence[i].ID
				if m[1] != id || m[2] != id || m[3] != id {
					t.Errorf("rune %d = %v, want %s", i, m[1:], id)
				}
			}
		})
	}
}

// TestTurnVerdictAttribute: a spirit turn tells the board where to go: yes for
// a clean answer, no when the fact check doubts it. User turns and the exhausted
// line have no verdict.
func TestTurnVerdictAttribute(t *testing.T) {
	clean := newSeanceServer(t, llm.Limits{MaxCalls: 2})
	res := createSeance(t, clean, crashNS, crashPod)
	opening := turnsOf(t, streamOnce(t, clean, res.ID))[0]
	if len(opening.Doubts) == 0 && !strings.Contains(opening.HTML, `data-verdict="yes"`) {
		t.Errorf("clean opening has no yes verdict: %s", opening.HTML)
	}
	askJSON(t, clean, res.ID, "怎么修复？")
	if rec := askJSON(t, clean, res.ID, "还有呢？"); rec.Code != http.StatusOK {
		t.Fatalf("exhausted ask: status = %d", rec.Code)
	}
	turns := clean.seances.get(res.ID).turnsSnapshot()
	for _, tr := range turns {
		has := strings.Contains(tr.HTML, "data-verdict=")
		switch {
		case tr.Role == roleUser && has:
			t.Errorf("user turn %d has a verdict: %s", tr.Index, tr.HTML)
		case llm.IsExhaustedLine(tr.Content) && has:
			t.Errorf("exhausted line %d has a verdict: %s", tr.Index, tr.HTML)
		}
	}
	if last := turns[len(turns)-1]; !llm.IsExhaustedLine(last.Content) {
		t.Errorf("last turn is not the exhausted line: %q", last.Content)
	}

	liar := newLiarServer(t)
	res = createSeance(t, liar, crashNS, crashPod)
	opening = turnsOf(t, streamOnce(t, liar, res.ID))[0]
	if !strings.Contains(opening.HTML, `data-verdict="no"`) {
		t.Errorf("doubted opening has no no verdict: %s", opening.HTML)
	}
}

// TestReplyMarkdownSafe: bold and code in a spirit reply are rendered after
// escaping, so the reply cannot inject a tag or an attribute.
func TestReplyMarkdownSafe(t *testing.T) {
	reply := "我被 **OOMKilled** 了，看 `kubectl describe pod` [E1]。<script>alert(1)</script> " +
		"**<img src=x onerror=alert(1)>** `<a href=\"javascript:x\">`"
	srv := newTestServer(t, Options{Fixtures: NewFixtureSource(), FixturesOnly: true, Provider: liarProvider{text: reply}})
	res := createSeance(t, srv, crashNS, crashPod)
	html := turnsOf(t, streamOnce(t, srv, res.ID))[0].HTML
	for _, want := range []string{
		"<strong>OOMKilled</strong>",
		"<code>kubectl describe pod</code>",
		`<a class="ev-link" href="#ev-E1">[E1]</a>`,
		"&lt;script&gt;alert(1)&lt;/script&gt;",
		"<strong>&lt;img src=x onerror=alert(1)&gt;</strong>",
		"<code>&lt;a href=&#34;javascript:x&#34;&gt;</code>",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("reply html does not contain %q:\n%s", want, html)
		}
	}
	for _, bad := range []string{"<script", "<img", `<a href="javascript`} {
		if strings.Contains(html, bad) {
			t.Errorf("reply html contains %q:\n%s", bad, html)
		}
	}
	// The page renders the same fragment.
	page := get(t, srv, res.PageURL).Body.String()
	if !strings.Contains(page, "<strong>OOMKilled</strong>") || strings.Contains(page, "<img src=x") {
		t.Error("séance page does not render the reply safely")
	}

	// A user question keeps its asterisks: markdown is for answers only.
	askJSON(t, srv, res.ID, "**为什么**？")
	turns := srv.seances.get(res.ID).turnsSnapshot()
	user := turns[1]
	if user.Role != roleUser || strings.Contains(user.HTML, "<strong>") {
		t.Errorf("user turn got markdown: %s", user.HTML)
	}
}

// TestMemoryPanelFolded: the forgetting panel is folded when its key memory is
// far from fading, and memory.js knows how to unfold it.
func TestMemoryPanelFolded(t *testing.T) {
	srv := newSeanceServer(t, llm.Limits{})
	created := createSeance(t, srv, crashNS, lostPod)
	html := get(t, srv, created.PageURL).Body.String()
	start := strings.Index(html, `<details id="memory-panel"`)
	if start < 0 {
		t.Fatal("no memory panel")
	}
	tag := html[start : start+strings.Index(html[start:], ">")+1]
	if !strings.Contains(tag, "data-memory-autoopen") || strings.Contains(tag, " open") {
		t.Errorf("memory panel tag = %s, want folded with data-memory-autoopen", tag)
	}
	if !strings.Contains(html, `<summary><h2>遗忘倒计时</h2> <span class="memory-count`) {
		t.Error("folded memory panel does not show the countdown in its summary")
	}
	js := get(t, srv, "/static/memory.js").Body.String()
	for _, want := range []string{"data-memory-autoopen", "scope.open = true", "data-memory-opened"} {
		if !strings.Contains(js, want) {
			t.Errorf("memory.js does not contain %q", want)
		}
	}
}

func TestListTagline(t *testing.T) {
	for _, lang := range []string{llm.LangZH, llm.LangEN} {
		srv := newTestServer(t, Options{Fixtures: NewFixtureSource(), FixturesOnly: true, Lang: lang})
		html := get(t, srv, "/spirits?src=fixtures").Body.String()
		if want := `<p class="tagline">` + tr(lang, "list_tagline") + `</p>`; !strings.Contains(html, want) {
			t.Errorf("%s list page does not contain %q", lang, want)
		}
	}
}

// TestPolishStyles: the board, the folded panel and the phone layout have
// their rules, and the board's motion stops under prefers-reduced-motion.
func TestPolishStyles(t *testing.T) {
	srv := newTestServer(t, Options{Fixtures: NewFixtureSource()})
	css := get(t, srv, "/static/style.css").Body.String()
	for _, want := range []string{
		".board {", ".planchette {", "@keyframes planchette-shake", ".evidence details.ev-lit",
		".turn .text code", ".tagline", ".memory-panel > summary",
		".topbar nav { flex-wrap: wrap;", ".lineage-diff td::before { content: attr(data-label)",
		".planchette.planchette-no { animation: none; }",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("style.css does not contain %q", want)
		}
	}
	// The planchette never takes clicks, so it does not cover the runes.
	i := strings.Index(css, ".planchette {")
	if !strings.Contains(css[i:i+700], "pointer-events: none") {
		t.Error("planchette takes pointer events")
	}
}
