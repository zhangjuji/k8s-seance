package resurrect

import (
	"fmt"
	"net"
	"path"
	"regexp"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/zhangjuji/k8s-seance/internal/autopsy"
)

// ---- OOMKilled ----

func (c *caster) oom() {
	ct := c.container(func(x autopsy.ContainerAutopsy) bool {
		return isTerminated(x.State, "OOMKilled") || (x.LastState != nil && isTerminated(*x.LastState, "OOMKilled"))
	})
	if ct == nil {
		c.gatherRecords()
		return
	}
	c.sp.Target.Container = ct.Name
	c.sp.Diagnosis = append(c.sp.Diagnosis, fmt.Sprintf(c.t(
		"容器 %s 超出内存上限，被内核杀死（OOMKilled%s）%s。",
		"Container %s ran out of memory and was killed by the kernel (OOMKilled%s)%s."),
		ct.Name, c.exitText(), c.citeText("status", "container "+ct.Name+" ", "OOMKilled")))

	limit := ct.Limits["memory"]
	q, err := resource.ParseQuantity(limit)
	if limit == "" || err != nil || q.Sign() <= 0 {
		c.nodeMemory(ct)
		return
	}
	raised := doubled(q)
	limitCite := c.citeText("spec", "container "+ct.Name+" limits:", "memory="+limit)
	c.sp.Remedy = RemedyRaiseMemory
	c.sp.Incantation = fmt.Sprintf(c.t(
		"以 %s 之名你窒息而亡，以 %s 之名你重新呼吸。",
		"Bound by %s you choked; with %s you shall breathe again."), limit, raised)
	c.sp.Diagnosis = append(c.sp.Diagnosis, fmt.Sprintf(c.t(
		"它的 memory limit 是 %s%s。咒语把它翻倍到 %s，requests 不变。",
		"Its memory limit is %s%s. The spell doubles it to %s and leaves the requests alone."), limit, limitCite, raised))

	patch := c.patchDoc("memory limit "+limit+" -> "+raised, []string{
		"containers:",
		"  - name: " + yq(ct.Name),
		"    resources:",
		"      limits:",
		"        memory: " + yq(raised),
	})
	checks := Step{Kind: StepCheck, Title: c.t("查看现在的资源设置", "Look at the current resources"),
		Commands: []string{c.targetCheck("resources")}}
	if c.a.NodeName != "" {
		checks.Title = c.t("查看现在的资源设置，以及节点还剩多少内存", "Look at the current resources and how much memory the node has left")
		checks.Commands = append(checks.Commands, "kubectl describe node "+shq(c.a.NodeName))
	}
	if act := c.action(ActionPatch); act != nil {
		act.Patch = patch
		c.sp.Action = act
	}
	c.sp.Steps = append(c.sp.Steps, checks, c.fileStep(patch),
		c.applyFix(c.t("施法：调高 memory limit", "Cast: raise the memory limit"),
			"kubectl patch "+c.sp.Target.ref()+" "+c.nsFlag()+" --type=strategic --patch-file="+c.sp.PatchFile),
		c.verifyStep())
	c.sp.Caveats = append(c.sp.Caveats,
		c.t("翻倍只是止血。如果内存一直涨，那是泄漏，limit 再大也会再死。",
			"Doubling only stops the bleeding. If memory keeps growing it is a leak, and it will die again at any limit."),
		c.t("节点要放得下新的 limit。requests 没改，调度器仍按旧的 requests 放 Pod。",
			"The node must have room for the new limit. The requests are unchanged, so the scheduler still places the Pod by the old requests."))
	c.inferredCaveat()
}

// nodeMemory is the OOM plan when the container has no memory limit: the node ran out.
func (c *caster) nodeMemory(ct *autopsy.ContainerAutopsy) {
	c.sp.Remedy = RemedyNodeMemory
	c.sp.Incantation = c.t("没有边界的贪食者，吞掉的是整个节点。先看看节点还剩什么。",
		"A glutton with no limit eats the whole node. First see what the node has left.")
	c.sp.Diagnosis = append(c.sp.Diagnosis, fmt.Sprintf(c.t(
		"容器 %s 没有 memory limit%s，所以是节点内存不够了。证据里没有可以照抄的新数值。",
		"Container %s has no memory limit%s, so the node itself ran short. The evidence gives no number to copy into a limit."),
		ct.Name, c.citeText("spec", "container "+ct.Name+" limits:")))
	st := Step{Kind: StepCheck, Title: c.t("查看节点内存和压力", "Look at the node's memory and pressure")}
	if c.a.NodeName != "" {
		st.Commands = append(st.Commands, "kubectl describe node "+shq(c.a.NodeName), "kubectl top node "+shq(c.a.NodeName))
	} else {
		st.Commands = append(st.Commands, "kubectl describe nodes", "kubectl top nodes")
	}
	st.Commands = append(st.Commands, "kubectl top pods "+c.nsFlag(), c.eventsCheck())
	c.sp.Steps = append(c.sp.Steps, st)
	c.sp.Caveats = append(c.sp.Caveats,
		c.t("kubectl top 需要 metrics-server。", "kubectl top needs metrics-server."),
		c.t("量出真实用量后再给容器设 requests 和 limit，不要凭感觉填。",
			"Set requests and a limit once you have measured the real usage, not by guessing."))
}

