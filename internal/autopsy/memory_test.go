package autopsy_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/zhangjuji/k8s-seance/fixtures"
	"github.com/zhangjuji/k8s-seance/internal/autopsy"
)

func at(s string) *time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return &t
}

func fixture(t *testing.T, name string) *autopsy.PodAutopsy {
	t.Helper()
	all, err := fixtures.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range all {
		if a.Name == name {
			return a
		}
	}
	t.Fatalf("no fixture %s", name)
	return nil
}

func TestRemember(t *testing.T) {
	asOf := *at("2026-11-01T05:00:00Z")
	tests := []struct {
		name    string
		events  []autopsy.EventRecord
		ttl     time.Duration
		wantKey int // index of the key event, -1 for none
		wantAt  string
		left    int64
		allAt   string // "" for none
		allLeft int64
	}{
		{
			name: "stopped warning fades before refreshed backoff",
			events: []autopsy.EventRecord{
				{Type: "Warning", Reason: "BackOff", LastTimestamp: at("2026-11-01T04:58:00Z")},
				{Type: "Warning", Reason: "Failed", LastTimestamp: at("2026-11-01T04:20:00Z")},
				{Type: "Normal", Reason: "Pulling", LastTimestamp: at("2026-11-01T04:59:30Z")},
			},
			wantKey: 1, wantAt: "2026-11-01T05:20:00Z", left: 20 * 60,
			allAt: "2026-11-01T05:59:30Z", allLeft: 59*60 + 30,
		},
		{
			name: "normal events only: no key, all still counted",
			events: []autopsy.EventRecord{
				{Type: "Normal", Reason: "Scheduled", LastTimestamp: at("2026-11-01T04:30:00Z")},
			},
			wantKey: -1, allAt: "2026-11-01T05:30:00Z", allLeft: 30 * 60,
		},
		{
			name:    "no events",
			wantKey: -1,
		},
		{
			name: "first timestamp when last is missing, custom ttl",
			events: []autopsy.EventRecord{
				{Type: "Warning", Reason: "Unhealthy", FirstTimestamp: at("2026-11-01T04:50:00Z")},
			},
			ttl:     15 * time.Minute,
			wantKey: 0, wantAt: "2026-11-01T05:05:00Z", left: 5 * 60,
			allAt: "2026-11-01T05:05:00Z", allLeft: 5 * 60,
		},
		{
			name: "already forgotten is negative",
			events: []autopsy.EventRecord{
				{Type: "Warning", Reason: "Failed", LastTimestamp: at("2026-11-01T03:59:59Z")},
			},
			wantKey: 0, wantAt: "2026-11-01T04:59:59Z", left: -1,
			allAt: "2026-11-01T04:59:59Z", allLeft: -1,
		},
		{
			name: "warning without timestamps is skipped; tie keeps the first",
			events: []autopsy.EventRecord{
				{Type: "Warning", Reason: "NoTime"},
				{Type: "Warning", Reason: "Failed", Message: "a", LastTimestamp: at("2026-11-01T04:40:00Z")},
				{Type: "Warning", Reason: "Failed", Message: "b", LastTimestamp: at("2026-11-01T04:40:00Z")},
			},
			wantKey: 1, wantAt: "2026-11-01T05:40:00Z", left: 40 * 60,
			allAt: "2026-11-01T05:40:00Z", allLeft: 40 * 60,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := &autopsy.PodAutopsy{Name: "p", Events: tc.events}
			m := autopsy.Remember(a, tc.ttl, asOf)
			wantTTL := tc.ttl
			if wantTTL == 0 {
				wantTTL = autopsy.DefaultEventTTL
			}
			if m.TTL != wantTTL || m.TTLSeconds != int64(wantTTL/time.Second) || !m.AsOf.Equal(asOf) {
				t.Errorf("TTL/AsOf = %v/%d/%v", m.TTL, m.TTLSeconds, m.AsOf)
			}
			if tc.wantKey < 0 {
				if m.Key != nil {
					t.Fatalf("Key = %+v, want nil", m.Key)
				}
				if m.LeftSeconds != 0 {
					t.Errorf("LeftSeconds = %d, want 0", m.LeftSeconds)
				}
			} else {
				if m.Key == nil {
					t.Fatal("Key = nil")
				}
				if m.Key.Index != tc.wantKey || m.Key.Reason != tc.events[tc.wantKey].Reason || m.Key.Message != tc.events[tc.wantKey].Message {
					t.Errorf("Key = %+v, want event %d", m.Key, tc.wantKey)
				}
				if !m.Key.FadesAt.Equal(*at(tc.wantAt)) {
					t.Errorf("FadesAt = %v, want %s", m.Key.FadesAt, tc.wantAt)
				}
				if m.LeftSeconds != tc.left {
					t.Errorf("LeftSeconds = %d, want %d", m.LeftSeconds, tc.left)
				}
			}
			if tc.allAt == "" {
				if m.AllFadeAt != nil {
					t.Errorf("AllFadeAt = %v, want nil", m.AllFadeAt)
				}
				return
			}
			if m.AllFadeAt == nil || !m.AllFadeAt.Equal(*at(tc.allAt)) {
				t.Errorf("AllFadeAt = %v, want %s", m.AllFadeAt, tc.allAt)
			}
			if m.AllLeftSeconds != tc.allLeft {
				t.Errorf("AllLeftSeconds = %d, want %d", m.AllLeftSeconds, tc.allLeft)
			}
		})
	}
}

