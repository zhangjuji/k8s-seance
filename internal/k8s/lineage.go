package k8s

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/zhangjuji/k8s-seance/internal/autopsy"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Revision labels set by the workload controllers.
const (
	labelPodTemplateHash    = "pod-template-hash"
	labelControllerRevision = "controller-revision-hash"
)

// recentDeath is how long after a container termination a running Pod still
// counts as dead. A crash-looping Pod is running for a moment between crashes.
const recentDeath = 5 * time.Minute

// maxShownRunes caps a displayed spec value, such as a long shell script.
// Values are compared in full.
const maxShownRunes = 120

// family identifies the controller family of a Pod and the Pod's revision.
type family struct {
	kind, name    string
	inferred      bool
	viaReplicaSet string
	hashLabel     string
	revision      string
}

// familyOf returns the family of p. Pods of a ReplicaSet with a
// pod-template-hash belong to the Deployment whose name is the ReplicaSet name
// without "-<hash>". StatefulSet and DaemonSet Pods use controller-revision-hash.
// Other Pods have no family.
func familyOf(p *corev1.Pod) (family, bool) {
	ref := metav1.GetControllerOf(p)
	if ref == nil {
		if len(p.OwnerReferences) == 0 {
			return family{}, false
		}
		ref = &p.OwnerReferences[0]
	}
	switch ref.Kind {
	case "ReplicaSet":
		hash := p.Labels[labelPodTemplateHash]
		if hash == "" || !strings.HasSuffix(ref.Name, "-"+hash) {
			return family{}, false
		}
		return family{
			kind: "Deployment", name: strings.TrimSuffix(ref.Name, "-"+hash),
			inferred: true, viaReplicaSet: ref.Name,
			hashLabel: labelPodTemplateHash, revision: hash,
		}, true
	case "StatefulSet", "DaemonSet":
		hash := p.Labels[labelControllerRevision]
		if hash == "" {
			return family{}, false
		}
		return family{kind: ref.Kind, name: ref.Name, hashLabel: labelControllerRevision, revision: hash}, true
	}
	return family{}, false
}

// CollectLineage finds the family of pod with one Pods list in its namespace
// (Requirement 16.1). It returns nil and no error when the Pod belongs to no
// Deployment, StatefulSet or DaemonSet. A failed list returns the error; the
// caller summons the spirit without a lineage.
func CollectLineage(ctx context.Context, cs kubernetes.Interface, pod *corev1.Pod) (*autopsy.Lineage, error) {
	if _, ok := familyOf(pod); !ok {
		return nil, nil
	}
	list, err := cs.CoreV1().Pods(pod.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list pods in namespace %q for the family of %s: %w", pod.Namespace, pod.Name, err)
	}
	return buildLineage(pod, list.Items, time.Now()), nil
}

// buildLineage groups the family members among pods into siblings and other
// revisions, and compares each other revision's Pod spec with the spirit's.
func buildLineage(self *corev1.Pod, pods []corev1.Pod, now time.Time) *autopsy.Lineage {
	fam, ok := familyOf(self)
	if !ok {
		return nil
	}
	l := &autopsy.Lineage{
		OwnerKind: fam.kind, OwnerName: fam.name,
		Inferred: fam.inferred, ViaReplicaSet: fam.viaReplicaSet,
		HashLabel: fam.hashLabel, Revision: fam.revision,
		Self: kinOf(self, now),
	}
	// The spirit is the one being summoned: it never counts as alive.
	l.Self.Alive = false

	ownOldest := self.CreationTimestamp.Time
	type group struct {
		pods   []*corev1.Pod
		oldest time.Time
	}
	groups := map[string]*group{}
	for i := range pods {
		p := &pods[i]
		if p.Name == self.Name || (self.UID != "" && p.UID == self.UID) {
			continue
		}
		f, ok := familyOf(p)
		if !ok || f.kind != fam.kind || f.name != fam.name {
			continue
		}
		created := p.CreationTimestamp.Time
		if f.revision == fam.revision {
			l.Siblings = append(l.Siblings, kinOf(p, now))
			if created.Before(ownOldest) {
				ownOldest = created
			}
			continue
		}
		g := groups[f.revision]
		if g == nil {
			g = &group{oldest: created}
			groups[f.revision] = g
		}
		g.pods = append(g.pods, p)
		if created.Before(g.oldest) {
			g.oldest = created
		}
	}
	sortKin(l.Siblings)

	revs := make([]string, 0, len(groups))
	for r := range groups {
		revs = append(revs, r)
	}
	sort.Slice(revs, func(i, j int) bool {
		a, b := groups[revs[i]], groups[revs[j]]
		if !a.oldest.Equal(b.oldest) {
			return a.oldest.Before(b.oldest)
		}
		return revs[i] < revs[j]
	})
	for _, r := range revs {
		g := groups[r]
		v := autopsy.Version{Revision: r, Age: autopsy.AgeNewer}
		if g.oldest.Before(ownOldest) {
			v.Age = autopsy.AgeOlder
		}
		var rep *corev1.Pod
		for _, p := range g.pods {
			k := kinOf(p, now)
			v.Pods = append(v.Pods, k)
			// Compare with a living Pod when there is one: it shows what works.
			if rep == nil || (k.Alive && !kinOf(rep, now).Alive) {
				rep = p
			}
		}
		sortKin(v.Pods)
		v.Diffs = diffSpecs(&rep.Spec, &self.Spec)
		l.Versions = append(l.Versions, v)
	}
	return l
}

