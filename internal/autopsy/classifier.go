package autopsy

import "strings"

const (
	reasonOOMKilled      = "OOMKilled"
	reasonCrashLoop      = "CrashLoopBackOff"
	reasonImagePull      = "ImagePullBackOff"
	reasonErrImagePull   = "ErrImagePull"
	reasonBackOff        = "BackOff"
	reasonUnhealthy      = "Unhealthy"
	msgBackOffRestarting = "Back-off restarting failed container"
	exitOOM              = 137
)

// Classify derives the cause of death from a collected PodAutopsy.
// It is a pure function: no I/O and no LLM.
//
// Rule order: OOMKilled, ImagePullBackOff, CrashLoopBackOff (primary causes),
// then ProbeFailure (primary if none matched, otherwise secondary), then
// Error, then Unknown.
func Classify(a *PodAutopsy) Classification {
	if a == nil {
		return Classification{Cause: CauseUnknown}
	}

	c := Classification{LastExitCode: lastNonZeroExit(a)}
	probe := hasProbeFailure(a)

	var primary CauseOfDeath
	switch {
	case isOOM(a):
		primary = CauseOOMKilled
	case hasWaitingReason(a, reasonImagePull, reasonErrImagePull):
		primary = CauseImagePullBackOff
	case isCrashLoop(a):
		primary = CauseCrashLoopBackOff
	}

	switch {
	case primary != "":
		c.Cause = primary
		if probe {
			c.Secondary = []CauseOfDeath{CauseProbeFailure}
		}
	case probe:
		c.Cause = CauseProbeFailure
	case hasErrorExit(a):
		c.Cause = CauseError
	default:
		c.Cause = CauseUnknown
	}
	return c
}

// isOOM: an OOMKilled reason on any container state, or exit 137 together
// with an OOM-mentioning event. Exit 137 alone is not OOM.
func isOOM(a *PodAutopsy) bool {
	for _, ct := range a.Containers {
		if isReason(ct.State, reasonOOMKilled) || (ct.LastState != nil && isReason(*ct.LastState, reasonOOMKilled)) {
			return true
		}
	}
	return hasExitCode(a, exitOOM) && hasOOMEvent(a)
}

// isCrashLoop: waiting reason CrashLoopBackOff, or (kind v1.37 shape) a
// restarted container plus a BackOff event saying it is restarting a failed
// container. The image-pull BackOff event ("Back-off pulling image") does not match.
func isCrashLoop(a *PodAutopsy) bool {
	if hasWaitingReason(a, reasonCrashLoop) {
		return true
	}
	restarted := false
	for _, ct := range a.Containers {
		if ct.RestartCount > 0 {
			restarted = true
		}
	}
	if !restarted {
		return false
	}
	for _, ev := range a.Events {
		if ev.Reason == reasonBackOff && strings.HasPrefix(ev.Message, msgBackOffRestarting) {
			return true
		}
	}
	return false
}

func hasErrorExit(a *PodAutopsy) bool {
	for _, ct := range a.Containers {
		if terminatedNonZero(ct.State) || (ct.LastState != nil && terminatedNonZero(*ct.LastState)) {
			return true
		}
	}
	return false
}

func hasProbeFailure(a *PodAutopsy) bool {
	for _, ev := range a.Events {
		if ev.Reason != reasonUnhealthy {
			continue
		}
		msg := strings.ToLower(ev.Message)
		if strings.Contains(msg, "liveness probe") || strings.Contains(msg, "readiness probe") || strings.Contains(msg, "startup probe") {
			return true
		}
	}
	return false
}

func hasOOMEvent(a *PodAutopsy) bool {
	for _, ev := range a.Events {
		if strings.Contains(strings.ToLower(ev.Reason+" "+ev.Message), "oom") {
			return true
		}
	}
	return false
}

func hasExitCode(a *PodAutopsy, code int32) bool {
	for _, ct := range a.Containers {
		if isExit(ct.State, code) || (ct.LastState != nil && isExit(*ct.LastState, code)) {
			return true
		}
	}
	return false
}

func hasWaitingReason(a *PodAutopsy, reasons ...string) bool {
	for _, ct := range a.Containers {
		if ct.State.Kind != "waiting" {
			continue
		}
		for _, r := range reasons {
			if ct.State.Reason == r {
				return true
			}
		}
	}
	return false
}

// lastNonZeroExit returns the first non-zero exit code per container, taking
// state.terminated before lastState.terminated. Nil if there is none.
func lastNonZeroExit(a *PodAutopsy) *int32 {
	for _, ct := range a.Containers {
		if code := nonZeroExit(ct.State); code != nil {
			return code
		}
		if ct.LastState != nil {
			if code := nonZeroExit(*ct.LastState); code != nil {
				return code
			}
		}
	}
	return nil
}

func nonZeroExit(s ContainerState) *int32 {
	if s.Kind == "terminated" && s.ExitCode != nil && *s.ExitCode != 0 {
		return s.ExitCode
	}
	return nil
}

func isReason(s ContainerState, reason string) bool {
	return s.Kind == "terminated" && s.Reason == reason
}

func isExit(s ContainerState, code int32) bool {
	return s.Kind == "terminated" && s.ExitCode != nil && *s.ExitCode == code
}

func terminatedNonZero(s ContainerState) bool {
	return nonZeroExit(s) != nil
}
