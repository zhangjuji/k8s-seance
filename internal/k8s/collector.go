// Package k8s holds the read-only Kubernetes collector.
// It only issues get/list/watch calls. Do not add write-style API calls here.
package k8s

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/zhangjuji/k8s-seance/internal/autopsy"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
)

// logTailLines is the number of trailing log lines kept per container (Req 2.3 default).
const logTailLines = 200

// defaultNamespace is used when the caller passes an empty namespace (Req 1.4).
const defaultNamespace = "default"

// abnormalWaitingReasons are waiting reasons that mark a container as a spirit (Req 1.1).
var abnormalWaitingReasons = map[string]bool{
	"CrashLoopBackOff":           true,
	"ImagePullBackOff":           true,
	"ErrImagePull":               true,
	"CreateContainerConfigError": true,
	"CreateContainerError":       true,
	"RunContainerError":          true,
	"InvalidImageName":           true,
}

// IsSpirit reports whether the Pod matches Req 1.1. Init containers count too.
func IsSpirit(pod *corev1.Pod) bool {
	for _, cs := range allContainerStatuses(pod) {
		if cs.RestartCount > 0 {
			return true
		}
		if cs.LastTerminationState.Terminated != nil {
			return true
		}
		if t := cs.State.Terminated; t != nil && t.ExitCode != 0 {
			return true
		}
		if w := cs.State.Waiting; w != nil && abnormalWaitingReasons[w.Reason] {
			return true
		}
	}
	return false
}

// ListSpirits returns summaries of the Pods in namespace that match IsSpirit.
// An empty namespace means "default".
func ListSpirits(ctx context.Context, cs kubernetes.Interface, namespace string) ([]autopsy.SpiritSummary, error) {
	if namespace == "" {
		namespace = defaultNamespace
	}

	pods, err := cs.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list pods in namespace %q: %w", namespace, err)
	}

	summaries := make([]autopsy.SpiritSummary, 0)
	for i := range pods.Items {
		pod := &pods.Items[i]
		if !IsSpirit(pod) {
			continue
		}
		summaries = append(summaries, summarize(pod))
	}
	// TODO(task 3.1): fill Cause using the classifier in internal/autopsy/classifier.go.
	return summaries, nil
}

// allContainerStatuses returns init container statuses followed by regular ones.
func allContainerStatuses(pod *corev1.Pod) []corev1.ContainerStatus {
	all := make([]corev1.ContainerStatus, 0, len(pod.Status.InitContainerStatuses)+len(pod.Status.ContainerStatuses))
	all = append(all, pod.Status.InitContainerStatuses...)
	return append(all, pod.Status.ContainerStatuses...)
}

// summarize builds the list row for one spirit Pod.
// RestartCount is the sum over all containers, and LastDeath is the latest
// FinishedAt among state and lastState terminated entries.
func summarize(pod *corev1.Pod) autopsy.SpiritSummary {
	s := autopsy.SpiritSummary{
		Namespace: pod.Namespace,
		Name:      pod.Name,
	}

	var latest time.Time
	consider := func(finished metav1.Time) {
		if finished.IsZero() {
			return
		}
		if t := finished.Time; t.After(latest) {
			latest = t
		}
	}

	for _, cs := range allContainerStatuses(pod) {
		s.RestartCount += cs.RestartCount
		if t := cs.State.Terminated; t != nil {
			consider(t.FinishedAt)
		}
		if t := cs.LastTerminationState.Terminated; t != nil {
			consider(t.FinishedAt)
		}
	}

	if !latest.IsZero() {
		s.LastDeath = &latest
	}
	return s
}

// CollectAutopsy reads one Pod, its Events and the log tails of its spec containers.
// Only Pods.Get and Events.List errors are returned. A failed log request leaves the
// matching tail nil instead of failing the whole autopsy (Req 2.5).
func CollectAutopsy(ctx context.Context, cs kubernetes.Interface, namespace, pod string) (*autopsy.PodAutopsy, error) {
	if namespace == "" {
		namespace = defaultNamespace
	}

	p, err := cs.CoreV1().Pods(namespace).Get(ctx, pod, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get pod %s/%s: %w", namespace, pod, err)
	}

	events, err := cs.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{
		FieldSelector: fields.OneTermEqualSelector("involvedObject.name", pod).String(),
	})
	if err != nil {
		return nil, fmt.Errorf("list events for pod %s/%s: %w", namespace, pod, err)
	}

	a := &autopsy.PodAutopsy{
		Namespace:   p.Namespace,
		Name:        p.Name,
		UID:         string(p.UID),
		CreatedAt:   p.CreationTimestamp.Time,
		Phase:       string(p.Status.Phase),
		NodeName:    p.Spec.NodeName,
		Events:      convertEvents(events.Items),
		CollectedAt: time.Now(),
		Source:      "cluster",
	}
	if len(p.OwnerReferences) > 0 {
		a.OwnerKind = p.OwnerReferences[0].Kind
		a.OwnerName = p.OwnerReferences[0].Name
	}

	statuses := make(map[string]corev1.ContainerStatus, len(p.Status.ContainerStatuses))
	for _, st := range p.Status.ContainerStatuses {
		statuses[st.Name] = st
	}

	for _, c := range p.Spec.Containers {
		st := statuses[c.Name] // zero value if the status is missing
		a.Containers = append(a.Containers, autopsy.ContainerAutopsy{
			Name:            c.Name,
			Image:           c.Image,
			RestartCount:    st.RestartCount,
			State:           convertState(st.State),
			LastState:       convertLastState(st.LastTerminationState),
			Limits:          resourceMap(c.Resources.Limits),
			Requests:        resourceMap(c.Resources.Requests),
			LogTail:         fetchLogTail(ctx, cs, namespace, pod, c.Name, false),
			PreviousLogTail: fetchLogTail(ctx, cs, namespace, pod, c.Name, true),
		})
	}

	return a, nil
}

