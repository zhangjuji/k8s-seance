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

// clusterDownError says why the cluster cannot be used. Its text goes to the
// user as is. An empty reason means no cluster connection was configured.
type clusterDownError struct{ reason string }

func (e *clusterDownError) Error() string { return e.message(llm.LangZH) }

// message is the error text in an interface language.
func (e *clusterDownError) message(lang string) string {
	reason := e.reason
	if reason == "" {
		reason = tr(lang, "cluster_no_conf")
	}
	return trf(lang, "cluster_down", reason)
}

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
	// Lang fixes the default interface language (the -lang flag): "zh", "en" or
	// "" (use Accept-Language). ?lang= and the seance_lang cookie still win.
	Lang string
	// EventTTL is the kube-apiserver event TTL for the forgetting countdown
	// (the -event-ttl flag). Zero means autopsy.DefaultEventTTL. It is used by the
	// séance page and by the default fixture source. The cluster source has its own.
	EventTTL time.Duration
}

// pageSet holds the templates of every page for one interface language. Pages
// that each define "content" get their own template set.
type pageSet struct {
	// list renders the spirit list. seance renders the séance page and its turn fragments.
	list   *template.Template
	seance *template.Template
	// graveyard renders the tombstone grid, tomb one tombstone.
	graveyard *template.Template
	tomb      *template.Template
}

// parsePageSet parses the embedded templates with T and Tf bound to lang.
func parsePageSet(lang string) (*pageSet, error) {
	parse := func(files ...string) (*template.Template, error) {
		return template.New("").Funcs(templateFuncs(lang)).ParseFS(templateFS, files...)
	}
	var ps pageSet
	var err error
	if ps.list, err = parse("templates/layout.html", "templates/spirits.html"); err != nil {
		return nil, fmt.Errorf("parse %s templates: %w", lang, err)
	}
	if ps.seance, err = parse("templates/layout.html", "templates/seance.html", "templates/turn.html", "templates/postmortem.html", "templates/spell.html"); err != nil {
		return nil, fmt.Errorf("parse %s séance templates: %w", lang, err)
	}
	if ps.graveyard, err = parse("templates/layout.html", "templates/graveyard.html"); err != nil {
		return nil, fmt.Errorf("parse %s graveyard templates: %w", lang, err)
	}
	if ps.tomb, err = parse("templates/layout.html", "templates/tombstone.html", "templates/postmortem.html"); err != nil {
		return nil, fmt.Errorf("parse %s tombstone templates: %w", lang, err)
	}
	return &ps, nil
}

// Server holds the routes and the parsed page templates.
type Server struct {
	opts Options
	// sets holds one pageSet per interface language.
	sets    map[string]*pageSet
	mux     *http.ServeMux
	seances *seanceStore
	cache   *llm.OpeningCache
	// bellDebounce and now drive the summoning bell stream. Tests shorten and fake them.
	bellDebounce time.Duration
	now          func() time.Time
}

// set returns the templates for an interface language.
func (s *Server) set(lang string) *pageSet { return s.sets[uiLang(lang)] }

