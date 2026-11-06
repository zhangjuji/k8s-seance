package graveyard

import (
	"os"
	"testing"
	"time"
)

func TestReviveMarksOnce(t *testing.T) {
	s, tomb := buried(t)
	first := at("2026-11-06T10:30:00Z")
	got, ok, err := s.Revive("ns", "web-a", first, "patch")
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if !got.RevivedAt.Equal(first) || got.RevivedBy != "patch" || got.ID != tomb.ID {
		t.Errorf("tombstone = %+v", got)
	}
	// The first revival is kept.
	got, ok, err = s.Revive("ns", "web-a", first.Add(time.Hour), "rollback")
	if err != nil || !ok || !got.RevivedAt.Equal(first) || got.RevivedBy != "patch" {
		t.Errorf("second revive: %+v ok=%v err=%v", got, ok, err)
	}
	stored, err := s.Get(tomb.ID)
	if err != nil || !stored.RevivedAt.Equal(first) {
		t.Errorf("stored = %+v err=%v", stored, err)
	}
}

func TestReviveUnknownPodWritesNothing(t *testing.T) {
	s, _ := buried(t)
	info, _ := os.Stat(s.Path())
	_, ok, err := s.Revive("ns", "someone-else", at("2026-11-06T10:30:00Z"), "patch")
	if err != nil || ok {
		t.Fatalf("ok=%v err=%v, want not found", ok, err)
	}
	after, _ := os.Stat(s.Path())
	if !after.ModTime().Equal(info.ModTime()) {
		t.Error("the file was rewritten")
	}
	empty := newStore(t)
	if _, ok, err := empty.Revive("ns", "web-a", time.Now(), "patch"); ok || err != nil {
		t.Errorf("empty graveyard: ok=%v err=%v", ok, err)
	}
	if _, err := os.Stat(empty.Path()); !os.IsNotExist(err) {
		t.Errorf("empty graveyard file was created: %v", err)
	}
}

func TestBuryAgainClearsRevival(t *testing.T) {
	s, tomb := buried(t)
	if _, _, err := s.Revive("ns", "web-a", at("2026-11-06T10:30:00Z"), "patch"); err != nil {
		t.Fatal(err)
	}
	tomb.RevivedAt = time.Time{}
	again, err := s.Bury(tomb)
	if err != nil {
		t.Fatal(err)
	}
	if !again.RevivedAt.IsZero() || again.RevivedBy != "" {
		t.Errorf("revival kept after a new burial: %+v", again)
	}
}