// sortKin orders Pods by creation time, then name.
func sortKin(k []autopsy.Kin) {
	sort.SliceStable(k, func(i, j int) bool {
		if !k[i].CreatedAt.Equal(k[j].CreatedAt) {
			return k[i].CreatedAt.Before(k[j].CreatedAt)
		}
		return k[i].Name < k[j].Name
	})
}

// kinOf summarizes one family member. A Pod is alive when it is Running, not
// being deleted, every container is running and ready, and no container
// terminated in the last recentDeath.
func kinOf(p *corev1.Pod, now time.Time) autopsy.Kin {
	k := autopsy.Kin{
		Name:      p.Name,
		Node:      p.Spec.NodeName,
		Phase:     string(p.Status.Phase),
		CreatedAt: p.CreationTimestamp.Time,
	}
	alive := p.Status.Phase == corev1.PodRunning && p.DeletionTimestamp == nil && len(p.Status.ContainerStatuses) > 0
	state, lastState := "", ""
	for _, cs := range p.Status.ContainerStatuses {
		k.Restarts += cs.RestartCount
		switch {
		case cs.State.Waiting != nil:
			if state == "" && cs.State.Waiting.Reason != "" {
				state = cs.State.Waiting.Reason
			}
		case cs.State.Terminated != nil:
			if state == "" {
				state = cs.State.Terminated.Reason
				if state == "" {
					state = "exitCode=" + strconv.Itoa(int(cs.State.Terminated.ExitCode))
				}
			}
		}
		if t := cs.LastTerminationState.Terminated; t != nil && now.Sub(t.FinishedAt.Time) < recentDeath {
			if lastState == "" {
				lastState = t.Reason
			}
			alive = false
		}
	}
	for _, cs := range p.Status.ContainerStatuses {
		if cs.State.Running == nil || !cs.Ready {
			alive = false
		}
	}
	k.Alive = alive
	switch {
	case alive:
		k.State = "running"
	case state != "":
		k.State = state
	case lastState != "":
		k.State = lastState
	case p.DeletionTimestamp != nil:
		k.State = "Terminating"
	default:
		k.State = string(p.Status.Phase)
	}
	return k
}

// specValue is one compared field: raw is compared, shown is displayed.
type specValue struct {
	raw, shown string
	hidden     bool
}

// specFields flattens the fields of a Pod spec that matter for troubleshooting,
// in a stable order. The projected service account token volume (kube-api-access-*)
// differs per Pod and is skipped.
type specFields struct {
	keys []string
	m    map[string]specValue
}

func (f *specFields) set(key, raw string) {
	shown := autopsy.Redact(raw)
	f.put(key, specValue{raw: raw, shown: shorten(shown), hidden: false})
}

func (f *specFields) put(key string, v specValue) {
	if _, ok := f.m[key]; !ok {
		f.keys = append(f.keys, key)
	}
	f.m[key] = v
}

