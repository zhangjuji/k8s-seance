package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zhangjuji/k8s-seance/internal/autopsy"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

// newTestServer builds a Server with fixtures and a quiet logger, applying opts on top.
func newTestServer(t *testing.T, opts Options) *Server {
	t.Helper()
	opts.Logger = log.New(io.Discard, "", 0)
	srv, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv
}

// get sends a GET to the server and returns the recorded response.
func get(t *testing.T, srv *Server, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec
}

// spiritPod returns a Pod whose only container is waiting with the given reason.
func spiritPod(ns, name, reason string, restarts int32) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "busybox"}}},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:         "app",
				RestartCount: restarts,
				State:        corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason}},
			}},
		},
	}
}

// clusterWith returns a ClusterSource over a fake clientset that holds objs.
func clusterWith(objs ...runtime.Object) *ClusterSource {
	return &ClusterSource{Client: fake.NewClientset(objs...)}
}

func TestFixturesListPage(t *testing.T) {
	srv := newTestServer(t, Options{Fixtures: NewFixtureSource(), FixturesOnly: true})

	rec := get(t, srv, "/spirits")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, name := range []string{"memory-glutton-7cc6df9966-bf87c", "restless-spirit-84fc8b7c4c-52nl8", "lost-soul-5ff5cb8464-hjtms"} {
		if !strings.Contains(body, name) {
			t.Errorf("page does not contain pod %q", name)
		}
	}
	if n := strings.Count(body, `class="badge `); n != 3 {
		t.Errorf("badge count = %d, want 3", n)
	}
	for _, class := range []string{"badge-oomkilled", "badge-crashloopbackoff", "badge-imagepullbackoff"} {
		if !strings.Contains(body, class) {
			t.Errorf("page has no badge class %q", class)
		}
	}
	if !strings.Contains(body, "样例模式") {
		t.Error("fixture page has no sample-mode banner")
	}
}

func TestFixturesAPI(t *testing.T) {
	srv := newTestServer(t, Options{Fixtures: NewFixtureSource()})

	rec := get(t, srv, "/api/spirits?src=fixtures")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp spiritsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Source != "fixtures" {
		t.Errorf("source = %q, want fixtures", resp.Source)
	}
	want := []autopsy.CauseOfDeath{autopsy.CauseOOMKilled, autopsy.CauseCrashLoopBackOff, autopsy.CauseImagePullBackOff}
	if len(resp.Spirits) != len(want) {
		t.Fatalf("got %d spirits, want %d", len(resp.Spirits), len(want))
	}
	for i, sp := range resp.Spirits {
		if sp.Cause != want[i] {
			t.Errorf("spirit %d (%s) cause = %q, want %q", i, sp.Name, sp.Cause, want[i])
		}
		if sp.RestartCount == 0 && sp.Cause != autopsy.CauseImagePullBackOff {
			t.Errorf("spirit %s restartCount = 0, want > 0", sp.Name)
		}
		if sp.LastDeath == nil && sp.Cause != autopsy.CauseImagePullBackOff {
			t.Errorf("spirit %s has no lastDeath", sp.Name)
		}
	}
}

func TestFixturesIgnoreNamespace(t *testing.T) {
	src := NewFixtureSource()
	got, err := src.List(context.Background(), "some-other-ns")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("List(other ns) returned %d spirits, want all 3", len(got))
	}
	if _, err := src.Autopsy(context.Background(), "", "missing-pod"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Autopsy(missing) error = %v, want ErrNotFound", err)
	}
}

func TestNilClusterShowsNotice(t *testing.T) {
	srv := newTestServer(t, Options{
		ClusterErr: errors.New("kubeconfig: no such file"),
		Fixtures:   NewFixtureSource(),
	})

	page := get(t, srv, "/spirits")
	if page.Code != http.StatusOK {
		t.Fatalf("page status = %d, want 200", page.Code)
	}
	body := page.Body.String()
	if !strings.Contains(body, "连不上集群：kubeconfig: no such file") {
		t.Error("page does not show the cluster error in plain words")
	}
	if !strings.Contains(body, `href="/spirits?src=fixtures"`) {
		t.Error("page has no link to the sample mode")
	}

	api := get(t, srv, "/api/spirits")
	if api.Code != http.StatusServiceUnavailable {
		t.Fatalf("api status = %d, want 503", api.Code)
	}
	var e apiError
	if err := json.Unmarshal(api.Body.Bytes(), &e); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if !strings.Contains(e.Error, "kubeconfig: no such file") || e.Fallback != fixturesAPIHref {
		t.Errorf("api error = %+v", e)
	}

	// Fixture mode still works without a cluster.
	if got := get(t, srv, "/api/spirits?src=fixtures"); got.Code != http.StatusOK {
		t.Errorf("fixtures api status = %d, want 200", got.Code)
	}
}

