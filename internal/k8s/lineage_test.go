package k8s

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zhangjuji/k8s-seance/internal/autopsy"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/yaml"
)

var lineageNow = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

// famPod is a Pod of Deployment deploy at revision hash, created at created.
// alive picks a running, ready container; otherwise it crash-loops.
func famPod(deploy, hash, name, node string, created time.Time, alive bool, spec corev1.PodSpec) corev1.Pod {
	rs := deploy + "-" + hash
	yes := true
	p := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:         "seance-demo",
			Name:              name,
			UID:               types.UID("uid-" + name),
			CreationTimestamp: metav1.NewTime(created),
			Labels:            map[string]string{labelPodTemplateHash: hash, "app": deploy},
			OwnerReferences:   []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rs, Controller: &yes}},
		},
		Spec:   *spec.DeepCopy(),
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	p.Spec.NodeName = node
	for _, c := range p.Spec.Containers {
		st := corev1.ContainerStatus{Name: c.Name}
		if alive {
			st.Ready = true
			st.State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(created)}}
		} else {
			st.RestartCount = 4
			st.State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}
			st.LastTerminationState = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				ExitCode: 1, Reason: "Error", FinishedAt: metav1.NewTime(lineageNow.Add(-time.Minute))}}
		}
		p.Status.ContainerStatuses = append(p.Status.ContainerStatuses, st)
	}
	return p
}

func appSpec(mode string) corev1.PodSpec {
	return corev1.PodSpec{
		Containers: []corev1.Container{{
			Name:  "app",
			Image: "busybox:1.36",
			Env:   []corev1.EnvVar{{Name: "MODE", Value: mode}},
		}},
	}
}

// withTokenVolume adds the per-Pod service account token volume and mount.
func withTokenVolume(s corev1.PodSpec, suffix string) corev1.PodSpec {
	s = *s.DeepCopy()
	name := "kube-api-access-" + suffix
	s.Volumes = append(s.Volumes, corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{}}})
	s.Containers[0].VolumeMounts = append(s.Containers[0].VolumeMounts, corev1.VolumeMount{Name: name, MountPath: "/var/run/secrets/kubernetes.io/serviceaccount", ReadOnly: true})
	return s
}

func TestFamilyOf(t *testing.T) {
	yes := true
	pod := func(kind, owner string, labels map[string]string) *corev1.Pod {
		p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Labels: labels}}
		if kind != "" {
			p.OwnerReferences = []metav1.OwnerReference{{Kind: kind, Name: owner, Controller: &yes}}
		}
		return p
	}
	tests := []struct {
		name   string
		pod    *corev1.Pod
		ok     bool
		kind   string
		owner  string
		hash   string
		viaRS  string
		infers bool
	}{
		{"deployment pod", pod("ReplicaSet", "web-api-5c6d7f8b9", map[string]string{labelPodTemplateHash: "5c6d7f8b9"}), true, "Deployment", "web-api", "5c6d7f8b9", "web-api-5c6d7f8b9", true},
		{"replicaset without hash label", pod("ReplicaSet", "lonely-rs", nil), false, "", "", "", "", false},
		{"hash label not in the ReplicaSet name", pod("ReplicaSet", "web-abc", map[string]string{labelPodTemplateHash: "zzz"}), false, "", "", "", "", false},
		{"statefulset", pod("StatefulSet", "db", map[string]string{labelControllerRevision: "db-6f9c"}), true, "StatefulSet", "db", "db-6f9c", "", false},
		{"daemonset", pod("DaemonSet", "agent", map[string]string{labelControllerRevision: "7d8e"}), true, "DaemonSet", "agent", "7d8e", "", false},
		{"statefulset without revision", pod("StatefulSet", "db", nil), false, "", "", "", "", false},
		{"job", pod("Job", "migrate", map[string]string{"controller-uid": "x"}), false, "", "", "", "", false},
		{"bare pod", pod("", "", nil), false, "", "", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, ok := familyOf(tt.pod)
			if ok != tt.ok {
				t.Fatalf("ok = %v, want %v", ok, tt.ok)
			}
			if !ok {
				return
			}
			if f.kind != tt.kind || f.name != tt.owner || f.revision != tt.hash || f.viaReplicaSet != tt.viaRS || f.inferred != tt.infers {
				t.Errorf("family = %+v", f)
			}
		})
	}
}

