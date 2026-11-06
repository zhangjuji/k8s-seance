package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"k8s.io/client-go/kubernetes"

	"github.com/zhangjuji/k8s-seance/internal/k8s"
	"github.com/zhangjuji/k8s-seance/internal/resurrect"
	"github.com/zhangjuji/k8s-seance/internal/rite"
)

// Casting (Requirement 19). Off unless the server runs with --allow-resurrect.
const (
	// castMaxBytes bounds the cast request body: the pasted config plus the form.
	castMaxBytes = resurrect.MaxConfigBytes + 16<<10
	// rehearsalTTL is how long a successful rehearsal allows the real cast.
	rehearsalTTL = 10 * time.Minute
	// vitalsWindow is how long the page keeps checking after a cast.
	vitalsWindow = 4 * time.Minute
	// vitalsPoll is the vitals polling interval of the page, in htmx syntax.
	vitalsPoll = "3s"
)

// Cast modes.
const (
	modeDry  = "dry"
	modeCast = "cast"
)

// Resurrect turns on casting. Nil in Options keeps the server read-only.
type Resurrect struct {
	// Client writes through internal/rite and reads the vitals.
	Client kubernetes.Interface
	// Namespaces are the only namespaces a spell may be cast in.
	Namespaces []string
	// ReadyHold is how long a Pod must stay ready to count as revived.
	// Zero means k8s.DefaultReadyHold.
	ReadyHold time.Duration
}

// castState is the casting record of one séance. It is guarded by mu.
type castState struct {
	mu sync.Mutex
	// busy is set while a cast request runs, so two never overlap.
	busy bool
	// rehearsed maps the fingerprint of a bound action to the time of its
	// last successful rehearsal.
	rehearsed map[string]time.Time
	// castAt is the time of the last real cast, castKind its action.
	castAt   time.Time
	castKind resurrect.ActionKind
}

func (s *Server) registerRiteRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/seances/{id}/cast", s.handleCast)
	mux.HandleFunc("GET /api/seances/{id}/vitals", s.handleVitals)
}

func seanceCastURL(id string) string { return "/api/seances/" + id + "/cast" }
func seanceVitalsURL(id string, since time.Time) string {
	return "/api/seances/" + id + "/vitals?since=" + strconv.FormatInt(since.Unix(), 10)
}

// castBlock says why a séance cannot be cast on, as a catalog key, or "".
func (s *Server) castBlock(se *seance, sp resurrect.Spell) string {
	switch {
	case s.opts.Resurrect == nil:
		return "cast_off"
	case se.source != clusterSource:
		return "cast_fixtures"
	case sp.Action == nil:
		return "cast_read_only"
	case !slices.Contains(s.opts.Resurrect.Namespaces, sp.Action.Namespace):
		return "cast_ns"
	}
	return ""
}

// castForm is the cast part of the spell panel.
type castForm struct {
	// Blocked is the reason the form is not shown, in the interface language.
	Blocked   string
	URL       string
	Kind      string
	Target    string
	Namespace string
	Inputs    []castInput
}

type castInput struct {
	Name  string
	Kind  string
	Label string
	Min   int
	Max   int
}

func (s *Server) castFormFor(se *seance, sp resurrect.Spell, lang string) castForm {
	if key := s.castBlock(se, sp); key != "" {
		f := castForm{Blocked: tr(lang, key)}
		if key == "cast_ns" && s.opts.Resurrect != nil {
			f.Blocked = trf(lang, key, strings.Join(s.opts.Resurrect.Namespaces, ", "))
		}
		return f
	}
	a := sp.Action
	f := castForm{
		URL: seanceCastURL(se.id), Kind: tr(lang, "cast_kind_"+string(a.Kind)),
		Target: a.TargetKind + "/" + a.TargetName, Namespace: a.Namespace,
	}
	for _, in := range a.Inputs {
		f.Inputs = append(f.Inputs, castInput{Name: in.Name, Kind: in.Kind, Label: tr(lang, "cast_input_"+in.Name), Min: in.Min, Max: in.Max})
	}
	return f
}

// castRequest is the body of POST /api/seances/{id}/cast.
type castRequest struct {
	Mode    string            `json:"mode"`
	Inputs  map[string]string `json:"inputs"`
	Confirm bool              `json:"confirm"`
}

// castReply is the JSON answer of a cast.
type castReply struct {
	Mode    string      `json:"mode"`
	OK      bool        `json:"ok"`
	Error   string      `json:"error,omitempty"`
	Result  rite.Result `json:"result"`
	Since   int64       `json:"since,omitempty"`
	Vitals  string      `json:"vitalsUrl,omitempty"`
	CanCast bool        `json:"canCast"`
}

// sameOrigin rejects cross-site requests: a page on another site must not be
// able to make this server write to the cluster.
func sameOrigin(r *http.Request) bool {
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" || r.Header.Get("Sec-Fetch-Site") == "same-site" {
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	return err == nil && u.Host == r.Host
}

