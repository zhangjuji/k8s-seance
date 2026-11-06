package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/zhangjuji/k8s-seance/internal/autopsy"
	"github.com/zhangjuji/k8s-seance/internal/graveyard"
	"github.com/zhangjuji/k8s-seance/internal/llm"
)

// All casting tests use a fake clientset: no test ever reaches a real cluster.

const castNS = "seance-demo"

// gluttonDeployment is the Deployment of the OOM fixture, with a 64Mi limit.
func gluttonDeployment() *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "memory-glutton", Namespace: castNS},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "stress", Image: "python:3.12-alpine",
			Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("64Mi")}},
		}}}}},
	}
}

// honorDryRun makes the fake behave like the API server for dryRun=All writes.
func honorDryRun(cs *fake.Clientset) {
	cs.PrependReactor("*", "*", func(a k8stesting.Action) (bool, runtime.Object, error) {
		var dry []string
		switch x := a.(type) {
		case k8stesting.PatchActionImpl:
			dry = x.PatchOptions.DryRun
		case k8stesting.CreateActionImpl:
			dry = x.CreateOptions.DryRun
		case k8stesting.UpdateActionImpl:
			dry = x.UpdateOptions.DryRun
		}
		return slices.Equal(dry, []string{metav1.DryRunAll}), nil, nil
	})
}

func writeActions(cs *fake.Clientset) []k8stesting.Action {
	var out []k8stesting.Action
	for _, a := range cs.Actions() {
		switch a.GetVerb() {
		case "create", "update", "patch", "delete", "deletecollection":
			out = append(out, a)
		}
	}
	return out
}

type castRig struct {
	srv   *Server
	cs    *fake.Clientset
	grave *graveyard.Store
	a     *autopsy.PodAutopsy
}

// newCastRig serves the fixture pod named prefix* as a cluster spirit, with
// casting on in namespaces (nil means seance-demo).
func newCastRig(t *testing.T, prefix string, namespaces []string, objs ...runtime.Object) castRig {
	t.Helper()
	all, err := NewFixtureSource().load()
	if err != nil {
		t.Fatal(err)
	}
	var a *autopsy.PodAutopsy
	for _, x := range all {
		if strings.HasPrefix(x.Name, prefix) {
			a = x
		}
	}
	if a == nil {
		t.Fatalf("no fixture %s", prefix)
	}
	if namespaces == nil {
		namespaces = []string{castNS}
	}
	cs := fake.NewClientset(objs...)
	honorDryRun(cs)
	grave, err := graveyard.Open(filepath.Join(t.TempDir(), "graveyard.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv := newTestServer(t, Options{
		Cluster:   fixedSource{a: a},
		Provider:  llm.MockProvider{},
		Graveyard: grave,
		Resurrect: &Resurrect{Client: cs, Namespaces: namespaces},
	})
	return castRig{srv: srv, cs: cs, grave: grave, a: a}
}

