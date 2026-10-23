package cli

import (
	"strings"
	"testing"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/envtest"
	"github.com/showmino/forge/internal/exitcode"
	"github.com/showmino/forge/internal/server"
)

func (h *harness) serveClusters(metrics bool) {
	kube, dyn := envtest.Nodes(metrics)
	h.clusters = server.NewClusters(server.ClusterOptions{Kube: kube, Dynamic: dyn, GrafanaURL: "https://grafana.example.com"})
	h.startAPI(h.authn)
}

func TestClusterListAndGet(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.serveClusters(true)

	golden(t, "cluster_list_table", h.ok("cluster", "list").out)
	golden(t, "cluster_get_table", h.ok("cluster", "get", "default").out)
	if r := h.ok("clusters", "ls", "-o", "name"); r.out != "clusters/default\n" {
		t.Errorf("names: %q", r.out)
	}
	if r := h.ok("cluster", "get", "clusters/default", "-o", "json"); !strings.Contains(r.out, `"name": "clusters/default"`) || !strings.Contains(r.out, `"condition": "MemoryPressure"`) {
		t.Errorf("json:\n%s", r.out)
	}
	if r := h.fails(int(exitcode.NotFound), "cluster", "get", "other"); !strings.Contains(r.err, "clusters/default") {
		t.Errorf("other cluster: %v", r)
	}

	h.env["FORGE_TOKEN"] = readerToken
	if r := h.fails(int(exitcode.PermissionDenied), "cluster", "list"); !strings.Contains(r.err, "only an admin may") {
		t.Errorf("reader: %v", r)
	}
}

func TestClusterWithoutMetrics(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.serveClusters(false)
	r := h.ok("cluster", "get", "default")
	if !strings.Contains(r.out, "12 cores") || strings.Contains(r.out, "%") || !strings.Contains(r.err, "metrics-server did not answer") {
		t.Errorf("without metrics:\n%v", r)
	}
	if r := h.ok("cluster", "list"); !strings.Contains(r.out, "Memory pressure on 1 node") {
		t.Errorf("list:\n%s", r.out)
	}
}

func TestClusterNotConfigured(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	if r := h.fails(int(exitcode.Conflict), "cluster", "list"); !strings.Contains(r.err, "--clusters") {
		t.Errorf("stderr: %q", r.err)
	}
}

func TestClusterStatus(t *testing.T) {
	t.Parallel()
	node := func(cond string) *forgev1.Node { return &forgev1.Node{Condition: cond} }
	used := func(cpu, mem int64) *forgev1.Capacity {
		return &forgev1.Capacity{CpuMillis: 1000, CpuUsedMillis: cpu, MemoryBytes: 1000, MemoryUsedBytes: mem}
	}
	for _, tc := range []struct {
		cl   *forgev1.Cluster
		want string
	}{
		{&forgev1.Cluster{MetricsAvailable: true, Totals: used(100, 100), Nodes: []*forgev1.Node{node("Ready")}}, "Healthy"},
		{&forgev1.Cluster{MetricsAvailable: true, Totals: used(100, 840)}, "Memory at 84%"},
		{&forgev1.Cluster{MetricsAvailable: true, Totals: used(910, 840)}, "CPU at 91%"},
		{&forgev1.Cluster{MetricsAvailable: false, Totals: used(910, 840)}, "Healthy"},
		{&forgev1.Cluster{Nodes: []*forgev1.Node{node("DiskPressure"), node("DiskPressure")}}, "Disk pressure on 2 nodes"},
		{&forgev1.Cluster{Nodes: []*forgev1.Node{node("MemoryPressure"), node("NotReady")}}, "1 node not ready"},
	} {
		if got := clusterStatus(tc.cl); got != tc.want {
			t.Errorf("%v: got %q, want %q", tc.cl, got, tc.want)
		}
	}
}
