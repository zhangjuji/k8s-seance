package web

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/zhangjuji/k8s-seance/internal/autopsy"
	"github.com/zhangjuji/k8s-seance/internal/k8s"
)

// The summoning bell (Requirement 12): GET /api/spirits/stream keeps the spirit
// list of a cluster namespace up to date and says which spirits just arrived,
// died again, or died together.
const (
	// bellDebounce is how long the stream waits after the last watch signal before it lists again.
	bellDebounce = 700 * time.Millisecond
	// massWindow is how far back the mass grave looks.
	massWindow = 60 * time.Second
	// massMin is how many different Pods with one cause make a mass grave.
	massMin = 2

	eventReady   = "ready"
	eventRows    = "rows"
	eventTrouble = "trouble"
)

// Watcher is implemented by a Source that can report changes as they happen.
// Only the cluster has one. The fixtures do not change, so they have no live stream.
type Watcher interface {
	// Watch sends a value whenever the spirits in namespace may have changed.
	// trouble is called each time the watch breaks. The watch reconnects by
	// itself and sends a value once it is back. The channel closes when ctx ends.
	Watch(ctx context.Context, namespace string, trouble func(error)) <-chan struct{}
}

// Watch implements Watcher with the read-only Pod watch.
func (c ClusterSource) Watch(ctx context.Context, namespace string, trouble func(error)) <-chan struct{} {
	return k8s.WatchPods(ctx, c.Client, namespace, k8s.WatchOptions{OnTrouble: trouble})
}

// liveWatcher returns src as a Watcher when the list page for src gets a live stream.
func liveWatcher(src Source) (Watcher, bool) {
	if src == nil || src.Name() == fixturesSource {
		return nil, false
	}
	w, ok := src.(Watcher)
	return w, ok
}

// bellSpirit names one spirit in a bell event.
type bellSpirit struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Cause     string `json:"cause"`
	Restarts  int32  `json:"restarts"`
	// Text is the notice for this spirit in the stream's language.
	Text string `json:"text"`
}

// bellWorse is a spirit that died again: its restart count went up or its cause changed.
type bellWorse struct {
	bellSpirit
	FromRestarts int32  `json:"fromRestarts"`
	FromCause    string `json:"fromCause"`
}

// bellMass is a mass grave: different Pods that died of one cause within massWindow.
type bellMass struct {
	Cause string   `json:"cause"`
	Pods  []string `json:"pods"`
	Title string   `json:"title"`
	Hint  string   `json:"hint"`
}

// bellReturn is a spirit whose new death is a return to a tombstone on the
// altar (Requirement 18.3). It replaces the arrived or died-again notice.
type bellReturn struct {
	bellSpirit
	// N is the return number on the tombstone, URL its photo.
	N   int    `json:"n"`
	URL string `json:"url"`
	// Self is set when the enshrined Pod itself died again. TombPod is the enshrined Pod.
	Self    bool   `json:"self"`
	TombPod string `json:"tombPod"`
}

// rowsEvent is the data of a rows event.
type rowsEvent struct {
	// HTML is the new table body.
	HTML     string       `json:"html"`
	Count    int          `json:"count"`
	Arrived  []bellSpirit `json:"arrived"`
	Worse    []bellWorse  `json:"worse"`
	Returned []bellReturn `json:"returned"`
	Mass     []bellMass   `json:"mass"`
}

// readyEvent is the data of the ready event.
type readyEvent struct {
	Namespace string `json:"namespace"`
	Text      string `json:"text"`
}

// troubleEvent is the data of a trouble event. The stream stays open.
type troubleEvent struct {
	Text  string `json:"text"`
	Error string `json:"error"`
}

// spiritKey identifies a spirit across lists.
func spiritKey(s autopsy.SpiritSummary) string { return s.Namespace + "/" + s.Name }