func (r castRig) summon(t *testing.T, src string) string {
	t.Helper()
	body := fmt.Sprintf(`{"ns":%q,"pod":%q,"src":%q}`, r.a.Namespace, r.a.Name, src)
	rec := send(t, r.srv, http.MethodPost, "/api/seances", "application/json", body, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var res createResponse
	decodeJSON(t, rec.Body.Bytes(), &res)
	return res.ID
}

func castJSON(t *testing.T, srv *Server, id string, body map[string]any, headers map[string]string) (int, castReply, string) {
	t.Helper()
	data, _ := json.Marshal(body)
	rec := send(t, srv, http.MethodPost, "/api/seances/"+id+"/cast", "application/json", string(data), headers)
	var rep castReply
	_ = json.Unmarshal(rec.Body.Bytes(), &rep)
	return rec.Code, rep, rec.Body.String()
}

func gluttonMemory(t *testing.T, cs *fake.Clientset) string {
	t.Helper()
	d, err := cs.AppsV1().Deployments(castNS).Get(context.Background(), "memory-glutton", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return d.Spec.Template.Spec.Containers[0].Resources.Limits.Memory().String()
}

func TestCastOffByDefault(t *testing.T) {
	srv := newSpellServerOver(t, nil)
	id := createSeance(t, srv, crashNS, oomPod).ID
	code, _, body := castJSON(t, srv, id, map[string]any{"mode": "dry"}, nil)
	if code != http.StatusForbidden || !strings.Contains(body, "--allow-resurrect") {
		t.Errorf("cast without --allow-resurrect: %d %s", code, body)
	}
	if rec := send(t, srv, http.MethodGet, "/api/seances/"+id+"/vitals?since=1", "", "", nil); rec.Code != http.StatusForbidden {
		t.Errorf("vitals without --allow-resurrect: %d", rec.Code)
	}
	html := send(t, srv, http.MethodGet, "/api/seances/"+id+"/spell?lang=en", "", "", map[string]string{"HX-Request": "true"}).Body.String()
	if !strings.Contains(html, `data-cast="off"`) || strings.Contains(html, `data-cast="on"`) || !strings.Contains(html, "Casting is off") {
		t.Errorf("spell panel without --allow-resurrect shows a form:\n%s", html)
	}
}

func newSpellServerOver(t *testing.T, r *Resurrect) *Server {
	return newTestServer(t, Options{Fixtures: NewFixtureSource(), Provider: llm.MockProvider{}, Resurrect: r})
}

func TestNewRejectsBadResurrectOptions(t *testing.T) {
	for _, r := range []*Resurrect{
		{Client: nil, Namespaces: []string{castNS}},
		{Client: fake.NewClientset()},
		{Client: fake.NewClientset(), Namespaces: []string{"Bad_NS"}},
	} {
		if _, err := New(Options{Resurrect: r}); err == nil {
			t.Errorf("New accepted %+v", r)
		}
	}
}

func TestCastRefusedInSampleMode(t *testing.T) {
	rig := newCastRig(t, "memory-glutton", nil, gluttonDeployment())
	id := rig.summon(t, "fixtures")
	code, _, body := castJSON(t, rig.srv, id, map[string]any{"mode": "dry"}, map[string]string{"Accept-Language": "en"})
	if code != http.StatusForbidden || !strings.Contains(body, "Sample mode") {
		t.Errorf("sample mode cast: %d %s", code, body)
	}
	html := send(t, rig.srv, http.MethodGet, "/api/seances/"+id+"/spell?lang=en", "", "", map[string]string{"HX-Request": "true"}).Body.String()
	if !strings.Contains(html, "Sample mode cannot cast") || strings.Contains(html, `data-cast="on"`) {
		t.Errorf("sample mode panel:\n%s", html)
	}
	if n := len(rig.cs.Actions()); n != 0 {
		t.Errorf("%d cluster requests in sample mode", n)
	}
}

func TestCastRehearseThenCast(t *testing.T) {
	rig := newCastRig(t, "memory-glutton", nil, gluttonDeployment())
	id := rig.summon(t, "")

	html := send(t, rig.srv, http.MethodGet, "/api/seances/"+id+"/spell?lang=en", "", "", map[string]string{"HX-Request": "true"}).Body.String()
	if !strings.Contains(html, `data-cast="on"`) || !strings.Contains(html, "Deployment/memory-glutton") || !strings.Contains(html, `"mode":"dry"`) {
		t.Fatalf("panel has no cast form:\n%s", html)
	}

	// A real cast before any rehearsal is refused.
	code, _, body := castJSON(t, rig.srv, id, map[string]any{"mode": "cast", "confirm": true}, nil)
	if code != http.StatusConflict {
		t.Fatalf("cast before rehearsal: %d %s", code, body)
	}
	if n := len(rig.cs.Actions()); n != 0 {
		t.Fatalf("%d requests before rehearsal", n)
	}

	code, rep, body := castJSON(t, rig.srv, id, map[string]any{"mode": "dry"}, nil)
	if code != http.StatusOK || !rep.OK || !rep.Result.DryRun || !rep.CanCast {
		t.Fatalf("rehearsal: %d %s", code, body)
	}
	if got := gluttonMemory(t, rig.cs); got != "64Mi" {
		t.Fatalf("memory after rehearsal = %s, want 64Mi", got)
	}

	code, _, body = castJSON(t, rig.srv, id, map[string]any{"mode": "cast"}, nil)
	if code != http.StatusBadRequest || !strings.Contains(body, "confirm") && !strings.Contains(body, "确认") {
		t.Fatalf("cast without confirm: %d %s", code, body)
	}

	code, rep, body = castJSON(t, rig.srv, id, map[string]any{"mode": "cast", "confirm": true}, nil)
	if code != http.StatusOK || !rep.OK || rep.Result.DryRun || rep.Vitals == "" || rep.Since == 0 {
		t.Fatalf("cast: %d %s", code, body)
	}
	if got := gluttonMemory(t, rig.cs); got != "128Mi" {
		t.Errorf("memory after cast = %s, want 128Mi", got)
	}
	w := writeActions(rig.cs)
	if len(w) != 2 {
		t.Fatalf("writes = %d, want the rehearsal and the cast", len(w))
	}
	if dry := w[0].(k8stesting.PatchActionImpl).PatchOptions.DryRun; !slices.Equal(dry, []string{metav1.DryRunAll}) {
		t.Errorf("first write dryRun = %v", dry)
	}
	if dry := w[1].(k8stesting.PatchActionImpl).PatchOptions.DryRun; dry != nil {
		t.Errorf("cast dryRun = %v", dry)
	}

	// The rehearsal is used up: casting again needs a new one.
	if code, _, _ := castJSON(t, rig.srv, id, map[string]any{"mode": "cast", "confirm": true}, nil); code != http.StatusConflict {
		t.Errorf("second cast without a new rehearsal: %d", code)
	}
}

func TestCastFormPostWithHTMX(t *testing.T) {
	rig := newCastRig(t, "memory-glutton", nil, gluttonDeployment())
	id := rig.summon(t, "")
	hx := map[string]string{"HX-Request": "true", "Accept-Language": "en"}
	rec := send(t, rig.srv, http.MethodPost, "/api/seances/"+id+"/cast", "application/x-www-form-urlencoded", "mode=dry", hx)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Rehearsal passed") || !strings.Contains(rec.Body.String(), "dryRun=All") {
		t.Fatalf("htmx rehearsal: %d %s", rec.Code, rec.Body)
	}
	rec = send(t, rig.srv, http.MethodPost, "/api/seances/"+id+"/cast", "application/x-www-form-urlencoded", "mode=cast&confirm=yes", hx)
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, "Cast done") || !strings.Contains(body, `hx-trigger="load delay:3s"`) || !strings.Contains(body, "/vitals?since=") {
		t.Fatalf("htmx cast: %d %s", rec.Code, body)
	}
	// An htmx refusal is an error fragment with the 4xx status.
	rec = send(t, rig.srv, http.MethodPost, "/api/seances/"+id+"/cast", "application/x-www-form-urlencoded", "mode=cast&confirm=yes", hx)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `class="cast-error"`) {
		t.Errorf("htmx refusal: %d %s", rec.Code, rec.Body)
	}
}

