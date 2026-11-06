package web

import (
	"fmt"
	"time"

	"github.com/zhangjuji/k8s-seance/internal/autopsy"
)

// The forgetting countdown (Requirement 14). The server renders the seconds left
// as data-memory-left, and static/memory.js counts them down in the browser.
const (
	// memoryFadingSeconds: below 15 minutes a row fades and the banner shows.
	memoryFadingSeconds = 15 * 60
	// memoryVanishingSeconds: below 5 minutes it flickers.
	memoryVanishingSeconds = 5 * 60
)

// memoryClass is the CSS state for seconds left: "memory-gone" at zero or below,
// "memory-fading memory-vanishing" below 5 minutes, "memory-fading" below 15
// minutes, and "" otherwise. memory.js applies the same rules.
func memoryClass(left int64) string {
	switch {
	case left <= 0:
		return "memory-gone"
	case left < memoryVanishingSeconds:
		return "memory-fading memory-vanishing"
	case left < memoryFadingSeconds:
		return "memory-fading"
	}
	return ""
}

// memoryText is the countdown label, for example "43 分钟后遗忘" or
// "forgotten in 1 h 5 min". memory.js builds the same text from #memory-i18n.
func memoryText(lang string, left int64) string {
	if left <= 0 {
		return tr(lang, "memory_gone")
	}
	return trf(lang, "memory_left", memorySpan(lang, left))
}

// memorySpan formats seconds left in whole minutes, rounded down.
func memorySpan(lang string, left int64) string {
	if left < 60 {
		return tr(lang, "memory_lt1")
	}
	mins := left / 60
	h, m := mins/60, mins%60
	switch {
	case h == 0:
		return fmt.Sprintf("%d %s", m, tr(lang, "memory_min"))
	case m == 0:
		return fmt.Sprintf("%d %s", h, tr(lang, "memory_h"))
	}
	return fmt.Sprintf("%d %s %d %s", h, tr(lang, "memory_h"), m, tr(lang, "memory_min"))
}

// memoryFading reports whether a countdown should raise the list banner: it is
// running and below 15 minutes.
func memoryFading(left int64) bool { return left > 0 && left < memoryFadingSeconds }

// countdown is one rendered countdown.
type countdown struct {
	Left  int64
	Class string
	Text  string
}

func newCountdown(lang string, left int64) countdown {
	return countdown{Left: left, Class: memoryClass(left), Text: memoryText(lang, left)}
}

// memoryPanel is the forgetting panel of the séance page.
type memoryPanel struct {
	// HasKey is false when there is no Warning event: the panel says "no event record".
	HasKey bool
	// KeyID is the evidence ID of the key memory, or "" when it is not in the evidence.
	KeyID      string
	KeyReason  string
	KeyMessage string
	KeyAt      string
	Key        countdown
	// AllAt is when every event is gone, "" when no event has a timestamp.
	AllAt string
	All   countdown
	// RestartsSoon are the containers whose --previous log the next restart overwrites.
	RestartsSoon []string
	// Fixture is set in sample mode, where the countdown starts at AsOf, the recording time.
	Fixture bool
	AsOf    string
}

// evidenceCard is one evidence card. Event cards have their own countdown.
type evidenceCard struct {
	autopsy.Evidence
	HasMemory bool
	Memory    countdown
}

// buildMemory renders the panel and the evidence cards for a séance at now.
func buildMemory(lang string, a *autopsy.PodAutopsy, ev []autopsy.Evidence, ttl time.Duration, now time.Time) (memoryPanel, []evidenceCard) {
	asOf := memoryAsOf(a, now)
	m := autopsy.Remember(a, ttl, asOf)
	p := memoryPanel{
		RestartsSoon: autopsy.RestartsSoon(a),
		Fixture:      a != nil && a.Source == "fixture" && !a.CollectedAt.IsZero(),
		AsOf:         formatTime(&asOf),
	}
	if m.Key != nil {
		p.HasKey = true
		p.KeyID = autopsy.EventEvidenceID(ev, a.Events[m.Key.Index])
		p.KeyReason = m.Key.Reason
		p.KeyMessage = m.Key.Message
		p.KeyAt = formatTime(&m.Key.FadesAt)
		p.Key = newCountdown(lang, m.LeftSeconds)
	}
	if m.AllFadeAt != nil {
		p.AllAt = formatTime(m.AllFadeAt)
		p.All = newCountdown(lang, m.AllLeftSeconds)
	}
	cards := make([]evidenceCard, 0, len(ev))
	for _, e := range ev {
		c := evidenceCard{Evidence: e}
		if e.Kind == "event" && e.Timestamp != nil {
			c.HasMemory = true
			c.Memory = newCountdown(lang, int64(e.Timestamp.Add(m.TTL).Sub(asOf)/time.Second))
		}
		cards = append(cards, c)
	}
	return p, cards
}
