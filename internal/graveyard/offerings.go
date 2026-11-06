package graveyard

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/zhangjuji/k8s-seance/internal/autopsy"
)

// Offering kinds (Requirement 18.1).
const (
	// OfferingDeath is how it died: the cause and the exit code.
	OfferingDeath = "death"
	// OfferingLastWords is the first key log line, or the last line when no line looks like an error.
	OfferingLastWords = "last_words"
	// OfferingChange is the one change between the two lives when a release broke it.
	OfferingChange = "change"
	// OfferingLimit is the memory limit of an OOMKilled container.
	OfferingLimit = "limit"
	// OfferingImage is the image that could not be pulled.
	OfferingImage = "image"
	// OfferingWarning is the first Warning event.
	OfferingWarning = "warning"
)

// MaxOfferings is how many offerings a tombstone keeps.
const MaxOfferings = 3

// maxOfferingRunes caps the text of one offering.
const maxOfferingRunes = 200

// Offering is one key piece of evidence placed on the altar with the
// postmortem. Text comes from the redacted evidence, and Evidence is the
// evidence ID it had in the séance, such as "E3". Evidence is empty when the
// item had been dropped from the evidence by the size cap.
type Offering struct {
	Kind     string `json:"kind"`
	Text     string `json:"text"`
	Evidence string `json:"evidence,omitempty"`
}

// logPrefix is the prefix the evidence builder puts before a log line.
var logPrefix = regexp.MustCompile(`^container \S+ (?:current|previous) log: `)

// OfferingsFor picks at most MaxOfferings offerings for a tombstone, in this
// order: how it died, its last words, the change between two lives when the
// release broke it, then by cause the memory limit (OOMKilled), the image that
// could not be pulled (ImagePullBackOff) or the first Warning event. ev is the
// séance evidence, already redacted; every text is redacted again.
func OfferingsFor(a *autopsy.PodAutopsy, cls autopsy.Classification, ev []autopsy.Evidence) []Offering {
	if a == nil {
		return nil
	}
	var out []Offering
	add := func(o Offering, ok bool) {
		if !ok || len(out) >= MaxOfferings || o.Text == "" {
			return
		}
		for _, have := range out {
			if have.Kind == o.Kind || (o.Evidence != "" && have.Evidence == o.Evidence) {
				return
			}
		}
		o.Text = clip(autopsy.Redact(o.Text))
		out = append(out, o)
	}
	dead := dyingContainer(a, cls)
	add(deathOffering(cls, dead, ev))
	add(lastWords(ev))
	add(changeOffering(a.Lineage, ev))
	switch cls.Cause {
	case autopsy.CauseOOMKilled:
		add(limitOffering(dead, ev))
	case autopsy.CauseImagePullBackOff:
		add(imageOffering(a, ev))
	}
	add(firstWarning(ev))
	return out
}

// dyingContainer is the container the cause points at: the one with the
// classified exit code or cause, then the one that restarted most. Nil when the
// Pod has no containers.
func dyingContainer(a *autopsy.PodAutopsy, cls autopsy.Classification) *autopsy.ContainerAutopsy {
	matches := func(s *autopsy.ContainerState) bool {
		if s == nil {
			return false
		}
		if cls.LastExitCode != nil && s.ExitCode != nil && *s.ExitCode == *cls.LastExitCode {
			return true
		}
		return cls.Cause != "" && s.Reason == string(cls.Cause)
	}
	for i := range a.Containers {
		c := &a.Containers[i]
		if matches(&c.State) || matches(c.LastState) {
			return c
		}
	}
	var best *autopsy.ContainerAutopsy
	for i := range a.Containers {
		if c := &a.Containers[i]; best == nil || c.RestartCount > best.RestartCount {
			best = c
		}
	}
	return best
}

func deathOffering(cls autopsy.Classification, c *autopsy.ContainerAutopsy, ev []autopsy.Evidence) (Offering, bool) {
	cause := cls.Cause
	if cause == "" {
		cause = autopsy.CauseUnknown
	}
	text := string(cause) + ", no exit code"
	if cls.LastExitCode != nil {
		text = fmt.Sprintf("%s, exit code %d", cause, *cls.LastExitCode)
	}
	o := Offering{Kind: OfferingDeath, Text: text}
	if c != nil {
		o.Evidence = findEvidence(ev, "status", "container "+c.Name+" state:")
		o.Text = "container " + c.Name + ": " + text
	}
	return o, true
}