// fetchLogTail returns the last logTailLines lines of a container log.
// It returns nil when the log cannot be read, for example when the container has
// not started yet or has no previous termination (Req 2.5).
func fetchLogTail(ctx context.Context, cs kubernetes.Interface, namespace, pod, container string, previous bool) []string {
	tail := int64(logTailLines)
	stream, err := cs.CoreV1().Pods(namespace).GetLogs(pod, &corev1.PodLogOptions{
		Container: container,
		TailLines: &tail,
		Previous:  previous,
	}).Stream(ctx)
	if err != nil {
		return nil
	}
	defer stream.Close()

	body, err := io.ReadAll(stream)
	if err != nil {
		return nil
	}
	if isLogUnavailable(string(body)) {
		return nil
	}
	return splitLines(string(body))
}

// logUnavailablePrefix is the text kubelet returns in a 200 body when a container's
// log is gone, for example after the previous container was garbage-collected (kind v1.37).
const logUnavailablePrefix = "unable to retrieve container logs for"

// isLogUnavailable reports whether a log body is the API server's placeholder for a
// missing log rather than real container output.
func isLogUnavailable(body string) bool {
	return strings.HasPrefix(strings.TrimSpace(body), logUnavailablePrefix)
}

// splitLines splits log text into lines. A single trailing newline does not create an empty line.
// The result is never nil, so an empty log is distinguishable from an unavailable one.
func splitLines(s string) []string {
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return []string{}
	}
	return strings.Split(s, "\n")
}

// convertState maps a container state to the simplified autopsy view.
// A state with no field set is reported as waiting.
func convertState(s corev1.ContainerState) autopsy.ContainerState {
	switch {
	case s.Running != nil:
		return autopsy.ContainerState{Kind: "running", StartedAt: timePtr(s.Running.StartedAt)}
	case s.Terminated != nil:
		t := s.Terminated
		exitCode := t.ExitCode
		return autopsy.ContainerState{
			Kind:       "terminated",
			Reason:     t.Reason,
			Message:    t.Message,
			ExitCode:   &exitCode,
			StartedAt:  timePtr(t.StartedAt),
			FinishedAt: timePtr(t.FinishedAt),
		}
	case s.Waiting != nil:
		return autopsy.ContainerState{Kind: "waiting", Reason: s.Waiting.Reason, Message: s.Waiting.Message}
	default:
		return autopsy.ContainerState{Kind: "waiting"}
	}
}

// convertLastState returns nil when the container has no last termination state.
func convertLastState(s corev1.ContainerState) *autopsy.ContainerState {
	if s.Running == nil && s.Terminated == nil && s.Waiting == nil {
		return nil
	}
	v := convertState(s)
	return &v
}

// resourceMap converts a resource list to name -> quantity string. It returns nil for an empty list.
func resourceMap(list corev1.ResourceList) map[string]string {
	if len(list) == 0 {
		return nil
	}
	m := make(map[string]string, len(list))
	for name, q := range list {
		m[string(name)] = q.String()
	}
	return m
}

// timePtr returns nil for a zero time.
func timePtr(t metav1.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	v := t.Time
	return &v
}

// eventLastTime returns LastTimestamp, falling back to EventTime and then FirstTimestamp.
func eventLastTime(e *corev1.Event) metav1.Time {
	if !e.LastTimestamp.IsZero() {
		return e.LastTimestamp
	}
	if !e.EventTime.IsZero() {
		return metav1.NewTime(e.EventTime.Time)
	}
	return e.FirstTimestamp
}

// convertEvents turns Events into records sorted by their effective last time, oldest first.
func convertEvents(items []corev1.Event) []autopsy.EventRecord {
	sort.SliceStable(items, func(i, j int) bool {
		a := eventLastTime(&items[i])
		b := eventLastTime(&items[j])
		return a.Time.Before(b.Time)
	})

	records := make([]autopsy.EventRecord, 0, len(items))
	for i := range items {
		e := &items[i]
		last := eventLastTime(e)
		records = append(records, autopsy.EventRecord{
			Type:           e.Type,
			Reason:         e.Reason,
			Message:        e.Message,
			Count:          e.Count,
			FirstTimestamp: timePtr(e.FirstTimestamp),
			LastTimestamp:  timePtr(last),
		})
	}
	return records
}
