package resurrect

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/yaml"

	"github.com/zhangjuji/k8s-seance/internal/autopsy"
)

// brokenRelease is the second-life demo: version bbb crashes, the older
// version aaa still has two living Pods.
func brokenRelease() *autopsy.PodAutopsy {
	a := crashPod("api-bbbbbbbbb", "ReplicaSet", "FATAL: unknown mode turbo")
	a.Lineage = &autopsy.Lineage{
		OwnerKind: "Deployment", OwnerName: "api", Inferred: true, ViaReplicaSet: "api-bbbbbbbbb",
		HashLabel: "pod-template-hash", Revision: "bbbbbbbbb",
		Self: autopsy.Kin{Name: a.Name, Node: "node-1", State: "CrashLoopBackOff"},
		Versions: []autopsy.Version{{
			Revision: "aaaaaaaaa", Age: autopsy.AgeOlder,
			Pods: []autopsy.Kin{{Name: "api-aaaaaaaaa-1", Alive: true}, {Name: "api-aaaaaaaaa-2", Alive: true}},
			Diffs: []autopsy.SpecDiff{
				{Field: "container api env MODE", From: `"normal"`, To: `"turbo"`},
				{Field: "container api env DB_PASSWORD", Hidden: true},
				{Field: "container api image", From: "api:1", To: "api:2"},
				{Field: "container api args", From: "[]", To: "[\"-v\"]"},
			},
		}},
	}
	return a
}

func brokenStatefulSet() *autopsy.PodAutopsy {
	a := crashPod("db", "StatefulSet", "panic: bad flag")
	a.Name = "db-1"
	a.Lineage = &autopsy.Lineage{
		OwnerKind: "StatefulSet", OwnerName: "db", HashLabel: "controller-revision-hash", Revision: "db-v2",
		Self:     autopsy.Kin{Name: "db-1"},
		Versions: []autopsy.Version{{Revision: "db-v1", Age: autopsy.AgeOlder, Pods: []autopsy.Kin{{Name: "db-0", Alive: true}}}},
	}
	return a
}

func TestRollbackFirstWhenTheReleaseBrokeIt(t *testing.T) {
	for _, lang := range []string{"zh", "en"} {
		a := brokenRelease()
		sp := castOf(t, a, lang)
		if sp.Remedy != RemedyRollback {
			t.Fatalf("%s: remedy = %q, want %q", lang, sp.Remedy, RemedyRollback)
		}
		if sp.Target.Kind != "Deployment" || sp.Target.Name != "api" || !sp.Target.Inferred || !sp.Target.InPlace {
			t.Errorf("%s: target = %+v", lang, sp.Target)
		}
		if len(sp.Steps) != 3 || sp.Steps[0].Kind != StepCheck || sp.Steps[1].Kind != StepCast || sp.Steps[2].Kind != StepVerify {
			t.Fatalf("%s: steps = %+v, want check, cast, verify", lang, sp.Steps)
		}
		cast := sp.Steps[1].Commands
		want := []string{"kubectl rollout undo deployment/api -n shop --dry-run=server", "kubectl rollout undo deployment/api -n shop"}
		if strings.Join(cast, "\n") != strings.Join(want, "\n") {
			t.Errorf("%s: cast = %q, want %q", lang, cast, want)
		}
		checks := strings.Join(sp.Steps[0].Commands, "\n")
		if !strings.Contains(checks, "kubectl rollout history deployment/api -n shop") ||
			!strings.Contains(checks, "kubectl get replicasets -n shop -l pod-template-hash=aaaaaaaaa -o wide") {
			t.Errorf("%s: checks = %s", lang, checks)
		}
		if sp.Patch != "" {
			t.Errorf("%s: a rollback has no patch file", lang)
		}
		diag := strings.Join(sp.Diagnosis, "\n")
		ev, _ := autopsy.BuildEvidence(a, autopsy.EvidenceOptions{})
		var famID, verID string
		for _, e := range ev {
			if e.Kind == "lineage" && strings.HasPrefix(e.Text, "family ") {
				famID = e.ID
			}
			if e.Kind == "lineage" && strings.HasPrefix(e.Text, "revision ") {
				verID = e.ID
			}
		}
		if famID == "" || !strings.Contains(diag, "["+famID+"]") || !strings.Contains(diag, "["+verID+"]") {
			t.Errorf("%s: diagnosis does not cite the family %s and the revision %s:\n%s", lang, famID, verID, diag)
		}
		if !strings.Contains(diag, `MODE: "normal" -> "turbo"`) || !strings.Contains(diag, "DB_PASSWORD: changed (value hidden)") {
			t.Errorf("%s: diagnosis does not name the differences:\n%s", lang, diag)
		}
		if strings.Contains(diag, "args") {
			t.Errorf("%s: diagnosis names more than %d differences:\n%s", lang, maxRollbackDiffs, diag)
		}
		caveats := strings.Join(sp.Caveats, "\n")
		stop := map[string]string{"zh": "回滚只是止血", "en": "A rollback only stops the bleeding"}[lang]
		if !strings.Contains(caveats, stop) {
			t.Errorf("%s: caveats do not say the rollback only stops the bleeding:\n%s", lang, caveats)
		}
	}
}

