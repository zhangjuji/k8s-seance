package web

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/zhangjuji/k8s-seance/internal/llm"
	"github.com/zhangjuji/k8s-seance/internal/resurrect"
)

const (
	oomPod  = "memory-glutton-7cc6df9966-bf87c"
	pullPod = "lost-soul-5ff5cb8464-hjtms"
)

// countingProvider counts LLM calls and answers like the mock.
type countingProvider struct{ calls *atomic.Int32 }

func (p countingProvider) Stream(ctx context.Context, req llm.ChatRequest, onDelta func(string) error) (llm.Usage, error) {
	p.calls.Add(1)
	return llm.MockProvider{}.Stream(ctx, req, onDelta)
}

func newSpellServer(t *testing.T) (*Server, *atomic.Int32) {
	t.Helper()
	calls := &atomic.Int32{}
	return newTestServer(t, Options{
		Fixtures:     NewFixtureSource(),
		FixturesOnly: true,
		Provider:     countingProvider{calls: calls},
	}), calls
}

func spellJSON(t *testing.T, srv *Server, id, lang string) spellReply {
	t.Helper()
	rec := getWith(t, srv, "/api/seances/"+id+"/spell?lang="+lang, map[string]string{"Accept": "application/json"})
	if rec.Code != http.StatusOK {
		t.Fatalf("spell: status = %d, body %s", rec.Code, rec.Body)
	}
	var sp spellReply
	decodeJSON(t, rec.Body.Bytes(), &sp)
	return sp
}

func budgetUsed(t *testing.T, srv *Server, id string) int {
	t.Helper()
	rec := getWith(t, srv, "/api/seances/"+id, map[string]string{"Accept": "application/json"})
	var d seanceDetail
	decodeJSON(t, rec.Body.Bytes(), &d)
	return d.BudgetUsed
}

func TestSpellJSONPerFixtureWithoutLLM(t *testing.T) {
	srv, calls := newSpellServer(t)
	for pod, want := range map[string]string{
		oomPod:   resurrect.RemedyRaiseMemory,
		crashPod: resurrect.RemedyMountConfig,
		pullPod:  resurrect.RemedySetImage,
	} {
		created := createSeance(t, srv, crashNS, pod)
		usedBefore, callsBefore := budgetUsed(t, srv, created.ID), calls.Load()
		sp := spellJSON(t, srv, created.ID, "en")
		if sp.Remedy != want || sp.Pod != pod || sp.Lang != "en" {
			t.Errorf("%s: remedy %q pod %q lang %q, want %q", pod, sp.Remedy, sp.Pod, sp.Lang, want)
		}
		if (sp.Patch != "") != (sp.PatchURL != "") {
			t.Errorf("%s: patch %q but patchUrl %q", pod, sp.Patch, sp.PatchURL)
		}
		if sp.Patch != "" && sp.PatchURL != "/api/seances/"+created.ID+"/spell.yaml" {
			t.Errorf("%s: patchUrl = %q", pod, sp.PatchURL)
		}
		if got := budgetUsed(t, srv, created.ID); got != usedBefore {
			t.Errorf("%s: the spell used the séance budget: %d -> %d", pod, usedBefore, got)
		}
		if calls.Load() != callsBefore {
			t.Errorf("%s: the spell called the LLM", pod)
		}
	}
}

func TestSpellPanelFragment(t *testing.T) {
	srv, _ := newSpellServer(t)
	created := createSeance(t, srv, crashNS, crashPod)
	rec := getWith(t, srv, "/api/seances/"+created.ID+"/spell?lang=zh", map[string]string{"HX-Request": "true"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q", ct)
	}
	html := rec.Body.String()
	for _, want := range []string{
		`id="spell-panel"`,
		"只读 · 查看", "补丁文件", "施法 · 会写集群", "只读 · 确认",
		`class="ev-link" href="#ev-E1"`,
		`href="/api/seances/` + created.ID + `/spell.yaml"`,
		`download="resurrect-` + crashPod + `.yaml"`,
		`class="spell-copy"`,
		"--dry-run=server",
		"restless-spirit-config",
		`&lt;&lt;&#39;EOF&#39;`, // the heredoc is escaped, not raw HTML
		"推断",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("fragment does not contain %q", want)
		}
	}
	if strings.Contains(html, "<<'EOF'") {
		t.Error("fragment has an unescaped command")
	}
}

func TestSpellPanelEnglish(t *testing.T) {
	srv, _ := newSpellServer(t)
	created := createSeance(t, srv, crashNS, oomPod)
	rec := getWith(t, srv, "/api/seances/"+created.ID+"/spell", map[string]string{"HX-Request": "true", "Accept-Language": "en"})
	html := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(html, "Cast · writes to the cluster") || !strings.Contains(html, "128Mi") {
		t.Fatalf("status %d, body %s", rec.Code, html)
	}
	if hanOutside(html) {
		t.Errorf("English spell panel has Chinese: %s", html)
	}
}

func TestSpellPatchDownload(t *testing.T) {
	srv, _ := newSpellServer(t)
	created := createSeance(t, srv, crashNS, oomPod)
	sp := spellJSON(t, srv, created.ID, "en")
	rec := get(t, srv, sp.PatchURL)
	if rec.Code != http.StatusOK {
		t.Fatalf("download: status = %d", rec.Code)
	}
	if cd := rec.Header().Get("Content-Disposition"); cd != `attachment; filename="resurrect-`+oomPod+`.yaml"` {
		t.Errorf("Content-Disposition = %q", cd)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/yaml") {
		t.Errorf("Content-Type = %q", ct)
	}
	if rec.Body.String() != sp.Patch {
		t.Errorf("download differs from the spell patch:\n%s\n---\n%s", rec.Body, sp.Patch)
	}
	if !strings.Contains(strings.Join(sp.Steps[2].Commands, "\n"), "--patch-file="+sp.PatchFile) {
		t.Errorf("cast step does not use the downloaded file name %s", sp.PatchFile)
	}

	// The image spell has no patch file.
	pull := createSeance(t, srv, crashNS, pullPod)
	if rec := get(t, srv, "/api/seances/"+pull.ID+"/spell.yaml"); rec.Code != http.StatusNotFound {
		t.Errorf("image spell download: status = %d, want 404", rec.Code)
	}
}

func TestSpellPageButtonAndUnknownSeance(t *testing.T) {
	srv, _ := newSpellServer(t)
	created := createSeance(t, srv, crashNS, crashPod)
	page := get(t, srv, created.PageURL).Body.String()
	if !strings.Contains(page, `hx-get="/api/seances/`+created.ID+`/spell"`) || !strings.Contains(page, `id="spell"`) {
		t.Errorf("séance page has no spell button or panel")
	}
	for _, path := range []string{"/api/seances/nope/spell", "/api/seances/nope/spell.yaml"} {
		if rec := get(t, srv, path); rec.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", path, rec.Code)
		}
	}
}
