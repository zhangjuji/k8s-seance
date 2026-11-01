package postmortem

import (
	"regexp"
	"strings"
	"testing"

	"github.com/zhangjuji/k8s-seance/internal/autopsy"
)

var citeRE = regexp.MustCompile(`\[E\d+\]`)

// ruleCauses lists every cause the rule template covers.
var ruleCauses = []autopsy.CauseOfDeath{
	autopsy.CauseOOMKilled,
	autopsy.CauseCrashLoopBackOff,
	autopsy.CauseImagePullBackOff,
	autopsy.CauseError,
	autopsy.CauseProbeFailure,
	autopsy.CauseUnknown,
}

// richPod is a snapshot with an exit code, restarts, an image and a memory limit.
func richPod() *autopsy.PodAutopsy {
	return &autopsy.PodAutopsy{
		Namespace: "ns",
		Name:      "app-7cc6df9966-x2",
		Containers: []autopsy.ContainerAutopsy{{
			Name:         "app",
			Image:        "busybox:1.36",
			RestartCount: 8,
			Limits:       map[string]string{"memory": "64Mi"},
		}},
	}
}

// richEvidence has one item of every kind the rules cite. E6 is the only error-looking log line.
func richEvidence() []autopsy.Evidence {
	return []autopsy.Evidence{
		{ID: "E1", Kind: "status", Text: "container app state: terminated reason=OOMKilled exitCode=137; restarts: 8; lastState: terminated reason=OOMKilled exitCode=137"},
		{ID: "E2", Kind: "spec", Text: "container app image: busybox:1.36"},
		{ID: "E3", Kind: "spec", Text: "container app limits: memory=64Mi; requests: none"},
		{ID: "E4", Kind: "event", Text: "event Warning BackOff: Back-off restarting failed container (count 3)"},
		{ID: "E5", Kind: "log", Text: "container app current log: starting"},
		{ID: "E6", Kind: "log", Text: "container app previous log: fatal: config not found"},
		{ID: "E7", Kind: "log", Text: "container app previous container logs: no record"},
	}
}

func exitCode(n int32) *int32 { return &n }

func TestRuleSectionsCiteOnlyExistingEvidence(t *testing.T) {
	evSets := map[string][]autopsy.Evidence{
		"none": nil,
		"rich": richEvidence(),
	}
	for _, cause := range ruleCauses {
		for name, ev := range evSets {
			t.Run(string(cause)+"/"+name, func(t *testing.T) {
				in := Input{
					Autopsy:  richPod(),
					Class:    autopsy.Classification{Cause: cause, LastExitCode: exitCode(137)},
					Evidence: ev,
				}
				sec := ruleSections(collectFacts(in))

				ids := make(map[string]bool, len(ev))
				for _, e := range ev {
					ids[e.ID] = true
				}
				for label, text := range map[string]string{
					"summary":     sec.summary,
					"rootCause":   sec.rootCause,
					"remediation": sec.remediation,
					"actions":     sec.actionItems,
				} {
					for _, c := range citeRE.FindAllString(text, -1) {
						if !ids[strings.Trim(c, "[]")] {
							t.Errorf("%s cites %s, which is not in the evidence list", label, c)
						}
					}
				}
				if len(ev) > 0 {
					for label, text := range map[string]string{"summary": sec.summary, "rootCause": sec.rootCause} {
						if !citeRE.MatchString(text) {
							t.Errorf("%s has no citation with %d evidence items: %q", label, len(ev), text)
						}
					}
				} else if citeRE.MatchString(sec.summary + sec.rootCause + sec.actionItems) {
					t.Errorf("text cites evidence although none was recorded")
				}
				if n := strings.Count(sec.actionItems, "- [ ] "); n != 3 {
					t.Errorf("action items = %d, want 3:\n%s", n, sec.actionItems)
				}
				if sec.summary == "" || sec.rootCause == "" || sec.remediation == "" {
					t.Errorf("empty block: %+v", sec)
				}
			})
		}
	}
}

