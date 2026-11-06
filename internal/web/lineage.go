package web

import (
	"strings"

	"github.com/zhangjuji/k8s-seance/internal/autopsy"
)

// maxPanelDiffs caps the rows of one difference table on the page.
const maxPanelDiffs = 20

// lineagePanel is the "family and rebirths" panel of the séance page
// (Requirement 16.4). Show is false when the Pod has no controller family.
type lineagePanel struct {
	Show bool
	// None is set when the Pod has a controller but no family record, such as
	// in sample mode or when the Pod list failed.
	None bool

	Owner    string
	Via      string // the ReplicaSet the Deployment was inferred from, or ""
	Verdict  string // autopsy.Verdict* value
	Text     string // verdict sentence in the page language
	FamilyID string // evidence ID of the family item, or ""
	Revision string // "this revision ..." line
	Self     kinView
	Siblings []kinView
	Versions []versionView
}

type kinView struct {
	Name  string
	Node  string
	State string
	Alive bool
}

type versionView struct {
	Revision string
	Age      string // "older" or "newer"
	AgeLabel string
	EvID     string
	Count    string // "alive 2/2"
	Alive    int
	Pods     []kinView
	Diffs    []diffView
	More     int
}

type diffView struct {
	Field, From, To string
	Hidden          bool
}

// buildLineage renders the family panel of a in lang, linked to the evidence ev.
func buildLineage(lang string, a *autopsy.PodAutopsy, ev []autopsy.Evidence) lineagePanel {
	if a == nil {
		return lineagePanel{}
	}
	l := a.Lineage
	if l == nil {
		switch a.OwnerKind {
		case "ReplicaSet", "StatefulSet", "DaemonSet":
			return lineagePanel{Show: true, None: true}
		}
		return lineagePanel{}
	}
	none := tr(lang, "lineage_none_val")
	p := lineagePanel{
		Show:     true,
		Owner:    l.Owner(),
		Verdict:  l.Verdict(),
		FamilyID: lineageEvidenceID(ev, "family "+l.Owner()),
		Revision: trf(lang, "lineage_revision", l.HashLabel, l.Revision, l.AliveSiblings(), len(l.Siblings)+1),
		Self:     kinFor(l.Self, none),
	}
	if l.Inferred {
		p.Via = l.ViaReplicaSet
	}
	for _, k := range l.Siblings {
		p.Siblings = append(p.Siblings, kinFor(k, none))
	}
	switch p.Verdict {
	case autopsy.VerdictInstance:
		var nodes []string
		for _, k := range l.Siblings {
			if k.Alive {
				nodes = appendUnique(nodes, orText(k.Node, none))
			}
		}
		p.Text = trf(lang, "lineage_v_instance", orText(l.Self.Node, none), strings.Join(nodes, ", "))
	default:
		p.Text = tr(lang, "lineage_v_"+p.Verdict)
	}
	for _, v := range l.Versions {
		vv := versionView{
			Revision: l.HashLabel + "=" + v.Revision,
			Age:      v.Age,
			AgeLabel: tr(lang, "lineage_age_"+v.Age),
			EvID:     lineageEvidenceID(ev, "revision "+l.HashLabel+"="+v.Revision+" "),
			Count:    trf(lang, "lineage_alive_count", v.Alive(), len(v.Pods)),
			Alive:    v.Alive(),
		}
		for _, k := range v.Pods {
			vv.Pods = append(vv.Pods, kinFor(k, none))
		}
		for i, d := range v.Diffs {
			if i == maxPanelDiffs {
				vv.More = len(v.Diffs) - maxPanelDiffs
				break
			}
			dv := diffView{Field: d.Field, Hidden: d.Hidden, From: orText(d.From, none), To: orText(d.To, none)}
			if d.Hidden {
				dv.From, dv.To = tr(lang, "lineage_hidden"), tr(lang, "lineage_hidden")
			} else {
				dv.From, dv.To = autopsy.Redact(dv.From), autopsy.Redact(dv.To)
			}
			vv.Diffs = append(vv.Diffs, dv)
		}
		p.Versions = append(p.Versions, vv)
	}
	return p
}

func kinFor(k autopsy.Kin, none string) kinView {
	return kinView{Name: k.Name, Node: orText(k.Node, none), State: k.State, Alive: k.Alive}
}

// lineageEvidenceID returns the ID of the first lineage evidence item whose
// text starts with prefix, or "".
func lineageEvidenceID(ev []autopsy.Evidence, prefix string) string {
	for _, e := range ev {
		if e.Kind == "lineage" && strings.HasPrefix(e.Text, prefix) {
			return e.ID
		}
	}
	return ""
}

func orText(s, none string) string {
	if s == "" {
		return none
	}
	return s
}

func appendUnique(list []string, s string) []string {
	for _, x := range list {
		if x == s {
			return list
		}
	}
	return append(list, s)
}