// doubled returns 2*q in the format of q, for example 64Mi -> 128Mi.
func doubled(q resource.Quantity) string {
	return resource.NewQuantity(q.Value()*2, q.Format).String()
}

// ---- CrashLoopBackOff and Error ----

func (c *caster) crash() {
	ct := c.container(func(x autopsy.ContainerAutopsy) bool {
		return x.RestartCount > 0 || exitOf(x) != nil
	})
	if ct == nil {
		c.gatherRecords()
		return
	}
	c.sp.Target.Container = ct.Name
	lines := logLines(ct)
	if p, line := missingFile(lines); p != "" {
		c.mountConfig(ct, p, line)
		return
	}
	if d := findDependency(lines); d != nil {
		c.dependency(ct, d)
		return
	}
	for _, s := range c.cls.Secondary {
		if s == autopsy.CauseProbeFailure {
			c.probe()
			return
		}
	}
	c.debugCopy(ct)
}

// crashLine is the first diagnosis line of a crash: exit code and restarts, cited.
func (c *caster) crashLine(ct *autopsy.ContainerAutopsy) string {
	exit := ""
	if c.cls.LastExitCode != nil {
		exit = fmt.Sprintf(c.t("，退出码 %d", " (exit code %d)"), *c.cls.LastExitCode)
	}
	return fmt.Sprintf(c.t("容器 %s 启动后退出%s，已重启 %d 次%s。", "Container %s exits after it starts%s and has restarted %d times%s."),
		ct.Name, exit, ct.RestartCount, c.citeText("status", "container "+ct.Name+" "))
}

var (
	// missingAfter matches "<path> not found", "<path>: no such file or directory" and the like.
	missingAfter = regexp.MustCompile(`(/[A-Za-z0-9._/-]*[A-Za-z0-9_-][A-Za-z0-9._-]*)['"]?:?\s+(?i:not found|no such file|does not exist|doesn't exist|is missing|missing)`)
	// missingBefore matches "cannot open /path", "failed to load config /path" and the like.
	missingBefore = regexp.MustCompile(`(?i:no such file or directory|not found|missing|cannot open|can't open|cannot read|can't read|failed to open|failed to read|failed to load|unable to open|unable to read|unable to load)[^/\n]{0,40}?(/[A-Za-z0-9._/-]*[A-Za-z0-9_-][A-Za-z0-9._-]*)`)
	// notConfigDirs hold executables and kernel files, which a ConfigMap should not replace.
	notConfigDirs = []string{"/proc/", "/sys/", "/dev/", "/bin/", "/sbin/", "/usr/bin/", "/usr/sbin/", "/usr/local/bin/"}
)

// missingFile finds a file that the log says is missing. It returns the path and the log line.
func missingFile(lines []string) (string, string) {
	for _, line := range lines {
		for _, re := range []*regexp.Regexp{missingAfter, missingBefore} {
			m := re.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			p := path.Clean(m[1])
			if len(p) > 253 || p == "/" || strings.HasSuffix(m[1], "/") {
				continue
			}
			skip := false
			for _, d := range notConfigDirs {
				if strings.HasPrefix(p, d) {
					skip = true
				}
			}
			if !skip {
				return p, line
			}
		}
	}
	return "", ""
}

// configKey matches a valid ConfigMap key.
var configKey = regexp.MustCompile(`^[-._a-zA-Z0-9]+$`)

