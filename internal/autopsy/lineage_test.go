package autopsy

import (
	"fmt"
	"strings"
	"testing"
)

func regressionLineage() *Lineage {
	return &Lineage{
		OwnerKind: "Deployment", OwnerName: "second-life", Inferred: true, ViaReplicaSet: "second-life-bbb",
		HashLabel: "pod-template-hash", Revision: "bbb",
		Self: Kin{Name: "second-life-bbb-1", Node: "kind-worker", State: "CrashLoopBackOff"},
		Versions: []Version{{
			Revision: "aaa", Age: AgeOlder,
			Pods: []Kin{
				{Name: "second-life-aaa-1", Node: "kind-worker", Alive: true},
				{Name: "second-life-aaa-2", Node: "kind-worker2", Alive: true},
			},
			Diffs: []SpecDiff{{Field: "container app env MODE", From: `"normal"`, To: `"turbo"`}, {Field: "container app env DB_PASSWORD", Hidden: true}},
		}},
	}
}

func TestLineageVerdictOrder(t *testing.T) {
	alive := Kin{Name: "a", Alive: true}
	dead := Kin{Name: "d"}
	tests := []struct {
		name string
		l    *Lineage
		want string
	}{
		{"nil", nil, ""},
		{"alone", &Lineage{}, VerdictAlone},
		{"instance", &Lineage{Siblings: []Kin{dead, alive}}, VerdictInstance},
		{"instance beats regression", &Lineage{Siblings: []Kin{alive}, Versions: []Version{{Pods: []Kin{alive}}}}, VerdictInstance},
		{"regression", &Lineage{Siblings: []Kin{dead}, Versions: []Version{{Pods: []Kin{dead, alive}}}}, VerdictRegression},
		{"family", &Lineage{Siblings: []Kin{dead}, Versions: []Version{{Pods: []Kin{dead}}}}, VerdictFamily},
		{"family without siblings", &Lineage{Versions: []Version{{Pods: []Kin{dead}}}}, VerdictFamily},
	}
	for _, tt := range tests {
		if got := tt.l.Verdict(); got != tt.want {
			t.Errorf("%s: verdict = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestLivingFormerLifePicksTheNewestOlderRevision(t *testing.T) {
	alive := []Kin{{Alive: true}}
	l := &Lineage{Versions: []Version{
		{Revision: "v1", Age: AgeOlder, Pods: alive},
		{Revision: "v2", Age: AgeOlder, Pods: alive},
		{Revision: "v4", Age: AgeNewer, Pods: alive},
	}}
	if v := l.LivingFormerLife(); v == nil || v.Revision != "v2" {
		t.Errorf("former life = %+v, want v2", v)
	}
	l.Versions = l.Versions[2:]
	if v := l.LivingFormerLife(); v != nil {
		t.Errorf("former life = %+v, want nil when only a newer revision lives", v)
	}
}

func TestLineageTexts(t *testing.T) {
	got := LineageTexts(regressionLineage())
	want := []string{
		"family Deployment/second-life (inferred from ReplicaSet second-life-bbb): verdict=regression (another revision has living Pods and none of this revision is alive, so the release broke it); this revision pod-template-hash=bbb: alive 0/1 (this Pod second-life-bbb-1 on node kind-worker, dead CrashLoopBackOff); siblings: none",
		`revision pod-template-hash=aaa (older, former life): alive 2/2; pods: second-life-aaa-1 on node kind-worker (alive), second-life-aaa-2 on node kind-worker2 (alive); differs from this revision: container app env MODE: "normal" -> "turbo"; container app env DB_PASSWORD: changed (value hidden)`,
	}
	if len(got) != len(want) {
		t.Fatalf("texts = %q", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("text %d =\n%s\nwant\n%s", i, got[i], want[i])
		}
	}
	if LineageTexts(nil) != nil {
		t.Error("nil lineage must give no evidence")
	}
}

func TestLineageTextsCapsDiffsAndPods(t *testing.T) {
	l := regressionLineage()
	v := &l.Versions[0]
	v.Diffs = nil
	for i := range 11 {
		v.Diffs = append(v.Diffs, SpecDiff{Field: fmt.Sprintf("f%d", i), From: "a", To: "b"})
	}
	for i := range 6 {
		v.Pods = append(v.Pods, Kin{Name: fmt.Sprintf("extra-%d", i)})
	}
	text := LineageTexts(l)[1]
	if !strings.Contains(text, "f7: a -> b; (+3 more)") || strings.Contains(text, "f8:") {
		t.Errorf("diffs not capped at 8: %s", text)
	}
	if !strings.Contains(text, "+3 more") || strings.Contains(text, "extra-3") {
		t.Errorf("pods not capped at 5: %s", text)
	}
}

func TestBuildEvidencePutsLineageBetweenEventsAndLogs(t *testing.T) {
	a := &PodAutopsy{
		Name: "p",
		Containers: []ContainerAutopsy{{
			Name: "app", Image: "busybox", LogTail: []string{"FATAL boom"}, PreviousLogTail: []string{},
		}},
		Events:  []EventRecord{{Type: "Warning", Reason: "BackOff", Message: "back-off", Count: 3}},
		Lineage: regressionLineage(),
	}
	a.Lineage.Versions[0].Diffs = append(a.Lineage.Versions[0].Diffs, SpecDiff{Field: "container app args", From: "--token=abc123", To: "--token=def456"})
	ev, err := BuildEvidence(a, EvidenceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, e := range ev {
		kinds = append(kinds, e.Kind)
	}
	want := "status spec spec event lineage lineage log"
	if got := strings.Join(kinds, " "); got != want {
		t.Errorf("kinds = %s, want %s", got, want)
	}
	for _, e := range ev {
		if strings.Contains(e.Text, "abc123") || strings.Contains(e.Text, "def456") {
			t.Errorf("lineage evidence not redacted: %s", e.Text)
		}
	}
}

func TestEnvValue(t *testing.T) {
	tests := []struct {
		name, value string
		shown       string
		hidden      bool
	}{
		{"MODE", "turbo", "turbo", false},
		{"DB_PASSWORD", "hunter2", "", true},
		{"GITHUB_TOKEN", "x", "", true},
		{"API_KEY", "x", "", true},
		{"CLIENT_SECRET", "x", "", true},
		{"DSN", "postgres://u@h?password=abc", "", true},
		{"HEADER", "Bearer abc.def", "", true},
		{"AWS_ID", "AKIAABCDEFGHIJKLMNOP", "", true},
		{"LOG_LEVEL", "debug", "debug", false},
	}
	for _, tt := range tests {
		shown, hidden := EnvValue(tt.name, tt.value)
		if shown != tt.shown || hidden != tt.hidden {
			t.Errorf("EnvValue(%s, %s) = %q, %v; want %q, %v", tt.name, tt.value, shown, hidden, tt.shown, tt.hidden)
		}
	}
}