func TestBuildLineageRegressionAfterBadRelease(t *testing.T) {
	t0 := lineageNow.Add(-2 * time.Hour)
	spirit := famPod("second-life", "bbbbbbbbb", "second-life-bbbbbbbbb-new01", "kind-worker", t0.Add(time.Hour), false, withTokenVolume(appSpec("turbo"), "zz9"))
	pods := []corev1.Pod{
		famPod("second-life", "aaaaaaaaa", "second-life-aaaaaaaaa-old01", "kind-worker", t0, true, withTokenVolume(appSpec("normal"), "aa1")),
		famPod("second-life", "aaaaaaaaa", "second-life-aaaaaaaaa-old02", "kind-worker2", t0.Add(time.Second), true, withTokenVolume(appSpec("normal"), "aa2")),
		spirit,
		// Another Deployment whose name starts with the same prefix is not family.
		famPod("second-life-api", "ccccccccc", "second-life-api-ccccccccc-x1", "kind-worker", t0, true, appSpec("normal")),
		// A bare Pod is not family either.
		{ObjectMeta: metav1.ObjectMeta{Name: "loner", Namespace: "seance-demo"}},
	}
	l := buildLineage(&spirit, pods, lineageNow)
	if l == nil {
		t.Fatal("lineage = nil")
	}
	if l.OwnerKind != "Deployment" || l.OwnerName != "second-life" || !l.Inferred || l.ViaReplicaSet != "second-life-bbbbbbbbb" {
		t.Errorf("owner = %s/%s inferred=%v via=%s", l.OwnerKind, l.OwnerName, l.Inferred, l.ViaReplicaSet)
	}
	if l.HashLabel != labelPodTemplateHash || l.Revision != "bbbbbbbbb" {
		t.Errorf("revision = %s=%s", l.HashLabel, l.Revision)
	}
	if len(l.Siblings) != 0 {
		t.Errorf("siblings = %+v, want none", l.Siblings)
	}
	if l.Self.Alive || l.Self.State != "CrashLoopBackOff" || l.Self.Node != "kind-worker" {
		t.Errorf("self = %+v", l.Self)
	}
	if len(l.Versions) != 1 {
		t.Fatalf("versions = %+v, want one", l.Versions)
	}
	v := l.Versions[0]
	if v.Revision != "aaaaaaaaa" || v.Age != autopsy.AgeOlder || v.Alive() != 2 || len(v.Pods) != 2 {
		t.Errorf("version = %+v", v)
	}
	if v.Pods[0].Name != "second-life-aaaaaaaaa-old01" || v.Pods[1].Node != "kind-worker2" {
		t.Errorf("version pods = %+v", v.Pods)
	}
	want := []autopsy.SpecDiff{{Field: "container app env MODE", From: `"normal"`, To: `"turbo"`}}
	if !reflect.DeepEqual(v.Diffs, want) {
		t.Errorf("diffs = %+v, want %+v (the token volume must be ignored)", v.Diffs, want)
	}
	if got := l.Verdict(); got != autopsy.VerdictRegression {
		t.Errorf("verdict = %q, want regression", got)
	}
	if f := l.LivingFormerLife(); f == nil || f.Revision != "aaaaaaaaa" {
		t.Errorf("living former life = %+v", f)
	}
}

