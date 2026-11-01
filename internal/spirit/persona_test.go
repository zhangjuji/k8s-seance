package spirit

import (
	"testing"
	"time"

	"github.com/zhangjuji/k8s-seance/internal/autopsy"
)

func ptrTime(s string) *time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return &t
}

func TestDeathTime(t *testing.T) {
	tests := []struct {
		name string
		a    *autopsy.PodAutopsy
		want string // RFC3339, empty when ok should be false
	}{
		{
			name: "nil autopsy",
			a:    nil,
		},
		{
			name: "running container has no death time",
			a: &autopsy.PodAutopsy{Containers: []autopsy.ContainerAutopsy{
				{State: autopsy.ContainerState{Kind: "running", FinishedAt: ptrTime("2026-10-31T20:00:00Z")}},
			}},
		},
		{
			name: "terminated current state",
			a: &autopsy.PodAutopsy{Containers: []autopsy.ContainerAutopsy{
				{State: autopsy.ContainerState{Kind: "terminated", FinishedAt: ptrTime("2026-10-31T20:15:03Z")}},
			}},
			want: "2026-10-31T20:15:03Z",
		},
		{
			name: "last state used when current has no finish time",
			a: &autopsy.PodAutopsy{Containers: []autopsy.ContainerAutopsy{
				{
					State:     autopsy.ContainerState{Kind: "waiting", Reason: "CrashLoopBackOff"},
					LastState: &autopsy.ContainerState{Kind: "terminated", FinishedAt: ptrTime("2026-10-31T19:00:00Z")},
				},
			}},
			want: "2026-10-31T19:00:00Z",
		},
		{
			name: "latest finish across containers wins",
			a: &autopsy.PodAutopsy{Containers: []autopsy.ContainerAutopsy{
				{
					State:     autopsy.ContainerState{Kind: "terminated", FinishedAt: ptrTime("2026-10-31T18:00:00Z")},
					LastState: &autopsy.ContainerState{Kind: "terminated", FinishedAt: ptrTime("2026-10-31T17:00:00Z")},
				},
				{
					LastState: &autopsy.ContainerState{Kind: "terminated", FinishedAt: ptrTime("2026-10-31T21:00:00Z")},
				},
			}},
			want: "2026-10-31T21:00:00Z",
		},
		{
			name: "warning event used when no container finished",
			a: &autopsy.PodAutopsy{
				Containers: []autopsy.ContainerAutopsy{{State: autopsy.ContainerState{Kind: "waiting"}}},
				Events: []autopsy.EventRecord{
					{Type: "Normal", LastTimestamp: ptrTime("2026-10-31T23:00:00Z")},
					{Type: "Warning", LastTimestamp: ptrTime("2026-10-31T20:30:00Z")},
					{Type: "Warning", LastTimestamp: ptrTime("2026-10-31T20:40:00Z")},
				},
			},
			want: "2026-10-31T20:40:00Z",
		},
		{
			name: "container finish beats a later warning event",
			a: &autopsy.PodAutopsy{
				Containers: []autopsy.ContainerAutopsy{{State: autopsy.ContainerState{Kind: "terminated", FinishedAt: ptrTime("2026-10-31T20:00:00Z")}}},
				Events:     []autopsy.EventRecord{{Type: "Warning", LastTimestamp: ptrTime("2026-10-31T22:00:00Z")}},
			},
			want: "2026-10-31T20:00:00Z",
		},
		{
			name: "normal events only",
			a: &autopsy.PodAutopsy{
				Events: []autopsy.EventRecord{{Type: "Normal", LastTimestamp: ptrTime("2026-10-31T20:00:00Z")}},
			},
		},
		{
			name: "warning without timestamp",
			a: &autopsy.PodAutopsy{
				Events: []autopsy.EventRecord{{Type: "Warning"}},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := DeathTime(tt.a)
			if tt.want == "" {
				if ok {
					t.Fatalf("DeathTime = %v, true; want false", got)
				}
				return
			}
			if !ok {
				t.Fatalf("DeathTime: ok = false, want %s", tt.want)
			}
			if want := ptrTime(tt.want); !got.Equal(*want) {
				t.Errorf("DeathTime = %v, want %v", got, *want)
			}
		})
	}
}

func TestFormatDeathTime(t *testing.T) {
	tests := []struct {
		name string
		in   time.Time
		want string
	}{
		{"utc input", time.Date(2026, 10, 31, 20, 15, 3, 0, time.UTC), "2026-10-31 20:15:03 UTC"},
		{"offset input is converted", time.Date(2026, 11, 1, 4, 55, 23, 0, time.FixedZone("CST", 8*3600)), "2026-10-31 20:55:23 UTC"},
		{"fractional seconds dropped", time.Date(2026, 11, 1, 0, 0, 0, 999, time.UTC), "2026-11-01 00:00:00 UTC"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := FormatDeathTime(tt.in); got != tt.want {
				t.Errorf("FormatDeathTime = %q, want %q", got, tt.want)
			}
		})
	}
}
