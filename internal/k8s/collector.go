// Package k8s holds the read-only Kubernetes collector.
// It only issues get/list/watch calls. Do not add write-style API calls here.
package k8s

import (
	"context"
	"fmt"
	"time"

	"github.com/zhangjuji/k8s-seance/internal/autopsy"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

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
