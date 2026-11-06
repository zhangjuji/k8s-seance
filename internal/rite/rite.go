// Package rite casts resurrection spells (Requirement 19). It is the only
// package of K8s Séance that writes to the cluster, it is used only when the
// server runs with --allow-resurrect, and only internal/web may import it
// (internal/readonly_test.go guards both rules).
//
// Every write can be a rehearsal: with DryRun the API server validates the
// request, runs admission and returns the result, but stores nothing.
package rite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"github.com/zhangjuji/k8s-seance/internal/resurrect"
)

// FieldManager is the field manager of every write.
const FieldManager = "k8s-seance"

// Labels that mark a ConfigMap as made by K8s Séance. A ConfigMap without
// them is never overwritten.
const (
	ManagedByLabel = "app.kubernetes.io/managed-by"
	ManagedByValue = "k8s-seance"
)

// ErrNamespaceNotAllowed is returned for a namespace outside Caster.Namespaces.
var ErrNamespaceNotAllowed = errors.New("rite: namespace is not allowed for casting")

// ErrForeignConfigMap is returned when the spell's ConfigMap exists and was
// not made by K8s Séance.
var ErrForeignConfigMap = errors.New("rite: the ConfigMap exists and is not managed by k8s-seance; refusing to overwrite it")

// Caster executes bound spells.
type Caster struct {
	Client kubernetes.Interface
	// Namespaces are the only namespaces a spell may be cast in. Empty allows none.
	Namespaces []string
	// Now is the clock of Result.At. Nil means time.Now.
	Now func() time.Time
}

