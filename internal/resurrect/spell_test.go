package resurrect

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"unicode"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"sigs.k8s.io/yaml"

	"github.com/zhangjuji/k8s-seance/fixtures"
	"github.com/zhangjuji/k8s-seance/internal/autopsy"
)

// castFixture loads one recorded spirit by Pod name prefix and casts its spell in lang.
func castFixture(t *testing.T, prefix, lang string) (*autopsy.PodAutopsy, Spell) {
	t.Helper()
	all, err := fixtures.Load()
	if err != nil {
		t.Fatalf("fixtures.Load: %v", err)
	}
	for _, a := range all {
		if strings.HasPrefix(a.Name, prefix) {
			return a, castOf(t, a, lang)
		}
	}
	t.Fatalf("no fixture %s*", prefix)
	return nil, Spell{}
}

func castOf(t *testing.T, a *autopsy.PodAutopsy, lang string) Spell {
	t.Helper()
	ev, err := autopsy.BuildEvidence(a, autopsy.EvidenceOptions{})
	if err != nil {
		t.Fatalf("BuildEvidence: %v", err)
	}
	return Cast(a, autopsy.Classify(a), ev, lang)
}

// applyToDemo applies the spell's patch to demo/<file> with strategic merge and
// returns the original and the patched Deployment.
func applyToDemo(t *testing.T, file string, sp Spell) (before, after appsv1.Deployment) {
	t.Helper()
	if sp.Patch == "" {
		t.Fatalf("spell for %s has no patch", sp.Pod)
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", "demo", file))
	if err != nil {
		t.Fatalf("read demo manifest: %v", err)
	}
	orig, err := yaml.YAMLToJSON(raw)
	if err != nil {
		t.Fatalf("demo/%s to JSON: %v", file, err)
	}
	patch, err := yaml.YAMLToJSON([]byte(sp.Patch))
	if err != nil {
		t.Fatalf("patch to JSON: %v\n%s", err, sp.Patch)
	}
	merged, err := strategicpatch.StrategicMergePatch(orig, patch, appsv1.Deployment{})
	if err != nil {
		t.Fatalf("strategic merge: %v", err)
	}
	if err := json.Unmarshal(orig, &before); err != nil {
		t.Fatalf("decode original: %v", err)
	}
	if err := json.Unmarshal(merged, &after); err != nil {
		t.Fatalf("decode merged: %v", err)
	}
	return before, after
}

func onlyContainer(t *testing.T, d appsv1.Deployment) corev1.Container {
	t.Helper()
	cs := d.Spec.Template.Spec.Containers
	if len(cs) != 1 {
		t.Fatalf("%d containers after the patch, want 1 (the patch must merge by container name)", len(cs))
	}
	return cs[0]
}

func TestOOMPatchRaisesLimitOnDemoManifest(t *testing.T) {
	_, sp := castFixture(t, "memory-glutton", "en")
	if sp.Remedy != RemedyRaiseMemory {
		t.Fatalf("remedy = %q, want %q", sp.Remedy, RemedyRaiseMemory)
	}
	if sp.Target.Kind != "Deployment" || sp.Target.Name != "memory-glutton" || !sp.Target.Inferred || !sp.Target.InPlace {
		t.Fatalf("target = %+v, want inferred in-place Deployment memory-glutton", sp.Target)
	}
	before, after := applyToDemo(t, "oom.yaml", sp)
	b, a := onlyContainer(t, before), onlyContainer(t, after)

	if got := a.Resources.Limits.Memory().String(); got != "128Mi" {
		t.Errorf("memory limit = %s, want 128Mi (64Mi doubled)", got)
	}
	if !reflect.DeepEqual(a.Resources.Requests, b.Resources.Requests) {
		t.Errorf("requests changed: %v -> %v", b.Resources.Requests, a.Resources.Requests)
	}
	if !a.Resources.Limits.Cpu().Equal(*b.Resources.Limits.Cpu()) {
		t.Errorf("cpu limit changed: %s -> %s", b.Resources.Limits.Cpu(), a.Resources.Limits.Cpu())
	}
	a.Resources, b.Resources = corev1.ResourceRequirements{}, corev1.ResourceRequirements{}
	if !reflect.DeepEqual(a, b) {
		t.Errorf("the patch changed more than resources:\nbefore %+v\nafter  %+v", b, a)
	}

	// The demo really dies at 64Mi and really lives at 128Mi.
	var hog int64
	for _, e := range b.Env {
		if e.Name == "HOG_BYTES" {
			q := resource.MustParse(e.Value)
			hog = q.Value()
		}
	}
	if hog == 0 {
		t.Fatal("demo/oom.yaml has no HOG_BYTES")
	}
	interpreter := int64(10 << 20)
	oldLimit := before.Spec.Template.Spec.Containers[0].Resources.Limits.Memory().Value()
	newLimit := after.Spec.Template.Spec.Containers[0].Resources.Limits.Memory().Value()
	if hog <= oldLimit {
		t.Errorf("HOG_BYTES %d fits in the old limit %d, so the demo would not die", hog, oldLimit)
	}
	if hog+interpreter >= newLimit {
		t.Errorf("HOG_BYTES %d plus the interpreter does not fit in the new limit %d, so the spell would not revive it", hog, newLimit)
	}
}

func TestCrashloopPatchMountsConfigOnDemoManifest(t *testing.T) {
	_, sp := castFixture(t, "restless-spirit", "en")
	if sp.Remedy != RemedyMountConfig {
		t.Fatalf("remedy = %q, want %q", sp.Remedy, RemedyMountConfig)
	}
	before, after := applyToDemo(t, "crashloop.yaml", sp)
	b, a := onlyContainer(t, before), onlyContainer(t, after)

	if len(a.VolumeMounts) != 1 {
		t.Fatalf("volumeMounts = %+v, want one", a.VolumeMounts)
	}
	m := a.VolumeMounts[0]
	if m.MountPath != "/etc/app/config.yaml" || m.SubPath != "config.yaml" || !m.ReadOnly {
		t.Errorf("mount = %+v, want /etc/app/config.yaml subPath config.yaml readOnly", m)
	}
	vols := after.Spec.Template.Spec.Volumes
	if len(vols) != 1 || vols[0].Name != m.Name || vols[0].ConfigMap == nil {
		t.Fatalf("volumes = %+v, want one ConfigMap volume named %s", vols, m.Name)
	}
	cm := vols[0].ConfigMap.Name
	if cm != "restless-spirit-config" {
		t.Errorf("ConfigMap = %q, want restless-spirit-config", cm)
	}
	if !strings.Contains(strings.Join(allCommands(sp), "\n"), "kubectl create configmap "+cm+" --from-file=config.yaml=") {
		t.Errorf("no command creates ConfigMap %s with key config.yaml", cm)
	}
	if !reflect.DeepEqual(a.Command, b.Command) || a.Image != b.Image {
		t.Errorf("the patch changed the command or the image")
	}
}

// TestCrashloopDemoChecksTheMountedFile runs the demo's start script with the
// config path moved into a temp dir: without the file it prints the FATAL line
// of the recording and exits 1, with the file it stays up.
func TestCrashloopDemoChecksTheMountedFile(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	_, sp := castFixture(t, "restless-spirit", "en")
	before, _ := applyToDemo(t, "crashloop.yaml", sp)
	cmd := onlyContainer(t, before).Command
	if len(cmd) != 3 || cmd[0] != "sh" || cmd[1] != "-c" {
		t.Fatalf("command = %q, want sh -c <script>", cmd)
	}
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	script := strings.ReplaceAll(cmd[2], "/etc/app/config.yaml", cfg)
	script = strings.ReplaceAll(script, "while true; do sleep 3600; done", "exit 0")
	if script == cmd[2] {
		t.Fatal("script does not mention /etc/app/config.yaml")
	}

	out, err := exec.Command(sh, "-c", script).CombinedOutput()
	if code := exitCode(err); code != 1 {
		t.Fatalf("without the file: exit %d, want 1\n%s", code, out)
	}
	if !strings.Contains(string(out), "FATAL: config "+cfg+" not found") {
		t.Errorf("without the file: output %q has no FATAL line", out)
	}
	if err := os.WriteFile(cfg, []byte("greeting: hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err = exec.Command(sh, "-c", script).CombinedOutput()
	if code := exitCode(err); code != 0 {
		t.Fatalf("with the file: exit %d, want 0\n%s", code, out)
	}
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode()
	}
	return -1
}

func TestImagePullSpellInventsNoTag(t *testing.T) {
	for _, lang := range []string{"zh", "en"} {
		_, sp := castFixture(t, "lost-soul", lang)
		if sp.Remedy != RemedySetImage {
			t.Fatalf("%s: remedy = %q, want %q", lang, sp.Remedy, RemedySetImage)
		}
		cmds := strings.Join(allCommands(sp), "\n")
		if !strings.Contains(cmds, `kubectl set image deployment/lost-soul web=nginx:"${NEW_TAG:?`) {
			t.Errorf("%s: no set image with ${NEW_TAG:?}:\n%s", lang, cmds)
		}
		// Every image reference in the commands uses the variable.
		for _, m := range regexp.MustCompile(`nginx:(\S*)`).FindAllStringSubmatch(cmds, -1) {
			if !strings.HasPrefix(m[1], `"${NEW_TAG:?`) {
				t.Errorf("%s: command uses a made-up tag %q", lang, m[0])
			}
		}
		if strings.Contains(cmds, "latest") {
			t.Errorf("%s: commands mention latest", lang)
		}
		if sp.Patch != "" || sp.PatchFile != "" {
			t.Errorf("%s: image spell has a patch file", lang)
		}
	}
}

// writeVerb matches a kubectl command that changes the cluster.
var writeVerb = regexp.MustCompile(`\bkubectl\s+(apply|patch|create|replace|delete|set|edit|scale|label|annotate|debug|rollout\s+(undo|restart))\b`)

// isWrite reports whether one shell line writes to the cluster. Each pipeline
// segment counts on its own, and --dry-run makes a segment harmless.
func isWrite(cmd string) bool {
	for _, seg := range strings.Split(cmd, "|") {
		if writeVerb.MatchString(seg) && !strings.Contains(seg, "--dry-run=") {
			return true
		}
	}
	return false
}

// TestWritesHaveDryRunFirst checks every spell: only cast steps write, and every
// write is right after the same command with --dry-run=server. kubectl debug
// has no dry-run; it only creates a copy Pod, and the spell says so.
func TestWritesHaveDryRunFirst(t *testing.T) {
	for name, sp := range everySpell(t) {
		for _, st := range sp.Steps {
			for i, cmd := range st.Commands {
				if !isWrite(cmd) {
					continue
				}
				if st.Kind != StepCast {
					t.Errorf("%s: %s step writes: %s", name, st.Kind, cmd)
					continue
				}
				if strings.HasPrefix(cmd, "kubectl debug ") {
					if !strings.Contains(strings.Join(sp.Caveats, " "), "--dry-run") {
						t.Errorf("%s: kubectl debug without a caveat about the missing dry run", name)
					}
					continue
				}
				if i == 0 || st.Commands[i-1] != cmd+DryRunFlag {
					t.Errorf("%s: write %q is not preceded by its --dry-run=server version", name, cmd)
				}
			}
		}
	}
}

func TestEnglishSpellHasNoChinese(t *testing.T) {
	for name, sp := range everySpell(t) {
		if sp.Lang != "en" {
			continue
		}
		data, _ := json.Marshal(sp)
		for _, r := range string(data) {
			if unicode.Is(unicode.Han, r) {
				t.Errorf("%s: English spell contains %q: %s", name, r, data)
				break
			}
		}
	}
}

func TestChineseSpellIsChinese(t *testing.T) {
	_, sp := castFixture(t, "memory-glutton", "zh")
	if !strings.ContainsFunc(sp.Incantation+strings.Join(sp.Diagnosis, ""), func(r rune) bool { return unicode.Is(unicode.Han, r) }) {
		t.Errorf("zh spell has no Chinese: %+v", sp)
	}
}

// TestDiagnosisCitesEvidence checks that every fixture spell cites existing evidence.
func TestDiagnosisCitesEvidence(t *testing.T) {
	all, err := fixtures.Load()
	if err != nil {
		t.Fatal(err)
	}
	ref := regexp.MustCompile(`\[(E\d+)\]`)
	for _, a := range all {
		ev, _ := autopsy.BuildEvidence(a, autopsy.EvidenceOptions{})
		ids := map[string]bool{}
		for _, e := range ev {
			ids[e.ID] = true
		}
		sp := Cast(a, autopsy.Classify(a), ev, "en")
		refs := ref.FindAllStringSubmatch(strings.Join(sp.Diagnosis, " "), -1)
		if len(refs) == 0 {
			t.Errorf("%s: diagnosis cites nothing: %q", a.Name, sp.Diagnosis)
		}
		for _, m := range refs {
			if !ids[m[1]] {
				t.Errorf("%s: diagnosis cites unknown %s", a.Name, m[1])
			}
		}
	}
}

// TestPatchFileStepWritesThePatch runs the file step in a shell: the file it
// writes has the download's name and exactly the patch.
func TestPatchFileStepWritesThePatch(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	for _, prefix := range []string{"memory-glutton", "restless-spirit"} {
		_, sp := castFixture(t, prefix, "en")
		if sp.PatchFile != "resurrect-"+sp.Pod+".yaml" {
			t.Errorf("%s: PatchFile = %q", prefix, sp.PatchFile)
		}
		var file *Step
		for i := range sp.Steps {
			if sp.Steps[i].Kind == StepFile {
				file = &sp.Steps[i]
			}
		}
		if file == nil || len(file.Commands) != 1 {
			t.Fatalf("%s: no file step", prefix)
		}
		dir := t.TempDir()
		c := exec.Command(sh, "-c", file.Commands[0])
		c.Dir = dir
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("%s: file step: %v\n%s", prefix, err, out)
		}
		got, err := os.ReadFile(filepath.Join(dir, sp.PatchFile))
		if err != nil {
			t.Fatalf("%s: %v", prefix, err)
		}
		if string(got) != sp.Patch {
			t.Errorf("%s: file differs from the patch:\n%s\n---\n%s", prefix, got, sp.Patch)
		}
		if !strings.Contains(strings.Join(allCommands(sp), "\n"), "--patch-file="+sp.PatchFile) {
			t.Errorf("%s: no command uses --patch-file=%s", prefix, sp.PatchFile)
		}
	}
}

// TestRequiredVariableStopsTheShell checks the ${NAME:?} guard: an unset
// variable stops the command before kubectl runs.
func TestRequiredVariableStopsTheShell(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	v := need("NEW_TAG", "set it to a tag {that} exists $HOME `x` \"q\"")
	if strings.ContainsAny(strings.TrimSuffix(strings.TrimPrefix(v, `"${`), `}"`), "{}$`\"\\") {
		t.Fatalf("hint not sanitized: %s", v)
	}
	c := exec.Command(sh, "-c", "echo should-not-run "+v)
	c.Env = []string{"PATH=" + os.Getenv("PATH")}
	out, err := c.CombinedOutput()
	if err == nil || strings.Contains(string(out), "should-not-run") {
		t.Fatalf("unset NEW_TAG did not stop the shell: %v %s", err, out)
	}
	if !strings.Contains(string(out), "NEW_TAG") {
		t.Errorf("error does not name the variable: %s", out)
	}
	c = exec.Command(sh, "-c", "echo "+v)
	c.Env = []string{"PATH=" + os.Getenv("PATH"), "NEW_TAG=1.27"}
	out, err = c.CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "1.27" {
		t.Errorf("set NEW_TAG: %v %q", err, out)
	}
}

func TestShellQuoting(t *testing.T) {
	cases := map[string]string{
		"memory-glutton":        "memory-glutton",
		"a b":                   "'a b'",
		"$(rm -rf ~)":           "'$(rm -rf ~)'",
		"it's":                  `'it'"'"'s'`,
		"involvedObject.name=x": "involvedObject.name=x",
	}
	for in, want := range cases {
		if got := shq(in); got != want {
			t.Errorf("shq(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestCastIsPure(t *testing.T) {
	all, err := fixtures.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range all {
		ev, _ := autopsy.BuildEvidence(a, autopsy.EvidenceOptions{})
		before, _ := json.Marshal(a)
		evBefore, _ := json.Marshal(ev)
		first := Cast(a, autopsy.Classify(a), ev, "en")
		second := Cast(a, autopsy.Classify(a), ev, "en")
		after, _ := json.Marshal(a)
		evAfter, _ := json.Marshal(ev)
		if string(before) != string(after) || string(evBefore) != string(evAfter) {
			t.Errorf("%s: Cast changed its input", a.Name)
		}
		if !reflect.DeepEqual(first, second) {
			t.Errorf("%s: Cast is not deterministic", a.Name)
		}
	}
	if sp := Cast(nil, autopsy.Classification{}, nil, "en"); sp.Remedy != RemedyGatherRecords {
		t.Errorf("nil autopsy: remedy = %q", sp.Remedy)
	}
}

func TestDoubled(t *testing.T) {
	for in, want := range map[string]string{"64Mi": "128Mi", "1Gi": "2Gi", "500M": "1G", "100Mi": "200Mi"} {
		if got := doubled(resource.MustParse(in)); got != want {
			t.Errorf("doubled(%s) = %s, want %s", in, got, want)
		}
	}
}

func TestConfigMapName(t *testing.T) {
	for _, c := range []struct{ target, key, want string }{
		{"restless-spirit", "config.yaml", "restless-spirit-config"},
		{"api", "App_Settings.JSON", "api-app-settings"},
		{"", ".env", "config"},
		{strings.Repeat("a", 70), "config.yaml", strings.Repeat("a", 63)},
	} {
		if got := configMapName(c.target, c.key); got != c.want {
			t.Errorf("configMapName(%q, %q) = %q, want %q", c.target, c.key, got, c.want)
		}
	}
}

func TestMissingFile(t *testing.T) {
	for _, c := range []struct{ line, want string }{
		{"FATAL: config /etc/app/config.yaml not found", "/etc/app/config.yaml"},
		{"open /srv/app/settings.json: no such file or directory", "/srv/app/settings.json"},
		{"error: cannot open config file /opt/conf/app.ini", "/opt/conf/app.ini"},
		{"sh: /usr/local/bin/start: not found", ""},
		{"GET /healthz 200", ""},
		{"directory /etc/app/ not found", ""},
	} {
		if got, _ := missingFile([]string{c.line}); got != c.want {
			t.Errorf("missingFile(%q) = %q, want %q", c.line, got, c.want)
		}
	}
}

func TestSplitImageAndRegistry(t *testing.T) {
	for _, c := range []struct{ image, repo, tag, registry string }{
		{"nginx:does-not-exist-666", "nginx", "does-not-exist-666", "docker.io"},
		{"localhost:5000/team/app:v1", "localhost:5000/team/app", "v1", "localhost:5000"},
		{"ghcr.io/org/app@sha256:abc", "ghcr.io/org/app", "", "ghcr.io"},
		{"library/busybox", "library/busybox", "", "docker.io"},
	} {
		repo, tag := splitImage(c.image)
		if repo != c.repo || tag != c.tag || registryOf(repo) != c.registry {
			t.Errorf("%s: got %s %s %s, want %s %s %s", c.image, repo, tag, registryOf(repo), c.repo, c.tag, c.registry)
		}
	}
}

// ---- synthetic spirits for every remedy ----

func i32(v int32) *int32 { return &v }

func crashPod(owner, kind string, logs ...string) *autopsy.PodAutopsy {
	return &autopsy.PodAutopsy{
		Namespace: "shop", Name: "api-6d4cf56db6-x7k2p", OwnerKind: kind, OwnerName: owner, NodeName: "node-1",
		Containers: []autopsy.ContainerAutopsy{{
			Name: "api", Image: "registry.example.com/shop/api:2.4.1", RestartCount: 7,
			State:     autopsy.ContainerState{Kind: "waiting", Reason: "CrashLoopBackOff"},
			LastState: &autopsy.ContainerState{Kind: "terminated", Reason: "Error", ExitCode: i32(2)},
			Limits:    map[string]string{"memory": "256Mi"}, PreviousLogTail: logs, LogTail: logs,
		}},
	}
}

func oomPod(owner, kind string, limits map[string]string) *autopsy.PodAutopsy {
	a := crashPod(owner, kind, "allocating cache")
	a.Containers[0].LastState = &autopsy.ContainerState{Kind: "terminated", Reason: "OOMKilled", ExitCode: i32(137)}
	a.Containers[0].Limits = limits
	return a
}

func imagePod(msg string) *autopsy.PodAutopsy {
	a := crashPod("api-6d4cf56db6", "ReplicaSet")
	a.Containers[0].RestartCount = 0
	a.Containers[0].LastState = nil
	a.Containers[0].State = autopsy.ContainerState{Kind: "waiting", Reason: "ImagePullBackOff", Message: msg}
	a.Events = []autopsy.EventRecord{{Type: "Warning", Reason: "Failed", Message: "Failed to pull image: " + msg, Count: 3}}
	return a
}

func probePod(msg string) *autopsy.PodAutopsy {
	a := crashPod("api-6d4cf56db6", "ReplicaSet", "listening on :8080")
	a.Containers[0].LastState = &autopsy.ContainerState{Kind: "terminated", Reason: "Completed", ExitCode: i32(0)}
	a.Containers[0].State = autopsy.ContainerState{Kind: "running"}
	a.Events = []autopsy.EventRecord{{Type: "Warning", Reason: "Unhealthy", Message: msg, Count: 9}}
	return a
}

type synthetic struct {
	name   string
	pod    *autopsy.PodAutopsy
	remedy string
	cast   bool // whether the spell has write steps
}

func synthetics() []synthetic {
	unknown := crashPod("api-6d4cf56db6", "ReplicaSet")
	unknown.Containers[0].RestartCount = 0
	unknown.Containers[0].LastState = nil
	unknown.Containers[0].State = autopsy.ContainerState{Kind: "waiting", Reason: "ContainerCreating"}
	return []synthetic{
		{"oom-limit-statefulset", oomPod("api", "StatefulSet", map[string]string{"memory": "1Gi"}), RemedyRaiseMemory, true},
		{"oom-limit-bare-pod", oomPod("", "", map[string]string{"memory": "64Mi"}), RemedyRaiseMemory, true},
		{"oom-limit-job", oomPod("nightly-report", "Job", map[string]string{"memory": "64Mi"}), RemedyRaiseMemory, true},
		{"oom-limit-other-owner", oomPod("api", "ReplicationController", map[string]string{"memory": "64Mi"}), RemedyRaiseMemory, true},
		{"oom-no-limit", oomPod("api-6d4cf56db6", "ReplicaSet", nil), RemedyNodeMemory, false},
		{"missing-file", crashPod("api-6d4cf56db6", "ReplicaSet", "open /srv/app/settings.json: no such file or directory"), RemedyMountConfig, true},
		{"dependency-service", crashPod("api-6d4cf56db6", "ReplicaSet", "dial tcp: lookup postgres.db.svc.cluster.local on 10.96.0.10:53: no such host"), RemedyDependency, false},
		{"dependency-ip", crashPod("api-6d4cf56db6", "ReplicaSet", "dial tcp 10.1.2.3:6379: connect: connection refused"), RemedyDependency, false},
		{"other-crash", crashPod("api-6d4cf56db6", "ReplicaSet", "panic: runtime error: index out of range"), RemedyDebugCopy, true},
		{"image-auth", imagePod(`pull access denied for registry.example.com/shop/api, repository does not exist or may require authorization: server message: insufficient_scope: authorization failed`), RemedyPullSecret, true},
		{"image-network", imagePod(`dial tcp: lookup registry.example.com on 10.96.0.10:53: read udp: i/o timeout`), RemedyRegistryNetwork, false},
		{"image-missing", imagePod(`registry.example.com/shop/api:2.4.1: not found`), RemedySetImage, true},
		{"image-unknown", imagePod(`something odd happened`), RemedyGatherRecords, false},
		{"probe-timeout", probePod(`Liveness probe failed: Get "http://10.244.0.9:8080/healthz": context deadline exceeded (Client.Timeout exceeded while awaiting headers)`), RemedySlowProbe, true},
		{"probe-404", probePod(`Readiness probe failed: HTTP probe failed with statuscode: 404`), RemedyFixProbe, false},
		{"probe-refused", probePod(`Liveness probe failed: Get "http://10.244.0.9:8080/healthz": dial tcp 10.244.0.9:8080: connect: connection refused`), RemedyFixProbe, false},
		{"unknown", unknown, RemedyGatherRecords, false},
	}
}

// everySpell casts the fixtures and the synthetic spirits in both languages.
func everySpell(t *testing.T) map[string]Spell {
	t.Helper()
	out := map[string]Spell{}
	all, err := fixtures.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, lang := range []string{"zh", "en"} {
		for _, a := range all {
			out[a.Name+"/"+lang] = castOf(t, a, lang)
		}
		for _, s := range synthetics() {
			out[s.name+"/"+lang] = castOf(t, s.pod, lang)
		}
	}
	return out
}

func hasCast(sp Spell) bool {
	for _, st := range sp.Steps {
		if st.Kind == StepCast {
			return true
		}
	}
	return false
}

func allCommands(sp Spell) []string {
	var out []string
	for _, st := range sp.Steps {
		out = append(out, st.Commands...)
	}
	return out
}

func TestRemedyPerCause(t *testing.T) {
	for _, s := range synthetics() {
		sp := castOf(t, s.pod, "en")
		if sp.Remedy != s.remedy {
			t.Errorf("%s: remedy = %q, want %q (cause %s)", s.name, sp.Remedy, s.remedy, sp.Cause)
			continue
		}
		if hasCast(sp) != s.cast {
			t.Errorf("%s: has cast steps = %v, want %v", s.name, hasCast(sp), s.cast)
		}
		if len(sp.Steps) == 0 || sp.Incantation == "" || len(sp.Diagnosis) == 0 || len(sp.Caveats) == 0 {
			t.Errorf("%s: incomplete spell %+v", s.name, sp)
		}
	}
}

func TestRemedyDetails(t *testing.T) {
	byName := map[string]Spell{}
	for _, s := range synthetics() {
		byName[s.name] = castOf(t, s.pod, "en")
	}
	contains := func(name, want string) {
		t.Helper()
		if !strings.Contains(strings.Join(allCommands(byName[name]), "\n"), want) {
			t.Errorf("%s: no command contains %q:\n%s", name, want, strings.Join(allCommands(byName[name]), "\n"))
		}
	}
	lacks := func(name, bad string) {
		t.Helper()
		if strings.Contains(strings.Join(allCommands(byName[name]), "\n"), bad) {
			t.Errorf("%s: a command contains %q", name, bad)
		}
	}

	// Targets (Requirement 13.5).
	if tg := byName["oom-limit-statefulset"].Target; tg.Kind != "StatefulSet" || tg.Inferred || !tg.InPlace {
		t.Errorf("statefulset target = %+v", tg)
	}
	contains("oom-limit-statefulset", "kubectl patch statefulset/api -n shop --type=strategic --patch-file=")
	if p := byName["oom-limit-statefulset"].Patch; !strings.Contains(p, `memory: "2Gi"`) {
		t.Errorf("statefulset patch: %s", p)
	}
	for _, name := range []string{"oom-limit-bare-pod", "oom-limit-job"} {
		sp := byName[name]
		if sp.Target.InPlace || sp.Target.Note == "" {
			t.Errorf("%s: target = %+v, want a recreate note", name, sp.Target)
		}
		contains(name, `kubectl replace --force -f "${MANIFEST:?`)
		lacks(name, "kubectl patch")
	}
	if p := byName["oom-limit-bare-pod"].Patch; strings.Contains(p, "template:") || !strings.HasPrefix(strings.SplitN(p, "spec:\n", 2)[1], "  containers:") {
		t.Errorf("bare Pod patch must sit under spec, got:\n%s", p)
	}
	if p := byName["oom-limit-job"].Patch; !strings.Contains(p, "template:") {
		t.Errorf("Job patch must sit under spec.template.spec, got:\n%s", p)
	}
	contains("oom-limit-other-owner", "kubectl delete pod api-6d4cf56db6-x7k2p -n shop")

	contains("oom-no-limit", "kubectl describe node node-1")
	contains("missing-file", "--from-file=settings.json=")
	contains("dependency-service", "kubectl get service postgres -n db -o wide")
	contains("dependency-service", "kubectl get endpoints postgres -n db")
	contains("dependency-ip", "kubectl get services,endpoints -n shop -o wide")
	contains("other-crash", "kubectl debug pod/api-6d4cf56db6-x7k2p -n shop -it --copy-to=api-6d4cf56db6-x7k2p-debug --container=api -- sh")
	contains("image-auth", `kubectl create secret docker-registry "${PULL_SECRET:?`)
	contains("image-auth", "--docker-server=registry.example.com")
	contains("image-auth", `"${REGISTRY_PASSWORD:?`)
	contains("image-missing", `registry.example.com/shop/api:"${NEW_TAG:?`)
	contains("image-network", "crictl pull registry.example.com/shop/api:2.4.1")
	contains("probe-timeout", `"livenessProbe":{"timeoutSeconds":'"${TIMEOUT_SECONDS:?`)
	contains("probe-404", `readinessProbe`)
	lacks("probe-404", "timeoutSeconds")
	lacks("probe-refused", "timeoutSeconds")
}

// TestInlinePatchesAreValidJSON expands the inline --patch arguments with
// sample values and checks that the result is JSON that the strategic merge accepts.
func TestInlinePatchesAreValidJSON(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	for _, name := range []string{"probe-timeout", "image-auth"} {
		var sp Spell
		for _, s := range synthetics() {
			if s.name == name {
				sp = castOf(t, s.pod, "en")
			}
		}
		checked := 0
		for _, cmd := range allCommands(sp) {
			i := strings.Index(cmd, "--patch '")
			if i < 0 || strings.HasSuffix(cmd, DryRunFlag) {
				continue
			}
			checked++
			arg := strings.TrimPrefix(cmd[i:], "--patch ")
			c := exec.Command(sh, "-c", "printf %s "+arg)
			c.Env = []string{"PATH=" + os.Getenv("PATH"), "TIMEOUT_SECONDS=5", "FAILURE_THRESHOLD=6", "PULL_SECRET=regcred"}
			out, err := c.Output()
			if err != nil {
				t.Fatalf("%s: expand %s: %v", name, arg, err)
			}
			orig, _ := json.Marshal(appsv1.Deployment{Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "api", LivenessProbe: &corev1.Probe{TimeoutSeconds: 1, PeriodSeconds: 10}}},
			}}}})
			merged, err := strategicpatch.StrategicMergePatch(orig, out, appsv1.Deployment{})
			if err != nil {
				t.Fatalf("%s: merge %s: %v", name, out, err)
			}
			var d appsv1.Deployment
			if err := json.Unmarshal(merged, &d); err != nil {
				t.Fatal(err)
			}
			ps := d.Spec.Template.Spec
			switch name {
			case "probe-timeout":
				p := ps.Containers[0].LivenessProbe
				if len(ps.Containers) != 1 || p.TimeoutSeconds != 5 || p.FailureThreshold != 6 || p.PeriodSeconds != 10 {
					t.Errorf("probe after patch = %+v", p)
				}
			case "image-auth":
				if len(ps.ImagePullSecrets) != 1 || ps.ImagePullSecrets[0].Name != "regcred" {
					t.Errorf("imagePullSecrets = %+v", ps.ImagePullSecrets)
				}
			}
		}
		if checked != 1 {
			t.Errorf("%s: checked %d inline patches, want 1", name, checked)
		}
	}
}
