package web

import (
	"math"
	"regexp"
	"strings"

	"github.com/zhangjuji/k8s-seance/internal/autopsy"
)

// The spirit board of the séance page (Requirement 17). Its runes are the
// evidence IDs of the séance. The server places them on one or two arcs, as
// percentages of the board, so the page needs no canvas and no images. The
// script in seance.html moves the planchette.

// boardArcMax is how many runes fit on one arc. More runes use two arcs.
const boardArcMax = 10

// boardRune is one rune: an evidence ID at Left/Top percent of the board.
type boardRune struct {
	ID   string
	Kind string
	Left float64
	Top  float64
}

// spiritBoard is the data of the board on the séance page.
type spiritBoard struct {
	Runes  []boardRune
	Digits []string
}

// boardDigits is the number row, in the order of a classic talking board.
var boardDigits = []string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "0"}

// buildBoard places the runes of the evidence ev.
func buildBoard(ev []autopsy.Evidence) spiritBoard {
	ids := make([]string, 0, len(ev))
	kinds := make(map[string]string, len(ev))
	for _, e := range ev {
		ids = append(ids, e.ID)
		kinds[e.ID] = e.Kind
	}
	runes := placeRunes(ids)
	for i := range runes {
		runes[i].Kind = kinds[runes[i].ID]
	}
	return spiritBoard{Runes: runes, Digits: boardDigits}
}

// boardArc is one arc of runes: an upper half-ellipse around (cx, cy).
type boardArc struct{ cx, cy, rx, ry float64 }

var (
	// arcSingle holds up to boardArcMax runes.
	arcSingle = boardArc{cx: 50, cy: 70, rx: 38, ry: 40}
	// arcOuter and arcInner hold the runes when there are more.
	arcOuter = boardArc{cx: 50, cy: 70, rx: 40, ry: 42}
	arcInner = boardArc{cx: 50, cy: 70, rx: 26, ry: 24}
)

// Arc ends, in degrees: from the left end (160°) to the right end (20°).
const (
	arcFrom = 160.0
	arcTo   = 20.0
)

// placeRunes puts ids on one arc when there are at most boardArcMax, and on two
// arcs otherwise. The outer arc takes the first half (rounded up), the inner
// arc the rest. Each arc runs left to right. Positions are rounded to 0.1 %.
func placeRunes(ids []string) []boardRune {
	if len(ids) == 0 {
		return nil
	}
	out := make([]boardRune, 0, len(ids))
	if len(ids) <= boardArcMax {
		return append(out, arcSingle.place(ids)...)
	}
	outer := (len(ids) + 1) / 2
	out = append(out, arcOuter.place(ids[:outer])...)
	return append(out, arcInner.place(ids[outer:])...)
}

// place spreads ids evenly over the arc. A single rune sits at the top.
func (a boardArc) place(ids []string) []boardRune {
	out := make([]boardRune, 0, len(ids))
	for i, id := range ids {
		deg := 90.0
		if len(ids) > 1 {
			deg = arcFrom - (arcFrom-arcTo)*float64(i)/float64(len(ids)-1)
		}
		rad := deg * math.Pi / 180
		out = append(out, boardRune{
			ID:   id,
			Left: round1(a.cx + a.rx*math.Cos(rad)),
			Top:  round1(a.cy - a.ry*math.Sin(rad)),
		})
	}
	return out
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }

// Board verdicts of a finished turn, rendered as data-verdict on the turn.
const (
	verdictYes = "yes"
	verdictNo  = "no"
)

// turnVerdict is where the planchette goes when a turn ends: "no" when the fact
// check found a contradiction, "yes" otherwise. User turns and the exhausted
// line are not answers, so they get "".
func turnVerdict(role string, exhausted bool, doubts []doubtItem) string {
	if role != roleSpirit || exhausted {
		return ""
	}
	if len(doubts) > 0 {
		return verdictNo
	}
	return verdictYes
}

var (
	// codeSpan is `code` on one line. boldSpan is **bold** on one line.
	codeSpan = regexp.MustCompile("`([^`\n]+)`")
	boldSpan = regexp.MustCompile(`\*\*([^*\n]+)\*\*`)
)

// markdownLite renders **bold** and `code` in text that is already HTML-escaped.
// The captured text stays escaped, so no tag can come from the reply. inline
// is applied to the text outside code spans (for evidence links); code spans
// are left as they are.
func markdownLite(escaped string, inline func(string) string) string {
	var b strings.Builder
	outside := func(s string) {
		s = boldSpan.ReplaceAllString(s, "<strong>$1</strong>")
		if inline != nil {
			s = inline(s)
		}
		b.WriteString(s)
	}
	last := 0
	for _, m := range codeSpan.FindAllStringSubmatchIndex(escaped, -1) {
		outside(escaped[last:m[0]])
		b.WriteString("<code>")
		b.WriteString(escaped[m[2]:m[3]])
		b.WriteString("</code>")
		last = m[1]
	}
	outside(escaped[last:])
	return b.String()
}
