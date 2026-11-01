package k8s

import (
	"context"
	"reflect"
	"sort"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
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
