package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/envtest"
)

const gib = int64(1) << 30

func adminCtx() context.Context {
	return context.WithValue(context.Background(), principalKey{}, &Principal{Subject: "ana", Permissions: []string{PermServicesRead, PermServicesWrite}})
}

func TestGetCluster(t *testing.T) {
	t.Parallel()
	kube, dyn := envtest.Nodes(true)
	c := NewClusters(ClusterOptions{Kube: kube, Dynamic: dyn, GrafanaURL: "https://grafana.example.com"})

	resp, err := c.GetCluster(adminCtx(), connect.NewRequest(&forgev1.GetClusterRequest{Name: "clusters/default"}))
	if err != nil {
		t.Fatal(err)
	}
	cl := resp.Msg.GetCluster()
	if cl.GetName() != "clusters/default" || cl.GetKubernetesVersion() != "v1.37.0+k3s1" || cl.GetGrafanaUrl() != "https://grafana.example.com" ||
		!cl.GetMetricsAvailable() || !cl.GetCreateTime().AsTime().Equal(time.Date(2026, 4, 22, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("cluster: %v", cl)
	}
	// The cordoned node takes no new pods, so it is not in the totals.
	want := &forgev1.Capacity{CpuMillis: 12000, CpuUsedMillis: 3500, MemoryBytes: 24 * gib, MemoryUsedBytes: 10 * gib, Pods: 3, PodsMax: 220}
	if got := cl.GetTotals(); got.String() != want.String() {
		t.Errorf("totals: %v\nwant    %v", got, want)
	}
	nodes := cl.GetNodes()
	if len(nodes) != 3 {
		t.Fatalf("nodes: %v", nodes)
	}
	cron, kubeNode, lab := nodes[0], nodes[1], nodes[2]
	if cron.GetName() != "srv-cron" || cron.GetRoles()[0] != "worker" || cron.GetCondition() != "Ready" || !cron.GetSchedulable() ||
		cron.GetCapacity().GetPods() != 2 || cron.GetCapacity().GetCpuUsedMillis() != 2500 || cron.GetKubeletVersion() != "v1.37.0+k3s1" {
		t.Errorf("srv-cron (a finished pod does not count): %v", cron)
	}
	if kubeNode.GetName() != "srv-kube" || len(kubeNode.GetRoles()) != 2 || kubeNode.GetRoles()[0] != "control-plane" || kubeNode.GetRoles()[1] != "etcd" {
		t.Errorf("srv-kube: %v", kubeNode)
	}
	if lab.GetName() != "srv-lab" || lab.GetCondition() != "MemoryPressure" || lab.GetSchedulable() || lab.GetCapacity().GetMemoryUsedBytes() != 7*gib {
		t.Errorf("srv-lab: %v", lab)
	}

	if _, err := c.GetCluster(adminCtx(), connect.NewRequest(&forgev1.GetClusterRequest{Name: "default"})); err != nil {
		t.Errorf("bare id: %v", err)
	}
	if _, err := c.GetCluster(adminCtx(), connect.NewRequest(&forgev1.GetClusterRequest{Name: "clusters/other"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("other cluster: %v", err)
	}
	if _, err := c.GetCluster(adminCtx(), connect.NewRequest(&forgev1.GetClusterRequest{})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("no name: %v", err)
	}
}

func TestClusterStorage(t *testing.T) {
	t.Parallel()
	prom := envtest.Prometheus()
	t.Cleanup(prom.Close)
	get := func(p *Prometheus) *forgev1.Cluster {
		t.Helper()
		kube, dyn := envtest.Nodes(true)
		resp, err := NewClusters(ClusterOptions{Kube: kube, Dynamic: dyn, Prometheus: p}).GetCluster(adminCtx(), connect.NewRequest(&forgev1.GetClusterRequest{Name: "clusters/default"}))
		if err != nil {
			t.Fatal(err)
		}
		return resp.Msg.GetCluster()
	}

	cl := get(&Prometheus{URL: prom.URL})
	if !cl.GetStorageAvailable() {
		t.Fatalf("storage: %v", cl)
	}
	disk := map[string][2]int64{}
	for _, n := range cl.GetNodes() {
		disk[n.GetName()] = [2]int64{n.GetCapacity().GetDiskUsedBytes() / gib, n.GetCapacity().GetDiskBytes() / gib}
	}
	// srv-cron's disk names only its IP: it is found by the node's address.
	if disk["srv-kube"] != [2]int64{75, 100} || disk["srv-cron"] != [2]int64{20, 200} || disk["srv-lab"] != [2]int64{45, 50} {
		t.Errorf("disks (used, size GiB): %v", disk)
	}
	if tot := cl.GetTotals(); tot.GetDiskBytes() != 300*gib || tot.GetDiskUsedBytes() != 95*gib {
		t.Errorf("totals leave out the cordoned node: %v", tot)
	}
	vols := cl.GetVolumes()
	if len(vols) != 2 {
		t.Fatalf("volumes, one per claim although reported by two API servers: %v", vols)
	}
	if v := vols[0]; v.GetNamespace() != "payments-production" || v.GetClaim() != "data" || v.GetUsedBytes() != 9*gib || v.GetCapacityBytes() != 10*gib ||
		v.GetService() != "services/payments" || v.GetEnvironment() != "production" {
		t.Errorf("fullest first, tied to its service: %v", v)
	}
	if v := vols[1]; v.GetNamespace() != "monitoring" || v.GetClaim() != "prometheus-db" || v.GetService() != "" {
		t.Errorf("not a Forge namespace: %v", v)
	}

	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "down", http.StatusServiceUnavailable) }))
	t.Cleanup(down.Close)
	for name, p := range map[string]*Prometheus{"none": nil, "down": {URL: down.URL}} {
		cl := get(p)
		if cl.GetStorageAvailable() || len(cl.GetVolumes()) != 0 || cl.GetTotals().GetDiskBytes() != 0 || !cl.GetMetricsAvailable() || len(cl.GetNodes()) != 3 {
			t.Errorf("%s: the rest still answers, without storage: %v", name, cl)
		}
	}
}

