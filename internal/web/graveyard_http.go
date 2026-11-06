package web

import (
	"bytes"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"time"

	"github.com/zhangjuji/k8s-seance/internal/autopsy"
	"github.com/zhangjuji/k8s-seance/internal/graveyard"
	"github.com/zhangjuji/k8s-seance/internal/postmortem"
	"github.com/zhangjuji/k8s-seance/internal/spirit"
)

// registerGraveyardRoutes adds the graveyard routes to mux (Requirement 9).
func (s *Server) registerGraveyardRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/graveyard", s.handleGraveyardAPI)
	mux.HandleFunc("GET /graveyard", s.handleGraveyardPage)
	mux.HandleFunc("GET /graveyard/{id}", s.handleTombstonePage)
	mux.HandleFunc("GET /graveyard/{id}/postmortem.md", s.handleTombstoneDownload)
}

// buryTombstone stores the postmortem of a as its tombstone and returns the ID.
// A failed write is logged and returns "", so the postmortem is still served.
// The tombstone also keeps the family and the offerings, picked from ev, the
// redacted séance evidence (Requirement 18.1).
func (s *Server) buryTombstone(a *autopsy.PodAutopsy, cls autopsy.Classification, ev []autopsy.Evidence, res postmortem.Result) string {
	t := graveyard.Tombstone{
		PodName:      a.Name,
		Namespace:    a.Namespace,
		UID:          a.UID,
		Cause:        cls.Cause,
		Epitaph:      res.LastWords,
		PostmortemMD: res.Markdown,
		Family:       autopsy.FamilyOf(a),
		Offerings:    graveyard.OfferingsFor(a, cls, ev),
	}
	if !a.CreatedAt.IsZero() {
		t.BornAt = a.CreatedAt.UTC()
	}
	if d, ok := spirit.DeathTime(a); ok {
		t.DiedAt = d.UTC()
	}
	saved, err := s.opts.Graveyard.Bury(t)
	if err != nil {
		s.opts.Logger.Printf("bury %s/%s: %v", a.Namespace, a.Name, err)
		return ""
	}
	return saved.ID
}

// tombstoneItem is one entry of GET /api/graveyard. The postmortem itself is
// left out; PostmortemURL downloads it.
type tombstoneItem struct {
	ID            string               `json:"id"`
	PodName       string               `json:"podName"`
	Namespace     string               `json:"namespace"`
	BornAt        *time.Time           `json:"bornAt,omitempty"`
	DiedAt        *time.Time           `json:"diedAt,omitempty"`
	Cause         autopsy.CauseOfDeath `json:"cause"`
	Epitaph       string               `json:"epitaph"`
	BuriedAt      time.Time            `json:"buriedAt"`
	URL           string               `json:"url"`
	PostmortemURL string               `json:"postmortemUrl"`
	// Family, Offerings and Returns come from Requirement 18. Returns is the
	// return count, ReturnLog the returns themselves, oldest first.
	Family    string               `json:"family,omitempty"`
	Offerings []graveyard.Offering `json:"offerings"`
	Returns   int                  `json:"returns"`
	ReturnLog []graveyard.Return   `json:"returnLog"`
	// RevivedAt is set when a cast brought the spirit back (Requirement 19.4).
	RevivedAt *time.Time `json:"revivedAt,omitempty"`
	RevivedBy string     `json:"revivedBy,omitempty"`
}

// graveyardResponse is the body of GET /api/graveyard.
type graveyardResponse struct {
	Tombstones []tombstoneItem `json:"tombstones"`
}