func TestBuildLineageVerdicts(t *testing.T) {
	t0 := lineageNow.Add(-2 * time.Hour)
	spec := appSpec("normal")
	spirit := famPod("web", "aaaaaaaaa", "web-aaaaaaaaa-s1", "node-a", t0, false, spec)

	t.Run("instance: a living sibling", func(t *testing.T) {
		l := buildLineage(&spirit, []corev1.Pod{spirit,
			famPod("web", "aaaaaaaaa", "web-aaaaaaaaa-s2", "node-b", t0, true, spec)}, lineageNow)
		if got := l.Verdict(); got != autopsy.VerdictInstance {
			t.Errorf("verdict = %q", got)
		}
		if len(l.Siblings) != 1 || !l.Siblings[0].Alive || l.Siblings[0].Node != "node-b" {
			t.Errorf("siblings = %+v", l.Siblings)
		}
	})
	t.Run("instance wins over a living other revision", func(t *testing.T) {
		l := buildLineage(&spirit, []corev1.Pod{
			famPod("web", "aaaaaaaaa", "web-aaaaaaaaa-s2", "node-b", t0, true, spec),
			famPod("web", "ccccccccc", "web-ccccccccc-n1", "node-b", t0.Add(time.Hour), true, appSpec("fast")),
		}, lineageNow)
		if got := l.Verdict(); got != autopsy.VerdictInstance {
			t.Errorf("verdict = %q", got)
		}
		if l.LivingFormerLife() != nil {
			t.Error("living former life must be nil unless the verdict is regression")
		}
	})
	t.Run("regression with a newer living revision is no rollback", func(t *testing.T) {
		l := buildLineage(&spirit, []corev1.Pod{
			famPod("web", "ccccccccc", "web-ccccccccc-n1", "node-b", t0.Add(time.Hour), true, appSpec("fast")),
		}, lineageNow)
		if got := l.Verdict(); got != autopsy.VerdictRegression {
			t.Errorf("verdict = %q", got)
		}
		if l.Versions[0].Age != autopsy.AgeNewer {
			t.Errorf("age = %q, want newer", l.Versions[0].Age)
		}
		if l.LivingFormerLife() != nil {
			t.Error("a newer living revision is not a former life")
		}
	})
	t.Run("family: everyone is dead", func(t *testing.T) {
		l := buildLineage(&spirit, []corev1.Pod{
			famPod("web", "aaaaaaaaa", "web-aaaaaaaaa-s2", "node-b", t0, false, spec),
			famPod("web", "ccccccccc", "web-ccccccccc-o1", "node-b", t0.Add(-time.Hour), false, spec),
		}, lineageNow)
		if got := l.Verdict(); got != autopsy.VerdictFamily {
			t.Errorf("verdict = %q", got)
		}
		if len(l.Versions[0].Diffs) != 0 {
			t.Errorf("diffs = %+v, want none for the same spec", l.Versions[0].Diffs)
		}
	})
	t.Run("alone", func(t *testing.T) {
		l := buildLineage(&spirit, []corev1.Pod{spirit}, lineageNow)
		if got := l.Verdict(); got != autopsy.VerdictAlone {
			t.Errorf("verdict = %q", got)
		}
	})
	t.Run("a running Pod that just died is not alive", func(t *testing.T) {
		p := famPod("web", "aaaaaaaaa", "web-aaaaaaaaa-s2", "node-b", t0, true, spec)
		p.Status.ContainerStatuses[0].RestartCount = 3
		p.Status.ContainerStatuses[0].LastTerminationState = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ExitCode: 137, Reason: "OOMKilled", FinishedAt: metav1.NewTime(lineageNow.Add(-30 * time.Second))}}
		l := buildLineage(&spirit, []corev1.Pod{p}, lineageNow)
		if l.Siblings[0].Alive || l.Siblings[0].State != "OOMKilled" {
			t.Errorf("sibling = %+v, want dead OOMKilled", l.Siblings[0])
		}
		// An old death does not count.
		p.Status.ContainerStatuses[0].LastTerminationState.Terminated.FinishedAt = metav1.NewTime(lineageNow.Add(-time.Hour))
		l = buildLineage(&spirit, []corev1.Pod{p}, lineageNow)
		if !l.Siblings[0].Alive {
			t.Errorf("sibling = %+v, want alive after an old death", l.Siblings[0])
		}
	})
	t.Run("a terminating Pod is not alive", func(t *testing.T) {
		p := famPod("web", "aaaaaaaaa", "web-aaaaaaaaa-s2", "node-b", t0, true, spec)
		del := metav1.NewTime(lineageNow)
		p.DeletionTimestamp = &del
		l := buildLineage(&spirit, []corev1.Pod{p}, lineageNow)
		if l.Siblings[0].Alive {
			t.Error("terminating sibling counted as alive")
		}
	})
}

