// Package resurrect writes the resurrection spell (Requirement 13): a fix plan
// built only from the collected autopsy. It never calls the LLM or the cluster.
// Every command it prints is for a human to read and run. Each command that
// writes to the cluster is preceded by the same command with --dry-run=server.
package resurrect

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/zhangjuji/k8s-seance/internal/autopsy"
	"github.com/zhangjuji/k8s-seance/internal/llm"
)

// StepKind labels a step by what it does to the cluster.
type StepKind string

const (
	// StepCheck only reads ("只读 · 查看").
	StepCheck StepKind = "check"
	// StepFile writes the patch file on your machine ("补丁文件").
	StepFile StepKind = "file"
	// StepCast writes to the cluster ("施法 · 会写集群").
	StepCast StepKind = "cast"
	// StepVerify only reads, after the cast ("只读 · 确认").
	StepVerify StepKind = "verify"
)

// Remedy names the plan that Cast picked.
const (
	RemedyRaiseMemory     = "raise-memory-limit"
	RemedyNodeMemory      = "check-node-memory"
	RemedyMountConfig     = "mount-config-file"
	RemedyDependency      = "check-dependency"
	RemedyDebugCopy       = "debug-copy"
	RemedySetImage        = "set-image-tag"
	RemedyPullSecret      = "image-pull-secret"
	RemedyRegistryNetwork = "registry-network"
	RemedySlowProbe       = "slow-down-probe"
	RemedyFixProbe        = "fix-probe-target"
	RemedyGatherRecords   = "gather-records"
)

// DryRunFlag is appended to a write command to get its rehearsal.
const DryRunFlag = " --dry-run=server"

// Step is one step of the spell. Commands are shell lines, in order.
type Step struct {
	Kind     StepKind `json:"kind"`
	Title    string   `json:"title"`
	Commands []string `json:"commands,omitempty"`
}

// Target is the object the spell acts on.
type Target struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	// Container is the container the fix is about.
	Container string `json:"container,omitempty"`
	// Inferred is true when the target was derived, not read (ReplicaSet → Deployment).
	Inferred bool `json:"inferred"`
	// InPlace is true for controllers that can be patched where they are
	// (Deployment, StatefulSet, DaemonSet). Bare Pods and Jobs are recreated
	// from an edited manifest instead.
	InPlace bool   `json:"inPlace"`
	Note    string `json:"note,omitempty"`
}

// Spell is the fix plan for one dead Pod.
type Spell struct {
	Pod       string               `json:"pod"`
	Namespace string               `json:"namespace"`
	Cause     autopsy.CauseOfDeath `json:"cause"`
	Lang      string               `json:"lang"`
	Remedy    string               `json:"remedy"`
	// Incantation is the playful line. Its numbers come from the evidence.
	Incantation string `json:"incantation"`
	// Diagnosis lines cite evidence as [E#].
	Diagnosis []string `json:"diagnosis"`
	Target    Target   `json:"target"`
	Steps     []Step   `json:"steps"`
	Caveats   []string `json:"caveats"`
	// Patch is a strategic merge patch in YAML, or "" when the plan has no patch file.
	Patch string `json:"patch,omitempty"`
	// PatchFile is the file name the commands use for Patch: resurrect-<pod>.yaml.
	PatchFile string `json:"patchFile,omitempty"`
}

// Cast builds the spell for a, using the classification and the numbered
// evidence. It is a pure function. lang is "zh" or "en"; anything else is Chinese.
func Cast(a *autopsy.PodAutopsy, cls autopsy.Classification, ev []autopsy.Evidence, lang string) Spell {
	if llm.NormalizeLang(lang) == llm.LangEN {
		lang = llm.LangEN
	} else {
		lang = llm.LangZH
	}
	if a == nil {
		a = &autopsy.PodAutopsy{}
	}
	c := &caster{a: a, cls: cls, ev: ev, lang: lang}
	c.sp = Spell{
		Pod:       a.Name,
		Namespace: a.Namespace,
		Cause:     cls.Cause,
		Lang:      lang,
		Target:    inferTarget(a, lang),
		Diagnosis: []string{},
		Steps:     []Step{},
		Caveats:   []string{},
	}
	c.plan()
	return c.sp
}