// handleGraveyardAPI returns every tombstone as JSON, the most recent death first.
func (s *Server) handleGraveyardAPI(w http.ResponseWriter, r *http.Request) {
	if s.opts.Graveyard == nil {
		writeJSON(w, http.StatusNotFound, apiError{Error: tr(s.requestLang(r), "gy_off")})
		return
	}
	all, err := s.opts.Graveyard.List()
	if err != nil {
		s.opts.Logger.Printf("list graveyard: %v", err)
		writeJSON(w, http.StatusInternalServerError, apiError{Error: tr(s.requestLang(r), "gy_read_error")})
		return
	}
	items := make([]tombstoneItem, 0, len(all))
	for _, t := range all {
		items = append(items, tombstoneItem{
			ID:            t.ID,
			PodName:       t.PodName,
			Namespace:     t.Namespace,
			BornAt:        timePtr(t.BornAt),
			DiedAt:        timePtr(t.DiedAt),
			Cause:         t.Cause,
			Epitaph:       t.Epitaph,
			BuriedAt:      t.BuriedAt,
			URL:           tombstoneURL(t.ID),
			PostmortemURL: tombstoneDownloadURL(t.ID),
			Family:        t.Family,
			Offerings:     nonNil(t.Offerings),
			Returns:       len(t.Returns),
			ReturnLog:     nonNil(t.Returns),
			RevivedAt:     timePtr(t.RevivedAt),
			RevivedBy:     t.RevivedBy,
		})
	}
	writeJSON(w, http.StatusOK, graveyardResponse{Tombstones: items})
}

// graveyardPage is the data of templates/graveyard.html: the ofrenda.
type graveyardPage struct {
	UI    chrome
	Title string
	// Tiers are the altar steps, the most recent deaths on top.
	Tiers  []altarTier
	Notice string
}

// stoneView is one tombstone, formatted for display.
type stoneView struct {
	ID         string
	URL        string
	Pod        string
	Namespace  string
	Cause      string
	BadgeClass string
	Born       string
	Died       string
	Epitaph    string
	// Family is "Kind/name" or "".
	Family string
	// Offerings are the key evidence kept with the postmortem (Requirement 18.1).
	Offerings []offeringView
	// Returns is how many times the spirit returned. Candles holds one entry
	// per return, at most maxCandles, for the small candles on its photo.
	// ReturnLog lists the latest returns, newest first (Requirement 18.3).
	Returns   int
	Candles   []struct{}
	ReturnLog []returnView
	// Revived is set when a cast brought the spirit back (Requirement 19.4).
	Revived   bool
	RevivedAt string
	RevivedBy string
}

// maxCandles is the number of return candles drawn on one photo.
const maxCandles = 7

// maxReturnLog is the number of returns listed on the photo page.
const maxReturnLog = 10

// offeringView is one offering. LabelKey is its catalog key.
type offeringView struct {
	Kind     string
	LabelKey string
	Text     string
	Evidence string
}

// returnView is one return, formatted for display.
type returnView struct {
	Pod   string
	Died  string
	Cause string
}

func stoneOf(t graveyard.Tombstone) stoneView {
	v := stoneView{
		ID:         t.ID,
		URL:        tombstoneURL(t.ID),
		Pod:        t.PodName,
		Namespace:  t.Namespace,
		Cause:      string(t.Cause),
		BadgeClass: badgeClass(t.Cause),
		Born:       formatTime(timePtr(t.BornAt)),
		Died:       formatTime(timePtr(t.DiedAt)),
		Epitaph:    t.Epitaph,
		Family:     t.Family,
		Returns:    len(t.Returns),
		Candles:    make([]struct{}, min(len(t.Returns), maxCandles)),
		Revived:    !t.RevivedAt.IsZero(),
		RevivedBy:  t.RevivedBy,
	}
	if v.Revived {
		v.RevivedAt = formatTime(timePtr(t.RevivedAt))
	}
	for _, o := range t.Offerings {
		v.Offerings = append(v.Offerings, offeringView{Kind: o.Kind, LabelKey: "offering_" + o.Kind, Text: o.Text, Evidence: o.Evidence})
	}
	for i := len(t.Returns) - 1; i >= 0 && len(v.ReturnLog) < maxReturnLog; i-- {
		r := t.Returns[i]
		v.ReturnLog = append(v.ReturnLog, returnView{Pod: r.Pod, Died: formatTime(timePtr(r.DiedAt)), Cause: causeLabel(r.Cause)})
	}
	return v
}

