package fixtures

import (
	"io/fs"
	"testing"

	"github.com/zhangjuji/k8s-seance/internal/autopsy"
)

func TestLoadReturnsThreeClassifiedSpirits(t *testing.T) {
	spirits, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	want := []autopsy.CauseOfDeath{
		autopsy.CauseOOMKilled,        // oom.json
		autopsy.CauseCrashLoopBackOff, // crashloop.json
		autopsy.CauseImagePullBackOff, // imagepull.json
	}
	if len(spirits) != len(want) {
		t.Fatalf("Load returned %d autopsies, want %d", len(spirits), len(want))
	}
	for i, a := range spirits {
		if a == nil {
			t.Fatalf("autopsy %d is nil", i)
		}
		if got := autopsy.Classify(a).Cause; got != want[i] {
			t.Errorf("autopsy %d (%s): cause = %s, want %s", i, a.Name, got, want[i])
		}
	}
}

func TestReplayFSHasReadme(t *testing.T) {
	if _, err := fs.Stat(ReplayFS(), "README.md"); err != nil {
		t.Fatalf("ReplayFS: %v", err)
	}
}