// New validates opts, parses the embedded templates and registers the routes.
func New(opts Options) (*Server, error) {
	if opts.DefaultNamespace == "" {
		opts.DefaultNamespace = defaultNamespace
	}
	if len(opts.DefaultNamespace) > 63 || !namespacePattern.MatchString(opts.DefaultNamespace) {
		return nil, fmt.Errorf("default namespace %q: %w", opts.DefaultNamespace, errInvalidNamespace)
	}
	if opts.EventTTL < 0 {
		return nil, fmt.Errorf("event TTL %v: must be positive", opts.EventTTL)
	}
	if opts.EventTTL == 0 {
		opts.EventTTL = autopsy.DefaultEventTTL
	}
	if opts.Fixtures == nil {
		fx := NewFixtureSource()
		fx.EventTTL = opts.EventTTL
		opts.Fixtures = fx
	}
	if opts.Logger == nil {
		opts.Logger = log.New(os.Stderr, "seance: ", log.LstdFlags)
	}
	if opts.Provider == nil {
		opts.Provider = llm.MockProvider{}
	}
	if opts.Lang != "" {
		l := llm.NormalizeLang(opts.Lang)
		if l == "" {
			return nil, fmt.Errorf("default language %q: use zh or en", opts.Lang)
		}
		opts.Lang = l
	}

	sets := make(map[string]*pageSet, len(uiLangs))
	for _, lang := range uiLangs {
		ps, err := parsePageSet(lang)
		if err != nil {
			return nil, err
		}
		sets[lang] = ps
	}
	static, err := fs.Sub(staticFS, "static")
	if err != nil {
		return nil, fmt.Errorf("embed static files: %w", err)
	}

	s := &Server{
		opts:         opts,
		sets:         sets,
		seances:      newSeanceStore(),
		cache:        &llm.OpeningCache{},
		bellDebounce: bellDebounce,
		now:          time.Now,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleRoot)
	mux.HandleFunc("GET /spirits", s.handleSpiritsPage)
	mux.HandleFunc("GET /api/spirits", s.handleSpiritsAPI)
	mux.HandleFunc("GET /api/spirits/stream", s.handleSpiritsStream)
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "ok")
	})
	s.registerSeanceRoutes(mux)
	s.registerSpellRoutes(mux)
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
		var reason string
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
	UI          chrome
	Title       string
	Namespace   string
	SourceName  string
	FixtureMode bool
	// Live is set when the list follows the cluster through the summoning bell
	// stream at StreamURL.
	Live      bool
	StreamURL string
	Rows      []spiritRow
	// Notice is shown above the table when the list failed or the cluster is unavailable.
	Notice string
	// MemoryBanner is set when a spirit's key memory fades in less than 15 minutes.
	MemoryBanner bool
}

// spiritRow is one table row, already formatted for display.
type spiritRow struct {
	Namespace    string
	Name         string
	Cause        string
	BadgeClass   string
	RestartCount int32
	LastDeath    string
	// HasMemory is false when the spirit has no Warning event ("no event record").
	// Memory is the countdown to its first fading Warning event, MemoryReason that
	// event's reason.
	HasMemory    bool
	Memory       countdown
	MemoryReason string
}

// handleSpiritsPage renders the list page. A cluster problem is shown as a notice on a 200 page.
func (s *Server) handleSpiritsPage(w http.ResponseWriter, r *http.Request) {
	ns, err := s.namespace(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	lang := s.pageLang(w, r)
	page := listPage{UI: chromeFor(r, lang), Title: tr(lang, "list_title"), Namespace: ns, SourceName: clusterSource}

	src, err := s.source(r)
	var down *clusterDownError
	switch {
	case errors.Is(err, errUnknownSource):
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	case errors.As(err, &down):
		page.Notice = down.message(lang)
	default:
		page.SourceName = src.Name()
		page.FixtureMode = src.Name() == fixturesSource
		if _, ok := liveWatcher(src); ok {
			page.Live = true
			page.StreamURL = streamURL(ns, lang)
		}
		spirits, lerr := s.list(r.Context(), src, ns)
		if lerr != nil {
			page.Notice = trf(lang, "read_spirits_failed", lerr.Error())
		}
		page.Rows = rowsOf(lang, spirits)
		page.MemoryBanner = anyFading(page.Rows)
	}

	var buf bytes.Buffer
	if err := s.set(lang).list.ExecuteTemplate(&buf, "layout", page); err != nil {
		s.opts.Logger.Printf("render spirits page: %v", err)
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(buf.Bytes())
}

// rowsOf formats summaries for the table in lang.
func rowsOf(lang string, spirits []autopsy.SpiritSummary) []spiritRow {
	rows := make([]spiritRow, 0, len(spirits))
	for _, sp := range spirits {
		row := spiritRow{
			Namespace:    sp.Namespace,
			Name:         sp.Name,
			Cause:        string(sp.Cause),
			BadgeClass:   badgeClass(sp.Cause),
			RestartCount: sp.RestartCount,
			LastDeath:    formatTime(sp.LastDeath),
		}
		if m := sp.Memory; m != nil && m.Key != nil {
			row.HasMemory = true
			row.Memory = newCountdown(lang, m.LeftSeconds)
			row.MemoryReason = m.Key.Reason
		}
		rows = append(rows, row)
	}
	return rows
}

// anyFading reports whether a row's key memory fades in less than 15 minutes.
func anyFading(rows []spiritRow) bool {
	for _, r := range rows {
		if r.HasMemory && memoryFading(r.Memory.Left) {
			return true
		}
	}
	return false
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