func TestBuildLineageStatefulSet(t *testing.T) {
	yes := true
	sts := func(name, rev string, alive bool) corev1.Pod {
		p := famPod("x", "y", name, "n1", lineageNow.Add(-time.Hour), alive, appSpec(rev))
		p.Labels = map[string]string{labelControllerRevision: "db-" + rev}
		p.OwnerReferences = []metav1.OwnerReference{{Kind: "StatefulSet", Name: "db", Controller: &yes}}
		return p
	}
	spirit := sts("db-1", "v2", false)
	l := buildLineage(&spirit, []corev1.Pod{sts("db-0", "v1", true), spirit}, lineageNow)
	if l.OwnerKind != "StatefulSet" || l.OwnerName != "db" || l.Inferred || l.HashLabel != labelControllerRevision {
		t.Errorf("lineage = %+v", l)
	}
	if l.Verdict() != autopsy.VerdictRegression || len(l.Versions) != 1 || l.Versions[0].Revision != "db-v1" {
		t.Errorf("verdict = %s versions = %+v", l.Verdict(), l.Versions)
	}
}

func TestDiffSpecsComparesTroubleshootingFields(t *testing.T) {
	q := func(s string) corev1.ResourceList {
		return corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(s)}
	}
	base := corev1.PodSpec{
		ServiceAccountName: "app",
		NodeSelector:       map[string]string{"pool": "a"},
		Volumes: []corev1.Volume{
			{Name: "cfg", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "cfg-v1"}}}},
		},
		Containers: []corev1.Container{{
			Name:    "app",
			Image:   "app:1",
			Command: []string{"run"},
			Args:    []string{"--port=8080"},
			Env: []corev1.EnvVar{
				{Name: "MODE", Value: "normal"},
				{Name: "DB_PASSWORD", Value: "hunter2"},
				{Name: "API_TOKEN", Value: "same"},
				{Name: "DSN", Value: "postgres://u@db?password=abc"},
				{Name: "FROM_SECRET", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "s1"}, Key: "k"}}},
			},
			Resources:      corev1.ResourceRequirements{Limits: q("64Mi"), Requests: q("32Mi")},
			ReadinessProbe: &corev1.Probe{PeriodSeconds: 10, FailureThreshold: 3},
			Ports:          []corev1.ContainerPort{{ContainerPort: 8080}},
			VolumeMounts:   []corev1.VolumeMount{{Name: "cfg", MountPath: "/etc/app"}},
		}, {
			Name: "sidecar", Image: "proxy:1",
		}},
	}
	other := *base.DeepCopy()
	c := &other.Containers[0]
	c.Image = "app:2"
	c.Args = []string{"--port=9090"}
	c.Env[0].Value = "turbo"
	c.Env[1].Value = "hunter3"
	c.Env[3].Value = "postgres://u@db?password=xyz"
	c.Env[4].ValueFrom.SecretKeyRef.Name = "s2"
	c.Resources.Limits = q("128Mi")
	c.ReadinessProbe.PeriodSeconds = 5
	c.Ports[0].ContainerPort = 9090
	c.VolumeMounts[0].MountPath = "/etc/app2"
	other.ServiceAccountName = "app2"
	other.NodeSelector = map[string]string{"pool": "b"}
	other.Volumes[0].ConfigMap.Name = "cfg-v2"
	other.Containers = other.Containers[:1] // the spirit has no sidecar

	diffs := diffSpecs(&base, &other)
	got := map[string]autopsy.SpecDiff{}
	for _, d := range diffs {
		got[d.Field] = d
	}
	expect := map[string]autopsy.SpecDiff{
		"serviceAccount":                {Field: "serviceAccount", From: "app", To: "app2"},
		"nodeSelector":                  {Field: "nodeSelector", From: "pool=a", To: "pool=b"},
		"volume cfg":                    {Field: "volume cfg", From: "configMap cfg-v1", To: "configMap cfg-v2"},
		"container app image":           {Field: "container app image", From: "app:1", To: "app:2"},
		"container app args":            {Field: "container app args", From: `["--port=8080"]`, To: `["--port=9090"]`},
		"container app env MODE":        {Field: "container app env MODE", From: `"normal"`, To: `"turbo"`},
		"container app env DB_PASSWORD": {Field: "container app env DB_PASSWORD", Hidden: true},
		"container app env DSN":         {Field: "container app env DSN", Hidden: true},
		"container app env FROM_SECRET": {Field: "container app env FROM_SECRET", From: "from secret s1 key k", To: "from secret s2 key k"},
		"container app limits":          {Field: "container app limits", From: "memory=64Mi", To: "memory=128Mi"},
		"container app readinessProbe":  {Field: "container app readinessProbe", From: "probe delay=0s period=10s timeout=0s failure=3", To: "probe delay=0s period=5s timeout=0s failure=3"},
		"container app ports":           {Field: "container app ports", From: "8080/TCP", To: "9090/TCP"},
		"container app mounts":          {Field: "container app mounts", From: "cfg at /etc/app", To: "cfg at /etc/app2"},
		"container sidecar":             {Field: "container sidecar", From: "present"},
	}
	if !reflect.DeepEqual(got, expect) {
		for k, v := range expect {
			if got[k] != v {
				t.Errorf("%s = %+v, want %+v", k, got[k], v)
			}
		}
		for k := range got {
			if _, ok := expect[k]; !ok {
				t.Errorf("unexpected diff %+v", got[k])
			}
		}
	}
	// Unchanged secrets are not reported, and no secret value shows anywhere.
	for _, d := range diffs {
		if strings.Contains(d.Field, "API_TOKEN") {
			t.Errorf("unchanged secret reported: %+v", d)
		}
		for _, s := range []string{"hunter", "abc", "xyz"} {
			if strings.Contains(d.From+d.To, s) {
				t.Errorf("secret %q leaked in %+v", s, d)
			}
		}
	}
	// The spirit's own containers come first, in spec order.
	if diffs[0].Field != "serviceAccount" {
		t.Errorf("first diff = %s, want serviceAccount", diffs[0].Field)
	}
}