// lastWords is the first log line that looks like an error, else the last log line.
func lastWords(ev []autopsy.Evidence) (Offering, bool) {
	var last *autopsy.Evidence
	for i := range ev {
		e := &ev[i]
		line, ok := logLine(*e)
		if !ok {
			continue
		}
		if autopsy.ImportantLine(line) {
			return Offering{Kind: OfferingLastWords, Text: line, Evidence: e.ID}, true
		}
		last = e
	}
	if last == nil {
		return Offering{}, false
	}
	line, _ := logLine(*last)
	return Offering{Kind: OfferingLastWords, Text: line, Evidence: last.ID}, true
}

// logLine returns the log line of a log evidence item. "No record" items have none.
func logLine(e autopsy.Evidence) (string, bool) {
	if e.Kind != "log" {
		return "", false
	}
	loc := logPrefix.FindStringIndex(e.Text)
	if loc == nil {
		return "", false
	}
	line := strings.TrimSpace(e.Text[loc[1]:])
	return line, line != ""
}

// changeOffering is the first difference of the living former life when the
// verdict is that the release broke it.
func changeOffering(l *autopsy.Lineage, ev []autopsy.Evidence) (Offering, bool) {
	if l == nil || l.Verdict() != autopsy.VerdictRegression {
		return Offering{}, false
	}
	idx := -1
	for i := range l.Versions {
		v := l.Versions[i]
		if v.Alive() == 0 || len(v.Diffs) == 0 {
			continue
		}
		if idx < 0 || v.Age == autopsy.AgeOlder {
			idx = i // prefer the newest living former life, as a rollback would
		}
	}
	if idx < 0 {
		return Offering{}, false
	}
	o := Offering{Kind: OfferingChange, Text: autopsy.DiffText(l.Versions[idx].Diffs[0])}
	// The lineage evidence is the family item, then one item per version in order.
	n := 0
	for _, e := range ev {
		if e.Kind != "lineage" {
			continue
		}
		if n == idx+1 {
			o.Evidence = e.ID
			break
		}
		n++
	}
	return o, true
}

func limitOffering(c *autopsy.ContainerAutopsy, ev []autopsy.Evidence) (Offering, bool) {
	if c == nil {
		return Offering{}, false
	}
	limit := c.Limits["memory"]
	if limit == "" {
		limit = "none"
	}
	return Offering{
		Kind:     OfferingLimit,
		Text:     "container " + c.Name + " memory limit: " + limit,
		Evidence: findEvidence(ev, "spec", "container "+c.Name+" limits:"),
	}, true
}

// imageOffering is the image of the first container that waits on a failed pull.
func imageOffering(a *autopsy.PodAutopsy, ev []autopsy.Evidence) (Offering, bool) {
	for _, c := range a.Containers {
		if r := c.State.Reason; r == "ImagePullBackOff" || r == "ErrImagePull" || r == "InvalidImageName" {
			return Offering{
				Kind:     OfferingImage,
				Text:     "container " + c.Name + " image: " + c.Image,
				Evidence: findEvidence(ev, "spec", "container "+c.Name+" image:"),
			}, true
		}
	}
	return Offering{}, false
}

func firstWarning(ev []autopsy.Evidence) (Offering, bool) {
	for _, e := range ev {
		if e.Kind == "event" && strings.HasPrefix(e.Text, "event Warning ") {
			return Offering{Kind: OfferingWarning, Text: strings.TrimPrefix(e.Text, "event "), Evidence: e.ID}, true
		}
	}
	return Offering{}, false
}

// findEvidence returns the ID of the first evidence item of kind whose text starts with prefix.
func findEvidence(ev []autopsy.Evidence, kind, prefix string) string {
	for _, e := range ev {
		if e.Kind == kind && strings.HasPrefix(e.Text, prefix) {
			return e.ID
		}
	}
	return ""
}

// clip cuts s to maxOfferingRunes runes.
func clip(s string) string {
	if utf8.RuneCountInString(s) <= maxOfferingRunes {
		return s
	}
	r := []rune(s)
	return string(r[:maxOfferingRunes-1]) + "…"
}