func shorten(s string) string {
	if utf8.RuneCountInString(s) <= maxShownRunes {
		return s
	}
	return string([]rune(s)[:maxShownRunes]) + "…"
}

func isTokenVolume(name string) bool { return strings.HasPrefix(name, "kube-api-access-") }

func flattenSpec(s *corev1.PodSpec) *specFields {
	f := &specFields{m: map[string]specValue{}}
	sa := s.ServiceAccountName
	if sa == "" {
		sa = "default"
	}
	f.set("serviceAccount", sa)
	if len(s.NodeSelector) > 0 {
		f.set("nodeSelector", mapText(s.NodeSelector))
	}
	for _, v := range s.Volumes {
		if isTokenVolume(v.Name) {
			continue
		}
		f.set("volume "+v.Name, volumeText(v))
	}
	for _, c := range s.Containers {
		p := "container " + c.Name
		f.set(p, "present")
		f.set(p+" image", c.Image)
		if len(c.Command) > 0 {
			f.set(p+" command", quoteList(c.Command))
		}
		if len(c.Args) > 0 {
			f.set(p+" args", quoteList(c.Args))
		}
		for _, e := range c.Env {
			f.put(p+" env "+e.Name, envValue(e))
		}
		if len(c.EnvFrom) > 0 {
			parts := make([]string, 0, len(c.EnvFrom))
			for _, ef := range c.EnvFrom {
				switch {
				case ef.ConfigMapRef != nil:
					parts = append(parts, ef.Prefix+"configMap "+ef.ConfigMapRef.Name)
				case ef.SecretRef != nil:
					parts = append(parts, ef.Prefix+"secret "+ef.SecretRef.Name)
				}
			}
			f.set(p+" envFrom", strings.Join(parts, ", "))
		}
		if len(c.Resources.Limits) > 0 {
			f.set(p+" limits", resourceText(c.Resources.Limits))
		}
		if len(c.Resources.Requests) > 0 {
			f.set(p+" requests", resourceText(c.Resources.Requests))
		}
		if c.LivenessProbe != nil {
			f.set(p+" livenessProbe", probeText(c.LivenessProbe))
		}
		if c.ReadinessProbe != nil {
			f.set(p+" readinessProbe", probeText(c.ReadinessProbe))
		}
		if c.StartupProbe != nil {
			f.set(p+" startupProbe", probeText(c.StartupProbe))
		}
		if len(c.Ports) > 0 {
			parts := make([]string, 0, len(c.Ports))
			for _, port := range c.Ports {
				proto := string(port.Protocol)
				if proto == "" {
					proto = "TCP"
				}
				s := fmt.Sprintf("%d/%s", port.ContainerPort, proto)
				if port.Name != "" {
					s += " " + port.Name
				}
				parts = append(parts, s)
			}
			f.set(p+" ports", strings.Join(parts, ", "))
		}
		var mounts []string
		for _, m := range c.VolumeMounts {
			if isTokenVolume(m.Name) {
				continue
			}
			s := m.Name + " at " + m.MountPath
			if m.SubPath != "" {
				s += " subPath=" + m.SubPath
			}
			if m.ReadOnly {
				s += " ro"
			}
			mounts = append(mounts, s)
		}
		if len(mounts) > 0 {
			f.set(p+" mounts", strings.Join(mounts, ", "))
		}
	}
	return f
}

// envValue shows a literal value unless it looks secret, and only the
// reference of a value that comes from a Secret, a ConfigMap or a field.
func envValue(e corev1.EnvVar) specValue {
	if vf := e.ValueFrom; vf != nil {
		var ref string
		switch {
		case vf.SecretKeyRef != nil:
			ref = "from secret " + vf.SecretKeyRef.Name + " key " + vf.SecretKeyRef.Key
		case vf.ConfigMapKeyRef != nil:
			ref = "from configMap " + vf.ConfigMapKeyRef.Name + " key " + vf.ConfigMapKeyRef.Key
		case vf.FieldRef != nil:
			ref = "from field " + vf.FieldRef.FieldPath
		case vf.ResourceFieldRef != nil:
			ref = "from resource " + vf.ResourceFieldRef.Resource
		default:
			ref = "from a reference"
		}
		return specValue{raw: ref, shown: autopsy.Redact(ref)}
	}
	shown, hidden := autopsy.EnvValue(e.Name, e.Value)
	if hidden {
		return specValue{raw: "value " + e.Value, hidden: true}
	}
	return specValue{raw: "value " + e.Value, shown: shorten(strconv.Quote(shown))}
}