func TestRememberNil(t *testing.T) {
	m := autopsy.Remember(nil, 0, time.Time{})
	if m.Key != nil || m.AllFadeAt != nil || m.TTL != autopsy.DefaultEventTTL {
		t.Errorf("Remember(nil) = %+v", m)
	}
}

// TestRememberFixtures pins the countdown of the recorded spirits, counted from
// their collectedAt as sample mode does.
func TestRememberFixtures(t *testing.T) {
	tests := []struct {
		pod    string
		reason string
		fades  string
	}{
		// The image pull failure stopped at 04:42:03, long before the last BackOff.
		{"lost-soul-5ff5cb8464-hjtms", "Failed", "2026-11-01T05:42:03Z"},
		{"restless-spirit-84fc8b7c4c-52nl8", "BackOff", "2026-11-01T05:57:38Z"},
		{"memory-glutton-7cc6df9966-bf87c", "BackOff", "2026-11-01T05:57:44Z"},
	}
	for _, tc := range tests {
		a := fixture(t, tc.pod)
		m := autopsy.Remember(a, 0, a.CollectedAt)
		if m.Key == nil {
			t.Fatalf("%s: no key memory", tc.pod)
		}
		if m.Key.Reason != tc.reason || !m.Key.FadesAt.Equal(*at(tc.fades)) {
			t.Errorf("%s: key = %s at %v, want %s at %s", tc.pod, m.Key.Reason, m.Key.FadesAt, tc.reason, tc.fades)
		}
		if m.LeftSeconds <= 0 || m.LeftSeconds > 3600 {
			t.Errorf("%s: LeftSeconds = %d, want within the hour", tc.pod, m.LeftSeconds)
		}
		if m.AllFadeAt == nil || m.AllFadeAt.Before(m.Key.FadesAt) {
			t.Errorf("%s: AllFadeAt = %v, before the key %v", tc.pod, m.AllFadeAt, m.Key.FadesAt)
		}
	}
	lost := fixture(t, "lost-soul-5ff5cb8464-hjtms")
	m := autopsy.Remember(lost, 0, lost.CollectedAt)
	if m.Key.Message == "" || m.Key.Message == "Error: ImagePullBackOff" {
		t.Errorf("lost-soul key message = %q, want the pull failure", m.Key.Message)
	}
}

func TestRestartsSoon(t *testing.T) {
	if got := autopsy.RestartsSoon(fixture(t, "restless-spirit-84fc8b7c4c-52nl8")); !reflect.DeepEqual(got, []string{"app"}) {
		t.Errorf("crashloop RestartsSoon = %v, want [app]", got)
	}
	if got := autopsy.RestartsSoon(fixture(t, "lost-soul-5ff5cb8464-hjtms")); got != nil {
		t.Errorf("imagepull RestartsSoon = %v, want none", got)
	}
	terminated := &autopsy.PodAutopsy{Containers: []autopsy.ContainerAutopsy{
		{Name: "a", State: autopsy.ContainerState{Kind: "terminated", Reason: "Error"}},
		{Name: "b", State: autopsy.ContainerState{Kind: "waiting", Reason: "CrashLoopBackOff"}},
	}}
	if got := autopsy.RestartsSoon(terminated); !reflect.DeepEqual(got, []string{"b"}) {
		t.Errorf("RestartsSoon = %v, want [b]", got)
	}
	if got := autopsy.RestartsSoon(nil); got != nil {
		t.Errorf("RestartsSoon(nil) = %v", got)
	}
}

func TestEventEvidenceID(t *testing.T) {
	a := fixture(t, "lost-soul-5ff5cb8464-hjtms")
	ev, err := autopsy.BuildEvidence(a, autopsy.EvidenceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	m := autopsy.Remember(a, 0, a.CollectedAt)
	id := autopsy.EventEvidenceID(ev, a.Events[m.Key.Index])
	if id == "" {
		t.Fatal("key memory has no evidence ID")
	}
	var found bool
	for _, e := range ev {
		if e.ID == id {
			found = true
			if e.Kind != "event" || e.Timestamp == nil || !e.Timestamp.Equal(m.Key.LastSeen) {
				t.Errorf("%s = %+v, want the key event", id, e)
			}
		}
	}
	if !found {
		t.Fatalf("ID %s not in evidence", id)
	}
	// Every event gets a distinct ID, so two Failed events at the same time are not mixed up.
	seen := map[string]bool{}
	for _, e := range a.Events {
		got := autopsy.EventEvidenceID(ev, e)
		if got == "" || seen[got] {
			t.Errorf("event %s %q: ID %q (duplicate or missing)", e.Reason, e.Message, got)
		}
		seen[got] = true
	}
	if got := autopsy.EventEvidenceID(ev, autopsy.EventRecord{Type: "Warning", Reason: "Ghost"}); got != "" {
		t.Errorf("unknown event got ID %q", got)
	}
}