// PatchFileName is the patch file name for a Pod: resurrect-<pod>.yaml, with
// every rune other than ASCII letters, digits and '-' replaced by '-'.
func PatchFileName(pod string) string {
	return "resurrect-" + strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-':
			return r
		}
		return '-'
	}, pod) + ".yaml"
}

// caster carries the inputs while the plan is built.
type caster struct {
	a    *autopsy.PodAutopsy
	cls  autopsy.Classification
	ev   []autopsy.Evidence
	lang string
	sp   Spell
}

// t picks the Chinese or the English text.
func (c *caster) t(zh, en string) string {
	if c.lang == llm.LangEN {
		return en
	}
	return zh
}

// plan picks the remedy from the cause and the evidence.
func (c *caster) plan() {
	switch c.cls.Cause {
	case autopsy.CauseOOMKilled:
		c.oom()
	case autopsy.CauseImagePullBackOff:
		c.image()
	case autopsy.CauseProbeFailure:
		c.probe()
	case autopsy.CauseCrashLoopBackOff, autopsy.CauseError:
		c.crash()
	default:
		c.gatherRecords()
	}
}

// ---- target inference (Requirement 13.5) ----

// templateHash matches the pod-template-hash suffix that a Deployment adds to its ReplicaSet names.
var templateHash = regexp.MustCompile(`^(.+)-[a-z0-9]{5,10}$`)

func inferTarget(a *autopsy.PodAutopsy, lang string) Target {
	en := lang == llm.LangEN
	pick := func(zh, e string) string {
		if en {
			return e
		}
		return zh
	}
	t := Target{Namespace: a.Namespace}
	switch a.OwnerKind {
	case "ReplicaSet":
		if m := templateHash.FindStringSubmatch(a.OwnerName); m != nil {
			t.Kind, t.Name, t.Inferred, t.InPlace = "Deployment", m[1], true, true
			t.Note = fmt.Sprintf(pick(
				"推断：Pod 属于 ReplicaSet %s，去掉 pod-template-hash 后得到 Deployment %s。施法前请先用第一步确认它存在。",
				"Inferred: the Pod belongs to ReplicaSet %s; removing the pod-template-hash gives Deployment %s. Confirm it exists with the first step before you cast."),
				a.OwnerName, m[1])
			return t
		}
		t.Kind, t.Name = "ReplicaSet", a.OwnerName
		t.Note = pick(
			"这个 ReplicaSet 看起来不属于 Deployment。改 ReplicaSet 的模板不会影响已有 Pod：改清单、apply，再删掉旧 Pod 让它重建。",
			"This ReplicaSet does not look like it belongs to a Deployment. Changing its template does not touch existing Pods: edit the manifest, apply it, then delete the old Pod so it is recreated.")
	case "StatefulSet", "DaemonSet":
		t.Kind, t.Name, t.InPlace = a.OwnerKind, a.OwnerName, true
	case "Job":
		t.Kind, t.Name = "Job", a.OwnerName
		t.Note = pick(
			"Job 的 Pod 模板不能原地修改：改 Job 清单后重建 Job，不要原地 patch。",
			"A Job's Pod template cannot be changed in place: edit the Job manifest and recreate the Job instead of patching it.")
	case "":
		t.Kind, t.Name = "Pod", a.Name
		t.Note = pick(
			"这是一个没有控制器的裸 Pod：改清单后重建它，不要原地 patch。",
			"This is a bare Pod with no controller: edit its manifest and recreate it instead of patching it in place.")
	default:
		t.Kind, t.Name = a.OwnerKind, a.OwnerName
		t.Note = fmt.Sprintf(pick(
			"Pod 由 %s 管理：改它的清单并 apply，再删掉旧 Pod 让它重建。",
			"The Pod is managed by a %s: edit its manifest and apply it, then delete the old Pod so it is recreated."), a.OwnerKind)
	}
	return t
}

