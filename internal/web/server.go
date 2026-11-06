package web

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/zhangjuji/k8s-seance/internal/autopsy"
	"github.com/zhangjuji/k8s-seance/internal/graveyard"
	"github.com/zhangjuji/k8s-seance/internal/llm"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static/*
var staticFS embed.FS

const (
	defaultNamespace = "default"
	requestTimeout   = 20 * time.Second
	fixturesAPIHref  = "/api/spirits?src=fixtures"
	fixturesSource   = "fixtures"
	clusterSource    = "cluster"
)

// namespacePattern is the DNS-1123 label rule that Kubernetes applies to namespace names.
var namespacePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// errUnknownSource is returned for a src value other than "", "cluster" or "fixtures".
var errUnknownSource = errors.New("src must be cluster or fixtures")

// errInvalidNamespace is returned for a namespace that is not a DNS label.
var errInvalidNamespace = errors.New("invalid namespace: use lowercase letters, digits and '-', at most 63 characters")

// clusterDownError says why the cluster cannot be used. Its text goes to the user as is.
type clusterDownError struct{ reason string }

func (e *clusterDownError) Error() string { return "连不上集群：" + e.reason }

// Options configures the Web UI.
type Options struct {
	// Cluster is the live cluster source. It may be nil when the cluster is unavailable.
	Cluster Source
	// ClusterErr says why Cluster is nil. The page shows it.
	ClusterErr error
	// Fixtures is the offline source. Nil means NewFixtureSource().
	Fixtures Source
	// DefaultNamespace is used when a request has no ns parameter. Empty means "default".
	DefaultNamespace string
	// FixturesOnly makes every request use Fixtures, whatever src says.
	FixturesOnly bool
	// Provider and Limits are used by the séance routes (task 7.2).
	Provider llm.Provider
	Limits   llm.Limits
	// Model is shown in the UI when it is set.
	Model string
	// Logger receives server-side errors. Nil means stderr.
	Logger *log.Logger
	// Graveyard stores a tombstone for each generated postmortem (Requirement 9).
	// Nil turns the graveyard off.
	Graveyard *graveyard.Store
}

// Server holds the routes and the parsed page templates.
type Server struct {
	opts Options
	// pages renders the list page. seancePages renders the séance page and its turn fragments.
	pages       *template.Template
	seancePages *template.Template
	// graveyardPages renders the graveyard grid, tombPages one tombstone.
	graveyardPages *template.Template
	tombPages      *template.Template
	mux            *http.ServeMux
	seances        *seanceStore
	cache          *llm.OpeningCache
}

// New validates opts, parses the embedded templates and registers the routes.
func New(opts Options) (*Server, error) {
	if opts.DefaultNamespace == "" {
		opts.DefaultNamespace = defaultNamespace
	}
	if len(opts.DefaultNamespace) > 63 || !namespacePattern.MatchString(opts.DefaultNamespace) {
		return nil, fmt.Errorf("default namespace %q: %w", opts.DefaultNamespace, errInvalidNamespace)
	}
	if opts.Fixtures == nil {
		opts.Fixtures = NewFixtureSource()
	}
	if opts.Logger == nil {
		opts.Logger = log.New(os.Stderr, "seance: ", log.LstdFlags)
	}
	if opts.Provider == nil {
		opts.Provider = llm.MockProvider{}
	}

	// The two pages both define "content", so each one gets its own template set.
	pages, err := template.ParseFS(templateFS, "templates/layout.html", "templates/spirits.html")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	seancePages, err := template.ParseFS(templateFS, "templates/layout.html", "templates/seance.html", "templates/turn.html", "templates/postmortem.html")
	if err != nil {
		return nil, fmt.Errorf("parse séance templates: %w", err)
	}
	graveyardPages, err := template.ParseFS(templateFS, "templates/layout.html", "templates/graveyard.html")
	if err != nil {
		return nil, fmt.Errorf("parse graveyard templates: %w", err)
	}
	tombPages, err := template.ParseFS(templateFS, "templates/layout.html", "templates/tombstone.html", "templates/postmortem.html")
	if err != nil {
		return nil, fmt.Errorf("parse tombstone templates: %w", err)
	}
	static, err := fs.Sub(staticFS, "static")
	if err != nil {
		return nil, fmt.Errorf("embed static files: %w", err)
	}

	s := &Server{
		opts:           opts,
		pages:          pages,
		seancePages:    seancePages,
		graveyardPages: graveyardPages,
		tombPages:      tombPages,
		seances:        newSeanceStore(),
		cache:          &llm.OpeningCache{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleRoot)
	mux.HandleFunc("GET /spirits", s.handleSpiritsPage)
	mux.HandleFunc("GET /api/spirits", s.handleSpiritsAPI)
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "ok")
	})
	s.registerSeanceRoutes(mux)
	s.registerGraveyardRoutes(mux)
	s.mux = mux
	return s, nil
}

// Handler returns the HTTP handler with all routes.
func (s *Server) Handler() http.Handler { return s.mux }

// handleRoot redirects the bare root to the spirit list and keeps the query string.
func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	target := "/spirits"
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	http.Redirect(w, r, target, http.StatusFound)
}

// source picks the Source for one request from its src query parameter.
func (s *Server) source(r *http.Request) (Source, error) {
	return s.sourceNamed(r.URL.Query().Get("src"))
}

// sourceNamed picks the Source for a src value: "", "cluster" or "fixtures".
// FixturesOnly and src=fixtures select the fixtures. Otherwise the cluster is used,
// and if it is nil the result is a clusterDownError.
func (s *Server) sourceNamed(name string) (Source, error) {
	switch name {
	case fixturesSource:
		return s.opts.Fixtures, nil
	case "", clusterSource:
	default:
		return nil, errUnknownSource
	}
	if s.opts.FixturesOnly {
		return s.opts.Fixtures, nil
	}
	if s.opts.Cluster == nil {
		reason := "未配置集群连接"
		if s.opts.ClusterErr != nil {
			reason = s.opts.ClusterErr.Error()
		}
		return nil, &clusterDownError{reason: reason}
	}
	return s.opts.Cluster, nil
}

// namespace returns the ns query parameter, or the default namespace when it is absent.
func (s *Server) namespace(r *http.Request) (string, error) {
	ns := strings.TrimSpace(r.URL.Query().Get("ns"))
	if ns == "" {
		return s.opts.DefaultNamespace, nil
	}
	if len(ns) > 63 || !namespacePattern.MatchString(ns) {
		return "", errInvalidNamespace
	}
	return ns, nil
}

// list calls src.List with the request timeout and logs failures.
func (s *Server) list(ctx context.Context, src Source, ns string) ([]autopsy.SpiritSummary, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	spirits, err := src.List(ctx, ns)
	if err != nil {
		s.opts.Logger.Printf("list %s spirits in %q: %v", src.Name(), ns, err)
	}
	return spirits, err
}

// listPage is the data for templates/spirits.html.
type listPage struct {
	Title       string
	Namespace   string
	SourceName  string
	FixtureMode bool
	Rows        []spiritRow
	// Notice is shown above the table when the list failed or the cluster is unavailable.
	Notice string
}

// spiritRow is one table row, already formatted for display.
type spiritRow struct {
	Namespace    string
	Name         string
	Cause        string
	BadgeClass   string
	RestartCount int32
	LastDeath    string
}

// handleSpiritsPage renders the list page. A cluster problem is shown as a notice on a 200 page.
func (s *Server) handleSpiritsPage(w http.ResponseWriter, r *http.Request) {
	ns, err := s.namespace(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	page := listPage{Title: "亡灵列表", Namespace: ns, SourceName: clusterSource}

	src, err := s.source(r)
	var down *clusterDownError
	switch {
	case errors.Is(err, errUnknownSource):
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	case errors.As(err, &down):
		page.Notice = err.Error()
	default:
		page.SourceName = src.Name()
		page.FixtureMode = src.Name() == fixturesSource
		spirits, lerr := s.list(r.Context(), src, ns)
		if lerr != nil {
			page.Notice = "读取亡灵失败：" + lerr.Error()
		}
		page.Rows = rowsOf(spirits)
	}

	var buf bytes.Buffer
	if err := s.pages.ExecuteTemplate(&buf, "layout", page); err != nil {
		s.opts.Logger.Printf("render spirits page: %v", err)
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(buf.Bytes())
}

// rowsOf formats summaries for the table.
func rowsOf(spirits []autopsy.SpiritSummary) []spiritRow {
	rows := make([]spiritRow, 0, len(spirits))
	for _, sp := range spirits {
		rows = append(rows, spiritRow{
			Namespace:    sp.Namespace,
			Name:         sp.Name,
			Cause:        string(sp.Cause),
			BadgeClass:   badgeClass(sp.Cause),
			RestartCount: sp.RestartCount,
			LastDeath:    formatTime(sp.LastDeath),
		})
	}
	return rows
}

// badgeClass maps a cause to its CSS class, for example OOMKilled to badge-oomkilled.
func badgeClass(c autopsy.CauseOfDeath) string {
	if c == "" {
		return "badge-unknown"
	}
	return "badge-" + strings.ToLower(string(c))
}

// formatTime renders a time in UTC, or "—" when it is unknown.
func formatTime(t *time.Time) string {
	if t == nil {
		return "—"
	}
	return t.UTC().Format("2006-01-02 15:04:05") + " UTC"
}

// spiritsResponse is the body of GET /api/spirits.
type spiritsResponse struct {
	Source    string                  `json:"source"`
	Namespace string                  `json:"namespace"`
	Spirits   []autopsy.SpiritSummary `json:"spirits"`
}

// apiError is the body of an API error.
type apiError struct {
	Error    string `json:"error"`
	Source   string `json:"source,omitempty"`
	Fallback string `json:"fallback,omitempty"`
}

// handleSpiritsAPI returns the spirit list as JSON. An unavailable cluster is 503.
func (s *Server) handleSpiritsAPI(w http.ResponseWriter, r *http.Request) {
	ns, err := s.namespace(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, apiError{Error: err.Error()})
		return
	}
	src, err := s.source(r)
	var down *clusterDownError
	switch {
	case errors.Is(err, errUnknownSource):
		writeJSON(w, http.StatusBadRequest, apiError{Error: err.Error()})
		return
	case errors.As(err, &down):
		writeJSON(w, http.StatusServiceUnavailable, apiError{Error: err.Error(), Source: clusterSource, Fallback: fixturesAPIHref})
		return
	}

	spirits, err := s.list(r.Context(), src, ns)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, apiError{
			Error:    "读取亡灵失败：" + err.Error(),
			Source:   src.Name(),
			Fallback: fixturesAPIHref,
		})
		return
	}
	if spirits == nil {
		spirits = []autopsy.SpiritSummary{}
	}
	writeJSON(w, http.StatusOK, spiritsResponse{Source: src.Name(), Namespace: ns, Spirits: spirits})
}

// writeJSON writes v as JSON with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