func (c *caster) mountConfig(ct *autopsy.ContainerAutopsy, file, line string) {
	key := path.Base(file)
	if !configKey.MatchString(key) || !dnsLabel.MatchString(ct.Name) {
		c.debugCopy(ct)
		return
	}
	cm := configMapName(c.sp.Target.Name, key)
	c.sp.Remedy = RemedyMountConfig
	c.sp.Incantation = fmt.Sprintf(c.t(
		"它每次醒来都在找 %s。把这页纸放回它手里，它就能安息地活着。",
		"Every time it wakes, it looks for %s. Put that page back in its hands and it can rest while it runs."), file)
	c.sp.Diagnosis = append(c.sp.Diagnosis, c.crashLine(ct), fmt.Sprintf(c.t(
		"日志说缺少文件 %s%s。咒语把你本地的文件放进 ConfigMap %s，用 subPath 只挂这一个文件。",
		"The log says the file %s is missing%s. The spell puts your local copy in ConfigMap %s and mounts only that file with subPath."),
		file, c.citeText("log", line), cm))

	patch := c.patchDoc("mount ConfigMap "+cm+" at "+file, []string{
		"containers:",
		"  - name: " + yq(ct.Name),
		"    volumeMounts:",
		"      - name: " + yq(cm),
		"        mountPath: " + yq(file),
		"        subPath: " + yq(key),
		"        readOnly: true",
		"volumes:",
		"  - name: " + yq(cm),
		"    configMap:",
		"      name: " + yq(cm),
	})
	if act := c.action(ActionConfigMapMount); act != nil {
		act.Patch, act.ConfigMap, act.ConfigKey, act.MountPath = patch, cm, key, file
		act.Inputs = []Input{{Name: InputConfig, Kind: "textarea"}}
		c.sp.Action = act
	}
	configFile := need("CONFIG_FILE", c.t("本地 "+key+" 的路径，内容要你自己确认", "path to your local "+key+", reviewed by you"))
	c.sp.Steps = append(c.sp.Steps,
		Step{Kind: StepCheck, Title: c.t("再看一眼遗言，确认缺的就是这个文件", "Read the last words again and confirm this is the missing file"),
			Commands: []string{
				"kubectl logs " + shq(c.a.Name) + " " + c.nsFlag() + " -c " + shq(ct.Name) + " --previous",
				"kubectl get configmap " + shq(cm) + " " + c.nsFlag() + " --ignore-not-found",
			}},
		c.fileStep(patch),
		writeStep(c.t("施法：用本地文件创建或更新 ConfigMap "+cm, "Cast: create or update ConfigMap "+cm+" from your local file"),
			"kubectl create configmap "+shq(cm)+" --from-file="+key+"="+configFile+" "+c.nsFlag()+" --dry-run=client -o yaml | kubectl apply -f -"),
		c.applyFix(c.t("施法：把文件挂到 "+file, "Cast: mount the file at "+file),
			"kubectl patch "+c.sp.Target.ref()+" "+c.nsFlag()+" --type=strategic --patch-file="+c.sp.PatchFile),
		c.verifyStep())
	c.sp.Caveats = append(c.sp.Caveats,
		c.t("文件内容来自你的电脑，不是证据。apply 前看一遍。", "The file content comes from your machine, not from the evidence. Read it before you apply it."),
		c.t("里面有密码或密钥时改用 Secret，ConfigMap 不加密。", "If the file holds passwords or keys, use a Secret instead. ConfigMaps are not encrypted."),
		c.t("subPath 挂载不会跟着 ConfigMap 更新，改了内容要重启 Pod。", "A subPath mount does not follow ConfigMap updates. Restart the Pod after you change the content."))
	c.inferredCaveat()
}

// configMapName is <target>-<file stem>, cut to a valid DNS label of at most 63 characters.
func configMapName(target, key string) string {
	stem := strings.TrimSuffix(key, path.Ext(key))
	clean := strings.Trim(strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		}
		return '-'
	}, stem), "-")
	if clean == "" {
		clean = "config"
	}
	name := clean
	if target != "" {
		name = target + "-" + clean
	}
	if len(name) > 63 {
		name = name[:63]
	}
	name = strings.TrimRight(name, "-.")
	if !dnsLabel.MatchString(name) {
		return "resurrect-config"
	}
	return name
}

// dependencyHit is a dependency the log could not reach.
type dependencyHit struct {
	Host, Port, Line string
}

var (
	depSignal = regexp.MustCompile(`(?i)connection refused|no such host|i/o timeout|econnrefused|could not connect|couldn't connect|failed to connect|unable to connect|connection timed out|network is unreachable|no route to host|dial tcp`)
	depHost   = regexp.MustCompile(`(?i)(?:dial tcp|lookup|connect(?:ing)? to|connection to|reach)\s+\[?([a-z0-9][a-z0-9.-]*[a-z0-9])\]?(?::(\d{1,5}))?`)
)

// findDependency finds a log line that says a dependency could not be reached.
// Host is "" when the line does not name one.
func findDependency(lines []string) *dependencyHit {
	for _, line := range lines {
		if !depSignal.MatchString(line) {
			continue
		}
		hit := &dependencyHit{Line: line}
		for _, m := range depHost.FindAllStringSubmatch(line, -1) {
			if strings.EqualFold(m[1], "tcp") {
				continue
			}
			hit.Host, hit.Port = strings.ToLower(m[1]), m[2]
			break
		}
		return hit
	}
	return nil
}

// serviceOf maps a host to an in-cluster Service. ok is false for IPs and external names.
func serviceOf(host, podNS string) (name, ns string, ok bool) {
	if host == "" || net.ParseIP(host) != nil {
		return "", "", false
	}
	parts := strings.Split(host, ".")
	switch {
	case len(parts) == 1:
		name, ns = parts[0], podNS
	case len(parts) >= 3 && parts[2] == "svc":
		name, ns = parts[0], parts[1]
	default:
		return "", "", false
	}
	if !dnsLabel.MatchString(name) || !dnsLabel.MatchString(ns) {
		return "", "", false
	}
	return name, ns, true
}

