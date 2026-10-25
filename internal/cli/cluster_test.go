package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/envtest"
	"github.com/showmino/forge/internal/exitcode"
	"github.com/showmino/forge/internal/server"
)

func (h *harness) serveClusters(metrics, storage bool) {
	kube, dyn := envtest.Nodes(metrics)
	o := server.ClusterOptions{Kube: kube, Dynamic: dyn, GrafanaURL: "https://grafana.example.com"}
	if storage {
		prom := envtest.Prometheus()
		h.t.Cleanup(prom.Close)
		o.Prometheus = &server.Prometheus{URL: prom.URL}
	}
	h.clusters = server.NewClusters(o)
	h.startAPI(h.authn)
}

func TestClusterListAndGet(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.serveClusters(true, true)

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
	h.serveClusters(false, false)
	r := h.ok("cluster", "get", "default")
	if !strings.Contains(r.out, "12 cores") || strings.Contains(r.out, "%") || !strings.Contains(r.err, "metrics-server did not answer") || !strings.Contains(r.err, "--prometheus-url") {
		t.Errorf("without metrics:\n%v", r)
	}
	if r := h.ok("cluster", "list"); !strings.Contains(r.out, "Memory pressure on 1 node") {
		t.Errorf("list:\n%s", r.out)
	}
}

func TestClusterListRemotes(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	kube, dyn := envtest.Nodes(true)
	remote, remoteDyn := envtest.Nodes(true)
	h.clusters = server.NewClusters(server.ClusterOptions{Name: "production", Kube: kube, Dynamic: dyn, Remotes: []server.RemoteCluster{
		{Name: "aks", Connect: func(context.Context) (kubernetes.Interface, dynamic.Interface, error) { return remote, remoteDyn, nil }},
		{Name: "gone", Connect: func(context.Context) (kubernetes.Interface, dynamic.Interface, error) {
			return nil, nil, errors.New("there is no Argo CD cluster Secret argocd/gone")
		}},
	}})
	h.startAPI(h.authn)

	r := h.ok("cluster", "list")
	lines := strings.Split(strings.TrimSpace(r.out), "\n")
	if len(lines) != 4 || !strings.HasPrefix(lines[1], "production ") || !strings.HasPrefix(lines[2], "aks ") ||
		!strings.HasPrefix(lines[3], "gone ") || !strings.Contains(lines[3], "Unavailable") || strings.Contains(lines[3], "%") {
		t.Errorf("list:\n%s", r.out)
	}
	if !strings.Contains(r.err, "gone: cluster gone: there is no Argo CD cluster Secret argocd/gone") {
		t.Errorf("the reason is said: %q", r.err)
	}
	if r := h.fails(int(exitcode.Conflict), "cluster", "get", "gone"); !strings.Contains(r.err, "argocd/gone") {
		t.Errorf("get the unreadable one: %v", r)
	}
	h.ok("cluster", "get", "aks")
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
		{&forgev1.Cluster{StorageAvailable: true, Totals: &forgev1.Capacity{DiskBytes: 100, DiskUsedBytes: 85}}, "Disk at 85%"},
		{&forgev1.Cluster{MetricsAvailable: true, StorageAvailable: true, Totals: used(100, 840), Volumes: []*forgev1.Volume{{Namespace: "payments-production", Claim: "data", CapacityBytes: 10, UsedBytes: 9}}}, "Volume payments-production/data at 90%"},
		{&forgev1.Cluster{StorageAvailable: false, Volumes: []*forgev1.Volume{{CapacityBytes: 10, UsedBytes: 9}}}, "Healthy"},
	} {
		if got := clusterStatus(tc.cl); got != tc.want {
			t.Errorf("%v: got %q, want %q", tc.cl, got, tc.want)
		}
	}
}
