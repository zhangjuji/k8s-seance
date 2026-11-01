package autopsy

import (
	"reflect"
	"testing"
)

func i32(n int32) *int32 { return &n }

func terminated(reason string, code int32) ContainerState {
	return ContainerState{Kind: "terminated", Reason: reason, ExitCode: i32(code)}
}

func waiting(reason string) ContainerState {
	return ContainerState{Kind: "waiting", Reason: reason}
}

func lastOf(s ContainerState) *ContainerState { return &s }

func pod(containers []ContainerAutopsy, events ...EventRecord) *PodAutopsy {
	return &PodAutopsy{Name: "demo", Containers: containers, Events: events}
}

func ctr(state ContainerState, restarts int32, last *ContainerState) ContainerAutopsy {
	return ContainerAutopsy{Name: "app", Image: "busybox", RestartCount: restarts, State: state, LastState: last}
}

func event(reason, msg string) EventRecord {
	return EventRecord{Type: "Warning", Reason: reason, Message: msg}
}

func TestClassify(t *testing.T) {
	const (
		backOffRestart = "Back-off restarting failed container app in pod demo_default"
		backOffPull    = "Back-off pulling image \"nosuch:latest\""
		liveness       = "Liveness probe failed: HTTP probe failed with statuscode: 500"
	)
	tests := []struct {
		name string
		a    *PodAutopsy
		want Classification
	}{
		{
			name: "empty autopsy is Unknown",
			a:    &PodAutopsy{},
			want: Classification{Cause: CauseUnknown},
		},
		{
			name: "nil autopsy is Unknown",
			a:    nil,
			want: Classification{Cause: CauseUnknown},
		},
		{
			name: "OOMKilled in state.terminated",
			a:    pod([]ContainerAutopsy{ctr(terminated("OOMKilled", 137), 0, nil)}),
			want: Classification{Cause: CauseOOMKilled, LastExitCode: i32(137)},
		},
		{
			name: "OOMKilled in lastState.terminated",
			a:    pod([]ContainerAutopsy{ctr(waiting("CrashLoopBackOff"), 3, lastOf(terminated("OOMKilled", 137)))}),
			want: Classification{Cause: CauseOOMKilled, LastExitCode: i32(137)},
		},
		{
			name: "exit 137 with OOM event is OOMKilled",
			a: pod([]ContainerAutopsy{ctr(terminated("Error", 137), 0, nil)},
				event("OOMKilling", "Memory cgroup out of memory: Killed process 42")),
			want: Classification{Cause: CauseOOMKilled, LastExitCode: i32(137)},
		},
		{
			name: "exit 137 without OOM reason or event is Error",
			a:    pod([]ContainerAutopsy{ctr(terminated("Error", 137), 0, nil)}),
			want: Classification{Cause: CauseError, LastExitCode: i32(137)},
		},
		{
			name: "exit 137 with CrashLoop waiting and no OOM is CrashLoopBackOff",
			a:    pod([]ContainerAutopsy{ctr(waiting("CrashLoopBackOff"), 4, lastOf(terminated("Error", 137)))}),
			want: Classification{Cause: CauseCrashLoopBackOff, LastExitCode: i32(137)},
		},
		{
			name: "waiting CrashLoopBackOff with lastState OOMKilled is OOMKilled",
			a:    pod([]ContainerAutopsy{ctr(waiting("CrashLoopBackOff"), 5, lastOf(terminated("OOMKilled", 137)))}),
			want: Classification{Cause: CauseOOMKilled, LastExitCode: i32(137)},
		},
		{
			name: "ImagePullBackOff from waiting",
			a:    pod([]ContainerAutopsy{ctr(waiting("ImagePullBackOff"), 0, nil)}),
			want: Classification{Cause: CauseImagePullBackOff},
		},
		{
			name: "ErrImagePull is ImagePullBackOff",
			a:    pod([]ContainerAutopsy{ctr(waiting("ErrImagePull"), 0, nil)}, event("Failed", "Failed to pull image")),
			want: Classification{Cause: CauseImagePullBackOff},
		},
		{
			name: "ErrImagePull with pulling BackOff event is ImagePullBackOff",
			a: pod([]ContainerAutopsy{ctr(waiting("ErrImagePull"), 0, nil)},
				event("BackOff", backOffPull)),
			want: Classification{Cause: CauseImagePullBackOff},
		},
		{
			name: "CrashLoopBackOff from waiting",
			a:    pod([]ContainerAutopsy{ctr(waiting("CrashLoopBackOff"), 7, lastOf(terminated("Error", 3)))}),
			want: Classification{Cause: CauseCrashLoopBackOff, LastExitCode: i32(3)},
		},
		{
			name: "kind v1.37 shape: terminated Error exit 1, restarts, BackOff event",
			a: pod([]ContainerAutopsy{ctr(terminated("Error", 1), 6, nil)},
				event("BackOff", backOffRestart)),
			want: Classification{Cause: CauseCrashLoopBackOff, LastExitCode: i32(1)},
		},
		{
			name: "BackOff restart event without restarts is not CrashLoop",
			a: pod([]ContainerAutopsy{ctr(terminated("Error", 1), 0, nil)},
				event("BackOff", backOffRestart)),
			want: Classification{Cause: CauseError, LastExitCode: i32(1)},
		},
		{
			name: "restarts with pulling BackOff event is not CrashLoop",
			a: pod([]ContainerAutopsy{ctr(terminated("Error", 1), 2, nil)},
				event("BackOff", backOffPull)),
			want: Classification{Cause: CauseError, LastExitCode: i32(1)},
		},
		{
			name: "CrashLoop plus Liveness probe event adds ProbeFailure as secondary",
			a: pod([]ContainerAutopsy{ctr(waiting("CrashLoopBackOff"), 2, lastOf(terminated("Error", 1)))},
				event("Unhealthy", liveness)),
			want: Classification{Cause: CauseCrashLoopBackOff, Secondary: []CauseOfDeath{CauseProbeFailure}, LastExitCode: i32(1)},
		},
		{
			name: "Readiness probe event alone is ProbeFailure",
			a: pod([]ContainerAutopsy{ctr(ContainerState{Kind: "running"}, 0, nil)},
				event("Unhealthy", "Readiness probe failed: connection refused")),
			want: Classification{Cause: CauseProbeFailure},
		},
		{
			name: "Startup probe event with non-zero exit is ProbeFailure, not Error",
			a: pod([]ContainerAutopsy{ctr(terminated("Error", 1), 1, nil)},
				event("Unhealthy", "Startup probe failed: dial tcp 10.0.0.1:8080: connection refused")),
			want: Classification{Cause: CauseProbeFailure, LastExitCode: i32(1)},
		},
		{
			name: "Unhealthy event without probe keyword is not ProbeFailure",
			a: pod([]ContainerAutopsy{ctr(ContainerState{Kind: "running"}, 0, nil)},
				event("Unhealthy", "something else")),
			want: Classification{Cause: CauseUnknown},
		},
		{
			name: "OOM plus probe event adds ProbeFailure as secondary",
			a: pod([]ContainerAutopsy{ctr(terminated("OOMKilled", 137), 0, nil)},
				event("Unhealthy", liveness)),
			want: Classification{Cause: CauseOOMKilled, Secondary: []CauseOfDeath{CauseProbeFailure}, LastExitCode: i32(137)},
		},
		{
			name: "non-zero terminated exit is Error",
			a:    pod([]ContainerAutopsy{ctr(terminated("Error", 2), 0, nil)}),
			want: Classification{Cause: CauseError, LastExitCode: i32(2)},
		},
		{
			name: "lastState non-zero exit is Error",
			a:    pod([]ContainerAutopsy{ctr(ContainerState{Kind: "running"}, 1, lastOf(terminated("Error", 2)))}),
			want: Classification{Cause: CauseError, LastExitCode: i32(2)},
		},
		{
			name: "exit 0 terminated is Unknown",
			a:    pod([]ContainerAutopsy{ctr(terminated("Completed", 0), 0, nil)}),
			want: Classification{Cause: CauseUnknown},
		},
		{
			name: "non-OOM waiting reason is Unknown",
			a:    pod([]ContainerAutopsy{ctr(waiting("ContainerCreating"), 0, nil)}),
			want: Classification{Cause: CauseUnknown},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Classify(tt.a)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Classify() = %+v, want %+v", got, tt.want)
			}
		})
	}
}
