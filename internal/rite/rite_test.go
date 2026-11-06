package rite

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/zhangjuji/k8s-seance/internal/resurrect"
)

const ns = "seance-demo"

func deployment(name string, tmpl corev1.PodTemplateSpec) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, UID: types.UID("uid-" + name)},
		Spec:       appsv1.DeploymentSpec{Template: tmpl},
	}
}

func template(image, mem string, extra map[string]string) corev1.PodTemplateSpec {
	l := map[string]string{"app": "x"}
	for k, v := range extra {
		l[k] = v
	}
	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: l},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "app", Image: image,
			Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(mem)}},
		}}},
	}
}

// dryRunHonoring makes the fake clientset behave like the API server for
// dryRun=All writes: they succeed and store nothing.
func dryRunHonoring(cs *fake.Clientset) {
	cs.PrependReactor("*", "*", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if isDry(a) {
			return true, nil, nil
		}
		return false, nil, nil
	})
}

func dryRunOf(a k8stesting.Action) []string {
	switch x := a.(type) {
	case k8stesting.PatchActionImpl:
		return x.PatchOptions.DryRun
	case k8stesting.CreateActionImpl:
		return x.CreateOptions.DryRun
	case k8stesting.UpdateActionImpl:
		return x.UpdateOptions.DryRun
	}
	return nil
}

func fieldManagerOf(a k8stesting.Action) string {
	switch x := a.(type) {
	case k8stesting.PatchActionImpl:
		return x.PatchOptions.FieldManager
	case k8stesting.CreateActionImpl:
		return x.CreateOptions.FieldManager
	case k8stesting.UpdateActionImpl:
		return x.UpdateOptions.FieldManager
	}
	return ""
}

func isDry(a k8stesting.Action) bool { return slices.Equal(dryRunOf(a), []string{metav1.DryRunAll}) }

func isWrite(a k8stesting.Action) bool {
	switch a.GetVerb() {
	case "create", "update", "patch", "delete", "deletecollection":
		return true
	}
	return false
}

// writes returns the write actions in order.
func writes(cs *fake.Clientset) []k8stesting.Action {
	var out []k8stesting.Action
	for _, a := range cs.Actions() {
		if isWrite(a) {
			out = append(out, a)
		}
	}
	return out
}

func caster(cs *fake.Clientset) *Caster {
	return &Caster{Client: cs, Namespaces: []string{ns}}
}

func memOf(t *testing.T, cs *fake.Clientset, name string) string {
	t.Helper()
	d, err := cs.AppsV1().Deployments(ns).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return d.Spec.Template.Spec.Containers[0].Resources.Limits.Memory().String()
}

func oomBound() resurrect.Bound {
	return resurrect.Bound{
		Kind: resurrect.ActionPatch, Namespace: ns, TargetKind: "Deployment", TargetName: "memory-glutton",
		Patch: []byte(`{"spec":{"template":{"spec":{"containers":[{"name":"app","resources":{"limits":{"memory":"128Mi"}}}]}}}}`),
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	cs := fake.NewClientset(deployment("memory-glutton", template("python:3.12-alpine", "64Mi", nil)))
	dryRunHonoring(cs)
	res, err := caster(cs).Cast(context.Background(), oomBound(), true)
	if err != nil || !res.OK || !res.DryRun {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
	w := writes(cs)
	if len(w) != 1 || !isDry(w[0]) || fieldManagerOf(w[0]) != FieldManager {
		t.Fatalf("writes = %+v, want one dryRun=All patch by %s", w, FieldManager)
	}
	if got := memOf(t, cs, "memory-glutton"); got != "64Mi" {
		t.Errorf("memory after rehearsal = %s, want 64Mi unchanged", got)
	}
}

func TestCastPatchesTheTarget(t *testing.T) {
	cs := fake.NewClientset(deployment("memory-glutton", template("python:3.12-alpine", "64Mi", nil)))
	res, err := caster(cs).Cast(context.Background(), oomBound(), false)
	if err != nil || !res.OK || res.DryRun {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
	w := writes(cs)
	if len(w) != 1 || dryRunOf(w[0]) != nil || w[0].GetResource().Resource != "deployments" {
		t.Fatalf("writes = %+v", w)
	}
	if pt := w[0].(k8stesting.PatchActionImpl).GetPatchType(); pt != types.StrategicMergePatchType {
		t.Errorf("patch type = %s", pt)
	}
	if got := memOf(t, cs, "memory-glutton"); got != "128Mi" {
		t.Errorf("memory = %s, want 128Mi", got)
	}
	if len(res.Requests) != 1 || !res.Requests[0].Write || res.Requests[0].Verb != "patch" {
		t.Errorf("requests = %+v", res.Requests)
	}
}

func TestStatefulSetAndDaemonSetTargets(t *testing.T) {
	cs := fake.NewClientset(
		&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: ns}, Spec: appsv1.StatefulSetSpec{Template: template("db:1", "64Mi", nil)}},
		&appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: ns}, Spec: appsv1.DaemonSetSpec{Template: template("agent:1", "64Mi", nil)}},
	)
	for kind, name := range map[string]string{"StatefulSet": "db", "DaemonSet": "agent"} {
		b := oomBound()
		b.TargetKind, b.TargetName = kind, name
		if _, err := caster(cs).Cast(context.Background(), b, false); err != nil {
			t.Errorf("%s: %v", kind, err)
		}
	}
	ss, _ := cs.AppsV1().StatefulSets(ns).Get(context.Background(), "db", metav1.GetOptions{})
	if got := ss.Spec.Template.Spec.Containers[0].Resources.Limits.Memory().String(); got != "128Mi" {
		t.Errorf("statefulset memory = %s", got)
	}
}