func (c *caster) dependency(ct *autopsy.ContainerAutopsy, d *dependencyHit) {
	c.sp.Remedy = RemedyDependency
	where := d.Host
	if d.Port != "" {
		where += ":" + d.Port
	}
	if where == "" {
		where = c.t("它的依赖", "its dependency")
	}
	c.sp.Incantation = fmt.Sprintf(c.t("它对着 %s 呼喊，没有回音。先让另一头的人应声。",
		"It calls out to %s and nothing answers. First make the other side answer."), where)
	c.sp.Diagnosis = append(c.sp.Diagnosis, c.crashLine(ct), fmt.Sprintf(c.t(
		"日志说连不上 %s%s。问题在依赖那一侧，这里不改这个 Pod。",
		"The log says it cannot reach %s%s. The problem is on the dependency's side, so the spell does not change this Pod."),
		where, c.citeText("log", d.Line)))
	st := Step{Kind: StepCheck, Title: c.t("查看依赖的 Service 和 Endpoints", "Look at the dependency's Service and Endpoints")}
	if svc, ns, ok := serviceOf(d.Host, c.a.Namespace); ok {
		st.Commands = append(st.Commands,
			"kubectl get service "+svc+" -n "+ns+" -o wide",
			"kubectl get endpoints "+svc+" -n "+ns,
			"kubectl get endpointslices -n "+ns+" -l kubernetes.io/service-name="+svc)
	} else {
		st.Commands = append(st.Commands, "kubectl get services,endpoints "+c.nsFlag()+" -o wide")
		c.sp.Caveats = append(c.sp.Caveats, c.t(
			"日志里的地址不是集群内的 Service 名，也要检查 DNS、出网策略和 NetworkPolicy。",
			"The address in the log is not an in-cluster Service name. Also check DNS, egress rules and NetworkPolicies."))
	}
	st.Commands = append(st.Commands, "kubectl logs "+shq(c.a.Name)+" "+c.nsFlag()+" -c "+shq(ct.Name)+" --previous")
	c.sp.Steps = append(c.sp.Steps, st, Step{Kind: StepVerify,
		Title:    c.t("修好依赖后，确认重启次数不再增加", "After the dependency is fixed, confirm the restart count stops growing"),
		Commands: []string{"kubectl get pod " + shq(c.a.Name) + " " + c.nsFlag()}})
	c.sp.Caveats = append(c.sp.Caveats,
		c.t("Endpoints 为空通常是 selector 对不上或后端 Pod 没就绪；端口不对也会被拒绝连接。",
			"Empty Endpoints usually mean the selector does not match or the backend Pods are not ready. A wrong port also refuses connections."),
		c.t("依赖恢复后，下一次重启它会自己活过来，不需要改它。",
			"Once the dependency is back, it comes back on its next restart without any change."))
}

// debugCopy is the plan for other crashes: a debugging copy of the Pod.
func (c *caster) debugCopy(ct *autopsy.ContainerAutopsy) {
	c.sp.Remedy = RemedyDebugCopy
	copyName := c.a.Name + "-debug"
	c.sp.Incantation = c.t("它说不清自己怎么死的。造一个替身，带着手电进去看看。",
		"It cannot say how it died. Make a double of it and go in with a lantern.")
	c.sp.Diagnosis = append(c.sp.Diagnosis, c.crashLine(ct), c.t(
		"证据里没有缺失文件或连不上依赖的线索，需要进到容器里现场查看。",
		"The evidence names no missing file and no unreachable dependency, so look inside the container."))
	if last := lastLine(ct); last != "" {
		c.sp.Diagnosis = append(c.sp.Diagnosis, fmt.Sprintf(c.t("最后一行日志见%s。", "Its last log line is in%s."), c.citeText("log", last)))
	}
	c.sp.Steps = append(c.sp.Steps,
		Step{Kind: StepCheck, Title: c.t("再看一眼遗言，确认替身的名字没被占用", "Read the last words again and make sure the double's name is free"),
			Commands: []string{
				"kubectl logs " + shq(c.a.Name) + " " + c.nsFlag() + " -c " + shq(ct.Name) + " --previous",
				"kubectl describe pod " + shq(c.a.Name) + " " + c.nsFlag(),
				"kubectl get pod " + shq(copyName) + " " + c.nsFlag() + " --ignore-not-found",
			}},
		Step{Kind: StepCast, Title: c.t("施法：创建一个启动命令换成 sh 的替身 Pod", "Cast: create a double of the Pod whose command is replaced by sh"),
			Commands: []string{"kubectl debug pod/" + shq(c.a.Name) + " " + c.nsFlag() + " -it --copy-to=" + shq(copyName) + " --container=" + shq(ct.Name) + " -- sh"}},
		writeStep(c.t("施法：查完后删掉替身", "Cast: delete the double when you are done"),
			"kubectl delete pod "+shq(copyName)+" "+c.nsFlag()),
		Step{Kind: StepVerify, Title: c.t("确认替身已经删掉", "Confirm the double is gone"),
			Commands: []string{"kubectl get pod " + shq(copyName) + " " + c.nsFlag() + " --ignore-not-found"}})
	c.sp.Caveats = append(c.sp.Caveats,
		c.t("kubectl debug 没有 --dry-run。它只新建一个 Pod "+copyName+"，原来的 Pod 不变。",
			"kubectl debug has no --dry-run. It only creates one new Pod, "+copyName+", and leaves the original Pod alone."),
		c.t("镜像里要有 sh。没有时加 --image=<调试镜像> --share-processes，从旁边的容器查看。",
			"The image must contain sh. If it does not, add --image=<debug image> --share-processes and look from a side container."),
		c.t("替身会占用资源，查完一定要删。", "The double uses resources. Delete it when you are done."))
}

