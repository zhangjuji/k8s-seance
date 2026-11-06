package graveyard

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhangjuji/k8s-seance/internal/autopsy"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "sub", "graveyard.json"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return s
}

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestOpenRejectsEmptyPath(t *testing.T) {
	if _, err := Open(""); err == nil {
		t.Fatal("Open(\"\") succeeded, want an error")
	}
}

func TestIDIsStableAndShort(t *testing.T) {
	a := ID("ns", "pod", "uid-1")
	if a != ID("ns", "pod", "uid-1") {
		t.Fatal("ID is not deterministic")
	}
	if !ValidID(a) {
		t.Fatalf("ID %q is not 12 lowercase hex characters", a)
	}
	for _, other := range []string{ID("ns2", "pod", "uid-1"), ID("ns", "pod2", "uid-1"), ID("ns", "pod", "uid-2"), ID("n", "spod", "uid-1")} {
		if other == a {
			t.Errorf("ID collision with %q", other)
		}
	}
}

func TestValidID(t *testing.T) {
	for id, want := range map[string]bool{
		"0123456789ab":  true,
		"0123456789AB":  false,
		"0123456789a":   false,
		"0123456789abc": false,
		"../etc/passwd": false,
		"":              false,
	} {
		if got := ValidID(id); got != want {
			t.Errorf("ValidID(%q) = %v, want %v", id, got, want)
		}
	}
}

func TestMissingFileIsEmpty(t *testing.T) {
	s := newStore(t)
	all, err := s.List()
	if err != nil || len(all) != 0 {
		t.Fatalf("List() = %v, %v; want empty, nil", all, err)
	}
	if _, err := s.Get("0123456789ab"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get on empty graveyard: err = %v, want ErrNotFound", err)
	}
}

func TestBuryCreatesPrivateFileAndRoundTrips(t *testing.T) {
	s := newStore(t)
	s.now = func() time.Time { return at("2026-11-01T10:00:00+08:00") }
	in := Tombstone{
		PodName:      "memory-glutton",
		Namespace:    "seance-demo",
		UID:          "u1",
		BornAt:       at("2026-11-01T01:00:00Z"),
		DiedAt:       at("2026-11-01T01:05:00Z"),
		Cause:        autopsy.CauseOOMKilled,
		Epitaph:      "I filled up.",
		PostmortemMD: "# Postmortem\n",
	}
	got, err := s.Bury(in)
	if err != nil {
		t.Fatalf("bury: %v", err)
	}
	if got.ID != ID("seance-demo", "memory-glutton", "u1") {
		t.Errorf("ID = %q", got.ID)
	}
	if !got.BuriedAt.Equal(at("2026-11-01T02:00:00Z")) || got.BuriedAt.Location() != time.UTC {
		t.Errorf("BuriedAt = %v, want 2026-11-01T02:00:00Z in UTC", got.BuriedAt)
	}

	back, err := s.Get(got.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if back.PodName != in.PodName || back.Cause != in.Cause || back.Epitaph != in.Epitaph ||
		back.PostmortemMD != in.PostmortemMD || !back.DiedAt.Equal(in.DiedAt) || !back.BornAt.Equal(in.BornAt) {
		t.Errorf("round trip changed the tombstone: %+v", back)
	}

	if runtime.GOOS != "windows" {
		fi, err := os.Stat(s.Path())
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("file mode = %v, want 0600", fi.Mode().Perm())
		}
		di, err := os.Stat(filepath.Dir(s.Path()))
		if err != nil {
			t.Fatal(err)
		}
		if di.Mode().Perm() != 0o700 {
			t.Errorf("dir mode = %v, want 0700", di.Mode().Perm())
		}
	}
	entries, _ := os.ReadDir(filepath.Dir(s.Path()))
	if len(entries) != 1 {
		t.Errorf("directory holds %d entries, want only the graveyard file (no temp leftovers)", len(entries))
	}
}

func TestBuryReplacesSamePod(t *testing.T) {
	s := newStore(t)
	first, err := s.Bury(Tombstone{PodName: "p", Namespace: "n", UID: "u", Epitaph: "old", PostmortemMD: "old"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Bury(Tombstone{PodName: "p", Namespace: "n", UID: "u", Epitaph: "new", PostmortemMD: "new"})
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Fatalf("same Pod got two IDs: %s, %s", first.ID, second.ID)
	}
	// A new UID is a new Pod with the same name: it gets its own tombstone.
	if _, err := s.Bury(Tombstone{PodName: "p", Namespace: "n", UID: "u2", Epitaph: "other"}); err != nil {
		t.Fatal(err)
	}
	all, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("len = %d, want 2", len(all))
	}
	got, _ := s.Get(first.ID)
	if got.Epitaph != "new" || got.PostmortemMD != "new" {
		t.Errorf("tombstone not replaced: %+v", got)
	}
}

func TestBuryNeedsNames(t *testing.T) {
	s := newStore(t)
	if _, err := s.Bury(Tombstone{PodName: "p"}); err == nil {
		t.Error("bury without namespace succeeded")
	}
	if _, err := s.Bury(Tombstone{Namespace: "n"}); err == nil {
		t.Error("bury without pod name succeeded")
	}
}

func TestListNewestDeathFirst(t *testing.T) {
	s := newStore(t)
	for _, tc := range []struct {
		pod  string
		died time.Time
	}{
		{"middle", at("2026-11-01T02:00:00Z")},
		{"unknown", time.Time{}},
		{"newest", at("2026-11-01T03:00:00Z")},
		{"oldest", at("2026-11-01T01:00:00Z")},
	} {
		if _, err := s.Bury(Tombstone{PodName: tc.pod, Namespace: "n", DiedAt: tc.died}); err != nil {
			t.Fatal(err)
		}
	}
	all, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, ts := range all {
		order = append(order, ts.PodName)
	}
	want := []string{"newest", "middle", "oldest", "unknown"}
	if len(order) != len(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
}

func TestCorruptFileIsNotOverwritten(t *testing.T) {
	s := newStore(t)
	if err := os.MkdirAll(filepath.Dir(s.Path()), 0o700); err != nil {
		t.Fatal(err)
	}
	garbage := []byte("{not json")
	if err := os.WriteFile(s.Path(), garbage, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.List(); err == nil {
		t.Error("List on a corrupt file succeeded")
	}
	if _, err := s.Bury(Tombstone{PodName: "p", Namespace: "n"}); err == nil {
		t.Error("Bury on a corrupt file succeeded")
	}
	data, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(garbage) {
		t.Errorf("corrupt file was overwritten: %q", data)
	}
}

func TestConcurrentBuryKeepsEveryTombstone(t *testing.T) {
	s := newStore(t)
	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.Bury(Tombstone{PodName: "p", Namespace: "n", UID: string(rune('a' + i))})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	all, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != n {
		t.Fatalf("len = %d, want %d", len(all), n)
	}
}

func TestJSONFieldNamesAreCamelCase(t *testing.T) {
	s := newStore(t)
	if _, err := s.Bury(Tombstone{PodName: "p", Namespace: "n", Cause: autopsy.CauseError, DiedAt: at("2026-11-01T01:00:00Z")}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"podName"`, `"namespace"`, `"diedAt"`, `"cause"`, `"epitaph"`, `"postmortemMD"`, `"buriedAt"`} {
		if !strings.Contains(string(data), key) {
			t.Errorf("file lacks key %s: %s", key, data)
		}
	}
	if strings.Contains(string(data), `"bornAt"`) {
		t.Errorf("zero bornAt should be omitted: %s", data)
	}
}
