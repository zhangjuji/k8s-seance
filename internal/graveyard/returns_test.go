package graveyard

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zhangjuji/k8s-seance/internal/autopsy"
)

// buried returns a store with one tombstone for ns/web-a of Deployment/web,
// died at 09:55 and buried at 10:00.
func buried(t *testing.T) (*Store, Tombstone) {
	t.Helper()
	s := newStore(t)
	s.now = func() time.Time { return at("2026-11-06T10:00:00Z") }
	tomb, err := s.Bury(Tombstone{
		Namespace: "ns", PodName: "web-a", UID: "u1", Cause: autopsy.CauseOOMKilled,
		DiedAt: at("2026-11-06T09:55:00Z"), Family: "Deployment/web",
		Offerings: []Offering{{Kind: OfferingDeath, Text: "OOMKilled, exit code 137", Evidence: "E1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return s, tomb
}

func TestCounts(t *testing.T) {
	_, tomb := buried(t)
	tomb.Returns = []Return{{Pod: "web-a", DiedAt: at("2026-11-06T10:05:00Z")}, {Pod: "web-b", DiedAt: at("2026-11-06T10:06:00Z")}}
	tests := []struct {
		name   string
		pod    string
		diedAt string
		want   bool
	}{
		{"self before the burial", "web-a", "2026-11-06T09:58:00Z", false},
		{"self at the burial", "web-a", "2026-11-06T10:00:00Z", false},
		{"self at a recorded death time", "web-a", "2026-11-06T10:05:00Z", false},
		{"self at a new death time", "web-a", "2026-11-06T10:10:00Z", true},
		{"family member already returned", "web-b", "2026-11-06T10:20:00Z", false},
		{"new family member after the burial", "web-c", "2026-11-06T10:01:00Z", true},
		{"new family member before the burial", "web-c", "2026-11-06T09:59:00Z", false},
		{"no death time", "web-c", "", false},
		{"no pod", "", "2026-11-06T10:30:00Z", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var d time.Time
			if tt.diedAt != "" {
				d = at(tt.diedAt)
			}
			if got := tomb.Counts(tt.pod, d); got != tt.want {
				t.Errorf("Counts(%s, %s) = %v, want %v", tt.pod, tt.diedAt, got, tt.want)
			}
		})
	}
	// A tombstone with an unknown death time counts any death after the burial.
	tomb.DiedAt = time.Time{}
	if !tomb.Counts("web-a", at("2026-11-06T10:00:01Z")) {
		t.Error("unknown death time: a death after the burial does not count")
	}
}

func TestReturnRecordsOnceAndPersists(t *testing.T) {
	s, tomb := buried(t)
	s.now = func() time.Time { return at("2026-11-06T10:30:00Z") }
	died := at("2026-11-06T18:10:00+08:00")

	got, added, err := s.Return(tomb.ID, Return{Pod: "web-a", DiedAt: died, Cause: autopsy.CauseOOMKilled})
	if err != nil || !added {
		t.Fatalf("first return: added=%v err=%v", added, err)
	}
	if len(got.Returns) != 1 || !got.Returns[0].DiedAt.Equal(died) || got.Returns[0].DiedAt.Location() != time.UTC || !got.Returns[0].SeenAt.Equal(at("2026-11-06T10:30:00Z")) {
		t.Fatalf("returns = %+v", got.Returns)
	}
	// The same death again does not count and does not write.
	before, _ := os.Stat(s.Path())
	if _, added, err := s.Return(tomb.ID, Return{Pod: "web-a", DiedAt: died}); err != nil || added {
		t.Fatalf("repeat: added=%v err=%v", added, err)
	}
	after, _ := os.Stat(s.Path())
	if !before.ModTime().Equal(after.ModTime()) {
		t.Error("a return that does not count rewrote the file")
	}
	// A family member returns once.
	if _, added, _ := s.Return(tomb.ID, Return{Pod: "web-b", DiedAt: at("2026-11-06T10:20:00Z")}); !added {
		t.Error("family member did not return")
	}
	if _, added, _ := s.Return(tomb.ID, Return{Pod: "web-b", DiedAt: at("2026-11-06T10:25:00Z")}); added {
		t.Error("family member returned twice")
	}

	stored, err := s.Get(tomb.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.Returns) != 2 || stored.Returns[0].Pod != "web-a" || stored.Returns[1].Pod != "web-b" {
		t.Fatalf("stored returns = %+v", stored.Returns)
	}
	if stored.Family != "Deployment/web" || len(stored.Offerings) != 1 || stored.Offerings[0].Evidence != "E1" {
		t.Errorf("family or offerings lost: %+v", stored)
	}
	if n, r, ok := stored.LastReturnOf("web-a"); !ok || n != 1 || !r.DiedAt.Equal(died) {
		t.Errorf("LastReturnOf(web-a) = %d, %+v, %v", n, r, ok)
	}
	if n, _, ok := stored.LastReturnOf("web-b"); !ok || n != 2 {
		t.Errorf("LastReturnOf(web-b) = %d, %v", n, ok)
	}
	if _, _, ok := stored.LastReturnOf("web-z"); ok {
		t.Error("LastReturnOf(web-z) found a return")
	}

	if _, _, err := s.Return("0123456789ab", Return{Pod: "x", DiedAt: died}); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown tombstone: err = %v, want ErrNotFound", err)
	}
}

func TestReburyClearsReturns(t *testing.T) {
	s, tomb := buried(t)
	if _, added, _ := s.Return(tomb.ID, Return{Pod: "web-a", DiedAt: at("2026-11-06T10:10:00Z")}); !added {
		t.Fatal("return not recorded")
	}
	s.now = func() time.Time { return at("2026-11-06T11:00:00Z") }
	again := tomb
	again.Returns = []Return{{Pod: "stale", DiedAt: at("2026-11-06T10:50:00Z")}}
	if _, err := s.Bury(again); err != nil {
		t.Fatal(err)
	}
	stored, _ := s.Get(tomb.ID)
	if len(stored.Returns) != 0 {
		t.Errorf("returns after reburial = %+v, want none", stored.Returns)
	}
}

func TestMatch(t *testing.T) {
	all := []Tombstone{
		{ID: "a", Namespace: "ns", PodName: "web-a", Family: "Deployment/web", BuriedAt: at("2026-11-06T10:00:00Z")},
		{ID: "b", Namespace: "ns", PodName: "web-b", Family: "Deployment/web", BuriedAt: at("2026-11-06T11:00:00Z")},
		{ID: "c", Namespace: "other", PodName: "web-c", Family: "Deployment/web", BuriedAt: at("2026-11-06T12:00:00Z")},
		{ID: "d", Namespace: "ns", PodName: "db-0", Family: "StatefulSet/db", BuriedAt: at("2026-11-06T09:00:00Z")},
	}
	tests := []struct {
		ns, pod, family string
		want            string
	}{
		{"ns", "web-a", "Deployment/web", "a"},    // its own tombstone first
		{"ns", "web-a", "", "a"},                  // even without a family
		{"ns", "web-x", "Deployment/web", "b"},    // the most recently buried of the family
		{"other", "web-x", "Deployment/web", "c"}, // the namespace must match
		{"ns", "web-x", "", ""},
		{"ns", "api-x", "Deployment/api", ""},
		{"elsewhere", "web-a", "Deployment/web", ""},
	}
	for _, tt := range tests {
		got, ok := Match(all, tt.ns, tt.pod, tt.family)
		if (tt.want == "") == ok || (ok && got.ID != tt.want) {
			t.Errorf("Match(%s, %s, %s) = %q, %v; want %q", tt.ns, tt.pod, tt.family, got.ID, ok, tt.want)
		}
	}
}

func TestOldGraveyardFileStillReads(t *testing.T) {
	s := newStore(t)
	if err := os.MkdirAll(strings.TrimSuffix(s.Path(), "/graveyard.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	old := `[{"id":"0123456789ab","podName":"p","namespace":"ns","cause":"Error","epitaph":"","postmortemMD":"# x","buriedAt":"2026-11-01T00:00:00Z"}]`
	if err := os.WriteFile(s.Path(), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get("0123456789ab")
	if err != nil || got.Family != "" || got.Offerings != nil || got.Returns != nil {
		t.Errorf("old tombstone = %+v, %v", got, err)
	}
}