// ---- ImagePullBackOff ----

var (
	imageAuth    = regexp.MustCompile(`(?i)unauthorized|authentication required|authorization failed|access denied|pull access denied|denied:|insufficient_scope|\b401\b|\b403\b`)
	imageNetwork = regexp.MustCompile(`(?i)no such host|i/o timeout|connection refused|tls handshake timeout|network is unreachable|no route to host|dial tcp|context deadline exceeded`)
	imageMissing = regexp.MustCompile(`(?i)not found|manifest unknown|notfound`)
)

func (c *caster) image() {
	ct := c.container(func(x autopsy.ContainerAutopsy) bool {
		return x.State.Kind == "waiting" && (x.State.Reason == "ImagePullBackOff" || x.State.Reason == "ErrImagePull")
	})
	if ct == nil || ct.Image == "" {
		c.gatherRecords()
		return
	}
	c.sp.Target.Container = ct.Name
	msgs := ct.State.Message
	for _, e := range c.a.Events {
		if e.Type == "Warning" {
			msgs += "\n" + e.Message
		}
	}
	repo, tag := splitImage(ct.Image)
	imgCite := c.citeText("spec", "container "+ct.Name+" image:")
	switch {
	case imageAuth.MatchString(msgs):
		c.pullSecret(ct, repo, imgCite)
	case imageNetwork.MatchString(msgs):
		c.registryNetwork(ct, imgCite)
	case imageMissing.MatchString(msgs):
		c.setImage(ct, repo, tag, imgCite)
	default:
		c.gatherRecords()
	}
}

// pullCite cites the first pull failure that matches re, in an event or the status.
func (c *caster) pullCite(re *regexp.Regexp) string {
	return c.cite(func(e autopsy.Evidence) bool {
		return (e.Kind == "event" || e.Kind == "status") && re.MatchString(e.Text)
	})
}

func (c *caster) setImage(ct *autopsy.ContainerAutopsy, repo, tag, imgCite string) {
	c.sp.Remedy = RemedySetImage
	c.sp.Incantation = c.t("它被许诺了一具不存在的躯体。给它一个真正存在的名字。",
		"It was promised a body that does not exist. Give it a name that does.")
	shown := tag
	if shown == "" {
		shown = ct.Image
	}
	c.sp.Diagnosis = append(c.sp.Diagnosis, fmt.Sprintf(c.t(
		"容器 %s 的镜像是 %s%s，仓库里找不到 %s%s。它从未真正启动过。",
		"Container %s uses image %s%s, and the registry has no tag %s%s. It never really started."),
		ct.Name, ct.Image, imgCite, shown, c.pullCite(imageMissing)),
		c.t("证据给不出正确的 tag，所以命令里用 ${NEW_TAG}，由你填写。",
			"The evidence cannot tell the right tag, so the commands use ${NEW_TAG}, which you set."))
	if act := c.action(ActionSetImage); act != nil {
		act.Repo, act.CurrentTag = repo, tag
		act.Inputs = []Input{{Name: InputTag, Kind: "text"}}
		c.sp.Action = act
	}
	newTag := need("NEW_TAG", c.t("设为仓库里真实存在的 tag", "set it to a tag that exists in the registry"))
	newImage := shq(repo) + ":" + newTag
	c.sp.Steps = append(c.sp.Steps,
		Step{Kind: StepCheck, Title: c.t("确认新 tag 在仓库里存在（在你的电脑上运行）", "Confirm the new tag exists in the registry (run on your machine)"),
			Commands: []string{c.targetCheck("image"), "docker manifest inspect " + newImage}})
	c.sp.Steps = append(c.sp.Steps,
		c.applyFix(c.t("施法：换上存在的镜像 tag", "Cast: switch to a tag that exists"),
			"kubectl set image "+c.sp.Target.ref()+" "+shq(ct.Name)+"="+newImage+" "+c.nsFlag()),
		c.verifyStep())
	c.sp.Caveats = append(c.sp.Caveats,
		c.t("tag 从你的发布记录里挑，不要用 latest 这种会变的 tag。", "Pick the tag from your release records. Avoid moving tags such as latest."),
		c.t("清单里的镜像也要一起改，否则下次部署又会写回坏的 tag。", "Change the image in your manifest too, or the next deploy writes the bad tag back."))
	if !c.sp.Target.InPlace {
		c.sp.Caveats = append(c.sp.Caveats, c.t("先在清单里把镜像改成 "+repo+":${NEW_TAG}。", "First set the image in the manifest to "+repo+":${NEW_TAG}."))
	}
	c.inferredCaveat()
}

