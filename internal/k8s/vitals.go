package k8s

import (
	"context"
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Vitals states (Requirement 19.4).
const (
	// VitalsRevived: a Pod of the family is running and ready for the hold
	// time, and no Pod is failing.
	VitalsRevived = "revived"
	// VitalsDying: a Pod created after the cast is failing again.
	VitalsDying = "dying"
	// VitalsWaiting: neither yet. A failing Pod from before the cast keeps the
	// state here until the controller replaces it.
	VitalsWaiting = "waiting"
)

// DefaultReadyHold is how long a Pod must stay ready to count as revived.
const DefaultReadyHold = 20 * time.Second

// failingReasons are the container waiting reasons that mean the Pod is dying.
var failingReasons = map[string]bool{
	"CrashLoopBackOff":           true,
	"ImagePullBackOff":           true,
	"ErrImagePull":               true,
	"InvalidImageName":           true,
	"CreateContainerConfigError": true,
	"CreateContainerError":       true,
	"RunContainerError":          true,
}

// VitalPod is one family member as the vitals check saw it.
type VitalPod struct {
	Name string `json:"name"`
	// New is true for a Pod created at or after the cast.
	New   bool `json:"new"`
	Ready bool `json:"ready"`
	// ReadySeconds is how long the Pod has been ready.
	ReadySeconds int `json:"readySeconds"`
	// Failing is the reason the Pod counts as failing, or "".
	Failing  string `json:"failing,omitempty"`
	Restarts int32  `json:"restarts"`
}

// VitalsReport is the result of one vitals check.
type VitalsReport struct {
	State     string     `json:"state"`
	Family    string     `json:"family"`
	Since     time.Time  `json:"since"`
	CheckedAt time.Time  `json:"checkedAt"`
	Pods      []VitalPod `json:"pods"`
}

// Vitals checks, with one Pods list, whether the family kind/name in ns came
// back after a cast at since (Requirement 19.4). kind is Deployment,
// StatefulSet or DaemonSet. A Pod is ready when it is Running, not being
// deleted and its Ready condition has been true for hold. A Pod created before
// since that is still failing keeps the state at waiting; a Pod created after
// since that fails, or restarts, makes it dying.
func Vitals(ctx context.Context, cs kubernetes.Interface, ns, kind, name string, since time.Time, hold time.Duration) (VitalsReport, error) {
	list, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return VitalsReport{}, fmt.Errorf("list pods in namespace %q for vitals: %w", ns, err)
	}
	return vitalsOf(list.Items, kind, name, since, hold, time.Now()), nil
}

// vitalsOf judges the family among pods at now.
func vitalsOf(pods []corev1.Pod, kind, name string, since time.Time, hold time.Duration, now time.Time) VitalsReport {
	if hold <= 0 {
		hold = DefaultReadyHold
	}
	rep := VitalsReport{Family: kind + "/" + name, Since: since.UTC(), CheckedAt: now.UTC(), Pods: []VitalPod{}}
	revivedOne, oldFailing, newFailing := false, false, false
	for i := range pods {
		p := &pods[i]
		f, ok := familyOf(p)
		if !ok || f.kind != kind || f.name != name || p.DeletionTimestamp != nil {
			continue
		}
		v := VitalPod{Name: p.Name, New: !p.CreationTimestamp.Time.Before(since.Truncate(time.Second))}
		for _, c := range p.Status.ContainerStatuses {
			v.Restarts += c.RestartCount
		}
		v.Failing = failing(p, since)
		if v.New && v.Failing == "" && v.Restarts > 0 {
			v.Failing = "Restarted"
		}
		if at, ok := readySince(p); ok && v.Failing == "" {
			v.Ready = true
			v.ReadySeconds = int(now.Sub(at) / time.Second)
			if now.Sub(at) >= hold {
				revivedOne = true
			}
		}
		switch {
		case v.Failing != "" && v.New:
			newFailing = true
		case v.Failing != "":
			oldFailing = true
		}
		rep.Pods = append(rep.Pods, v)
	}
	sort.Slice(rep.Pods, func(i, j int) bool { return rep.Pods[i].Name < rep.Pods[j].Name })
	switch {
	case newFailing:
		rep.State = VitalsDying
	case revivedOne && !oldFailing:
		rep.State = VitalsRevived
	default:
		rep.State = VitalsWaiting
	}
	return rep
}

// failing returns why p is failing, or "". A container waiting in a failing
// reason, a terminated container with a non-zero exit, a Failed phase, or a
// container that terminated after since all count.
func failing(p *corev1.Pod, since time.Time) string {
	if p.Status.Phase == corev1.PodFailed {
		return "Failed"
	}
	for _, c := range p.Status.ContainerStatuses {
		if w := c.State.Waiting; w != nil && failingReasons[w.Reason] {
			return w.Reason
		}
		if t := c.State.Terminated; t != nil && t.ExitCode != 0 {
			return terminatedReason(t)
		}
		if t := c.LastTerminationState.Terminated; t != nil && !t.FinishedAt.Time.Before(since) && t.ExitCode != 0 {
			return terminatedReason(t)
		}
	}
	return ""
}

func terminatedReason(t *corev1.ContainerStateTerminated) string {
	if t.Reason != "" {
		return t.Reason
	}
	return fmt.Sprintf("exitCode=%d", t.ExitCode)
}

// readySince returns when p became ready, if it is Running and Ready now.
func readySince(p *corev1.Pod) (time.Time, bool) {
	if p.Status.Phase != corev1.PodRunning {
		return time.Time{}, false
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.LastTransitionTime.Time, c.Status == corev1.ConditionTrue
		}
	}
	return time.Time{}, false
}
