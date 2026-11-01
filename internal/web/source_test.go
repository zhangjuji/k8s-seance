package web

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/zhangjuji/k8s-seance/internal/autopsy"
	"github.com/zhangjuji/k8s-seance/internal/k8s"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

const srcNamespace = "seance-demo"

func srcRunning() corev1.ContainerState {
	return corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
}

func srcWaiting(reason string) corev1.ContainerState {
	return corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason}}
}

func srcTerminated(reason string, exitCode int32, finished time.Time) corev1.ContainerState {
	return corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
		Reason:     reason,
		ExitCode:   exitCode,
		FinishedAt: metav1.NewTime(finished),
	}}
}

// srcPod builds a Pod in srcNamespace with one spec container per status.
func srcPod(name, uid string, statuses ...corev1.ContainerStatus) *corev1.Pod {
	containers := make([]corev1.Container, 0, len(statuses))
	for _, st := range statuses {
		containers = append(containers, corev1.Container{Name: st.Name, Image: "busybox"})
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: srcNamespace, UID: types.UID(uid)},
		Spec:       corev1.PodSpec{Containers: containers},
		Status: corev1.PodStatus{
			Phase:             corev1.PodRunning,
			ContainerStatuses: statuses,
		},
	}
}

// srcEvent is a Warning event about the Pod named pod with UID podUID.
func srcEvent(name, pod, podUID, reason, message string) *corev1.Event {
	return &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: srcNamespace},
		InvolvedObject: corev1.ObjectReference{
			Kind:      "Pod",
			Name:      pod,
			Namespace: srcNamespace,
			UID:       types.UID(podUID),
		},
		Type:          "Warning",
		Reason:        reason,
		Message:       message,
		Count:         1,
		LastTimestamp: metav1.NewTime(time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)),
	}
}

const backOffRestarting = "Back-off restarting failed container"

// srcSpirits returns the spirit Pods and their events. Each Pod exercises one cause, and
// extra adds ImagePull Pods to change the Pod count.
func srcSpirits(extra int) []runtime.Object {
	finished := time.Date(2026, 11, 1, 9, 59, 0, 0, time.UTC)
	objs := []runtime.Object{
		// OOM: last state OOMKilled.
		srcPod("oom", "uid-oom", corev1.ContainerStatus{
			Name:                 "stress",
			RestartCount:         3,
			State:                srcRunning(),
			LastTerminationState: srcTerminated("OOMKilled", 137, finished),
		}),
		// CrashLoop: waiting CrashLoopBackOff, last state Error with exit 1.
		srcPod("crash", "uid-crash", corev1.ContainerStatus{
			Name:                 "app",
			RestartCount:         4,
			State:                srcWaiting("CrashLoopBackOff"),
			LastTerminationState: srcTerminated("Error", 1, finished),
		}),
		// ImagePull: waiting ImagePullBackOff.
		srcPod("img", "uid-img", corev1.ContainerStatus{
			Name:  "app",
			State: srcWaiting("ImagePullBackOff"),
		}),
		// Exit 137 with no OOM reason: only the OOM event makes it OOMKilled.
		srcPod("sig", "uid-sig", corev1.ContainerStatus{
			Name:  "app",
			State: srcTerminated("Error", 137, finished),
		}),
		srcEvent("ev-oom", "oom", "uid-oom", "BackOff", backOffRestarting),
		srcEvent("ev-crash", "crash", "uid-crash", "BackOff", backOffRestarting),
		srcEvent("ev-img", "img", "uid-img", "Failed", "Failed to pull image \"nope:latest\""),
		srcEvent("ev-sig", "sig", "uid-sig", "OOMKilling", "Memory cgroup out of memory: Killed process 42"),
		// Event for a Pod that is not a spirit. It must not affect any row.
		srcEvent("ev-other", "healthy", "uid-healthy", "Pulled", "Container image already present"),
	}
	for i := 0; i < extra; i++ {
		objs = append(objs, srcPod(fmt.Sprintf("img-extra-%d", i), fmt.Sprintf("uid-extra-%d", i),
			corev1.ContainerStatus{Name: "app", State: srcWaiting("ImagePullBackOff")}))
	}
	return objs
}

