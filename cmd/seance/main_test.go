package main

import (
	"io"
	"log"
	"testing"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/zhangjuji/k8s-seance/internal/web"
)

func TestLoopbackAddr(t *testing.T) {
	for addr, want := range map[string]bool{
		"localhost:6660": true, "127.0.0.1:6660": true, "127.0.0.2:80": true, "[::1]:6660": true,
		":6660": false, "0.0.0.0:6660": false, "192.168.1.5:6660": false, "example.com:80": false, "localhost": false,
	} {
		if got := loopbackAddr(addr); got != want {
			t.Errorf("loopbackAddr(%q) = %v, want %v", addr, got, want)
		}
	}
}

func TestResurrectNamespaces(t *testing.T) {
	got, err := resurrectNamespaces(" seance-demo , demo2,")
	if err != nil || len(got) != 2 || got[0] != "seance-demo" || got[1] != "demo2" {
		t.Errorf("got %v, %v", got, err)
	}
	for _, bad := range []string{"", " , ", "Kube_System", "a/b"} {
		if _, err := resurrectNamespaces(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestResurrectOptionsOffByDefault(t *testing.T) {
	logger := log.New(io.Discard, "", 0)
	cluster := web.ClusterSource{Client: fake.NewClientset()}
	if r := resurrectOptions(false, cluster, []string{"seance-demo"}, logger); r != nil {
		t.Errorf("casting on without -allow-resurrect: %+v", r)
	}
	if r := resurrectOptions(true, nil, []string{"seance-demo"}, logger); r != nil {
		t.Errorf("casting on without a cluster: %+v", r)
	}
	r := resurrectOptions(true, cluster, []string{"seance-demo"}, logger)
	if r == nil || r.Client == nil || len(r.Namespaces) != 1 {
		t.Errorf("casting not on with -allow-resurrect and a cluster: %+v", r)
	}
}