// diffSpirits compares the previous and current lists. A spirit missing from
// prev arrived. A spirit in both died again when its restart count went up or
// its cause changed. Results keep the order of cur.
func diffSpirits(prev, cur []autopsy.SpiritSummary) (arrived []autopsy.SpiritSummary, worse []bellWorse) {
	before := make(map[string]autopsy.SpiritSummary, len(prev))
	for _, s := range prev {
		before[spiritKey(s)] = s
	}
	for _, s := range cur {
		old, ok := before[spiritKey(s)]
		switch {
		case !ok:
			arrived = append(arrived, s)
		case s.RestartCount > old.RestartCount || s.Cause != old.Cause:
			worse = append(worse, bellWorse{
				bellSpirit:   bellSpirit{Namespace: s.Namespace, Name: s.Name, Cause: string(s.Cause), Restarts: s.RestartCount},
				FromRestarts: old.RestartCount,
				FromCause:    string(old.Cause),
			})
		}
	}
	return arrived, worse
}

// massGrave remembers the deaths one stream has seen in the last massWindow and
// reports a cause once at least massMin different Pods died of it, and again
// each time that group grows.
type massGrave struct {
	deaths   []death
	reported map[string]int // cause -> group size last reported
}

type death struct {
	at    time.Time
	cause string
	pod   string
}

// add records deaths at now and returns the groups to report, sorted by cause.
func (m *massGrave) add(now time.Time, deaths []death) []bellMass {
	if m.reported == nil {
		m.reported = map[string]int{}
	}
	m.deaths = append(m.deaths, deaths...)
	kept := m.deaths[:0]
	for _, d := range m.deaths {
		if now.Sub(d.at) <= massWindow {
			kept = append(kept, d)
		}
	}
	m.deaths = kept

	groups := map[string][]string{}
	for _, d := range m.deaths {
		if !containsString(groups[d.cause], d.pod) {
			groups[d.cause] = append(groups[d.cause], d.pod)
		}
	}
	for cause, n := range m.reported {
		if len(groups[cause]) < n {
			// The group shrank as old deaths left the window. Let it be reported when it grows again.
			if len(groups[cause]) < massMin {
				delete(m.reported, cause)
			} else {
				m.reported[cause] = len(groups[cause])
			}
		}
	}

	var out []bellMass
	for cause, pods := range groups {
		if len(pods) < massMin || len(pods) <= m.reported[cause] {
			continue
		}
		m.reported[cause] = len(pods)
		p := append([]string(nil), pods...)
		sort.Strings(p)
		out = append(out, bellMass{Cause: cause, Pods: p})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Cause < out[j].Cause })
	return out
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// massHintKey is the catalog key of the first thing to check for a mass grave of cause.
func massHintKey(cause string) string {
	switch autopsy.CauseOfDeath(cause) {
	case autopsy.CauseOOMKilled, autopsy.CauseCrashLoopBackOff, autopsy.CauseImagePullBackOff,
		autopsy.CauseProbeFailure, autopsy.CauseError:
		return "mass_hint_" + strings.ToLower(cause)
	}
	return "mass_hint_unknown"
}

// causeLabel is the cause as shown in notices. An empty cause reads as Unknown.
func causeLabel(c autopsy.CauseOfDeath) string {
	if c == "" {
		return string(autopsy.CauseUnknown)
	}
	return string(c)
}

// bell is the state of one open stream.
type bell struct {
	s     *Server
	src   Source
	ns    string
	lang  string
	now   func() time.Time
	mass  massGrave
	prev  []autopsy.SpiritSummary
	ready bool   // prev holds the baseline
	last  string // rowsSignature of the table body last sent
	sick  bool   // the last list failed or the watch broke
}