func (c *caster) pullSecret(ct *autopsy.ContainerAutopsy, repo, imgCite string) {
	c.sp.Remedy = RemedyPullSecret
	registry := registryOf(repo)
	c.sp.Incantation = c.t("守门人不认识它。给它一张通行证。", "The gatekeeper does not know it. Give it a pass.")
	c.sp.Diagnosis = append(c.sp.Diagnosis, fmt.Sprintf(c.t(
		"容器 %s 的镜像 %s%s 拉取被拒绝%s，仓库 %s 要求登录或账号没有权限。",
		"Container %s: pulling image %s%s was refused%s, so registry %s wants a login or the account lacks access."),
		ct.Name, ct.Image, imgCite, c.pullCite(imageAuth), registry),
		c.t("证据里没有账号，命令里的 ${REGISTRY_USER}、${REGISTRY_PASSWORD}、${PULL_SECRET} 由你填写。",
			"The evidence holds no account, so you set ${REGISTRY_USER}, ${REGISTRY_PASSWORD} and ${PULL_SECRET}."))
	secret := need("PULL_SECRET", c.t("拉取凭据 Secret 的名字", "name for the pull Secret"))
	pathPrefix := ".spec.template.spec"
	patchJSON := `'{"spec":{"template":{"spec":{"imagePullSecrets":[{"name":"'` + secret + `'"}]}}}}'`
	if !c.sp.Target.templated() {
		pathPrefix = ".spec"
	}
	c.sp.Steps = append(c.sp.Steps,
		Step{Kind: StepCheck, Title: c.t("查看现在用的拉取凭据", "Look at the pull credentials in use"),
			Commands: []string{"kubectl get " + c.sp.Target.ref() + " " + c.nsFlag() + ` -o jsonpath='{` + pathPrefix + `.imagePullSecrets}{"\n"}'`}},
		writeStep(c.t("施法：创建拉取凭据 Secret", "Cast: create the pull Secret"),
			"kubectl create secret docker-registry "+secret+" --docker-server="+shq(registry)+
				" --docker-username="+need("REGISTRY_USER", c.t("仓库账号", "registry user"))+
				" --docker-password="+need("REGISTRY_PASSWORD", c.t("仓库密码或令牌，先用 read -rs 读入", "registry password or token, read it with read -rs first"))+
				" "+c.nsFlag()),
		c.applyFix(c.t("施法：让 Pod 使用这个凭据", "Cast: make the Pod use the Secret"),
			"kubectl patch "+c.sp.Target.ref()+" "+c.nsFlag()+" --type=strategic --patch "+patchJSON),
		c.verifyStep())
	c.sp.Caveats = append(c.sp.Caveats,
		c.t("先运行 read -rs REGISTRY_PASSWORD，密码就不会留在 shell 历史里。", "Run read -rs REGISTRY_PASSWORD first, so the password stays out of your shell history."),
		c.t("用只读的拉取令牌。Secret 只是 base64 编码，不是加密。", "Use a read-only pull token. A Secret is only base64-encoded, not encrypted."),
		c.t("如果仓库地址或镜像名写错，也会报没有权限。", "A misspelled registry or image name can also look like a permission error."))
	c.inferredCaveat()
}

func (c *caster) registryNetwork(ct *autopsy.ContainerAutopsy, imgCite string) {
	c.sp.Remedy = RemedyRegistryNetwork
	c.sp.Incantation = c.t("通往仓库的路断了。先把路修好，它自己会走过来。",
		"The road to the registry is broken. Mend the road and it will walk here on its own.")
	c.sp.Diagnosis = append(c.sp.Diagnosis, fmt.Sprintf(c.t(
		"节点拉不到镜像 %s%s：连不上仓库%s。这是网络问题，不是镜像或清单的问题。",
		"The node cannot pull image %s%s because it cannot reach the registry%s. This is a network problem, not an image or manifest problem."),
		ct.Image, imgCite, c.pullCite(imageNetwork)))
	node := c.a.NodeName
	title := c.t("在节点上试着拉一次镜像", "Try the pull on the node")
	if node != "" {
		title = fmt.Sprintf(c.t("在节点 %s 上试着拉一次镜像（kind：docker exec -it %s bash）", "Try the pull on node %s (kind: docker exec -it %s bash)"), node, node)
	}
	c.sp.Steps = append(c.sp.Steps,
		Step{Kind: StepCheck, Title: c.t("查看拉取失败的事件和节点", "Look at the pull failures and the nodes"),
			Commands: []string{c.eventsCheck(), "kubectl get nodes -o wide"}},
		Step{Kind: StepCheck, Title: title, Commands: []string{"crictl pull " + shq(ct.Image)}},
		Step{Kind: StepVerify, Title: c.t("网络恢复后，确认镜像拉下来了", "After the network is fixed, confirm the image was pulled"),
			Commands: []string{"kubectl get pod " + shq(c.a.Name) + " " + c.nsFlag()}})
	c.sp.Caveats = append(c.sp.Caveats,
		c.t("检查节点的 DNS、代理和防火墙。kubelet 会自己重试拉取，不需要改清单。",
			"Check the node's DNS, proxy and firewall. The kubelet retries the pull on its own, so the manifest needs no change."))
}