// nonNil returns s, or an empty slice for nil, so JSON shows [] instead of null.
func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// handleGraveyardPage renders the ofrenda: the tombstones as framed photos on
// altar tiers. A missing or unreadable graveyard is shown as a notice on a 200 page.
func (s *Server) handleGraveyardPage(w http.ResponseWriter, r *http.Request) {
	lang := s.pageLang(w, r)
	page := graveyardPage{UI: chromeFor(r, lang), Title: tr(lang, "gy_title")}
	if s.opts.Graveyard == nil {
		page.Notice = tr(lang, "gy_off")
	} else if all, err := s.opts.Graveyard.List(); err != nil {
		s.opts.Logger.Printf("list graveyard: %v", err)
		page.Notice = tr(lang, "gy_read_failed")
	} else {
		stones := make([]stoneView, 0, len(all))
		for _, t := range all {
			stones = append(stones, stoneOf(t))
		}
		page.Tiers = altarTiers(stones)
	}
	s.render(w, s.set(lang).graveyard, page)
}

// tombstonePage is the data of templates/tombstone.html.
type tombstonePage struct {
	UI    chrome
	Title string
	Stone stoneView
	PM    postmortemView
}

// handleTombstonePage shows one spirit's photo on its own altar step, in the
// style of the ofrenda, and its postmortem.
func (s *Server) handleTombstonePage(w http.ResponseWriter, r *http.Request) {
	t, ok := s.tombstoneFor(w, r)
	if !ok {
		return
	}
	lang := s.pageLang(w, r)
	s.render(w, s.set(lang).tomb, tombstonePage{
		UI:    chromeFor(r, lang),
		Title: trf(lang, "tomb_title", t.PodName),
		Stone: stoneOf(t),
		PM:    postmortemView{Markdown: t.PostmortemMD, DownloadURL: tombstoneDownloadURL(t.ID)},
	})
}

// handleTombstoneDownload returns the stored postmortem as a .md attachment.
func (s *Server) handleTombstoneDownload(w http.ResponseWriter, r *http.Request) {
	t, ok := s.tombstoneFor(w, r)
	if !ok {
		return
	}
	name := "postmortem-" + fileSafe(t.PodName) + "-" + t.BuriedAt.UTC().Format("20060102") + ".md"
	h := w.Header()
	h.Set("Content-Type", "text/markdown; charset=utf-8")
	h.Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	_, _ = io.WriteString(w, t.PostmortemMD)
}

// tombstoneFor looks up the tombstone named in the path and writes the error
// response itself when there is none.
func (s *Server) tombstoneFor(w http.ResponseWriter, r *http.Request) (graveyard.Tombstone, bool) {
	if s.opts.Graveyard == nil {
		s.fail(w, r, http.StatusNotFound, tr(s.requestLang(r), "gy_off"))
		return graveyard.Tombstone{}, false
	}
	id := r.PathValue("id")
	if !graveyard.ValidID(id) {
		s.fail(w, r, http.StatusNotFound, "no such tombstone")
		return graveyard.Tombstone{}, false
	}
	t, err := s.opts.Graveyard.Get(id)
	switch {
	case errors.Is(err, graveyard.ErrNotFound):
		s.fail(w, r, http.StatusNotFound, "no such tombstone")
		return graveyard.Tombstone{}, false
	case err != nil:
		s.opts.Logger.Printf("read tombstone %s: %v", id, err)
		s.fail(w, r, http.StatusInternalServerError, tr(s.requestLang(r), "gy_read_error"))
		return graveyard.Tombstone{}, false
	}
	return t, true
}

// render executes the layout of set with data and writes it as HTML.
func (s *Server) render(w http.ResponseWriter, set *template.Template, data any) {
	var buf bytes.Buffer
	if err := set.ExecuteTemplate(&buf, "layout", data); err != nil {
		s.opts.Logger.Printf("render page: %v", err)
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(buf.Bytes())
}

// timePtr returns nil for the zero time, so formatTime and JSON show it as unknown.
func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// tombstoneURL is the page of a tombstone, or "" for an empty id.
func tombstoneURL(id string) string {
	if id == "" {
		return ""
	}
	return "/graveyard/" + id
}

func tombstoneDownloadURL(id string) string { return "/graveyard/" + id + "/postmortem.md" }