func TestNamespaceAllowList(t *testing.T) {
	cs := fake.NewClientset()
	b := oomBound()
	b.Namespace = "kube-system"
	_, err := caster(cs).Cast(context.Background(), b, true)
	if !errors.Is(err, ErrNamespaceNotAllowed) {
		t.Fatalf("err = %v, want ErrNamespaceNotAllowed", err)
	}
	if n := len(cs.Actions()); n != 0 {
		t.Errorf("%d API requests for a forbidden namespace, want 0", n)
	}
	empty := &Caster{Client: cs}
	if _, err := empty.Cast(context.Background(), oomBound(), true); !errors.Is(err, ErrNamespaceNotAllowed) {
		t.Errorf("empty allow list: err = %v", err)
	}
}

func TestPodKindIsRefused(t *testing.T) {
	cs := fake.NewClientset()
	b := oomBound()
	b.TargetKind = "Pod"
	if _, err := caster(cs).Cast(context.Background(), b, false); err == nil {
		t.Fatal("cast on a Pod succeeded")
	}
	if len(writes(cs)) != 0 {
		t.Errorf("writes = %+v", writes(cs))
	}
}

func cmBound() resurrect.Bound {
	return resurrect.Bound{
		Kind: resurrect.ActionConfigMapMount, Namespace: ns, TargetKind: "Deployment", TargetName: "restless-spirit",
		Patch:     []byte(`{"spec":{"template":{"spec":{"volumes":[{"name":"restless-spirit-config","configMap":{"name":"restless-spirit-config"}}]}}}}`),
		ConfigMap: &resurrect.ConfigMapData{Name: "restless-spirit-config", Key: "config.yaml", Data: "mode: calm\n"},
	}
}

func TestConfigMapMountCreatesThenPatches(t *testing.T) {
	cs := fake.NewClientset(deployment("restless-spirit", template("busybox:1.36", "16Mi", nil)))
	res, err := caster(cs).Cast(context.Background(), cmBound(), false)
	if err != nil {
		t.Fatal(err)
	}
	w := writes(cs)
	if len(w) != 2 || w[0].GetVerb() != "create" || w[0].GetResource().Resource != "configmaps" || w[1].GetVerb() != "patch" {
		t.Fatalf("writes = %+v, want create configmap then patch", w)
	}
	cm, err := cs.CoreV1().ConfigMaps(ns).Get(context.Background(), "restless-spirit-config", metav1.GetOptions{})
	if err != nil || cm.Data["config.yaml"] != "mode: calm\n" || cm.Labels[ManagedByLabel] != ManagedByValue {
		t.Fatalf("configmap = %+v, err = %v", cm, err)
	}
	d, _ := cs.AppsV1().Deployments(ns).Get(context.Background(), "restless-spirit", metav1.GetOptions{})
	if len(d.Spec.Template.Spec.Volumes) != 1 {
		t.Errorf("volumes = %+v", d.Spec.Template.Spec.Volumes)
	}
	if len(res.Requests) != 3 || res.Requests[0].Verb != "get" || res.Requests[0].Write {
		t.Errorf("requests = %+v", res.Requests)
	}

	// A second cast updates the managed ConfigMap.
	b := cmBound()
	b.ConfigMap.Data = "mode: calmer\n"
	if _, err := caster(cs).Cast(context.Background(), b, false); err != nil {
		t.Fatal(err)
	}
	cm, _ = cs.CoreV1().ConfigMaps(ns).Get(context.Background(), "restless-spirit-config", metav1.GetOptions{})
	if cm.Data["config.yaml"] != "mode: calmer\n" {
		t.Errorf("data after update = %q", cm.Data["config.yaml"])
	}
}

