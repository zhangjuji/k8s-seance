package web

import (
	"net/http"
	"strings"
	"testing"

	"github.com/zhangjuji/k8s-seance/internal/llm"
)

// TestStyleSheetServed checks that the dark theme file is served as CSS and keeps its
// motion rules: the reduced-motion switch and the keyframes for candle and fog.
func TestStyleSheetServed(t *testing.T) {
	srv := newTestServer(t, Options{Fixtures: NewFixtureSource()})
	rec := get(t, srv, "/static/style.css")
	if rec.Code != http.StatusOK {
		t.Fatalf("style.css: status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/css") {
		t.Errorf("style.css: Content-Type = %q, want text/css", ct)
	}
	css := rec.Body.String()
	for _, want := range []string{"prefers-reduced-motion", "@keyframes", ":root"} {
		if !strings.Contains(css, want) {
			t.Errorf("style.css does not contain %q", want)
		}
	}
}

// TestSeancePageKeepsHooks guards the séance page against template changes that would break
// the stream connection or the postmortem button. Both are hx-* and sse attributes.
func TestSeancePageKeepsHooks(t *testing.T) {
	srv := newSeanceServer(t, llm.Limits{})
	created := createSeance(t, srv, crashNS, crashPod)

	rec := get(t, srv, created.PageURL)
	if rec.Code != http.StatusOK {
		t.Fatalf("seance page: status = %d, want 200", rec.Code)
	}
	html := rec.Body.String()
	for _, want := range []string{
		`sse-connect="` + created.StreamURL + `"`,
		`hx-post="/api/seances/` + created.ID + `/postmortem"`,
		`id="meter"`,
		`id="log"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("seance page does not contain %q", want)
		}
	}
}