func TestCastRefusesCrossSite(t *testing.T) {
	rig := newCastRig(t, "memory-glutton", nil, gluttonDeployment())
	id := rig.summon(t, "")
	for _, h := range []map[string]string{
		{"Origin": "http://evil.example"},
		{"Sec-Fetch-Site": "cross-site"},
		{"Sec-Fetch-Site": "same-site"},
	} {
		if code, _, body := castJSON(t, rig.srv, id, map[string]any{"mode": "dry"}, h); code != http.StatusForbidden {
			t.Errorf("%v: %d %s", h, code, body)
		}
	}
	if code, _, _ := castJSON(t, rig.srv, id, map[string]any{"mode": "dry"}, map[string]string{"Origin": "http://example.com", "Sec-Fetch-Site": "same-origin"}); code != http.StatusOK {
		t.Errorf("same-origin rehearsal: %d", code)
	}
	if n := len(writeActions(rig.cs)); n != 1 {
		t.Errorf("writes = %d, want only the same-origin rehearsal", n)
	}
}

func TestCastRefusedOutsideAllowedNamespaces(t *testing.T) {
	rig := newCastRig(t, "memory-glutton", []string{"other-demo"}, gluttonDeployment())
	id := rig.summon(t, "")
	code, _, body := castJSON(t, rig.srv, id, map[string]any{"mode": "dry"}, map[string]string{"Accept-Language": "en"})
	if code != http.StatusForbidden || !strings.Contains(body, "other-demo") {
		t.Errorf("cast in a namespace not allowed: %d %s", code, body)
	}
	if n := len(rig.cs.Actions()); n != 0 {
		t.Errorf("%d cluster requests", n)
	}
}

