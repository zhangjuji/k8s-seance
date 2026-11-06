package k8s

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

var castAt = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

func vpod(name, hash string, created time.Time, mutate func(p *corev1.Pod)) corev1.Pod {
	yes := true
	p := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "seance-demo", Name: name, CreationTimestamp: metav1.NewTime(created),
			Labels:          map[string]string{labelPodTemplateHash: hash},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "web-" + hash, Controller: &yes}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodPending},
	}
	if mutate != nil {
		mutate(&p)
	}
	return p
}

// readyFor makes p running and ready since at.
func readyFor(at time.Time) func(p *corev1.Pod) {
	return func(p *corev1.Pod) {
		p.Status.Phase = corev1.PodRunning
		p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(at)}}
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", Ready: true,
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(at)}}}}
	}
}

func crashLooping(p *corev1.Pod) {
	p.Status.Phase = corev1.PodRunning
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", RestartCount: 3,
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}}}
}

func TestVitalsStates(t *testing.T) {
	now := castAt.Add(time.Minute)
	before := castAt.Add(-time.Hour)
	after := castAt.Add(5 * time.Second)
	tests := []struct {
		name string
		pods []corev1.Pod
		want string
	}{
		{"nothing yet", nil, VitalsWaiting},
		{"new pod ready for 30s", []corev1.Pod{vpod("web-new-1", "new", after, readyFor(now.Add(-30*time.Second)))}, VitalsRevived},
		{"new pod ready for 10s", []corev1.Pod{vpod("web-new-1", "new", after, readyFor(now.Add(-10*time.Second)))}, VitalsWaiting},
		{"new pod crash-loops", []corev1.Pod{vpod("web-new-1", "new", after, crashLooping)}, VitalsDying},
		{"old bad pod not replaced yet", []corev1.Pod{
			vpod("web-old-1", "old", before, crashLooping),
			vpod("web-new-1", "new", after, readyFor(now.Add(-30*time.Second))),
		}, VitalsWaiting},
		{"old bad pod gone", []corev1.Pod{
			vpod("web-old-1", "old", before, func(p *corev1.Pod) {
				crashLooping(p)
				ts := metav1.NewTime(now)
				p.DeletionTimestamp = &ts
			}),
			vpod("web-new-1", "new", after, readyFor(now.Add(-30*time.Second))),
		}, VitalsRevived},
		{"new pod restarted once", []corev1.Pod{vpod("web-new-1", "new", after, func(p *corev1.Pod) {
			readyFor(now.Add(-30 * time.Second))(p)
			p.Status.ContainerStatuses[0].RestartCount = 1
		})}, VitalsDying},
		{"new pod OOMKilled", []corev1.Pod{vpod("web-new-1", "new", after, func(p *corev1.Pod) {
			p.Status.Phase = corev1.PodRunning
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app",
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 137, Reason: "OOMKilled"}}}}
		})}, VitalsDying},
		{"older living pod (rollback)", []corev1.Pod{vpod("web-old-2", "old", before, readyFor(before))}, VitalsRevived},
		{"other family ignored", []corev1.Pod{
			func() corev1.Pod {
				p := vpod("api-x-1", "x", after, crashLooping)
				p.OwnerReferences[0].Name = "api-x"
				return p
			}(),
			vpod("web-new-1", "new", after, readyFor(now.Add(-time.Minute))),
		}, VitalsRevived},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rep := vitalsOf(tt.pods, "Deployment", "web", castAt, DefaultReadyHold, now)
			if rep.State != tt.want {
				t.Errorf("state = %s, want %s (pods %+v)", rep.State, tt.want, rep.Pods)
			}
			if rep.Family != "Deployment/web" {
				t.Errorf("family = %s", rep.Family)
			}
		})
	}
}

func TestVitalsPodDetails(t *testing.T) {
	now := castAt.Add(time.Minute)
	rep := vitalsOf([]corev1.Pod{
		vpod("web-new-1", "new", castAt.Add(time.Second), readyFor(now.Add(-25*time.Second))),
		vpod("web-old-1", "old", castAt.Add(-time.Hour), crashLooping),
	}, "Deployment", "web", castAt, DefaultReadyHold, now)
	if len(rep.Pods) != 2 {
		t.Fatalf("pods = %+v", rep.Pods)
	}
	n, o := rep.Pods[0], rep.Pods[1]
	if !n.New || !n.Ready || n.ReadySeconds != 25 || n.Failing != "" {
		t.Errorf("new pod = %+v", n)
	}
	if o.New || o.Ready || o.Failing != "CrashLoopBackOff" || o.Restarts != 3 {
		t.Errorf("old pod = %+v", o)
	}
}

func TestVitalsMakesOneListOnly(t *testing.T) {
	p := vpod("web-new-1", "new", time.Now(), readyFor(time.Now().Add(-time.Minute)))
	cs := fake.NewClientset(&p)
	rep, err := Vitals(context.Background(), cs, "seance-demo", "Deployment", "web", time.Now().Add(-time.Hour), DefaultReadyHold)
	if err != nil {
		t.Fatal(err)
	}
	if rep.State != VitalsRevived {
		t.Errorf("state = %s", rep.State)
	}
	acts := cs.Actions()
	if len(acts) != 1 || acts[0].GetVerb() != "list" || acts[0].GetResource().Resource != "pods" {
		t.Errorf("actions = %+v, want one pods list", acts)
	}
}

func TestVitalsListError(t *testing.T) {
	cs := fake.NewClientset()
	cs.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("forbidden")
	})
	if _, err := Vitals(context.Background(), cs, "seance-demo", "Deployment", "web", castAt, 0); err == nil {
		t.Fatal("want an error")
	}
}