func TestListClusters(t *testing.T) {
	t.Parallel()
	kube, dyn := envtest.Nodes(true)
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	c := NewClusters(ClusterOptions{Name: "homelab", Kube: kube, Dynamic: dyn, Now: func() time.Time { return now }})
	lists := 0
	kube.PrependReactor("list", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
		lists++
		return false, nil, nil
	})
	list := func() []*forgev1.Cluster {
		t.Helper()
		resp, err := c.ListClusters(adminCtx(), connect.NewRequest(&forgev1.ListClustersRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		return resp.Msg.GetClusters()
	}
	cls := list()
	if len(cls) != 1 || cls[0].GetName() != "clusters/homelab" || len(cls[0].GetNodes()) != 3 || cls[0].GetTotals().GetCpuMillis() != 12000 {
		t.Errorf("list: %v", cls)
	}
	// What a caller gets is a copy: changing it does not change the cache.
	cls[0].Nodes = nil
	resp, err := c.GetCluster(adminCtx(), connect.NewRequest(&forgev1.GetClusterRequest{Name: "clusters/homelab"}))
	if err != nil || len(resp.Msg.GetCluster().GetNodes()) != 3 {
		t.Errorf("get after list: %v %v", resp, err)
	}
	if lists != 1 {
		t.Errorf("the API server was asked %d times within %s", lists, clustersFresh)
	}
	now = now.Add(clustersFresh)
	list()
	if lists != 2 {
		t.Errorf("after %s: asked %d times", clustersFresh, lists)
	}
}

func TestClustersWithoutMetrics(t *testing.T) {
	t.Parallel()
	kube, dyn := envtest.Nodes(false)
	resp, err := NewClusters(ClusterOptions{Kube: kube, Dynamic: dyn}).GetCluster(adminCtx(), connect.NewRequest(&forgev1.GetClusterRequest{Name: "clusters/default"}))
	if err != nil {
		t.Fatal(err)
	}
	cl := resp.Msg.GetCluster()
	if cl.GetMetricsAvailable() || cl.GetTotals().GetCpuUsedMillis() != 0 || cl.GetTotals().GetCpuMillis() != 12000 || len(cl.GetNodes()) != 3 {
		t.Errorf("without metrics-server: %v", cl)
	}
}

func TestClustersErrors(t *testing.T) {
	t.Parallel()
	reader := context.WithValue(context.Background(), principalKey{}, &Principal{Subject: "bo", Permissions: []string{PermServicesRead}})
	kube, dyn := envtest.Nodes(true)
	c := NewClusters(ClusterOptions{Kube: kube, Dynamic: dyn})
	if _, err := c.ListClusters(reader, connect.NewRequest(&forgev1.ListClustersRequest{})); connect.CodeOf(err) != connect.CodePermissionDenied || !contains(err, "only an admin may") {
		t.Errorf("reader: %v", err)
	}
	if _, err := c.GetCluster(context.Background(), connect.NewRequest(&forgev1.GetClusterRequest{Name: "clusters/default"})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("no login: %v", err)
	}
	var off *Clusters
	if _, err := off.ListClusters(adminCtx(), connect.NewRequest(&forgev1.ListClustersRequest{})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("not configured: %v", err)
	}
	// Switched off, it still checks who asks first.
	if _, err := off.GetCluster(reader, connect.NewRequest(&forgev1.GetClusterRequest{Name: "clusters/default"})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("not configured, reader: %v", err)
	}
	workload := context.WithValue(context.Background(), principalKey{}, &Principal{Subject: "repo:showmino/payments", Workload: &Workload{}, Permissions: []string{PermServicesRead, PermServicesWrite}})
	if _, err := c.ListClusters(workload, connect.NewRequest(&forgev1.ListClustersRequest{})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("CI workload: %v", err)
	}

	denied, _ := envtest.Nodes(true)
	denied.PrependReactor("list", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "nodes"}, "", nil)
	})
	_, err := NewClusters(ClusterOptions{Kube: denied, Dynamic: dyn}).ListClusters(adminCtx(), connect.NewRequest(&forgev1.ListClustersRequest{}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !contains(err, "forge-api-clusters") {
		t.Errorf("no ClusterRole: %v", err)
	}
}