func TestReadOnlySpellCannotBeCast(t *testing.T) {
	rig := newCastRig(t, "memory-glutton", nil)
	rig.a.OwnerKind, rig.a.OwnerName = "", "" // a bare Pod: the spell says to recreate it by hand
	id := rig.summon(t, "")
	code, _, body := castJSON(t, rig.srv, id, map[string]any{"mode": "dry"}, map[string]string{"Accept-Language": "en"})
	if code != http.StatusForbidden || !strings.Contains(body, "can only be read") {
		t.Errorf("read-only spell: %d %s", code, body)
	}
}

func TestCastInputErrors(t *testing.T) {
	rig := newCastRig(t, "lost-soul", nil)
	id := rig.summon(t, "")
	for _, tc := range []struct{ tag, want string }{{"", "Please fill in"}, {"1.0; rm -rf /", "Invalid format"}} {
		code, _, body := castJSON(t, rig.srv, id, map[string]any{"mode": "dry", "inputs": map[string]string{"tag": tc.tag}}, map[string]string{"Accept-Language": "en"})
		if code != http.StatusBadRequest || !strings.Contains(body, tc.want) {
			t.Errorf("tag %q: %d %s", tc.tag, code, body)
		}
	}
	if code, _, _ := castJSON(t, rig.srv, id, map[string]any{"mode": "delete"}, nil); code != http.StatusBadRequest {
		t.Errorf("unknown mode: %d", code)
	}
	if n := len(rig.cs.Actions()); n != 0 {
		t.Errorf("%d cluster requests for bad input", n)
	}
}

// livingPod is a Pod of memory-glutton that is running and ready.
func livingPod(name string, created, readyAt time.Time) *corev1.Pod {
	yes := true
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: castNS, CreationTimestamp: metav1.NewTime(created),
			Labels:          map[string]string{"pod-template-hash": "5f5b7d9c8d"},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "memory-glutton-5f5b7d9c8d", Controller: &yes}},
		},
		Status: corev1.PodStatus{
			Phase:             corev1.PodRunning,
			Conditions:        []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(readyAt)}},
			ContainerStatuses: []corev1.ContainerStatus{{Name: "stress", Ready: true, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}},
		},
	}
}