// diffSpecs lists the compared fields that differ between from (another
// revision) and to (the spirit). A field present on one side only is shown as
// "(none)" on the other. A container present on one side only is one
// difference, without its subfields. A changed secret value is reported as
// hidden, with neither value.
func diffSpecs(from, to *corev1.PodSpec) []autopsy.SpecDiff {
	a, b := flattenSpec(from), flattenSpec(to)
	keys := append([]string(nil), b.keys...)
	for _, k := range a.keys {
		if _, ok := b.m[k]; !ok {
			keys = append(keys, k)
		}
	}
	// Containers that exist on one side only are reported once.
	lonely := map[string]bool{}
	for _, k := range keys {
		if strings.HasPrefix(k, "container ") && strings.Count(k, " ") == 1 {
			_, inA := a.m[k]
			_, inB := b.m[k]
			if inA != inB {
				lonely[k] = true
			}
		}
	}
	var out []autopsy.SpecDiff
	for _, k := range keys {
		if c := containerKey(k); c != "" && c != k && lonely[c] {
			continue
		}
		va, inA := a.m[k]
		vb, inB := b.m[k]
		if inA && inB && va.raw == vb.raw {
			continue
		}
		d := autopsy.SpecDiff{Field: k}
		if (inA && va.hidden) || (inB && vb.hidden) {
			d.Hidden = true
		} else {
			if inA {
				d.From = va.shown
			}
			if inB {
				d.To = vb.shown
			}
		}
		out = append(out, d)
	}
	return out
}

// containerKey returns "container <name>" for a container field key, or "".
func containerKey(k string) string {
	if !strings.HasPrefix(k, "container ") {
		return ""
	}
	rest := strings.TrimPrefix(k, "container ")
	if i := strings.IndexByte(rest, ' '); i >= 0 {
		return "container " + rest[:i]
	}
	return k
}

func mapText(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+m[k])
	}
	return strings.Join(parts, ", ")
}

func resourceText(list corev1.ResourceList) string {
	m := resourceMap(list)
	return mapText(m)
}

func quoteList(items []string) string {
	parts := make([]string, 0, len(items))
	for _, s := range items {
		parts = append(parts, strconv.Quote(s))
	}
	return "[" + strings.Join(parts, " ") + "]"
}

func volumeText(v corev1.Volume) string {
	src := v.VolumeSource
	switch {
	case src.ConfigMap != nil:
		return "configMap " + src.ConfigMap.Name
	case src.Secret != nil:
		return "secret " + src.Secret.SecretName
	case src.EmptyDir != nil:
		s := "emptyDir"
		if src.EmptyDir.Medium != "" {
			s += " medium=" + string(src.EmptyDir.Medium)
		}
		if src.EmptyDir.SizeLimit != nil {
			s += " sizeLimit=" + src.EmptyDir.SizeLimit.String()
		}
		return s
	case src.PersistentVolumeClaim != nil:
		return "persistentVolumeClaim " + src.PersistentVolumeClaim.ClaimName
	case src.HostPath != nil:
		return "hostPath " + src.HostPath.Path
	case src.Projected != nil:
		return "projected"
	case src.DownwardAPI != nil:
		return "downwardAPI"
	}
	return "other"
}

func probeText(p *corev1.Probe) string {
	var h string
	switch {
	case p.HTTPGet != nil:
		h = "httpGet " + p.HTTPGet.Path + " port " + p.HTTPGet.Port.String()
	case p.TCPSocket != nil:
		h = "tcpSocket port " + p.TCPSocket.Port.String()
	case p.Exec != nil:
		h = "exec " + quoteList(p.Exec.Command)
	case p.GRPC != nil:
		h = "grpc port " + strconv.Itoa(int(p.GRPC.Port))
	default:
		h = "probe"
	}
	return fmt.Sprintf("%s delay=%ds period=%ds timeout=%ds failure=%d",
		h, p.InitialDelaySeconds, p.PeriodSeconds, p.TimeoutSeconds, p.FailureThreshold)
}