// ref is the kubectl reference of the target, such as deployment/memory-glutton.
func (t Target) ref() string { return strings.ToLower(t.Kind) + "/" + t.Name }

// templated reports whether the patch goes under spec.template.spec.
func (t Target) templated() bool { return t.Kind != "Pod" }

// ---- evidence helpers ----

// cite returns " [E#]" for the first evidence item that matches, or "".
func (c *caster) cite(match func(autopsy.Evidence) bool) string {
	for _, e := range c.ev {
		if match(e) {
			return " [" + e.ID + "]"
		}
	}
	return ""
}

// citeText cites the first item of kind whose text contains every part.
func (c *caster) citeText(kind string, parts ...string) string {
	return c.cite(func(e autopsy.Evidence) bool {
		if kind != "" && e.Kind != kind {
			return false
		}
		for _, p := range parts {
			if !strings.Contains(e.Text, p) {
				return false
			}
		}
		return true
	})
}

// container returns the container that matters most: the first one for which
// pick is true, else the one with the most restarts, else the first.
func (c *caster) container(pick func(autopsy.ContainerAutopsy) bool) *autopsy.ContainerAutopsy {
	if len(c.a.Containers) == 0 {
		return nil
	}
	if pick != nil {
		for i := range c.a.Containers {
			if pick(c.a.Containers[i]) {
				return &c.a.Containers[i]
			}
		}
	}
	best := 0
	for i, ct := range c.a.Containers {
		if ct.RestartCount > c.a.Containers[best].RestartCount {
			best = i
		}
	}
	return &c.a.Containers[best]
}

// logLines returns every log line of a container, previous log first.
func logLines(ct *autopsy.ContainerAutopsy) []string {
	if ct == nil {
		return nil
	}
	out := append([]string(nil), ct.PreviousLogTail...)
	return append(out, ct.LogTail...)
}

// ---- shell helpers ----

// shellSafe matches words that need no quoting.
var shellSafe = regexp.MustCompile(`^[A-Za-z0-9._/:=@,+-]+$`)