// handleCast rehearses or casts the spell of a séance. Every check below must
// pass before internal/rite is called; any failure is a 4xx and no request
// reaches the cluster.
func (s *Server) handleCast(w http.ResponseWriter, r *http.Request) {
	lang := s.requestLang(r)
	if s.opts.Resurrect == nil {
		s.castFail(w, r, http.StatusForbidden, tr(lang, "cast_off"))
		return
	}
	if !sameOrigin(r) {
		s.castFail(w, r, http.StatusForbidden, tr(lang, "cast_cross_site"))
		return
	}
	se := s.seanceFor(w, r)
	if se == nil {
		return
	}
	sp := se.spellFor(lang)
	if key := s.castBlock(se, sp); key != "" {
		msg := tr(lang, key)
		if key == "cast_ns" {
			msg = trf(lang, key, strings.Join(s.opts.Resurrect.Namespaces, ", "))
		}
		s.castFail(w, r, http.StatusForbidden, msg)
		return
	}
	req, err := readCastRequest(w, r)
	if err != nil {
		s.castFail(w, r, http.StatusBadRequest, err.Error())
		return
	}
	if req.Mode != modeDry && req.Mode != modeCast {
		s.castFail(w, r, http.StatusBadRequest, tr(lang, "cast_bad_mode"))
		return
	}
	bound, err := sp.Action.Bind(req.Inputs)
	if err != nil {
		var ie *resurrect.InputError
		if errors.As(err, &ie) {
			s.castFail(w, r, http.StatusBadRequest, trf(lang, "cast_err_"+ie.Code, tr(lang, "cast_input_"+ie.Input)))
			return
		}
		s.castFail(w, r, http.StatusBadRequest, err.Error())
		return
	}
	fp := bound.Fingerprint()
	cs := &se.cast
	cs.mu.Lock()
	if cs.busy {
		cs.mu.Unlock()
		s.castFail(w, r, http.StatusConflict, tr(lang, "cast_busy"))
		return
	}
	if req.Mode == modeCast {
		at, ok := cs.rehearsed[fp]
		if !ok || s.now().Sub(at) > rehearsalTTL {
			cs.mu.Unlock()
			s.castFail(w, r, http.StatusConflict, tr(lang, "cast_rehearse_first"))
			return
		}
		if !req.Confirm {
			cs.mu.Unlock()
			s.castFail(w, r, http.StatusBadRequest, tr(lang, "cast_confirm_needed"))
			return
		}
	}
	cs.busy = true
	cs.mu.Unlock()
	defer func() {
		cs.mu.Lock()
		cs.busy = false
		cs.mu.Unlock()
	}()

	caster := &rite.Caster{Client: s.opts.Resurrect.Client, Namespaces: s.opts.Resurrect.Namespaces, Now: s.now}
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	dry := req.Mode == modeDry
	res, cerr := caster.Cast(ctx, bound, dry)
	if cerr != nil {
		s.opts.Logger.Printf("cast %s (%s, dry=%v) on %s/%s: %v", se.id, bound.Kind, dry, bound.Namespace, bound.TargetName, cerr)
	} else {
		s.opts.Logger.Printf("cast %s (%s, dry=%v) on %s/%s: ok", se.id, bound.Kind, dry, bound.Namespace, bound.TargetName)
	}
	reply := castReply{Mode: req.Mode, OK: cerr == nil, Result: res}
	if cerr != nil {
		reply.Error = cerr.Error()
	}
	cs.mu.Lock()
	switch {
	case cerr == nil && dry:
		if cs.rehearsed == nil {
			cs.rehearsed = map[string]time.Time{}
		}
		cs.rehearsed[fp] = s.now()
		reply.CanCast = true
	case cerr == nil:
		delete(cs.rehearsed, fp)
		cs.castAt, cs.castKind = res.At, bound.Kind
		reply.Since = res.At.Unix()
		reply.Vitals = seanceVitalsURL(se.id, res.At)
	}
	cs.mu.Unlock()

	if wantsJSON(r) {
		writeJSON(w, http.StatusOK, reply)
		return
	}
	s.renderFragment(w, lang, "cast_result", castResultView{castReply: reply, DryRun: dry, Target: bound.TargetKind + "/" + bound.TargetName})
}

// castResultView is the data of the "cast_result" template.
type castResultView struct {
	castReply
	DryRun bool
	Target string
}

