package resurrect

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"sigs.k8s.io/yaml"
)

// applyBound applies a bound patch to demo/<file> with strategic merge.
func applyBound(t *testing.T, file string, b Bound) appsv1.Deployment {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "demo", file))
	if err != nil {
		t.Fatal(err)
	}
	orig, err := yaml.YAMLToJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	merged, err := strategicpatch.StrategicMergePatch(orig, b.Patch, appsv1.Deployment{})
	if err != nil {
		t.Fatalf("strategic merge: %v\n%s", err, b.Patch)
	}
	var d appsv1.Deployment
	if err := json.Unmarshal(merged, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func inputCode(err error) string {
	var ie *InputError
	if errors.As(err, &ie) {
		return ie.Input + ":" + ie.Code
	}
	return ""
}

func TestOOMActionRaisesLimitOnDemo(t *testing.T) {
	_, sp := castFixture(t, "memory-glutton", "en")
	act := sp.Action
	if act == nil || act.Kind != ActionPatch || act.TargetKind != "Deployment" || act.TargetName != "memory-glutton" || len(act.Inputs) != 0 {
		t.Fatalf("action = %+v", act)
	}
	b, err := act.Bind(nil)
	if err != nil {
		t.Fatal(err)
	}
	if b.Namespace != "seance-demo" || b.ConfigMap != nil || b.Rollback != nil {
		t.Errorf("bound = %+v", b)
	}
	d := applyBound(t, "oom.yaml", b)
	if got := d.Spec.Template.Spec.Containers[0].Resources.Limits.Memory().String(); got != "128Mi" {
		t.Errorf("memory limit = %s, want 128Mi", got)
	}
}

func TestConfigMapMountActionOnDemo(t *testing.T) {
	_, sp := castFixture(t, "restless-spirit", "zh")
	act := sp.Action
	if act == nil || act.Kind != ActionConfigMapMount || act.MountPath != "/etc/app/config.yaml" || act.ConfigKey != "config.yaml" {
		t.Fatalf("action = %+v", act)
	}
	if len(act.Inputs) != 1 || act.Inputs[0].Name != InputConfig || act.Inputs[0].Kind != "textarea" {
		t.Errorf("inputs = %+v", act.Inputs)
	}
	for _, tc := range []struct{ in, want string }{
		{"", "config:missing"},
		{"   \n", "config:missing"},
		{strings.Repeat("x", MaxConfigBytes+1), "config:too_large"},
		{"bad\x00byte", "config:invalid"},
		{"\xff\xfe", "config:invalid"},
	} {
		if _, err := act.Bind(map[string]string{InputConfig: tc.in}); inputCode(err) != tc.want {
			t.Errorf("config %q: err = %v, want %s", tc.in[:min(len(tc.in), 10)], err, tc.want)
		}
	}
	b, err := act.Bind(map[string]string{InputConfig: "mode: calm\r\nlevel: 1\r\n"})
	if err != nil {
		t.Fatal(err)
	}
	if b.ConfigMap == nil || b.ConfigMap.Name != act.ConfigMap || b.ConfigMap.Key != "config.yaml" || b.ConfigMap.Data != "mode: calm\nlevel: 1\n" {
		t.Errorf("configmap = %+v", b.ConfigMap)
	}
	d := applyBound(t, "crashloop.yaml", b)
	ct := d.Spec.Template.Spec.Containers[0]
	if len(ct.VolumeMounts) != 1 || ct.VolumeMounts[0].MountPath != "/etc/app/config.yaml" || ct.VolumeMounts[0].SubPath != "config.yaml" {
		t.Errorf("mounts = %+v", ct.VolumeMounts)
	}
	vols := d.Spec.Template.Spec.Volumes
	if len(vols) != 1 || vols[0].ConfigMap == nil || vols[0].ConfigMap.Name != act.ConfigMap {
		t.Errorf("volumes = %+v", vols)
	}
}

func TestSetImageActionValidatesTag(t *testing.T) {
	_, sp := castFixture(t, "lost-soul", "en")
	act := sp.Action
	if act == nil || act.Kind != ActionSetImage || act.Repo == "" || act.CurrentTag == "" || act.Container == "" {
		t.Fatalf("action = %+v", act)
	}
	for _, tc := range []struct{ tag, want string }{
		{"", "tag:missing"},
		{"1.0; rm -rf /", "tag:invalid"},
		{"-leading", "tag:invalid"},
		{"a/b", "tag:invalid"},
		{"$(id)", "tag:invalid"},
		{strings.Repeat("a", 129), "tag:invalid"},
		{act.CurrentTag, "tag:same"},
	} {
		if _, err := act.Bind(map[string]string{InputTag: tc.tag}); inputCode(err) != tc.want {
			t.Errorf("tag %q: err = %v, want %s", tc.tag, err, tc.want)
		}
	}
	b, err := act.Bind(map[string]string{InputTag: " 1.36 "})
	if err != nil {
		t.Fatal(err)
	}
	var p struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers []map[string]string `json:"containers"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(b.Patch, &p); err != nil {
		t.Fatal(err)
	}
	cs := p.Spec.Template.Spec.Containers
	if len(cs) != 1 || cs[0]["name"] != act.Container || cs[0]["image"] != act.Repo+":1.36" || len(cs[0]) != 2 {
		t.Errorf("patch = %s", b.Patch)
	}
}

func TestRollbackActionOnlyForDeployments(t *testing.T) {
	sp := castOf(t, brokenRelease(), "en")
	act := sp.Action
	if act == nil || act.Kind != ActionRollback || act.TargetName != "api" || act.HashLabel != "pod-template-hash" || act.Revision != "aaaaaaaaa" {
		t.Fatalf("action = %+v", act)
	}
	b, err := act.Bind(nil)
	if err != nil {
		t.Fatal(err)
	}
	if b.Rollback == nil || b.Rollback.Revision != "aaaaaaaaa" || b.Patch != nil {
		t.Errorf("bound = %+v", b)
	}
	if sp := castOf(t, brokenStatefulSet(), "en"); sp.Action != nil {
		t.Errorf("StatefulSet rollback action = %+v, want read-only", sp.Action)
	}
}

func TestProbeActionTakesTiming(t *testing.T) {
	sp := castOf(t, probePod(`Liveness probe failed: Get "http://10.244.0.9:8080/healthz": context deadline exceeded`), "en")
	act := sp.Action
	if act == nil || act.Kind != ActionPatch || act.Probe != "livenessProbe" || len(act.Inputs) != 2 {
		t.Fatalf("action = %+v", act)
	}
	for _, tc := range []struct {
		in   map[string]string
		want string
	}{
		{map[string]string{}, "timeoutSeconds:missing"},
		{map[string]string{InputTimeoutSeconds: "x", InputFailureThreshold: "3"}, "timeoutSeconds:invalid"},
		{map[string]string{InputTimeoutSeconds: "0", InputFailureThreshold: "3"}, "timeoutSeconds:range"},
		{map[string]string{InputTimeoutSeconds: "301", InputFailureThreshold: "3"}, "timeoutSeconds:range"},
		{map[string]string{InputTimeoutSeconds: "5", InputFailureThreshold: "101"}, "failureThreshold:range"},
	} {
		if _, err := act.Bind(tc.in); inputCode(err) != tc.want {
			t.Errorf("%v: err = %v, want %s", tc.in, err, tc.want)
		}
	}
	b, err := act.Bind(map[string]string{InputTimeoutSeconds: "10", InputFailureThreshold: "6"})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"spec":{"template":{"spec":{"containers":[{"livenessProbe":{"failureThreshold":6,"timeoutSeconds":10},"name":"api"}]}}}}`
	if string(b.Patch) != want {
		t.Errorf("patch = %s, want %s", b.Patch, want)
	}
}

func TestReadOnlySpellsHaveNoAction(t *testing.T) {
	castable := map[string]bool{
		"oom-limit-statefulset": true, "missing-file": true, "image-missing": true,
		"probe-timeout": true, "release-broke-it": true,
	}
	for _, s := range synthetics() {
		sp := castOf(t, s.pod, "en")
		if got := sp.Action != nil; got != castable[s.name] {
			t.Errorf("%s (%s): castable = %v, want %v", s.name, sp.Remedy, got, castable[s.name])
		}
		if sp.Action != nil && !castableKinds[sp.Action.TargetKind] {
			t.Errorf("%s: action target %s", s.name, sp.Action.TargetKind)
		}
	}
}

func TestBindRejectsForeignTargets(t *testing.T) {
	good := Action{Kind: ActionPatch, TargetKind: "Deployment", TargetName: "api", Namespace: "shop", Patch: "spec: {}\n"}
	if _, err := good.Bind(nil); err != nil {
		t.Fatalf("good action: %v", err)
	}
	for name, mut := range map[string]func(a *Action){
		"pod":            func(a *Action) { a.TargetKind = "Pod" },
		"job":            func(a *Action) { a.TargetKind = "Job" },
		"bad namespace":  func(a *Action) { a.Namespace = "../kube-system" },
		"bad name":       func(a *Action) { a.TargetName = "API/x" },
		"unknown kind":   func(a *Action) { a.Kind = "delete" },
		"empty patch":    func(a *Action) { a.Patch = "" },
		"rollback no rs": func(a *Action) { a.Kind = ActionRollback },
	} {
		a := good
		mut(&a)
		if _, err := a.Bind(nil); err == nil {
			t.Errorf("%s: Bind succeeded", name)
		}
	}
	var nilAction *Action
	if _, err := nilAction.Bind(nil); err == nil {
		t.Error("nil action bound")
	}
}

func TestFingerprintFollowsInputs(t *testing.T) {
	_, sp := castFixture(t, "lost-soul", "en")
	a, _ := sp.Action.Bind(map[string]string{InputTag: "1.36"})
	b, _ := sp.Action.Bind(map[string]string{InputTag: "1.36"})
	c, _ := sp.Action.Bind(map[string]string{InputTag: "1.37"})
	if a.Fingerprint() != b.Fingerprint() || a.Fingerprint() == c.Fingerprint() || len(a.Fingerprint()) != 64 {
		t.Errorf("fingerprints %s %s %s", a.Fingerprint(), b.Fingerprint(), c.Fingerprint())
	}
}