// shq quotes s for a POSIX shell when it is not a plain word.
func shq(s string) string {
	if shellSafe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// hintUnsafe are the runes that would end or change a ${NAME:?hint} expansion.
var hintUnsafe = strings.NewReplacer("}", ")", "{", "(", `"`, "", "$", "", "`", "", `\`, "")

// need is a required shell variable: "${NAME:?hint}". The shell stops the
// command when NAME is unset or empty, so a pasted command cannot run with a
// made-up value.
func need(name, hint string) string {
	return `"${` + name + `:?` + hintUnsafe.Replace(hint) + `}"`
}

// writeStep is a cast step: the command with --dry-run=server, then the command.
func writeStep(title string, cmds ...string) Step {
	st := Step{Kind: StepCast, Title: title}
	for _, cmd := range cmds {
		st.Commands = append(st.Commands, cmd+DryRunFlag, cmd)
	}
	return st
}

// nsFlag is "-n <namespace>".
func (c *caster) nsFlag() string { return "-n " + shq(c.a.Namespace) }

// ---- YAML helpers ----

// yq quotes a YAML string scalar. JSON string syntax is valid YAML.
func yq(s string) string { return strconv.Quote(s) }

// patchDoc wraps container-level and pod-level lines into a strategic merge
// patch for the target. podLines are indented as children of the Pod spec.
func (c *caster) patchDoc(summary string, podLines []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# K8s Seance resurrection patch for %s/%s\n", c.a.Namespace, c.a.Name)
	fmt.Fprintf(&b, "# Strategic merge patch for %s: %s\n", c.sp.Target.ref(), summary)
	indent := "  "
	b.WriteString("spec:\n")
	if c.sp.Target.templated() {
		b.WriteString("  template:\n    spec:\n")
		indent = "      "
	}
	for _, l := range podLines {
		b.WriteString(indent + l + "\n")
	}
	return b.String()
}

// fileStep writes Patch to PatchFile with a quoted heredoc, so the shell
// copies it exactly and expands nothing.
func (c *caster) fileStep(patch string) Step {
	c.sp.Patch = patch
	c.sp.PatchFile = PatchFileName(c.a.Name)
	title := c.t("把补丁写进 "+c.sp.PatchFile+"（也可以直接下载）",
		"Write the patch to "+c.sp.PatchFile+" (or download it)")
	if !c.sp.Target.InPlace {
		title = c.t("把补丁写进 "+c.sp.PatchFile+"，再把它合进你的清单",
			"Write the patch to "+c.sp.PatchFile+", then merge it into your manifest")
	}
	return Step{
		Kind:     StepFile,
		Title:    title,
		Commands: []string{"cat > " + c.sp.PatchFile + " <<'EOF'\n" + patch + "EOF"},
	}
}

// applyFix is how the fix reaches the cluster. For an in-place controller it
// runs inPlace. For a bare Pod or a Job the edited manifest replaces the old
// object. For other owners the manifest is applied and the Pod is deleted so
// the owner recreates it.
func (c *caster) applyFix(title string, inPlace ...string) Step {
	t := c.sp.Target
	switch {
	case t.InPlace:
		return writeStep(title, inPlace...)
	case t.Kind == "Pod" || t.Kind == "Job":
		manifest := need("MANIFEST", c.t("改好的 "+t.Kind+" 清单路径", "path to the edited "+t.Kind+" manifest"))
		return writeStep(c.t("用改好的清单重建 ", "Recreate the ")+t.Kind+c.t("", " from the edited manifest"),
			"kubectl replace --force -f "+manifest+" "+c.nsFlag())
	default:
		manifest := need("MANIFEST", c.t("改好的 "+t.Kind+" 清单路径", "path to the edited "+t.Kind+" manifest"))
		return writeStep(c.t("apply 改好的清单，再删掉旧 Pod 让它重建", "Apply the edited manifest, then delete the old Pod so it is recreated"),
			"kubectl apply -f "+manifest+" "+c.nsFlag(),
			"kubectl delete pod "+shq(c.a.Name)+" "+c.nsFlag())
	}
}

// verifyStep watches the target come back, read-only.
func (c *caster) verifyStep() Step {
	t := c.sp.Target
	st := Step{Kind: StepVerify, Title: c.t("确认它复活了", "Confirm it came back")}
	switch t.Kind {
	case "Deployment", "StatefulSet", "DaemonSet":
		st.Commands = []string{
			"kubectl rollout status " + t.ref() + " " + c.nsFlag() + " --timeout=180s",
			"kubectl get pods " + c.nsFlag() + " -o wide",
		}
	case "Job":
		st.Commands = []string{"kubectl get job " + shq(t.Name) + " " + c.nsFlag(), "kubectl get pods " + c.nsFlag() + " -o wide"}
	default:
		st.Commands = []string{"kubectl get pods " + c.nsFlag() + " -o wide"}
	}
	return st
}

// targetCheck shows the target, or a field of one container in it, read-only.
func (c *caster) targetCheck(field string) string {
	t := c.sp.Target
	ct := c.sp.Target.Container
	if field != "" && ct != "" && dnsLabel.MatchString(ct) {
		path := ".spec.containers"
		if t.templated() {
			path = ".spec.template.spec.containers"
		}
		return "kubectl get " + t.ref() + " " + c.nsFlag() + ` -o jsonpath='{` + path + `[?(@.name=="` + ct + `")].` + field + `}{"\n"}'`
	}
	return "kubectl get " + t.ref() + " " + c.nsFlag() + " -o yaml"
}

// eventsCheck lists the events of the Pod.
func (c *caster) eventsCheck() string {
	return "kubectl get events " + c.nsFlag() + " --field-selector " + shq("involvedObject.name="+c.a.Name) + " --sort-by=.lastTimestamp"
}

// dnsLabel is the Kubernetes rule for container, Service and ConfigMap names used here.
var dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