// update lists the spirits once and returns the event to send, or "" when
// nothing needs sending. The first successful list is the baseline: it is sent
// as rows without notices, because those spirits died before the page opened.
func (b *bell) update(ctx context.Context) (string, any) {
	spirits, err := b.s.list(ctx, b.src, b.ns)
	if err != nil {
		b.sick = true
		return eventTrouble, troubleEvent{Text: tr(b.lang, "bell_trouble"), Error: trf(b.lang, "read_spirits_failed", err.Error())}
	}
	altar := b.s.visitAltar(spirits)
	html, err := b.s.renderRows(b.lang, b.src.Name(), spirits, altar)
	if err != nil {
		b.s.opts.Logger.Printf("render live rows: %v", err)
		return "", nil
	}
	ev := rowsEvent{HTML: html, Count: len(spirits), Arrived: []bellSpirit{}, Worse: []bellWorse{}, Returned: []bellReturn{}, Mass: []bellMass{}}
	if b.ready {
		arrived, worse := diffSpirits(b.prev, spirits)
		now := b.now()
		var deaths []death
		cur := make(map[string]autopsy.SpiritSummary, len(spirits))
		for _, sp := range spirits {
			cur[spiritKey(sp)] = sp
		}
		// returned takes a spirit whose latest death is a return out of the
		// ordinary notices. Its death still counts for the mass grave.
		returned := func(sp autopsy.SpiritSummary, cause string) bool {
			m, ok := altar.returnOf(sp.Namespace, sp.Name)
			if !ok || sp.LastDeath == nil || !m.DiedAt.Equal(*sp.LastDeath) {
				return false
			}
			r := bellReturn{
				bellSpirit: bellSpirit{Namespace: sp.Namespace, Name: sp.Name, Cause: cause, Restarts: sp.RestartCount},
				N:          m.N, URL: m.URL, Self: m.Self, TombPod: m.TombPod,
			}
			if m.Self {
				r.Text = trf(b.lang, "bell_return_self", sp.Name, m.N, cause)
			} else {
				r.Text = trf(b.lang, "bell_return_family", sp.Name, m.Family, m.TombPod, m.N, cause)
			}
			ev.Returned = append(ev.Returned, r)
			deaths = append(deaths, death{at: now, cause: cause, pod: sp.Name})
			return true
		}
		for _, a := range arrived {
			if returned(a, causeLabel(a.Cause)) {
				continue
			}
			ev.Arrived = append(ev.Arrived, bellSpirit{
				Namespace: a.Namespace, Name: a.Name, Cause: causeLabel(a.Cause), Restarts: a.RestartCount,
				Text: trf(b.lang, "bell_arrived", a.Name, causeLabel(a.Cause), a.RestartCount),
			})
			deaths = append(deaths, death{at: now, cause: causeLabel(a.Cause), pod: a.Name})
		}
		for _, w := range worse {
			w.Cause = causeLabel(autopsy.CauseOfDeath(w.Cause))
			if returned(cur[w.Namespace+"/"+w.Name], w.Cause) {
				continue
			}
			w.FromCause = causeLabel(autopsy.CauseOfDeath(w.FromCause))
			if w.Cause != w.FromCause {
				w.Text = trf(b.lang, "bell_worse_cause", w.Name, w.FromRestarts, w.Restarts, w.FromCause, w.Cause)
			} else {
				w.Text = trf(b.lang, "bell_worse", w.Name, w.FromRestarts, w.Restarts, w.Cause)
			}
			ev.Worse = append(ev.Worse, w)
			deaths = append(deaths, death{at: now, cause: w.Cause, pod: w.Name})
		}
		// Returns follow the order of the list, like the other notices.
		order := make(map[string]int, len(spirits))
		for i, sp := range spirits {
			order[spiritKey(sp)] = i
		}
		sort.SliceStable(ev.Returned, func(i, j int) bool {
			return order[ev.Returned[i].Namespace+"/"+ev.Returned[i].Name] < order[ev.Returned[j].Namespace+"/"+ev.Returned[j].Name]
		})
		for _, m := range b.mass.add(now, deaths) {
			m.Title = trf(b.lang, "mass_title", len(m.Pods), m.Cause)
			m.Hint = tr(b.lang, massHintKey(m.Cause))
			ev.Mass = append(ev.Mass, m)
		}
	}
	quiet := len(ev.Arrived) == 0 && len(ev.Worse) == 0 && len(ev.Returned) == 0 && len(ev.Mass) == 0
	sig := rowsSignature(b.lang, spirits, altar)
	unchanged := b.ready && sig == b.last && !b.sick
	b.prev, b.ready, b.last, b.sick = spirits, true, sig, false
	if quiet && unchanged {
		return "", nil
	}
	return eventRows, ev
}