// splitImage splits an image reference into the repository and the tag. A
// digest reference returns the part before '@' and no tag.
func splitImage(image string) (repo, tag string) {
	if i := strings.Index(image, "@"); i >= 0 {
		return image[:i], ""
	}
	slash := strings.LastIndex(image, "/")
	if i := strings.LastIndex(image, ":"); i > slash {
		return image[:i], image[i+1:]
	}
	return image, ""
}

// registryOf returns the registry host of a repository, docker.io when it has none.
func registryOf(repo string) string {
	first, _, ok := strings.Cut(repo, "/")
	if ok && (strings.ContainsAny(first, ".:") || first == "localhost") {
		return first
	}
	return "docker.io"
}

// ---- ProbeFailure ----

var (
	probeKind    = regexp.MustCompile(`(?i)(liveness|readiness|startup) probe`)
	probeTimeout = regexp.MustCompile(`(?i)context deadline exceeded|timeout|timed out|deadline`)
	probeRefused = regexp.MustCompile(`(?i)statuscode: ?\d+|connection refused|no such host|not found|404`)
)

func (c *caster) probe() {
	var hit *autopsy.EventRecord
	for i, e := range c.a.Events {
		if e.Reason == "Unhealthy" && probeKind.MatchString(e.Message) {
			hit = &c.a.Events[i]
			break
		}
	}
	ct := c.container(nil)
	if hit == nil || ct == nil {
		c.gatherRecords()
		return
	}
	c.sp.Target.Container = ct.Name
	kind := strings.ToLower(probeKind.FindStringSubmatch(hit.Message)[1])
	field := kind + "Probe"
	evCite := c.cite(func(e autopsy.Evidence) bool {
		return e.Kind == "event" && strings.Contains(e.Text, "Unhealthy") && strings.Contains(strings.ToLower(e.Text), kind+" probe")
	})
	switch {
	case probeRefused.MatchString(hit.Message):
		c.fixProbe(ct, field, evCite)
	case probeTimeout.MatchString(hit.Message):
		c.slowProbe(ct, field, evCite)
	default:
		c.gatherRecords()
	}
}

func (c *caster) slowProbe(ct *autopsy.ContainerAutopsy, field, evCite string) {
	c.sp.Remedy = RemedySlowProbe
	c.sp.Incantation = c.t("他们以为它死了，于是真的杀了它。让守夜人多等一会儿。",
		"They thought it was dead, so they killed it for real. Make the night watch wait a little longer.")
	c.sp.Diagnosis = append(c.sp.Diagnosis, fmt.Sprintf(c.t(
		"容器 %s 的 %s 超时%s：应用回答得太慢，不是不在。",
		"Container %s: its %s timed out%s. The app answered too slowly, it was not gone."),
		ct.Name, field, evCite),
		c.t("证据里没有探针现在的计时，新的 timeoutSeconds 和 failureThreshold 由你填写，要比第一步看到的大。",
			"The evidence does not hold the probe's current timing, so you set the new timeoutSeconds and failureThreshold, larger than the values the first step shows."))
	c.sp.Steps = append(c.sp.Steps, Step{Kind: StepCheck, Title: c.t("查看探针现在的设置和失败事件", "Look at the probe settings and its failures"),
		Commands: []string{c.targetCheck(field), c.eventsCheck()}})
	if dnsLabel.MatchString(ct.Name) {
		if act := c.action(ActionPatch); act != nil {
			act.Probe = field
			act.Inputs = []Input{
				{Name: InputTimeoutSeconds, Kind: "number", Min: 1, Max: maxProbeTimeout},
				{Name: InputFailureThreshold, Kind: "number", Min: 1, Max: maxProbeFailures},
			}
			c.sp.Action = act
		}
		patch := `'{"spec":{"template":{"spec":{"containers":[{"name":"` + ct.Name + `","` + field + `":{"timeoutSeconds":'` +
			need("TIMEOUT_SECONDS", c.t("新的 timeoutSeconds", "new timeoutSeconds")) + `',"failureThreshold":'` +
			need("FAILURE_THRESHOLD", c.t("新的 failureThreshold", "new failureThreshold")) + `'}}]}}}}'`
		c.sp.Steps = append(c.sp.Steps,
			c.applyFix(c.t("施法：放宽探针计时", "Cast: give the probe more time"),
				"kubectl patch "+c.sp.Target.ref()+" "+c.nsFlag()+" --type=strategic --patch "+patch),
			c.verifyStep())
	}
	c.sp.Caveats = append(c.sp.Caveats,
		c.t("放宽探针只是让它别被误杀。应用为什么慢（GC、依赖延迟、CPU 限流）还要查。",
			"A slower probe only stops the false kills. Still find out why the app is slow: GC, dependency latency or CPU throttling."))
	c.inferredCaveat()
}