func TestVitalsAfterCastMarksTheAltar(t *testing.T) {
	rig := newCastRig(t, "memory-glutton", nil, gluttonDeployment())
	id := rig.summon(t, "")
	tomb, err := rig.grave.Bury(graveyard.Tombstone{Namespace: rig.a.Namespace, PodName: rig.a.Name, UID: rig.a.UID, Cause: autopsy.CauseOOMKilled})
	if err != nil {
		t.Fatal(err)
	}
	castJSON(t, rig.srv, id, map[string]any{"mode": "dry"}, nil)
	_, rep, body := castJSON(t, rig.srv, id, map[string]any{"mode": "cast", "confirm": true}, nil)
	if rep.Vitals == "" {
		t.Fatalf("no vitals URL: %s", body)
	}

	// Nothing has come back yet: the page keeps polling.
	rec := send(t, rig.srv, http.MethodGet, rep.Vitals, "", "", map[string]string{"HX-Request": "true", "Accept-Language": "en"})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `data-state="waiting"`) || !strings.Contains(rec.Body.String(), `hx-trigger="load delay:3s"`) {
		t.Fatalf("waiting vitals: %d %s", rec.Code, rec.Body)
	}

	now := time.Now()
	if _, err := rig.cs.CoreV1().Pods(castNS).Create(context.Background(), livingPod("memory-glutton-5f5b7d9c8d-new01", now.Add(2*time.Second), now.Add(-30*time.Second)), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	rec = send(t, rig.srv, http.MethodGet, rep.Vitals, "", "", map[string]string{"Accept": "application/json"})
	var v vitalsView
	decodeJSON(t, rec.Body.Bytes(), &v)
	if v.State != "revived" || v.AltarURL != tombstoneURL(tomb.ID) {
		t.Fatalf("vitals = %+v (%s)", v, rec.Body)
	}
	stored, _ := rig.grave.Get(tomb.ID)
	if stored.RevivedAt.IsZero() || stored.RevivedBy != "patch" {
		t.Errorf("tombstone = %+v, want revived by patch", stored)
	}
	page := send(t, rig.srv, http.MethodGet, "/graveyard?lang=en", "", "", nil).Body.String()
	if !strings.Contains(page, "Revived") {
		t.Errorf("altar does not show the revival")
	}
	api := send(t, rig.srv, http.MethodGet, "/api/graveyard", "", "", nil).Body.String()
	if !strings.Contains(api, `"revivedAt"`) {
		t.Errorf("graveyard API has no revivedAt: %s", api)
	}
}

func TestVitalsWithoutACastDoesNotMarkTheAltar(t *testing.T) {
	rig := newCastRig(t, "memory-glutton", nil, livingPod("memory-glutton-5f5b7d9c8d-old01", time.Now().Add(-time.Hour), time.Now().Add(-time.Hour)))
	id := rig.summon(t, "")
	tomb, _ := rig.grave.Bury(graveyard.Tombstone{Namespace: rig.a.Namespace, PodName: rig.a.Name, UID: rig.a.UID})
	since := time.Now().Add(-time.Minute).Unix()
	rec := send(t, rig.srv, http.MethodGet, fmt.Sprintf("/api/seances/%s/vitals?since=%d", id, since), "", "", map[string]string{"Accept": "application/json"})
	var v vitalsView
	decodeJSON(t, rec.Body.Bytes(), &v)
	if v.State != "revived" || v.AltarURL != "" {
		t.Errorf("vitals = %+v", v)
	}
	if stored, _ := rig.grave.Get(tomb.ID); !stored.RevivedAt.IsZero() {
		t.Error("a vitals check without a cast marked the altar")
	}
	for _, q := range []string{"", "since=abc", "since=-5", "since=" + url.QueryEscape(fmt.Sprint(time.Now().Add(time.Hour).Unix()))} {
		if rec := send(t, rig.srv, http.MethodGet, "/api/seances/"+id+"/vitals?"+q, "", "", nil); rec.Code != http.StatusBadRequest {
			t.Errorf("vitals?%s: %d", q, rec.Code)
		}
	}
	for _, a := range rig.cs.Actions() {
		if a.GetVerb() != "list" {
			t.Errorf("vitals made a %s request", a.GetVerb())
		}
	}
}

func TestVitalsStopsPollingAfterTheWindow(t *testing.T) {
	rig := newCastRig(t, "memory-glutton", nil)
	id := rig.summon(t, "")
	since := time.Now().Add(-5 * time.Minute).Unix()
	rec := send(t, rig.srv, http.MethodGet, fmt.Sprintf("/api/seances/%s/vitals?since=%d", id, since), "", "", map[string]string{"HX-Request": "true", "Accept-Language": "en"})
	body := rec.Body.String()
	if rec.Code != http.StatusOK || strings.Contains(body, "hx-get") || !strings.Contains(body, "within 4 minutes") {
		t.Errorf("vitals after the window: %d %s", rec.Code, body)
	}
}
