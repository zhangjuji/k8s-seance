package autopsy

import (
	"strings"
	"time"
)

// DefaultEventTTL is how long kube-apiserver keeps an Event after its last
// update (the --event-ttl default). The -event-ttl flag changes it.
const DefaultEventTTL = time.Hour

// FadingEvent is one Event and the moment Kubernetes deletes it.
type FadingEvent struct {
	// Index is the position of the event in PodAutopsy.Events.
	Index    int       `json:"index"`
	Type     string    `json:"type"`
	Reason   string    `json:"reason"`
	Message  string    `json:"message"`
	LastSeen time.Time `json:"lastSeen"`
	FadesAt  time.Time `json:"fadesAt"`
}

// Memory says how long the cluster still remembers a spirit (Requirement 14).
type Memory struct {
	// TTL is the event TTL used, and AsOf the moment the seconds count from.
	TTL        time.Duration `json:"-"`
	TTLSeconds int64         `json:"ttlSeconds"`
	AsOf       time.Time     `json:"asOf"`
	// Key is the Warning event that fades first. Nil means there is no Warning
	// event with a timestamp, which the UI shows as "no event record".
	Key *FadingEvent `json:"key,omitempty"`
	// LeftSeconds counts from AsOf to Key.FadesAt. Zero or less means the key
	// memory is already forgotten. It is 0 when Key is nil.
	LeftSeconds int64 `json:"leftSeconds"`
	// AllFadeAt is when the last event of any type fades, nil when no event has
	// a timestamp. AllLeftSeconds counts from AsOf to it.
	AllFadeAt      *time.Time `json:"allFadeAt,omitempty"`
	AllLeftSeconds int64      `json:"allLeftSeconds"`
}

// EventFadesAt returns when Kubernetes deletes e: its last update plus ttl.
// The last update is LastTimestamp, or FirstTimestamp when that is missing.
// ok is false when the event has neither.
func EventFadesAt(e EventRecord, ttl time.Duration) (time.Time, bool) {
	t := eventTime(e)
	if t == nil {
		return time.Time{}, false
	}
	return t.Add(normalTTL(ttl)), true
}

// Remember computes the memory of a at asOf with the event TTL ttl (zero or
// less means DefaultEventTTL). The key memory is the Warning event that fades
// first: a Warning that stopped repeating, such as a failed image pull, fades
// before the BackOff events that keep being refreshed, so it is the first piece
// of evidence about the cause that is lost. Ties go to the earlier event.
func Remember(a *PodAutopsy, ttl time.Duration, asOf time.Time) Memory {
	ttl = normalTTL(ttl)
	m := Memory{TTL: ttl, TTLSeconds: int64(ttl / time.Second), AsOf: asOf}
	if a == nil {
		return m
	}
	var all *time.Time
	for i, e := range a.Events {
		fades, ok := EventFadesAt(e, ttl)
		if !ok {
			continue
		}
		if all == nil || fades.After(*all) {
			v := fades
			all = &v
		}
		if !strings.EqualFold(e.Type, "Warning") {
			continue
		}
		if m.Key == nil || fades.Before(m.Key.FadesAt) {
			m.Key = &FadingEvent{
				Index:    i,
				Type:     e.Type,
				Reason:   e.Reason,
				Message:  e.Message,
				LastSeen: *eventTime(e),
				FadesAt:  fades,
			}
		}
	}
	if m.Key != nil {
		m.LeftSeconds = secondsUntil(asOf, m.Key.FadesAt)
	}
	if all != nil {
		m.AllFadeAt = all
		m.AllLeftSeconds = secondsUntil(asOf, *all)
	}
	return m
}

// RestartsSoon returns the containers of a that wait in CrashLoopBackOff. The
// kubelet keeps only one previous log per container, so their next restart
// overwrites the log that `kubectl logs --previous` shows now.
func RestartsSoon(a *PodAutopsy) []string {
	if a == nil {
		return nil
	}
	var out []string
	for _, c := range a.Containers {
		if c.State.Kind == "waiting" && c.State.Reason == string(CauseCrashLoopBackOff) {
			out = append(out, c.Name)
		}
	}
	return out
}

// EventEvidenceID returns the ID of the event evidence built from e by
// BuildEvidence, matched by its timestamp and its text (type, reason, message,
// count), or "" when e is not in ev (for example cut by the size limit).
func EventEvidenceID(ev []Evidence, e EventRecord) string {
	text := redact(eventText(e))
	ts := eventTime(e)
	for _, item := range ev {
		if item.Kind != "event" || item.Text != text {
			continue
		}
		if (ts == nil) != (item.Timestamp == nil) || (ts != nil && !ts.Equal(*item.Timestamp)) {
			continue
		}
		return item.ID
	}
	return ""
}

func normalTTL(ttl time.Duration) time.Duration {
	if ttl <= 0 {
		return DefaultEventTTL
	}
	return ttl
}

// secondsUntil is the whole seconds from asOf to t, rounded down, and negative when t has passed.
func secondsUntil(asOf, t time.Time) int64 {
	d := t.Sub(asOf)
	s := int64(d / time.Second)
	if d < 0 && d%time.Second != 0 {
		s--
	}
	return s
}
