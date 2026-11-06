package web

import (
	"bytes"
	"fmt"
	"html/template"
	"io"
	"net/http"

	"github.com/zhangjuji/k8s-seance/internal/resurrect"
)

// registerSpellRoutes adds the resurrection spell routes (Requirement 13). The
// spell comes from the séance's stored autopsy only: no LLM call, no cluster
// call, and no use of the séance budget.
func (s *Server) registerSpellRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/seances/{id}/spell", s.handleSpell)
	mux.HandleFunc("GET /api/seances/{id}/spell.yaml", s.handleSpellPatch)
}

func seanceSpellURL(id string) string      { return "/api/seances/" + id + "/spell" }
func seanceSpellPatchURL(id string) string { return "/api/seances/" + id + "/spell.yaml" }

// spellFor casts the spell of a séance in lang.
func (se *seance) spellFor(lang string) resurrect.Spell {
	return resurrect.Cast(se.sess.Autopsy, se.sess.Class, se.sess.Evidence, lang)
}

// spellReply is the JSON body of GET /api/seances/{id}/spell. PatchURL is set
// when the spell has a patch file.
type spellReply struct {
	resurrect.Spell
	PatchURL string `json:"patchUrl,omitempty"`
}

// spellView is the data of the "spell" template.
type spellView struct {
	Incantation string
	Diagnosis   []template.HTML
	Target      resurrect.Target
	Steps       []spellStepView
	Caveats     []string
	Patch       string
	PatchFile   string
	PatchURL    string
	// Cast is the cast form, or the reason there is none (Requirement 19.1).
	Cast castForm
}

// spellStepView is one step with its label in the interface language.
type spellStepView struct {
	Kind     string
	Label    string
	Title    string
	Commands []string
}

// handleSpell returns the spell as JSON for JSON clients and as the panel
// fragment otherwise. The text is in the interface language of the request.
func (s *Server) handleSpell(w http.ResponseWriter, r *http.Request) {
	se := s.seanceFor(w, r)
	if se == nil {
		return
	}
	lang := s.requestLang(r)
	sp := se.spellFor(lang)
	patchURL := ""
	if sp.Patch != "" {
		patchURL = seanceSpellPatchURL(se.id)
	}
	if wantsJSON(r) {
		writeJSON(w, http.StatusOK, spellReply{Spell: sp, PatchURL: patchURL})
		return
	}
	v := spellView{
		Incantation: sp.Incantation,
		Target:      sp.Target,
		Caveats:     sp.Caveats,
		Patch:       sp.Patch,
		PatchFile:   sp.PatchFile,
		PatchURL:    patchURL,
		Cast:        s.castFormFor(se, sp, lang),
	}
	for _, d := range sp.Diagnosis {
		v.Diagnosis = append(v.Diagnosis, se.linkEvidence(d))
	}
	for _, st := range sp.Steps {
		v.Steps = append(v.Steps, spellStepView{
			Kind:     string(st.Kind),
			Label:    tr(lang, "spell_step_"+string(st.Kind)),
			Title:    st.Title,
			Commands: st.Commands,
		})
	}
	var buf bytes.Buffer
	if err := s.set(lang).seance.ExecuteTemplate(&buf, "spell", v); err != nil {
		s.opts.Logger.Printf("render spell %s: %v", se.id, err)
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(buf.Bytes())
}

// handleSpellPatch downloads the spell's patch as resurrect-<pod>.yaml, the
// name the commands use. A spell without a patch gets a 404.
func (s *Server) handleSpellPatch(w http.ResponseWriter, r *http.Request) {
	se := s.seanceFor(w, r)
	if se == nil {
		return
	}
	sp := se.spellFor(s.requestLang(r))
	if sp.Patch == "" {
		s.fail(w, r, http.StatusNotFound, tr(s.requestLang(r), "spell_no_patch"))
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/yaml; charset=utf-8")
	h.Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", sp.PatchFile))
	h.Set("X-Content-Type-Options", "nosniff")
	_, _ = io.WriteString(w, sp.Patch)
}
