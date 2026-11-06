package graveyard

import (
	"strings"
	"testing"

	"github.com/zhangjuji/k8s-seance/fixtures"
	"github.com/zhangjuji/k8s-seance/internal/autopsy"
)

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

func offeringsOf(t *testing.T, a *autopsy.PodAutopsy) ([]Offering, []autopsy.Evidence) {
	t.Helper()
	ev, err := autopsy.BuildEvidence(a, autopsy.EvidenceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return OfferingsFor(a, autopsy.Classify(a), ev), ev
}

// evidenceText returns the text of evidence id.
func evidenceText(ev []autopsy.Evidence, id string) string {
	for _, e := range ev {
		if e.ID == id {
			return e.Text
		}
	}
	return ""
}

func kinds(offs []Offering) string {
	k := make([]string, len(offs))
	for i, o := range offs {
		k[i] = o.Kind
	}
	return strings.Join(k, ",")
}

func TestOfferingsForFixtures(t *testing.T) {
	tests := []struct {
		pod   string
		kinds string
		// texts must appear in the offering of the same index.
		texts []string
	}{
		{"memory-glutton-7cc6df9966-bf87c", "death,last_words,limit", []string{"OOMKilled, exit code 137", "", "memory limit: 64Mi"}},
		{"restless-spirit-84fc8b7c4c-52nl8", "death,last_words,warning", []string{"CrashLoopBackOff, exit code 1", "FATAL: config /etc/app/config.yaml not found", "Warning BackOff"}},
		{"lost-soul-5ff5cb8464-hjtms", "death,image,warning", []string{"ImagePullBackOff, no exit code", "nginx:does-not-exist-666", "Warning Failed"}},
	}
	for _, tt := range tests {
		t.Run(tt.pod, func(t *testing.T) {
			offs, ev := offeringsOf(t, fixture(t, tt.pod))
			if got := kinds(offs); got != tt.kinds {
				t.Fatalf("kinds = %s, want %s (%+v)", got, tt.kinds, offs)
			}
			for i, o := range offs {
				if !strings.Contains(o.Text, tt.texts[i]) {
					t.Errorf("offering %d = %q, want it to contain %q", i, o.Text, tt.texts[i])
				}
				// Every offering names the evidence it came from, and that evidence says the same.
				if o.Evidence == "" {
					t.Errorf("offering %d (%s) has no evidence ID", i, o.Kind)
					continue
				}
				src := evidenceText(ev, o.Evidence)
				if src == "" {
					t.Errorf("offering %d cites %s, which is not in the evidence", i, o.Evidence)
				}
				if o.Kind == OfferingLastWords && !strings.HasSuffix(src, o.Text) {
					t.Errorf("last words %q are not the line of %s (%q)", o.Text, o.Evidence, src)
				}
			}
		})
	}
}

func exit(c int32) *int32 { return &c }

// regressionAutopsy is a crash-looping Pod whose older revision is alive.
func regressionAutopsy() *autopsy.PodAutopsy {
	return &autopsy.PodAutopsy{
		Namespace: "seance-demo", Name: "second-life-new-a", OwnerKind: "ReplicaSet", OwnerName: "second-life-new",
		Containers: []autopsy.ContainerAutopsy{{
			Name: "app", Image: "busybox", RestartCount: 3,
			State:           autopsy.ContainerState{Kind: "waiting", Reason: "CrashLoopBackOff"},
			LastState:       &autopsy.ContainerState{Kind: "terminated", Reason: "Error", ExitCode: exit(1)},
			PreviousLogTail: []string{"starting", "mode turbo: unsupported, error", "bye password=hunter2"},
		}},
		Events: []autopsy.EventRecord{{Type: "Normal", Reason: "Pulled", Message: "ok"}, {Type: "Warning", Reason: "BackOff", Message: "back-off"}},
		Lineage: &autopsy.Lineage{
			OwnerKind: "Deployment", OwnerName: "second-life", HashLabel: "pod-template-hash", Revision: "new",
			Self: autopsy.Kin{Name: "second-life-new-a"},
			Versions: []autopsy.Version{{
				Revision: "old", Age: autopsy.AgeOlder,
				Pods:  []autopsy.Kin{{Name: "second-life-old-a", Alive: true}},
				Diffs: []autopsy.SpecDiff{{Field: "container app env MODE", From: "normal", To: "turbo"}, {Field: "container app image", From: "busybox:1", To: "busybox:2"}},
			}},
		},
	}
}

func TestOfferingsRegressionKeepsTheChange(t *testing.T) {
	a := regressionAutopsy()
	offs, ev := offeringsOf(t, a)
	if got := kinds(offs); got != "death,last_words,change" {
		t.Fatalf("kinds = %s, want death,last_words,change (%+v)", got, offs)
	}
	if offs[1].Text != "mode turbo: unsupported, error" {
		t.Errorf("last words = %q, want the first key line", offs[1].Text)
	}
	ch := offs[2]
	if ch.Text != "container app env MODE: normal -> turbo" {
		t.Errorf("change = %q", ch.Text)
	}
	if src := evidenceText(ev, ch.Evidence); !strings.HasPrefix(src, "revision pod-template-hash=old") {
		t.Errorf("change cites %s = %q, want the old revision's lineage item", ch.Evidence, src)
	}
	if len(offs) > MaxOfferings {
		t.Errorf("%d offerings, want at most %d", len(offs), MaxOfferings)
	}

	// Without a living former life there is no change; the first Warning fills in.
	a.Lineage.Versions[0].Pods[0].Alive = false
	offs, _ = offeringsOf(t, a)
	if got := kinds(offs); got != "death,last_words,warning" {
		t.Errorf("family verdict: kinds = %s", got)
	}
}

func TestOfferingsAreRedacted(t *testing.T) {
	a := regressionAutopsy()
	a.Lineage = nil
	a.Containers[0].PreviousLogTail = []string{"connecting with token=s3cr3t-value"}
	offs, _ := offeringsOf(t, a)
	for _, o := range offs {
		if strings.Contains(o.Text, "s3cr3t-value") {
			t.Errorf("offering %s leaks the secret: %q", o.Kind, o.Text)
		}
	}
	if offs[1].Kind != OfferingLastWords || !strings.Contains(offs[1].Text, "[REDACTED]") {
		t.Errorf("last words = %+v, want the redacted line", offs[1])
	}
	// A hidden lineage value stays hidden. The text follows the evidence
	// redaction, which also masks the word after "TOKEN:".
	d := autopsy.SpecDiff{Field: "container app env API_TOKEN", Hidden: true}
	a = regressionAutopsy()
	a.Lineage.Versions[0].Diffs = []autopsy.SpecDiff{d}
	offs, ev := offeringsOf(t, a)
	if !strings.HasPrefix(offs[2].Text, "container app env API_TOKEN: ") || !strings.HasSuffix(offs[2].Text, "(value hidden)") {
		t.Errorf("hidden change = %q", offs[2].Text)
	}
	if src := evidenceText(ev, offs[2].Evidence); !strings.Contains(src, offs[2].Text) {
		t.Errorf("hidden change %q differs from its evidence %q", offs[2].Text, src)
	}
}

func TestOfferingsEdgeCases(t *testing.T) {
	if OfferingsFor(nil, autopsy.Classification{}, nil) != nil {
		t.Error("nil autopsy has offerings")
	}
	// No containers and no evidence: only how it died, without an evidence ID.
	offs := OfferingsFor(&autopsy.PodAutopsy{Name: "x"}, autopsy.Classification{}, nil)
	if len(offs) != 1 || offs[0].Kind != OfferingDeath || offs[0].Text != "Unknown, no exit code" || offs[0].Evidence != "" {
		t.Errorf("empty autopsy: %+v", offs)
	}
	// A long line is clipped.
	a := regressionAutopsy()
	a.Containers[0].PreviousLogTail = []string{"fatal " + strings.Repeat("x", 500)}
	offs, _ = offeringsOf(t, a)
	if n := len([]rune(offs[1].Text)); n != maxOfferingRunes {
		t.Errorf("clipped length = %d, want %d", n, maxOfferingRunes)
	}
}