func TestCollectAutopsyAddsLineage(t *testing.T) {
	t0 := lineageNow.Add(-2 * time.Hour)
	spirit := famPod("second-life", "bbbbbbbbb", "second-life-bbbbbbbbb-new01", "kind-worker", t0.Add(time.Hour), false, appSpec("turbo"))
	old := famPod("second-life", "aaaaaaaaa", "second-life-aaaaaaaaa-old01", "kind-worker", t0, true, appSpec("normal"))
	cs := fake.NewClientset(&spirit, &old)

	a, err := CollectAutopsy(context.Background(), cs, "seance-demo", spirit.Name)
	if err != nil {
		t.Fatal(err)
	}
	if a.Lineage == nil || a.Lineage.Verdict() != autopsy.VerdictRegression {
		t.Fatalf("lineage = %+v, want regression", a.Lineage)
	}
	var lists int
	for _, act := range cs.Actions() {
		if act.GetVerb() == "list" && act.GetResource().Resource == "pods" {
			lists++
		}
		switch act.GetVerb() {
		case "get", "list", "watch":
		default:
			t.Errorf("write-style action %s %s", act.GetVerb(), act.GetResource().Resource)
		}
	}
	if lists != 1 {
		t.Errorf("pods lists = %d, want exactly 1", lists)
	}

	ev, err := autopsy.BuildEvidence(a, autopsy.EvidenceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var lineage []string
	for _, e := range ev {
		if e.Kind == "lineage" {
			lineage = append(lineage, e.Text)
		}
	}
	if len(lineage) != 2 || !strings.HasPrefix(lineage[0], "family Deployment/second-life (inferred from ReplicaSet second-life-bbbbbbbbb): verdict=regression") ||
		!strings.Contains(lineage[1], `container app env MODE: "normal" -> "turbo"`) {
		t.Errorf("lineage evidence = %q", lineage)
	}
}

func TestCollectAutopsyWithoutLineageWhenListFails(t *testing.T) {
	spirit := famPod("web", "aaaaaaaaa", "web-aaaaaaaaa-s1", "n", lineageNow, false, appSpec("x"))
	cs := fake.NewClientset(&spirit)
	cs.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "", errors.New("no list"))
	})
	a, err := CollectAutopsy(context.Background(), cs, "seance-demo", spirit.Name)
	if err != nil {
		t.Fatalf("CollectAutopsy error = %v, want nil when the family list fails", err)
	}
	if a.Lineage != nil {
		t.Errorf("lineage = %+v, want nil", a.Lineage)
	}
}