// Request is one API request of a cast, in the order it was made.
type Request struct {
	// Verb is get, list, create, update or patch.
	Verb     string `json:"verb"`
	Resource string `json:"resource"`
	Name     string `json:"name"`
	// Write is true for requests that change the cluster (or would, in a rehearsal).
	Write bool   `json:"write"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// Result is what a cast did. DryRun is true for a rehearsal.
type Result struct {
	DryRun   bool      `json:"dryRun"`
	OK       bool      `json:"ok"`
	At       time.Time `json:"at"`
	Requests []Request `json:"requests"`
}

// Cast runs b. With dryRun every write carries dryRun=All. It stops at the
// first failed request; the error is also recorded in the Result.
func (c *Caster) Cast(ctx context.Context, b resurrect.Bound, dryRun bool) (Result, error) {
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	res := Result{DryRun: dryRun, At: now().UTC(), Requests: []Request{}}
	if c.Client == nil {
		return res, errors.New("rite: no cluster client")
	}
	if !slices.Contains(c.Namespaces, b.Namespace) {
		return res, fmt.Errorf("%w: %q", ErrNamespaceNotAllowed, b.Namespace)
	}
	r := &run{c: c, ctx: ctx, b: b, dryRun: dryRun, res: &res}
	var err error
	switch b.Kind {
	case resurrect.ActionPatch, resurrect.ActionSetImage:
		err = r.patchTarget()
	case resurrect.ActionConfigMapMount:
		if err = r.writeConfigMap(); err == nil {
			err = r.patchTarget()
		}
	case resurrect.ActionRollback:
		err = r.rollback()
	default:
		err = fmt.Errorf("rite: unknown action %q", b.Kind)
	}
	res.OK = err == nil
	return res, err
}

// run carries one cast.
type run struct {
	c      *Caster
	ctx    context.Context
	b      resurrect.Bound
	dryRun bool
	res    *Result
}

// dry returns the dryRun option value.
func (r *run) dry() []string {
	if r.dryRun {
		return []string{metav1.DryRunAll}
	}
	return nil
}

// record adds one request to the result and returns err.
func (r *run) record(verb, resource, name string, write bool, err error) error {
	req := Request{Verb: verb, Resource: resource, Name: name, Write: write, OK: err == nil}
	if err != nil {
		req.Error = err.Error()
	}
	r.res.Requests = append(r.res.Requests, req)
	return err
}

func (r *run) patchOpts() metav1.PatchOptions {
	return metav1.PatchOptions{DryRun: r.dry(), FieldManager: FieldManager}
}

// patchTarget applies the strategic merge patch to the target controller.
func (r *run) patchTarget() error {
	if len(r.b.Patch) == 0 {
		return errors.New("rite: nothing to patch")
	}
	return r.patch(types.StrategicMergePatchType, r.b.Patch)
}

// patch sends one patch of type pt to the target.
func (r *run) patch(pt types.PatchType, data []byte) error {
	apps := r.c.Client.AppsV1()
	ns, name := r.b.Namespace, r.b.TargetName
	var err error
	switch r.b.TargetKind {
	case "Deployment":
		_, err = apps.Deployments(ns).Patch(r.ctx, name, pt, data, r.patchOpts())
	case "StatefulSet":
		_, err = apps.StatefulSets(ns).Patch(r.ctx, name, pt, data, r.patchOpts())
	case "DaemonSet":
		_, err = apps.DaemonSets(ns).Patch(r.ctx, name, pt, data, r.patchOpts())
	default:
		err = fmt.Errorf("rite: cannot cast on a %s", r.b.TargetKind)
	}
	return r.record("patch", resourceOf(r.b.TargetKind), name, true, err)
}

func resourceOf(kind string) string {
	switch kind {
	case "Deployment":
		return "deployments"
	case "StatefulSet":
		return "statefulsets"
	case "DaemonSet":
		return "daemonsets"
	}
	return kind
}

// writeConfigMap creates the ConfigMap, or updates the key in a ConfigMap
// that K8s Séance made earlier. Other ConfigMaps are left alone.
func (r *run) writeConfigMap() error {
	cmd := r.b.ConfigMap
	if cmd == nil {
		return errors.New("rite: no ConfigMap data")
	}
	cms := r.c.Client.CoreV1().ConfigMaps(r.b.Namespace)
	cur, err := cms.Get(r.ctx, cmd.Name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		r.record("get", "configmaps", cmd.Name, false, nil)
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name: cmd.Name, Namespace: r.b.Namespace,
				Labels: map[string]string{ManagedByLabel: ManagedByValue},
			},
			Data: map[string]string{cmd.Key: cmd.Data},
		}
		_, err = cms.Create(r.ctx, cm, metav1.CreateOptions{DryRun: r.dry(), FieldManager: FieldManager})
		return r.record("create", "configmaps", cmd.Name, true, err)
	case err != nil:
		return r.record("get", "configmaps", cmd.Name, false, err)
	}
	r.record("get", "configmaps", cmd.Name, false, nil)
	if cur.Labels[ManagedByLabel] != ManagedByValue {
		return r.record("update", "configmaps", cmd.Name, true, ErrForeignConfigMap)
	}
	next := cur.DeepCopy()
	if next.Data == nil {
		next.Data = map[string]string{}
	}
	next.Data[cmd.Key] = cmd.Data
	_, err = cms.Update(r.ctx, next, metav1.UpdateOptions{DryRun: r.dry(), FieldManager: FieldManager})
	return r.record("update", "configmaps", cmd.Name, true, err)
}

// rollback replaces the Deployment's Pod template with the template of its
// ReplicaSet at the living revision, like kubectl rollout undo --to-revision.
// A JSON Patch test on resourceVersion makes it fail rather than overwrite a
// Deployment that changed after it was read.
func (r *run) rollback() error {
	rb := r.b.Rollback
	if rb == nil || r.b.TargetKind != "Deployment" {
		return errors.New("rite: rollback needs a Deployment and a revision")
	}
	apps := r.c.Client.AppsV1()
	ns := r.b.Namespace
	dep, err := apps.Deployments(ns).Get(r.ctx, r.b.TargetName, metav1.GetOptions{})
	if err := r.record("get", "deployments", r.b.TargetName, false, err); err != nil {
		return err
	}
	sel := labels.SelectorFromSet(labels.Set{rb.HashLabel: rb.Revision}).String()
	list, err := apps.ReplicaSets(ns).List(r.ctx, metav1.ListOptions{LabelSelector: sel})
	var rs *appsv1.ReplicaSet
	if err == nil {
		if rs = ownedBy(list.Items, dep); rs == nil {
			err = fmt.Errorf("rite: no ReplicaSet of deployment %s has %s", dep.Name, sel)
		}
	}
	if err := r.record("list", "replicasets", sel, false, err); err != nil {
		return err
	}
	tmpl := rs.Spec.Template.DeepCopy()
	delete(tmpl.Labels, appsv1.DefaultDeploymentUniqueLabelKey)
	ops := []map[string]any{
		{"op": "test", "path": "/metadata/resourceVersion", "value": dep.ResourceVersion},
		{"op": "replace", "path": "/spec/template", "value": tmpl},
	}
	data, err := json.Marshal(ops)
	if err != nil {
		return err
	}
	return r.patch(types.JSONPatchType, data)
}

// ownedBy returns the ReplicaSet controlled by dep, or nil.
func ownedBy(items []appsv1.ReplicaSet, dep *appsv1.Deployment) *appsv1.ReplicaSet {
	for i := range items {
		ref := metav1.GetControllerOf(&items[i])
		if ref != nil && ref.Kind == "Deployment" && ref.Name == dep.Name && (dep.UID == "" || ref.UID == dep.UID) {
			return &items[i]
		}
	}
	return nil
}
