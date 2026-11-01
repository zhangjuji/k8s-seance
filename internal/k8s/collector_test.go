package k8s

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func running() corev1.ContainerState {
	return corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
}

func waiting(reason string) corev1.ContainerState {
	return corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason}}
}

func terminated(exitCode int32, finished time.Time) corev1.ContainerState {
	return corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
		ExitCode:   exitCode,
		FinishedAt: metav1.NewTime(finished),
	}}
}

func podWith(init, regular []corev1.ContainerStatus) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"},
		Status: corev1.PodStatus{
			InitContainerStatuses: init,
			ContainerStatuses:     regular,
		},
	}
}

func TestIsSpirit(t *testing.T) {
	now := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)

	tests := []struct {
		name string
		pod  *corev1.Pod
		want bool
	}{
		{
			name: "healthy running pod",
			pod: podWith(nil, []corev1.ContainerStatus{
				{Name: "app", RestartCount: 0, State: running()},
			}),
			want: false,
		},
		{
			name: "completed container with exit 0 and no restarts",
			pod: podWith(nil, []corev1.ContainerStatus{
				{Name: "job", State: terminated(0, now)},
			}),
			want: false,
		},
		{
			name: "restartCount greater than zero",
			pod: podWith(nil, []corev1.ContainerStatus{
				{Name: "app", RestartCount: 2, State: running()},
			}),
			want: true,
		},
		{
			name: "restartCount greater than zero on init container",
			pod: podWith([]corev1.ContainerStatus{
				{Name: "init", RestartCount: 1, State: terminated(0, now)},
			}, []corev1.ContainerStatus{
				{Name: "app", State: running()},
			}),
			want: true,
		},
		{
			name: "lastState terminated",
			pod: podWith(nil, []corev1.ContainerStatus{
				{Name: "app", State: running(), LastTerminationState: terminated(0, now)},
			}),
			want: true,
		},
		{
			name: "state terminated with non-zero exit code",
			pod: podWith(nil, []corev1.ContainerStatus{
				{Name: "app", State: terminated(1, now)},
			}),
			want: true,
		},
		{
			name: "waiting in CrashLoopBackOff",
			pod: podWith(nil, []corev1.ContainerStatus{
				{Name: "app", State: waiting("CrashLoopBackOff")},
			}),
			want: true,
		},
		{
			name: "waiting ContainerCreating is not abnormal",
			pod: podWith(nil, []corev1.ContainerStatus{
				{Name: "app", State: waiting("ContainerCreating")},
			}),
			want: false,
		},
		{
			name: "waiting ImagePullBackOff",
			pod: podWith(nil, []corev1.ContainerStatus{
				{Name: "app", State: waiting("ImagePullBackOff")},
			}),
			want: true,
		},
		{
			name: "waiting ErrImagePull",
			pod: podWith(nil, []corev1.ContainerStatus{
				{Name: "app", State: waiting("ErrImagePull")},
			}),
			want: true,
		},
		{
			name: "waiting CreateContainerConfigError",
			pod: podWith(nil, []corev1.ContainerStatus{
				{Name: "app", State: waiting("CreateContainerConfigError")},
			}),
			want: true,
		},
		{
			name: "waiting CreateContainerError",
			pod: podWith(nil, []corev1.ContainerStatus{
				{Name: "app", State: waiting("CreateContainerError")},
			}),
			want: true,
		},
		{
			name: "waiting RunContainerError",
			pod: podWith(nil, []corev1.ContainerStatus{
				{Name: "app", State: waiting("RunContainerError")},
			}),
			want: true,
		},
		{
			name: "waiting InvalidImageName",
			pod: podWith(nil, []corev1.ContainerStatus{
				{Name: "app", State: waiting("InvalidImageName")},
			}),
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsSpirit(tt.pod); got != tt.want {
				t.Errorf("IsSpirit() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestListSpirits(t *testing.T) {
	older := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 11, 1, 10, 30, 0, 0, time.UTC)

	pod := func(ns, name string, statuses ...corev1.ContainerStatus) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Status:     corev1.PodStatus{ContainerStatuses: statuses},
		}
	}

	objects := []runtime.Object{
		pod("default", "healthy", corev1.ContainerStatus{Name: "app", State: running()}),
		pod("default", "crashy",
			corev1.ContainerStatus{
				Name:                 "app",
				RestartCount:         3,
				State:                waiting("CrashLoopBackOff"),
				LastTerminationState: terminated(1, older),
			},
			corev1.ContainerStatus{
				Name:         "sidecar",
				RestartCount: 2,
				State:        terminated(137, newer),
			},
		),
		pod("default", "lost", corev1.ContainerStatus{Name: "app", State: waiting("ImagePullBackOff")}),
		pod("other", "elsewhere", corev1.ContainerStatus{Name: "app", State: waiting("CrashLoopBackOff")}),
	}

	cs := fake.NewClientset(objects...)

	tests := []struct {
		name      string
		namespace string
		wantNames []string
	}{
		{name: "empty namespace defaults to default", namespace: "", wantNames: []string{"crashy", "lost"}},
		{name: "explicit namespace", namespace: "other", wantNames: []string{"elsewhere"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ListSpirits(context.Background(), cs, tt.namespace)
			if err != nil {
				t.Fatalf("ListSpirits() error = %v", err)
			}
			names := make([]string, 0, len(got))
			for _, s := range got {
				names = append(names, s.Name)
			}
			sort.Strings(names)
			want := append([]string(nil), tt.wantNames...)
			sort.Strings(want)
			if !reflect.DeepEqual(names, want) {
				t.Fatalf("ListSpirits() names = %v, want %v", names, want)
			}
		})
	}

	t.Run("restart sum and latest death", func(t *testing.T) {
		got, err := ListSpirits(context.Background(), cs, "default")
		if err != nil {
			t.Fatalf("ListSpirits() error = %v", err)
		}
		for _, s := range got {
			if s.Name != "crashy" {
				continue
			}
			if s.Namespace != "default" {
				t.Errorf("Namespace = %q, want default", s.Namespace)
			}
			if s.RestartCount != 5 {
				t.Errorf("RestartCount = %d, want 5", s.RestartCount)
			}
			if s.LastDeath == nil || !s.LastDeath.Equal(newer) {
				t.Errorf("LastDeath = %v, want %v", s.LastDeath, newer)
			}
			return
		}
		t.Fatal("crashy not found in default namespace results")
	})
}