// castFail answers a refused cast. htmx swaps 4xx answers into the result box.
func (s *Server) castFail(w http.ResponseWriter, r *http.Request, status int, msg string) {
	if wantsJSON(r) || r.Header.Get("HX-Request") == "" {
		s.fail(w, r, status, msg)
		return
	}
	var buf bytes.Buffer
	if err := s.set(s.requestLang(r)).seance.ExecuteTemplate(&buf, "cast_error", msg); err != nil {
		http.Error(w, msg, status)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// readCastRequest reads the cast body from JSON or from a form.
func readCastRequest(w http.ResponseWriter, r *http.Request) (castRequest, error) {
	r.Body = http.MaxBytesReader(w, r.Body, castMaxBytes)
	var req castRequest
	if isJSONRequest(r) {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			return req, fmt.Errorf("invalid JSON body: %w", err)
		}
	} else {
		if err := r.ParseForm(); err != nil {
			return req, fmt.Errorf("invalid form body: %w", err)
		}
		req.Mode = r.PostForm.Get("mode")
		req.Confirm = r.PostForm.Get("confirm") == "yes"
		req.Inputs = map[string]string{}
		for _, name := range []string{resurrect.InputTag, resurrect.InputConfig, resurrect.InputTimeoutSeconds, resurrect.InputFailureThreshold} {
			if v, ok := r.PostForm[name]; ok && len(v) > 0 {
				req.Inputs[name] = v[0]
			}
		}
	}
	req.Mode = strings.TrimSpace(req.Mode)
	return req, nil
}

// vitalsView is the data of the "vitals" template.
type vitalsView struct {
	k8s.VitalsReport
	StateText string
	// Poll is set while the page should check again, at URL.
	Poll bool
	URL  string
	// AltarURL links to the spirit's photo when the revival was marked on the altar.
	AltarURL string
}

// handleVitals checks, read-only, whether the family came back after the cast
// at ?since= (Unix seconds). It needs --allow-resurrect, like the cast.
func (s *Server) handleVitals(w http.ResponseWriter, r *http.Request) {
	lang := s.requestLang(r)
	if s.opts.Resurrect == nil {
		s.castFail(w, r, http.StatusForbidden, tr(lang, "cast_off"))
		return
	}
	se := s.seanceFor(w, r)
	if se == nil {
		return
	}
	sp := se.spellFor(lang)
	if key := s.castBlock(se, sp); key != "" {
		s.castFail(w, r, http.StatusForbidden, tr(lang, key))
		return
	}
	n, err := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	if err != nil || n <= 0 {
		s.castFail(w, r, http.StatusBadRequest, "since must be Unix seconds")
		return
	}
	since := time.Unix(n, 0).UTC()
	now := s.now()
	if since.After(now.Add(time.Minute)) {
		s.castFail(w, r, http.StatusBadRequest, "since is in the future")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	rep, err := k8s.Vitals(ctx, s.opts.Resurrect.Client, sp.Action.Namespace, sp.Action.TargetKind, sp.Action.TargetName, since, s.opts.Resurrect.ReadyHold)
	if err != nil {
		s.opts.Logger.Printf("vitals %s: %v", se.id, err)
		s.castFail(w, r, http.StatusServiceUnavailable, trf(lang, "read_spirits_failed", err.Error()))
		return
	}
	v := vitalsView{VitalsReport: rep, StateText: tr(lang, "vitals_"+rep.State)}
	if rep.State == k8s.VitalsRevived {
		v.AltarURL = s.markRevived(se, since)
	}
	if rep.State == k8s.VitalsWaiting && now.Sub(since) < vitalsWindow {
		v.Poll, v.URL = true, seanceVitalsURL(se.id, since)
	}
	if rep.State == k8s.VitalsWaiting && !v.Poll {
		v.StateText = tr(lang, "vitals_timeout")
	}
	if wantsJSON(r) {
		writeJSON(w, http.StatusOK, v)
		return
	}
	s.renderFragment(w, lang, "vitals", v)
}

// markRevived marks the spirit's photo on the altar as revived, when the
// séance really cast at since and the spirit is enshrined. It returns the
// photo URL, or "".
func (s *Server) markRevived(se *seance, since time.Time) string {
	if s.opts.Graveyard == nil {
		return ""
	}
	se.cast.mu.Lock()
	castAt, kind := se.cast.castAt, se.cast.castKind
	se.cast.mu.Unlock()
	if castAt.IsZero() || castAt.Unix() != since.Unix() {
		return ""
	}
	t, ok, err := s.opts.Graveyard.Revive(se.ns, se.pod, s.now(), string(kind))
	if err != nil {
		s.opts.Logger.Printf("mark %s/%s revived: %v", se.ns, se.pod, err)
		return ""
	}
	if !ok {
		return ""
	}
	return tombstoneURL(t.ID)
}

// renderFragment renders one séance template fragment.
func (s *Server) renderFragment(w http.ResponseWriter, lang, name string, data any) {
	var buf bytes.Buffer
	if err := s.set(lang).seance.ExecuteTemplate(&buf, name, data); err != nil {
		s.opts.Logger.Printf("render %s: %v", name, err)
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(buf.Bytes())
}