func TestConfigMapMountDryRun(t *testing.T) {
	cs := fake.NewClientset(deployment("restless-spirit", template("busybox:1.36", "16Mi", nil)))
	dryRunHonoring(cs)
	if _, err := caster(cs).Cast(context.Background(), cmBound(), true); err != nil {
		t.Fatal(err)
	}
	for _, a := range writes(cs) {
		if !isDry(a) {
			t.Errorf("write %s %s without dryRun=All", a.GetVerb(), a.GetResource().Resource)
		}
	}
	if _, err := cs.CoreV1().ConfigMaps(ns).Get(context.Background(), "restless-spirit-config", metav1.GetOptions{}); err == nil {
		t.Error("rehearsal stored the ConfigMap")
	}
}

func TestForeignConfigMapIsNotOverwritten(t *testing.T) {
	foreign := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "restless-spirit-config", Namespace: ns}, Data: map[string]string{"config.yaml": "theirs"}}
	cs := fake.NewClientset(foreign, deployment("restless-spirit", template("busybox:1.36", "16Mi", nil)))
	res, err := caster(cs).Cast(context.Background(), cmBound(), false)
	if !errors.Is(err, ErrForeignConfigMap) || res.OK {
		t.Fatalf("err = %v, res = %+v", err, res)
	}
	if w := writes(cs); len(w) != 0 {
		t.Errorf("writes = %+v, want none", w)
	}
	last := res.Requests[len(res.Requests)-1]
	if last.OK || !strings.Contains(last.Error, "not managed by k8s-seance") {
		t.Errorf("last request = %+v", last)
	}
}

func replicaSet(name, hash string, owner *appsv1.Deployment, tmpl corev1.PodTemplateSpec) *appsv1.ReplicaSet {
	isController := true
	return &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: ns, Labels: map[string]string{"pod-template-hash": hash},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: owner.Name, UID: owner.UID, Controller: &isController}},
		},
		Spec: appsv1.ReplicaSetSpec{Template: tmpl},
	}
}

func TestRollbackRestoresTheLivingTemplate(t *testing.T) {
	dep := deployment("second-life", template("busybox:1.36", "16Mi", map[string]string{"mode": "turbo"}))
	dep.ResourceVersion = "7"
	good := replicaSet("second-life-aaa", "aaa", dep, template("busybox:1.36", "16Mi", map[string]string{"mode": "normal", "pod-template-hash": "aaa"}))
	other := deployment("someone-else", template("x", "1Mi", nil))
	stranger := replicaSet("someone-else-bbb", "bbb", other, template("x", "1Mi", nil))
	cs := fake.NewClientset(dep, good, stranger)
	b := resurrect.Bound{Kind: resurrect.ActionRollback, Namespace: ns, TargetKind: "Deployment", TargetName: "second-life",
		Rollback: &resurrect.RollbackRef{HashLabel: "pod-template-hash", Revision: "aaa"}}

	res, err := caster(cs).Cast(context.Background(), b, false)
	if err != nil {
		t.Fatalf("err = %v, res = %+v", err, res)
	}
	w := writes(cs)
	if len(w) != 1 {
		t.Fatalf("writes = %+v", w)
	}
	pa := w[0].(k8stesting.PatchActionImpl)
	if pa.GetPatchType() != types.JSONPatchType {
		t.Errorf("patch type = %s", pa.GetPatchType())
	}
	var ops []map[string]any
	if err := json.Unmarshal(pa.GetPatch(), &ops); err != nil || len(ops) != 2 || ops[0]["op"] != "test" || ops[0]["value"] != "7" || ops[1]["path"] != "/spec/template" {
		t.Fatalf("ops = %s (%v)", pa.GetPatch(), err)
	}
	got, _ := cs.AppsV1().Deployments(ns).Get(context.Background(), "second-life", metav1.GetOptions{})
	l := got.Spec.Template.Labels
	if l["mode"] != "normal" {
		t.Errorf("template labels = %v, want the living revision's", l)
	}
	if _, ok := l["pod-template-hash"]; ok {
		t.Errorf("template keeps pod-template-hash: %v", l)
	}

	// A revision whose ReplicaSet belongs to another Deployment is refused.
	b.Rollback.Revision = "bbb"
	res, err = caster(cs).Cast(context.Background(), b, true)
	if err == nil || res.OK || len(writes(cs)) != 1 {
		t.Errorf("foreign ReplicaSet: err = %v, res = %+v", err, res)
	}
}

func TestFailedPatchIsReported(t *testing.T) {
	cs := fake.NewClientset() // the Deployment does not exist
	res, err := caster(cs).Cast(context.Background(), oomBound(), true)
	if err == nil || res.OK {
		t.Fatalf("err = %v, res = %+v", err, res)
	}
	if len(res.Requests) != 1 || res.Requests[0].OK || res.Requests[0].Error == "" {
		t.Errorf("requests = %+v", res.Requests)
	}
}