// oomPod is a Pod whose container was OOMKilled and restarted.
func oomPod(now time.Time) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "memory-glutton-7d9f",
			Namespace:         "seance-demo",
			UID:               "uid-123",
			CreationTimestamp: metav1.NewTime(now.Add(-time.Hour)),
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "ReplicaSet", Name: "memory-glutton-7d9f"},
				{Kind: "Other", Name: "ignored"},
			},
		},
		Spec: corev1.PodSpec{
			NodeName: "kind-worker",
			Containers: []corev1.Container{{
				Name:  "stress",
				Image: "polinux/stress",
				Resources: corev1.ResourceRequirements{
					Limits: corev1.ResourceList{
						corev1.ResourceMemory: resource.MustParse("64Mi"),
					},
					Requests: corev1.ResourceList{
						corev1.ResourceMemory: resource.MustParse("32Mi"),
						corev1.ResourceCPU:    resource.MustParse("100m"),
					},
				},
			}},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:         "stress",
				Image:        "polinux/stress",
				RestartCount: 3,
				State:        running(),
				LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					Reason:     "OOMKilled",
					ExitCode:   137,
					StartedAt:  metav1.NewTime(now.Add(-2 * time.Minute)),
					FinishedAt: metav1.NewTime(now.Add(-time.Minute)),
				}},
			}},
		},
	}
}

