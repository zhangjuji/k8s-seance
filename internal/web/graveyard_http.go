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

// errGraveyardOff is reported when the server runs without a graveyard file.
var errGraveyardOff = errors.New("墓地未开启（-graveyard 为空）")

// registerGraveyardRoutes adds the graveyard routes to mux (Requirement 9).
func (s *Server) registerGraveyardRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/graveyard", s.handleGraveyardAPI)
	mux.HandleFunc("GET /graveyard", s.handleGraveyardPage)
	mux.HandleFunc("GET /graveyard/{id}", s.handleTombstonePage)
	mux.HandleFunc("GET /graveyard/{id}/postmortem.md", s.handleTombstoneDownload)
}

// buryTombstone stores the postmortem of a as its tombstone and returns the ID.
// A failed write is logged and returns "", so the postmortem is still served.
func (s *Server) buryTombstone(a *autopsy.PodAutopsy, cls autopsy.Classification, res postmortem.Result) string {
	t := graveyard.Tombstone{
		PodName:      a.Name,
		Namespace:    a.Namespace,
		UID:          a.UID,
		Cause:        cls.Cause,
		Epitaph:      res.LastWords,
		PostmortemMD: res.Markdown,
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
}

// graveyardResponse is the body of GET /api/graveyard.
type graveyardResponse struct {
	Tombstones []tombstoneItem `json:"tombstones"`
}

// handleGraveyardAPI returns every tombstone as JSON, the most recent death first.
func (s *Server) handleGraveyardAPI(w http.ResponseWriter, r *http.Request) {
	if s.opts.Graveyard == nil {
		writeJSON(w, http.StatusNotFound, apiError{Error: errGraveyardOff.Error()})
		return
	}
	all, err := s.opts.Graveyard.List()
	if err != nil {
		s.opts.Logger.Printf("list graveyard: %v", err)
		writeJSON(w, http.StatusInternalServerError, apiError{Error: "读取墓地失败"})
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
		})
	}
	writeJSON(w, http.StatusOK, graveyardResponse{Tombstones: items})
}

// graveyardPage is the data of templates/graveyard.html.
type graveyardPage struct {
	Title  string
	Stones []stoneView
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
}

func stoneOf(t graveyard.Tombstone) stoneView {
	return stoneView{
		ID:         t.ID,
		URL:        tombstoneURL(t.ID),
		Pod:        t.PodName,
		Namespace:  t.Namespace,
		Cause:      string(t.Cause),
		BadgeClass: badgeClass(t.Cause),
		Born:       formatTime(timePtr(t.BornAt)),
		Died:       formatTime(timePtr(t.DiedAt)),
		Epitaph:    t.Epitaph,
	}
}

// handleGraveyardPage renders the tombstone grid. A missing or unreadable
// graveyard is shown as a notice on a 200 page.
func (s *Server) handleGraveyardPage(w http.ResponseWriter, r *http.Request) {
	page := graveyardPage{Title: "墓地"}
	if s.opts.Graveyard == nil {
		page.Notice = errGraveyardOff.Error()
	} else if all, err := s.opts.Graveyard.List(); err != nil {
		s.opts.Logger.Printf("list graveyard: %v", err)
		page.Notice = "读取墓地失败，请检查墓地文件。"
	} else {
		for _, t := range all {
			page.Stones = append(page.Stones, stoneOf(t))
		}
	}
	s.render(w, s.graveyardPages, page)
}

// tombstonePage is the data of templates/tombstone.html.
type tombstonePage struct {
	Title string
	Stone stoneView
	PM    postmortemView
}

// handleTombstonePage shows one tombstone and its postmortem.
func (s *Server) handleTombstonePage(w http.ResponseWriter, r *http.Request) {
	t, ok := s.tombstoneFor(w, r)
	if !ok {
		return
	}
	s.render(w, s.tombPages, tombstonePage{
		Title: t.PodName + " 之墓",
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
		s.fail(w, r, http.StatusNotFound, errGraveyardOff.Error())
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
		s.fail(w, r, http.StatusInternalServerError, "读取墓地失败")
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