func TestCollectAutopsyBarePodListsNothing(t *testing.T) {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "bare"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "busybox"}}},
	}
	cs := fake.NewClientset(p)
	a, err := CollectAutopsy(context.Background(), cs, "default", "bare")
	if err != nil {
		t.Fatal(err)
	}
	if a.Lineage != nil {
		t.Errorf("lineage = %+v, want nil for a bare Pod", a.Lineage)
	}
	for _, act := range cs.Actions() {
		if act.GetVerb() == "list" && act.GetResource().Resource == "pods" {
			t.Error("a bare Pod must not list the namespace")
		}
	}
}

// TestSecondLifeDemoDiffsOnlyMode builds the two revisions of
// demo/second-life.yaml, as kill-them-all.sh releases them, and checks the
// family: the bad release differs from v1 only in MODE, and v1 lives on.
func TestSecondLifeDemoDiffsOnlyMode(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "demo", "second-life.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var d appsv1.Deployment
	if err := yaml.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	v1 := d.Spec.Template.Spec
	v2 := *v1.DeepCopy()
	for i, e := range v2.Containers[0].Env {
		if e.Name == "MODE" {
			v2.Containers[0].Env[i].Value = "turbo" // kubectl set env ... MODE=turbo
		}
	}
	t0 := lineageNow.Add(-time.Hour)
	spirit := famPod("second-life", "bbbbbbbbb", "second-life-bbbbbbbbb-z", "kind-control-plane", t0.Add(10*time.Minute), false, v2)
	pods := []corev1.Pod{
		famPod("second-life", "aaaaaaaaa", "second-life-aaaaaaaaa-x", "kind-control-plane", t0, true, v1),
		famPod("second-life", "aaaaaaaaa", "second-life-aaaaaaaaa-y", "kind-control-plane", t0, true, v1),
		spirit,
	}
	l := buildLineage(&spirit, pods, lineageNow)
	if l.Verdict() != autopsy.VerdictRegression || l.LivingFormerLife() == nil {
		t.Fatalf("verdict = %s, want regression with a living former life", l.Verdict())
	}
	want := []autopsy.SpecDiff{{Field: "container app env MODE", From: `"normal"`, To: `"turbo"`}}
	if !reflect.DeepEqual(l.Versions[0].Diffs, want) {
		t.Errorf("diffs = %+v, want %+v", l.Versions[0].Diffs, want)
	}
}