// oomEvents returns three events for oomPod. Their effective last times are
// 09:00 (EventTime only), 09:30 (FirstTimestamp only) and 10:00 (LastTimestamp),
// so sorting should give Created, Pulled, BackOff.
func oomEvents() []runtime.Object {
	t0 := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	t1 := time.Date(2026, 11, 1, 9, 30, 0, 0, time.UTC)
	t2 := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	involved := corev1.ObjectReference{Kind: "Pod", Name: "memory-glutton-7d9f"}

	return []runtime.Object{
		&corev1.Event{
			ObjectMeta:     metav1.ObjectMeta{Name: "lastOnly", Namespace: "seance-demo"},
			InvolvedObject: involved,
			Type:           "Warning",
			Reason:         "BackOff",
			Message:        "Back-off restarting failed container",
			Count:          4,
			LastTimestamp:  metav1.NewTime(t2),
		},
		&corev1.Event{
			ObjectMeta:     metav1.ObjectMeta{Name: "onlyFirst", Namespace: "seance-demo"},
			InvolvedObject: involved,
			Type:           "Normal",
			Reason:         "Pulled",
			Count:          1,
			FirstTimestamp: metav1.NewTime(t1),
		},
		&corev1.Event{
			ObjectMeta:     metav1.ObjectMeta{Name: "onlyEventTime", Namespace: "seance-demo"},
			InvolvedObject: involved,
			Type:           "Normal",
			Reason:         "Created",
			Count:          1,
			EventTime:      metav1.NewMicroTime(t0),
		},
	}
}

func TestCollectAutopsy(t *testing.T) {
	now := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)

	tests := []struct {
		name        string
		failPrevLog bool
		failCurLog  bool
		wantPrevNil bool
		wantCurNil  bool
	}{
		{name: "both logs available"},
		{name: "previous logs unavailable", failPrevLog: true, wantPrevNil: true},
		{name: "current logs unavailable", failCurLog: true, wantCurNil: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cs := fake.NewClientset(append([]runtime.Object{oomPod(now)}, oomEvents()...)...)
			cs.PrependReactor("get", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
				if action.GetSubresource() != "log" {
					return false, nil, nil
				}
				opts, ok := action.(k8stesting.GenericAction).GetValue().(*corev1.PodLogOptions)
				if !ok {
					return false, nil, nil
				}
				if (opts.Previous && tt.failPrevLog) || (!opts.Previous && tt.failCurLog) {
					return true, nil, errors.New("injected log error")
				}
				return false, nil, nil
			})

			a, err := CollectAutopsy(context.Background(), cs, "seance-demo", "memory-glutton-7d9f")
			if err != nil {
				t.Fatalf("CollectAutopsy() error = %v", err)
			}
			if len(a.Containers) != 1 {
				t.Fatalf("containers = %d, want 1", len(a.Containers))
			}
			c := a.Containers[0]

			if c.LogTail == nil && !tt.wantCurNil {
				t.Errorf("LogTail = nil, want non-nil")
			}
			if tt.wantCurNil && c.LogTail != nil {
				t.Errorf("LogTail = %v, want nil", c.LogTail)
			}
			if c.PreviousLogTail == nil && !tt.wantPrevNil {
				t.Errorf("PreviousLogTail = nil, want non-nil")
			}
			if tt.wantPrevNil && c.PreviousLogTail != nil {
				t.Errorf("PreviousLogTail = %v, want nil", c.PreviousLogTail)
			}
		})
	}
}

