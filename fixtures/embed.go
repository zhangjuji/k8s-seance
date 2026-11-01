// Package fixtures bundles the recorded spirits so the CLI can run offline
// (-fixtures). Recordings are written by SEANCE_RECORD=1 against a real cluster.
package fixtures

import (
	"embed"
	"encoding/json"
	"fmt"

	"github.com/zhangjuji/k8s-seance/internal/autopsy"
)

//go:embed *.json
var files embed.FS

// fixtureFiles lists the recordings in the order Load returns them.
var fixtureFiles = []string{"oom.json", "crashloop.json", "imagepull.json"}

// Load decodes the three recorded PodAutopsy snapshots in fixtureFiles order.
// It returns an error if any of them is missing or invalid.
func Load() ([]*autopsy.PodAutopsy, error) {
	out := make([]*autopsy.PodAutopsy, 0, len(fixtureFiles))
	for _, name := range fixtureFiles {
		data, err := files.ReadFile(name)
		if err != nil {
			return nil, fmt.Errorf("read fixture %s: %w", name, err)
		}
		var a autopsy.PodAutopsy
		if err := json.Unmarshal(data, &a); err != nil {
			return nil, fmt.Errorf("decode fixture %s: %w", name, err)
		}
		out = append(out, &a)
	}
	return out, nil
}