func TestRollbackStatefulSetChecksControllerRevisions(t *testing.T) {
	sp := castOf(t, brokenStatefulSet(), "en")
	if sp.Remedy != RemedyRollback || sp.Target.Kind != "StatefulSet" || sp.Target.Inferred {
		t.Fatalf("spell = %s %+v", sp.Remedy, sp.Target)
	}
	if got := sp.Steps[1].Commands[1]; got != "kubectl rollout undo statefulset/db -n shop" {
		t.Errorf("cast = %q", got)
	}
	if !strings.Contains(strings.Join(sp.Steps[0].Commands, "\n"), "kubectl get controllerrevisions -n shop") {
		t.Errorf("checks = %q", sp.Steps[0].Commands)
	}
	if !strings.Contains(strings.Join(sp.Diagnosis, "\n"), "do not differ") {
		t.Errorf("diagnosis = %q", sp.Diagnosis)
	}
}

func TestNoRollbackUnlessAnOlderRevisionLives(t *testing.T) {
	cases := map[string]func(*autopsy.Lineage){
		"newer revision lives": func(l *autopsy.Lineage) { l.Versions[0].Age = autopsy.AgeNewer },
		"older revision dead": func(l *autopsy.Lineage) {
			for i := range l.Versions[0].Pods {
				l.Versions[0].Pods[i].Alive = false
			}
		},
		"living sibling": func(l *autopsy.Lineage) { l.Siblings = []autopsy.Kin{{Name: "api-bbbbbbbbb-2", Alive: true}} },
	}
	for name, change := range cases {
		a := brokenRelease()
		change(a.Lineage)
		if sp := castOf(t, a, "en"); sp.Remedy == RemedyRollback {
			t.Errorf("%s: remedy = rollback", name)
		}
	}
	// A bare Pod cannot be rolled back, whatever the lineage says.
	a := brokenRelease()
	a.OwnerKind, a.OwnerName = "", ""
	a.Lineage.OwnerKind = "Deployment"
	sp := castOf(t, a, "en")
	if sp.Remedy == RemedyRollback {
		t.Error("bare Pod: remedy = rollback")
	}
}

// TestSecondLifeDemoDiesOnlyInTurboMode runs the demo's start script: MODE=normal
// stays up and becomes ready, MODE=turbo exits 1 with a FATAL line.
func TestSecondLifeDemoDiesOnlyInTurboMode(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", "demo", "second-life.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var d appsv1.Deployment
	if err := yaml.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	ru := d.Spec.Strategy.RollingUpdate
	if ru == nil || ru.MaxUnavailable == nil || ru.MaxUnavailable.IntValue() != 0 {
		t.Fatalf("maxUnavailable = %v, want 0 so the old Pods stay alive", ru)
	}
	if d.Spec.Replicas == nil || *d.Spec.Replicas < 1 {
		t.Fatalf("replicas = %v", d.Spec.Replicas)
	}
	c := onlyContainer(t, d)
	if len(c.Env) != 1 || c.Env[0].Name != "MODE" || c.Env[0].Value != "normal" {
		t.Fatalf("env = %+v, want MODE=normal in version 1", c.Env)
	}
	if c.ReadinessProbe == nil || c.ReadinessProbe.Exec == nil {
		t.Fatal("no readiness probe: a crashing Pod could count as available")
	}
	if len(c.Command) != 3 || c.Command[0] != "sh" || c.Command[1] != "-c" {
		t.Fatalf("command = %q, want sh -c <script>", c.Command)
	}
	dir := t.TempDir()
	ready := filepath.Join(dir, "ready")
	script := strings.ReplaceAll(c.Command[2], "/tmp/ready", ready)
	script = strings.ReplaceAll(script, "while true; do sleep 3600; done", "exit 0")
	if script == c.Command[2] {
		t.Fatal("script changed nothing")
	}
	run := func(mode string) (string, int) {
		cmd := exec.Command(sh, "-c", script)
		cmd.Env = append(os.Environ(), "MODE="+mode)
		out, err := cmd.CombinedOutput()
		return string(out), exitCode(err)
	}
	out, code := run("normal")
	if code != 0 {
		t.Fatalf("MODE=normal: exit %d\n%s", code, out)
	}
	if _, err := os.Stat(ready); err != nil {
		t.Errorf("MODE=normal did not create the readiness file: %v", err)
	}
	_ = os.Remove(ready)
	out, code = run("turbo")
	if code != 1 || !strings.Contains(out, "FATAL: unknown mode turbo") {
		t.Errorf("MODE=turbo: exit %d, output %q; want exit 1 with a FATAL line", code, out)
	}
	if _, err := os.Stat(ready); err == nil {
		t.Error("MODE=turbo became ready")
	}

	// kill-them-all.sh deploys it and then releases the bad version.
	script2, err := os.ReadFile(filepath.Join("..", "..", "demo", "kill-them-all.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`second-life.yaml`, `kubectl rollout status deployment/second-life`, `kubectl set env deployment/second-life -n "${NAMESPACE}" MODE=turbo`} {
		if !strings.Contains(string(script2), want) {
			t.Errorf("kill-them-all.sh does not contain %q", want)
		}
	}
}