// srcCauses maps each row name to its cause.
func srcCauses(rows []autopsy.SpiritSummary) map[string]autopsy.CauseOfDeath {
	out := make(map[string]autopsy.CauseOfDeath, len(rows))
	for _, r := range rows {
		out[r.Name] = r.Cause
	}
	return out
}

// srcActionCounts counts the actions by "verb resource[/subresource]".
func srcActionCounts(actions []k8stesting.Action) map[string]int {
	counts := make(map[string]int)
	for _, a := range actions {
		key := a.GetVerb() + " " + a.GetResource().Resource
		if sub := a.GetSubresource(); sub != "" {
			key += "/" + sub
		}
		counts[key]++
	}
	return counts
}

func TestClusterSourceListMatchesCollectedClassification(t *testing.T) {
	ctx := context.Background()

	want := map[string]autopsy.CauseOfDeath{
		"oom":   autopsy.CauseOOMKilled,
		"crash": autopsy.CauseCrashLoopBackOff,
		"img":   autopsy.CauseImagePullBackOff,
		"sig":   autopsy.CauseOOMKilled,
	}

	// The cause the seance page gets: CollectAutopsy then Classify, one Pod at a time.
	collected := make(map[string]autopsy.CauseOfDeath, len(want))
	for name := range want {
		cs := fake.NewClientset(srcSpirits(0)...)
		a, err := k8s.CollectAutopsy(ctx, cs, srcNamespace, name)
		if err != nil {
			t.Fatalf("CollectAutopsy(%s) error = %v", name, err)
		}
		collected[name] = autopsy.Classify(a).Cause
	}
	if !reflect.DeepEqual(collected, want) {
		t.Fatalf("CollectAutopsy causes = %v, want %v", collected, want)
	}

	for _, extra := range []int{0, 7} {
		t.Run(fmt.Sprintf("extra pods %d", extra), func(t *testing.T) {
			cs := fake.NewClientset(srcSpirits(extra)...)
			src := ClusterSource{Client: cs}

			rows, err := src.List(ctx, srcNamespace)
			if err != nil {
				t.Fatalf("List() error = %v", err)
			}

			got := srcCauses(rows)
			for name, cause := range want {
				if got[name] != cause {
					t.Errorf("List() cause for %s = %q, want %q (CollectAutopsy path)", name, got[name], cause)
				}
				if collected[name] != got[name] {
					t.Errorf("List() cause for %s = %q, differs from CollectAutopsy path %q", name, got[name], collected[name])
				}
			}
			if len(rows) != len(want)+extra {
				t.Errorf("List() rows = %d, want %d", len(rows), len(want)+extra)
			}

			counts := srcActionCounts(cs.Actions())
			wantCounts := map[string]int{"list pods": 1, "list events": 1}
			if !reflect.DeepEqual(counts, wantCounts) {
				t.Errorf("API actions = %v, want %v", counts, wantCounts)
			}
		})
	}
}

func TestClusterSourceListWithoutEventsWhenForbidden(t *testing.T) {
	cs := fake.NewClientset(srcSpirits(0)...)
	cs.PrependReactor("list", "events", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "events"}, "", errors.New("no list events"))
	})
	src := ClusterSource{Client: cs}

	rows, err := src.List(context.Background(), srcNamespace)
	if err != nil {
		t.Fatalf("List() error = %v, want nil when events are forbidden", err)
	}

	got := srcCauses(rows)
	want := map[string]autopsy.CauseOfDeath{
		// These causes come from container states alone.
		"oom":   autopsy.CauseOOMKilled,
		"crash": autopsy.CauseCrashLoopBackOff,
		"img":   autopsy.CauseImagePullBackOff,
		// Without the OOM event, exit 137 alone is not OOM.
		"sig": autopsy.CauseError,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("causes without events = %v, want %v", got, want)
	}

	counts := srcActionCounts(cs.Actions())
	if want := map[string]int{"list pods": 1, "list events": 1}; !reflect.DeepEqual(counts, want) {
		t.Errorf("API actions = %v, want %v", counts, want)
	}
}
