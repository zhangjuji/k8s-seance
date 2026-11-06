package web

import (
	"encoding/json"
	"html/template"
	"net/http"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	"github.com/zhangjuji/k8s-seance/internal/autopsy"
	"github.com/zhangjuji/k8s-seance/internal/llm"
	"github.com/zhangjuji/k8s-seance/internal/resurrect"
)

// secondLifePod is a Pod of the second-life Deployment at revision hash.
func secondLifePod(hash, name, node, mode string, created time.Time, alive bool) *corev1.Pod {
	yes := true
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "seance-demo", Name: name, UID: types.UID("uid-" + name),
			CreationTimestamp: metav1.NewTime(created),
			Labels:            map[string]string{"pod-template-hash": hash},
			OwnerReferences:   []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "second-life-" + hash, Controller: &yes}},
		},
		Spec: corev1.PodSpec{
			NodeName: node,
			Containers: []corev1.Container{{
				Name: "app", Image: "busybox:1.36",
				Env: []corev1.EnvVar{{Name: "MODE", Value: mode}, {Name: "API_TOKEN", Value: "tok-" + mode}},
			}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	st := corev1.ContainerStatus{Name: "app"}
	if alive {
		st.Ready = true
		st.State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
	} else {
		st.RestartCount = 3
		st.State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}
		st.LastTerminationState = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error"}}
	}
	p.Status.ContainerStatuses = []corev1.ContainerStatus{st}
	return p
}

const brokenPod = "second-life-bbbbbbbbb-new01"

func secondLifeCluster(extra ...runtime.Object) *ClusterSource {
	t0 := time.Now().Add(-2 * time.Hour)
	objs := []runtime.Object{
		secondLifePod("aaaaaaaaa", "second-life-aaaaaaaaa-old01", "kind-worker", "normal", t0, true),
		secondLifePod("aaaaaaaaa", "second-life-aaaaaaaaa-old02", "kind-worker2", "normal", t0, true),
		secondLifePod("bbbbbbbbb", brokenPod, "kind-worker", "turbo", t0.Add(time.Hour), false),
	}
	return clusterWith(append(objs, extra...)...)
}

func createClusterSeance(t *testing.T, srv *Server, pod, lang string) createResponse {
	t.Helper()
	body := `{"ns":"seance-demo","pod":"` + pod + `","lang":"` + lang + `"}`
	rec := send(t, srv, http.MethodPost, "/api/seances", "application/json", body, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: status = %d, body %s", rec.Code, rec.Body)
	}
	var res createResponse
	decodeJSON(t, rec.Body.Bytes(), &res)
	return res
}

