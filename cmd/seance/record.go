package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/zhangjuji/k8s-seance/internal/autopsy"
)

// fixtureNames maps the causes that have a fixture file to the file base name.
var fixtureNames = map[autopsy.CauseOfDeath]string{
	autopsy.CauseOOMKilled:        "oom",
	autopsy.CauseCrashLoopBackOff: "crashloop",
	autopsy.CauseImagePullBackOff: "imagepull",
}

// recorder writes collected autopsies to local fixture files. Only the
// local filesystem is touched, never the cluster. A nil *recorder records nothing.
type recorder struct {
	dir    string
	errOut io.Writer
}

// recorderFromEnv returns a recorder when SEANCE_RECORD=1, otherwise nil.
// The target directory is SEANCE_FIXTURES_DIR, defaulting to "fixtures".
func recorderFromEnv(errOut io.Writer) *recorder {
	if os.Getenv("SEANCE_RECORD") != "1" {
		return nil
	}
	dir := os.Getenv("SEANCE_FIXTURES_DIR")
	if dir == "" {
		dir = "fixtures"
	}
	return &recorder{dir: dir, errOut: errOut}
}

// record writes a as <dir>/<name>.json, named by its classified cause.
// Pods with other causes are skipped with a note on errOut.
func (r *recorder) record(a *autopsy.PodAutopsy) error {
	if r == nil {
		return nil
	}
	cause := autopsy.Classify(a).Cause
	name, ok := fixtureNames[cause]
	if !ok {
		fmt.Fprintf(r.errOut, "skip %s/%s: cause=%s has no fixture name\n", a.Namespace, a.Name, cause)
		return nil
	}

	snap := *a
	snap.Source = "fixture"
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return fmt.Errorf("encode fixture for %s/%s: %w", a.Namespace, a.Name, err)
	}
	if err := os.MkdirAll(r.dir, 0o755); err != nil {
		return fmt.Errorf("create fixtures dir: %w", err)
	}
	path := filepath.Join(r.dir, name+".json")
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write fixture %s: %w", path, err)
	}
	fmt.Fprintf(r.errOut, "recorded %s\n", path)
	return nil
}