// rowsView is what the spirit_rows template reads. listPage has the same fields.
type rowsView struct {
	UI         chrome
	SourceName string
	Rows       []spiritRow
}

// renderRows renders the table body for spirits in lang, with the spirits in
// altar marked enshrined.
func (s *Server) renderRows(lang, source string, spirits []autopsy.SpiritSummary, altar *altarVisit) (string, error) {
	var buf bytes.Buffer
	view := rowsView{UI: chrome{Lang: lang}, SourceName: source, Rows: rowsOf(lang, spirits, altar)}
	if err := s.set(lang).list.ExecuteTemplate(&buf, "spirit_rows", view); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// rowsSignature describes the table body with the moment each key memory fades
// instead of the seconds left, which change on every list of a live cluster.
// The browser counts those down by itself, so a list that differs only in them
// is not sent again, while a refreshed or new Warning event is. A spirit put on
// the altar since the last list changes the signature too.
func rowsSignature(lang string, spirits []autopsy.SpiritSummary, altar *altarVisit) string {
	rows := rowsOf(lang, spirits, altar)
	fades := make([]int64, len(spirits))
	for i := range rows {
		rows[i].Memory = countdown{}
		if m := spirits[i].Memory; m != nil && m.Key != nil {
			fades[i] = m.Key.FadesAt.Unix()
		}
	}
	return fmt.Sprintf("%+v %v", rows, fades)
}

// streamURL is the live stream address for a list page.
func streamURL(ns, lang string) string {
	return "/api/spirits/stream?ns=" + ns + "&lang=" + lang
}

// handleSpiritsStream is the summoning bell SSE endpoint: ready, then rows after
// each debounced change, and trouble when a list fails or the watch breaks.
// The stream stays open through troubles. Sources without a Watcher get 404.
func (s *Server) handleSpiritsStream(w http.ResponseWriter, r *http.Request) {
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
	watcher, ok := liveWatcher(src)
	if !ok {
		writeJSON(w, http.StatusNotFound, apiError{Error: "live stream needs a cluster; " + src.Name() + " has none", Source: src.Name()})
		return
	}
	lang := s.requestLang(r)

	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	send := func(name string, v any) bool {
		if err := writeEvent(w, sseEvent{name: name, data: mustJSON(v)}); err != nil {
			return false
		}
		return rc.Flush() == nil
	}

	ctx := r.Context()
	troubles := make(chan error, 1)
	changes := watcher.Watch(ctx, ns, func(err error) {
		select {
		case troubles <- err:
		default:
		}
	})

	b := &bell{s: s, src: src, ns: ns, lang: lang, now: s.now}
	if !send(eventReady, readyEvent{Namespace: ns, Text: tr(lang, "bell_ready")}) {
		return
	}
	if name, v := b.update(ctx); name != "" && !send(name, v) {
		return
	}

	debounce := time.NewTimer(time.Hour)
	debounce.Stop()
	defer debounce.Stop()
	ping := time.NewTicker(pingInterval)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-changes:
			if !ok {
				return
			}
			debounce.Reset(s.bellDebounce)
		case err := <-troubles:
			s.opts.Logger.Printf("spirit watch in %q: %v", ns, err)
			b.sick = true
			if !send(eventTrouble, troubleEvent{Text: tr(lang, "bell_trouble"), Error: err.Error()}) {
				return
			}
		case <-debounce.C:
			if name, v := b.update(ctx); name != "" && !send(name, v) {
				return
			}
		case <-ping.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
			if rc.Flush() != nil {
				return
			}
		}
	}
}