func TestNamespaceSwitchOnFakeCluster(t *testing.T) {
	srv := newTestServer(t, Options{
		Cluster:  clusterWith(spiritPod("default", "web-1", "CrashLoopBackOff", 2), spiritPod("jobs", "batch-9", "ImagePullBackOff", 0)),
		Fixtures: NewFixtureSource(),
	})

	cases := []struct {
		query string
		want  []string
	}{
		{"/api/spirits", []string{"web-1"}},
		{"/api/spirits?ns=default", []string{"web-1"}},
		{"/api/spirits?ns=jobs", []string{"batch-9"}},
		{"/api/spirits?ns=empty-ns", nil},
	}
	for _, tc := range cases {
		rec := get(t, srv, tc.query)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200", tc.query, rec.Code)
		}
		var resp spiritsResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("%s: decode: %v", tc.query, err)
		}
		if resp.Source != "cluster" {
			t.Errorf("%s: source = %q, want cluster", tc.query, resp.Source)
		}
		var names []string
		for _, sp := range resp.Spirits {
			names = append(names, sp.Name)
		}
		if strings.Join(names, ",") != strings.Join(tc.want, ",") {
			t.Errorf("%s: names = %v, want %v", tc.query, names, tc.want)
		}
	}

	// The cluster list carries Cause and RestartCount from the collector.
	rec := get(t, srv, "/api/spirits?ns=default")
	var resp spiritsResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Spirits[0].Cause != autopsy.CauseCrashLoopBackOff || resp.Spirits[0].RestartCount != 2 {
		t.Errorf("summary = %+v, want CrashLoopBackOff with restartCount 2", resp.Spirits[0])
	}

	// The page shows only the switched namespace.
	page := get(t, srv, "/spirits?ns=jobs")
	if !strings.Contains(page.Body.String(), "batch-9") || strings.Contains(page.Body.String(), "web-1") {
		t.Error("page for ns=jobs does not list only batch-9")
	}
}

func TestEmptyListMessage(t *testing.T) {
	srv := newTestServer(t, Options{Cluster: clusterWith(), Fixtures: NewFixtureSource()})

	page := get(t, srv, "/spirits")
	if !strings.Contains(page.Body.String(), "这里没有亡灵") {
		t.Error("empty list page has no friendly empty text")
	}
	api := get(t, srv, "/api/spirits")
	if !strings.Contains(api.Body.String(), `"spirits":[]`) {
		t.Errorf("empty api body = %s, want spirits as []", api.Body.String())
	}
}

func TestStaticHTMX(t *testing.T) {
	srv := newTestServer(t, Options{Fixtures: NewFixtureSource()})

	for _, file := range []string{"htmx.min.js", "sse.js"} {
		rec := get(t, srv, "/static/"+file)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200", file, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
			t.Errorf("%s: Content-Type = %q, want a javascript type", file, ct)
		}
		if rec.Body.Len() == 0 {
			t.Errorf("%s: empty body", file)
		}
	}
	if rec := get(t, srv, "/static/style.css"); rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Content-Type"), "text/css") {
		t.Errorf("style.css: status %d, content type %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if rec := get(t, srv, "/static/missing.js"); rec.Code != http.StatusNotFound {
		t.Errorf("missing static file status = %d, want 404", rec.Code)
	}
}

func TestPodNameIsEscaped(t *testing.T) {
	const evil = "<script>alert(1)</script>"
	srv := newTestServer(t, Options{Cluster: clusterWith(spiritPod("default", evil, "CrashLoopBackOff", 1)), Fixtures: NewFixtureSource()})

	body := get(t, srv, "/spirits").Body.String()
	if strings.Contains(body, evil) {
		t.Error("raw <script> from a pod name reached the page")
	}
	if !strings.Contains(body, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Error("pod name is not HTML-escaped in the page")
	}
}

func TestRoutesAndErrors(t *testing.T) {
	srv := newTestServer(t, Options{Fixtures: NewFixtureSource(), FixturesOnly: true})

	root := get(t, srv, "/")
	if root.Code != http.StatusFound || root.Header().Get("Location") != "/spirits" {
		t.Errorf("GET / = %d to %q, want 302 to /spirits", root.Code, root.Header().Get("Location"))
	}
	if h := get(t, srv, "/healthz"); h.Code != http.StatusOK || strings.TrimSpace(h.Body.String()) != "ok" {
		t.Errorf("healthz = %d %q", h.Code, h.Body.String())
	}

	bad := []string{
		"/spirits?ns=Bad_Name",
		"/api/spirits?ns=-leading",
		"/api/spirits?src=elsewhere",
		"/spirits?src=elsewhere",
	}
	for _, target := range bad {
		if rec := get(t, srv, target); rec.Code != http.StatusBadRequest {
			t.Errorf("%s status = %d, want 400", target, rec.Code)
		}
	}
}

func TestFixturesOnlyIgnoresClusterSrc(t *testing.T) {
	srv := newTestServer(t, Options{Cluster: clusterWith(spiritPod("default", "web-1", "CrashLoopBackOff", 1)), FixturesOnly: true, Fixtures: NewFixtureSource()})

	rec := get(t, srv, "/api/spirits?src=cluster")
	var resp spiritsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Source != "fixtures" || len(resp.Spirits) != 3 {
		t.Errorf("source = %q with %d spirits, want fixtures with 3", resp.Source, len(resp.Spirits))
	}
}

func TestNewRejectsBadDefaultNamespace(t *testing.T) {
	if _, err := New(Options{DefaultNamespace: "Not_Valid", Logger: log.New(io.Discard, "", 0)}); err == nil {
		t.Error("New accepted an invalid default namespace")
	}
}

func TestTailKeepsLastLines(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		n    int
		want []string
	}{
		{"nil stays nil", nil, 2, nil},
		{"zero keeps all", []string{"a", "b"}, 0, []string{"a", "b"}},
		{"shorter than n", []string{"a"}, 3, []string{"a"}},
		{"keeps last n", []string{"a", "b", "c"}, 2, []string{"b", "c"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := keepLastLines(tc.in, tc.n)
			if (got == nil) != (tc.want == nil) || strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("keepLastLines(%v, %d) = %v, want %v", tc.in, tc.n, got, tc.want)
			}
		})
	}
}