func TestSeanceShowsFamilyAfterBadRelease(t *testing.T) {
	srv := newTestServer(t, Options{Cluster: secondLifeCluster(), Provider: llm.MockProvider{}})
	created := createClusterSeance(t, srv, brokenPod, "zh")

	d := detailOf(t, srv, created.ID)
	if d.Lineage == nil || d.Lineage.Verdict != autopsy.VerdictRegression || d.Lineage.OwnerName != "second-life" {
		t.Fatalf("detail lineage = %+v", d.Lineage)
	}
	var famID, verID string
	for _, e := range d.Evidence {
		if e.Kind == "lineage" && strings.HasPrefix(e.Text, "family ") {
			famID = e.ID
		}
		if e.Kind == "lineage" && strings.HasPrefix(e.Text, "revision ") {
			verID = e.ID
		}
		if strings.Contains(e.Text, "tok-") {
			t.Errorf("secret env value in evidence %s: %s", e.ID, e.Text)
		}
	}
	if famID == "" || verID == "" {
		t.Fatalf("no lineage evidence in %+v", d.Evidence)
	}

	for _, lang := range []string{"zh", "en"} {
		rec := send(t, srv, http.MethodGet, created.PageURL+"?lang="+lang, "", "", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s page status = %d", lang, rec.Code)
		}
		page := rec.Body.String()
		for _, want := range []string{
			`id="lineage-panel"`,
			`lineage-regression`,
			tr(lang, "lineage_title"),
			tr(lang, "lineage_v_regression"),
			`href="#ev-` + famID + `"`,
			`href="#ev-` + verID + `"`,
			tr(lang, "lineage_age_older"),
			"second-life-aaaaaaaaa-old01",
			"kind-worker2",
			"<code>container app env MODE</code>",
			"<code>&#34;normal&#34;</code>",
			"<code>&#34;turbo&#34;</code>",
			"<code>container app env API_TOKEN</code>",
			tr(lang, "lineage_hidden"),
			"Deployment/second-life",
		} {
			if !strings.Contains(page, want) {
				t.Errorf("%s page lacks %q", lang, want)
			}
		}
		if strings.Contains(page, "tok-") {
			t.Errorf("%s page shows a secret env value", lang)
		}
	}

	// The mock opening mentions the living former life.
	turns := turnsOf(t, streamOnce(t, srv, created.ID))
	if len(turns) == 0 || !strings.Contains(turns[0].Content, "我的前世还活着") || !strings.Contains(turns[0].Content, "["+verID+"]") {
		t.Errorf("opening = %+v, want the living former life cited as %s", turns, verID)
	}
	if len(turns[0].Doubts) != 0 {
		t.Errorf("opening doubts = %+v, want none: the lineage evidence backs it", turns[0].Doubts)
	}

	// The spell rolls back first.
	rec := send(t, srv, http.MethodGet, "/api/seances/"+created.ID+"/spell", "", "", map[string]string{"Accept": "application/json"})
	var sp spellReply
	if err := json.Unmarshal(rec.Body.Bytes(), &sp); err != nil {
		t.Fatal(err)
	}
	if sp.Remedy != resurrect.RemedyRollback {
		t.Errorf("spell remedy = %q, want %q", sp.Remedy, resurrect.RemedyRollback)
	}
	html := send(t, srv, http.MethodGet, "/api/seances/"+created.ID+"/spell", "", "", map[string]string{"HX-Request": "true"}).Body.String()
	if !strings.Contains(html, "kubectl rollout undo deployment/second-life -n seance-demo --dry-run=server") {
		t.Errorf("spell panel lacks the dry-run rollback:\n%s", html)
	}
}

func TestSeanceFamilyInstanceShowsBothNodes(t *testing.T) {
	t0 := time.Now().Add(-time.Hour)
	sib := secondLifePod("bbbbbbbbb", "second-life-bbbbbbbbb-sib02", "kind-worker3", "turbo", t0, true)
	srv := newTestServer(t, Options{Cluster: secondLifeCluster(sib), Provider: llm.MockProvider{}})
	created := createClusterSeance(t, srv, brokenPod, "en")
	page := send(t, srv, http.MethodGet, created.PageURL+"?lang=en", "", "", nil).Body.String()
	want := trf("en", "lineage_v_instance", "kind-worker", "kind-worker3")
	if !strings.Contains(page, template.HTMLEscapeString(want)) {
		t.Errorf("page lacks the instance verdict %q", want)
	}
	turns := turnsOf(t, streamOnce(t, srv, created.ID))
	if len(turns) > 0 && strings.Contains(turns[0].Content, "former life") {
		t.Errorf("instance opening mentions a former life: %s", turns[0].Content)
	}
	var sp spellReply
	rec := send(t, srv, http.MethodGet, "/api/seances/"+created.ID+"/spell", "", "", map[string]string{"Accept": "application/json"})
	_ = json.Unmarshal(rec.Body.Bytes(), &sp)
	if sp.Remedy == resurrect.RemedyRollback {
		t.Error("instance problem: spell rolls back")
	}
}

func TestFixtureSeanceHasNoFamilyRecord(t *testing.T) {
	srv := newSeanceServer(t, llm.Limits{})
	created := createSeance(t, srv, crashNS, crashPod)
	page := send(t, srv, http.MethodGet, created.PageURL+"?lang=en", "", "", nil).Body.String()
	if !strings.Contains(page, `id="lineage-panel"`) || !strings.Contains(page, tr("en", "lineage_none")) {
		t.Errorf("fixture page lacks the 'no family record' panel")
	}
	if d := detailOf(t, srv, created.ID); d.Lineage != nil {
		t.Errorf("fixture lineage = %+v, want none", d.Lineage)
	}
}

func TestBarePodHasNoFamilyPanel(t *testing.T) {
	p := buildLineage("en", &autopsy.PodAutopsy{Name: "bare"}, nil)
	if p.Show {
		t.Errorf("bare Pod panel = %+v, want hidden", p)
	}
}
