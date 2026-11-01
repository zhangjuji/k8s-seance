package spirit

import (
	"time"

	"github.com/zhangjuji/k8s-seance/internal/autopsy"
)

// deathTimeFormat is the layout of the time of death shown to the user and sent to the model.
const deathTimeFormat = "2006-01-02 15:04:05 UTC"

// terminatedKind is the ContainerState.Kind of a container that has exited.
const terminatedKind = "terminated"

// DeathTime returns when the Pod died. It is the latest FinishedAt of a terminated
// container state (the current state or the last state) across all containers.
// If no container has one, it is the latest LastTimestamp of a Warning event.
// ok is false when neither source has a time, or when a is nil.
func DeathTime(a *autopsy.PodAutopsy) (time.Time, bool) {
	if a == nil {
		return time.Time{}, false
	}

	var latest time.Time
	found := false
	consider := func(t time.Time) {
		if !found || t.After(latest) {
			latest = t
			found = true
		}
	}
	for _, c := range a.Containers {
		if t, ok := finishedAt(c.State); ok {
			consider(t)
		}
		if c.LastState != nil {
			if t, ok := finishedAt(*c.LastState); ok {
				consider(t)
			}
		}
	}
	if found {
		return latest, true
	}

	for _, e := range a.Events {
		if e.Type == "Warning" && e.LastTimestamp != nil {
			consider(*e.LastTimestamp)
		}
	}
	return latest, found
}

// finishedAt returns the finish time of a terminated state.
func finishedAt(s autopsy.ContainerState) (time.Time, bool) {
	if s.Kind != terminatedKind || s.FinishedAt == nil {
		return time.Time{}, false
	}
	return *s.FinishedAt, true
}

// FormatDeathTime renders t in UTC as "2006-01-02 15:04:05 UTC".
func FormatDeathTime(t time.Time) string {
	return t.UTC().Format(deathTimeFormat)
}