func TestCollectAutopsyFields(t *testing.T) {
	now := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	t1 := time.Date(2026, 11, 1, 9, 30, 0, 0, time.UTC)
	t2 := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)

	cs := fake.NewClientset(append([]runtime.Object{oomPod(now)}, oomEvents()...)...)

	a, err := CollectAutopsy(context.Background(), cs, "seance-demo", "memory-glutton-7d9f")
	if err != nil {
		t.Fatalf("CollectAutopsy() error = %v", err)
	}

	if a.Namespace != "seance-demo" || a.Name != "memory-glutton-7d9f" || a.UID != "uid-123" {
		t.Errorf("identity = %s/%s uid=%s", a.Namespace, a.Name, a.UID)
	}
	if !a.CreatedAt.Equal(now.Add(-time.Hour)) {
		t.Errorf("CreatedAt = %v", a.CreatedAt)
	}
	if a.Phase != "Running" || a.NodeName != "kind-worker" {
		t.Errorf("Phase = %q NodeName = %q", a.Phase, a.NodeName)
	}
	if a.OwnerKind != "ReplicaSet" || a.OwnerName != "memory-glutton-7d9f" {
		t.Errorf("owner = %s/%s, want ReplicaSet/memory-glutton-7d9f", a.OwnerKind, a.OwnerName)
	}
	if a.Source != "cluster" || a.CollectedAt.IsZero() {
		t.Errorf("Source = %q CollectedAt = %v", a.Source, a.CollectedAt)
	}

	c := a.Containers[0]
	if c.Name != "stress" || c.Image != "polinux/stress" || c.RestartCount != 3 {
		t.Errorf("container = %+v", c)
	}
	if c.State.Kind != "running" || c.State.ExitCode != nil {
		t.Errorf("State = %+v, want running without exit code", c.State)
	}
	if c.LastState == nil || c.LastState.Kind != "terminated" || c.LastState.Reason != "OOMKilled" {
		t.Fatalf("LastState = %+v, want terminated OOMKilled", c.LastState)
	}
	if c.LastState.ExitCode == nil || *c.LastState.ExitCode != 137 {
		t.Errorf("LastState.ExitCode = %v, want 137", c.LastState.ExitCode)
	}
	if c.LastState.FinishedAt == nil || !c.LastState.FinishedAt.Equal(now.Add(-time.Minute)) {
		t.Errorf("LastState.FinishedAt = %v", c.LastState.FinishedAt)
	}
	if c.Limits["memory"] != "64Mi" {
		t.Errorf("Limits = %v, want memory=64Mi", c.Limits)
	}
	if c.Requests["memory"] != "32Mi" || c.Requests["cpu"] != "100m" {
		t.Errorf("Requests = %v", c.Requests)
	}

	var reasons []string
	for _, e := range a.Events {
		reasons = append(reasons, e.Reason)
	}
	if want := []string{"Created", "Pulled", "BackOff"}; !reflect.DeepEqual(reasons, want) {
		t.Errorf("event order = %v, want %v", reasons, want)
	}
	if last := a.Events[2]; last.Count != 4 || last.LastTimestamp == nil || !last.LastTimestamp.Equal(t2) {
		t.Errorf("last event = %+v", last)
	}
	if first := a.Events[1]; first.FirstTimestamp == nil || !first.FirstTimestamp.Equal(t1) || first.LastTimestamp == nil || !first.LastTimestamp.Equal(t1) {
		t.Errorf("FirstTimestamp fallback event = %+v", first)
	}
}

func TestCollectAutopsyNotFound(t *testing.T) {
	cs := fake.NewClientset()
	_, err := CollectAutopsy(context.Background(), cs, "default", "gone")
	if err == nil {
		t.Fatal("CollectAutopsy() error = nil, want not found")
	}
	if !apierrors.IsNotFound(err) {
		t.Errorf("error = %v, want wrapped NotFound", err)
	}
}

func TestIsLogUnavailable(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{name: "gc placeholder", body: "unable to retrieve container logs for containerd://61ee874b7f9d", want: true},
		{name: "placeholder with surrounding whitespace", body: "\n  unable to retrieve container logs for docker://abc\n", want: true},
		{name: "real log line", body: "panic: out of memory\n", want: false},
		{name: "log line mentioning the phrase later", body: "level=warn unable to retrieve container logs for x", want: false},
		{name: "empty body is an empty log, not unavailable", body: "", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isLogUnavailable(tt.body); got != tt.want {
				t.Errorf("isLogUnavailable(%q) = %v, want %v", tt.body, got, tt.want)
			}
		})
	}
}

func TestSplitLines(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{in: "", want: []string{}},
		{in: "one", want: []string{"one"}},
		{in: "one\ntwo\n", want: []string{"one", "two"}},
		{in: "one\n\ntwo", want: []string{"one", "", "two"}},
	}
	for _, tt := range tests {
		if got := splitLines(tt.in); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("splitLines(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