func (c *caster) fixProbe(ct *autopsy.ContainerAutopsy, field, evCite string) {
	c.sp.Remedy = RemedyFixProbe
	c.sp.Incantation = c.t("守夜人敲错了门。给他正确的门牌，而不是更多的耐心。",
		"The night watch knocks on the wrong door. Give it the right address, not more patience.")
	c.sp.Diagnosis = append(c.sp.Diagnosis, fmt.Sprintf(c.t(
		"容器 %s 的 %s 被拒绝或返回错误状态码%s。等再久也一样，所以咒语不调慢探针。",
		"Container %s: its %s was refused or got an error status%s. Waiting longer would not help, so the spell does not slow the probe down."),
		ct.Name, field, evCite))
	c.sp.Steps = append(c.sp.Steps, Step{Kind: StepCheck, Title: c.t("对照探针的路径和端口，看应用实际在听什么", "Compare the probe's path and port with what the app actually serves"),
		Commands: []string{
			c.targetCheck(field),
			c.targetCheck("ports"),
			c.eventsCheck(),
			"kubectl logs " + shq(c.a.Name) + " " + c.nsFlag() + " -c " + shq(ct.Name) + " --previous",
		}})
	c.sp.Caveats = append(c.sp.Caveats,
		c.t("改清单里的探针路径或端口，或者修好应用的健康检查接口。", "Fix the probe path or port in the manifest, or fix the app's health endpoint."),
		c.t("应用还没监听端口时会拒绝连接，这种情况加 startupProbe。", "An app that has not opened its port yet refuses connections. Add a startupProbe for that."))
}

// ---- not enough evidence ----

func (c *caster) gatherRecords() {
	c.sp.Remedy = RemedyGatherRecords
	c.sp.Incantation = c.t("亡灵的记忆太模糊。先把它留下的痕迹收齐，再来念咒。",
		"The spirit's memory is too faint. Gather what it left behind, then cast again.")
	c.sp.Diagnosis = append(c.sp.Diagnosis, fmt.Sprintf(c.t(
		"死因是 %s%s，但证据不足以给出修复方法。下面的命令只补全记录。",
		"The cause is %s%s, but the evidence is not enough for a fix. The commands below only fill in the records."),
		c.cls.Cause, c.citeText("status")))
	c.sp.Steps = append(c.sp.Steps, Step{Kind: StepCheck, Title: c.t("补全记录", "Fill in the records"),
		Commands: []string{
			"kubectl describe pod " + shq(c.a.Name) + " " + c.nsFlag(),
			c.eventsCheck(),
			"kubectl logs " + shq(c.a.Name) + " " + c.nsFlag() + " --all-containers --previous",
			"kubectl logs " + shq(c.a.Name) + " " + c.nsFlag() + " --all-containers",
		}})
	c.sp.Caveats = append(c.sp.Caveats, c.t("Event 默认一小时后就会被删除，尽快查。", "Events are deleted after one hour by default, so look soon."))
}

// ---- small helpers ----

// inferredCaveat warns when the target was inferred.
func (c *caster) inferredCaveat() {
	if c.sp.Target.Inferred {
		c.sp.Caveats = append(c.sp.Caveats, c.t(
			"施咒对象是推断出来的。dry-run 报 NotFound 时，换成真正管理这个 Pod 的对象。",
			"The target is inferred. If the dry run says NotFound, use the object that really manages this Pod."))
	}
}

// exitText is "，退出码 N" / ", exit code N" from the classification, or "".
func (c *caster) exitText() string {
	if c.cls.LastExitCode == nil {
		return ""
	}
	return fmt.Sprintf(c.t("，退出码 %d", ", exit code %d"), *c.cls.LastExitCode)
}

func isTerminated(s autopsy.ContainerState, reason string) bool {
	return s.Kind == "terminated" && s.Reason == reason
}

// exitOf returns the non-zero exit code of a container, or nil.
func exitOf(ct autopsy.ContainerAutopsy) *int32 {
	for _, s := range []*autopsy.ContainerState{&ct.State, ct.LastState} {
		if s != nil && s.Kind == "terminated" && s.ExitCode != nil && *s.ExitCode != 0 {
			return s.ExitCode
		}
	}
	return nil
}

// lastLine is the last log line of a container: the previous log first, as that is the one that died.
func lastLine(ct *autopsy.ContainerAutopsy) string {
	if n := len(ct.PreviousLogTail); n > 0 {
		return ct.PreviousLogTail[n-1]
	}
	if n := len(ct.LogTail); n > 0 {
		return ct.LogTail[n-1]
	}
	return ""
}