func TestRuleSectionsFactsOnlyWhenPresent(t *testing.T) {
	bare := &autopsy.PodAutopsy{Namespace: "ns", Name: "p"}
	tests := []struct {
		name    string
		cause   autopsy.CauseOfDeath
		pod     *autopsy.PodAutopsy
		exit    *int32
		ev      []autopsy.Evidence
		want    string
		notWant []string
	}{
		{
			name:  "oom with facts",
			cause: autopsy.CauseOOMKilled,
			pod:   richPod(),
			exit:  exitCode(137),
			ev:    richEvidence(),
			want:  "The container was terminated by the kernel out-of-memory killer (reason OOMKilled) (exit code 137, 8 restarts) [E1].",
		},
		{
			name:    "oom without exit code, restarts or limit",
			cause:   autopsy.CauseOOMKilled,
			pod:     bare,
			want:    "The container was terminated by the kernel out-of-memory killer (reason OOMKilled).",
			notWant: []string{"exit code", "restarts", "memory limit of"},
		},
		{
			name:    "image pull without image",
			cause:   autopsy.CauseImagePullBackOff,
			pod:     bare,
			notWant: []string{"(image "},
		},
		{
			name:  "image pull with image",
			cause: autopsy.CauseImagePullBackOff,
			pod:   richPod(),
			ev:    richEvidence(),
			want:  "The container image could not be pulled, so the container never started (image busybox:1.36) [E1].",
		},
		{
			name:  "oom limit from the spec evidence",
			cause: autopsy.CauseOOMKilled,
			pod:   richPod(),
			ev:    richEvidence(),
			want:  "The memory use of the container exceeded the memory limit of 64Mi, so the kernel ended the process [E3][E1].",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := Input{Autopsy: tt.pod, Class: autopsy.Classification{Cause: tt.cause, LastExitCode: tt.exit}, Evidence: tt.ev}
			sec := ruleSections(collectFacts(in))
			all := sec.summary + "\n" + sec.rootCause + "\n" + sec.remediation + "\n" + sec.actionItems
			if tt.want != "" && !strings.Contains(all, tt.want) && !strings.Contains(sec.summary, tt.want) && !strings.Contains(sec.rootCause, tt.want) {
				t.Errorf("want %q in\nsummary: %s\nroot: %s", tt.want, sec.summary, sec.rootCause)
			}
			for _, bad := range tt.notWant {
				if strings.Contains(all, bad) {
					t.Errorf("text contains %q, but the fact is absent:\n%s", bad, all)
				}
			}
		})
	}
}

func TestRuleSectionsCitationChoice(t *testing.T) {
	tests := []struct {
		name  string
		cause autopsy.CauseOfDeath
		want  []string // substrings that must appear
	}{
		{
			name:  "oom cites status and limit spec",
			cause: autopsy.CauseOOMKilled,
			want:  []string{"[E1]", "[E3]", "Raise the memory limit of 64Mi above the observed peak usage [E3]."},
		},
		{
			name:  "crash cites the first error log line, not the plain one",
			cause: autopsy.CauseCrashLoopBackOff,
			want:  []string{"the first error-looking log line records the failure [E6].", "[E6]."},
		},
		{
			name:  "image pull cites the image spec and the warning event",
			cause: autopsy.CauseImagePullBackOff,
			want:  []string{"- [ ] Correct the image name or tag in the workload spec [E2].", "[E4]"},
		},
		{
			name:  "probe cites the warning event",
			cause: autopsy.CauseProbeFailure,
			want:  []string{"after failed probes (exit code 137, 8 restarts) [E4][E1]."},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := Input{
				Autopsy:  richPod(),
				Class:    autopsy.Classification{Cause: tt.cause, LastExitCode: exitCode(137)},
				Evidence: richEvidence(),
			}
			sec := ruleSections(collectFacts(in))
			all := sec.summary + "\n" + sec.rootCause + "\n" + sec.remediation + "\n" + sec.actionItems
			for _, w := range tt.want {
				if !strings.Contains(all, w) {
					t.Errorf("missing %q in:\n%s", w, all)
				}
			}
		})
	}
}

func TestRuleSectionsDeterministic(t *testing.T) {
	in := Input{Autopsy: richPod(), Class: autopsy.Classification{Cause: autopsy.CauseError, LastExitCode: exitCode(1)}, Evidence: richEvidence()}
	first := ruleSections(collectFacts(in))
	for i := 0; i < 20; i++ {
		if got := ruleSections(collectFacts(in)); got != first {
			t.Fatalf("run %d differs from the first run", i)
		}
	}
}
