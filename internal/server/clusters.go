package server

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"net"
	"slices"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
)

const clusterPrefix = "clusters/"

// clustersFresh is how long a cluster's figures are served from memory, so a
// dashboard left open does not load the API server.
const clustersFresh = 30 * time.Second

// clustersTimeout bounds one read: callers wait on the lock behind it.
const clustersTimeout = 10 * time.Second

// NodeMetrics is metrics-server's resource.
var NodeMetrics = schema.GroupVersionResource{Group: "metrics.k8s.io", Version: "v1beta1", Resource: "nodes"}

// ClusterOptions configure ClusterService.
type ClusterOptions struct {
	// Name is the cluster's id in "clusters/{id}"; default "default".
	Name string
	// GrafanaURL is linked from the cluster; empty for no link.
	GrafanaURL string
	Kube       kubernetes.Interface
	// Dynamic reads metrics-server's node metrics.
	Dynamic dynamic.Interface
	// Prometheus, when set, gives node disk and volume use.
	Prometheus *Prometheus
	// Remotes are other clusters, listed after this one. They get no Grafana
	// link and no Prometheus.
	Remotes []RemoteCluster
	Log     *slog.Logger
	Now     func() time.Time
}

// RemoteCluster is a cluster forge-api does not run on and reads with a
// credential of its own.
type RemoteCluster struct {
	// Name is its id in "clusters/{id}".
	Name string
	// Connect returns clients for it. It is called on every read, so it can
	// notice a changed credential; it may keep clients between calls.
	Connect func(ctx context.Context) (kubernetes.Interface, dynamic.Interface, error)
}

// Clusters serves ClusterService from the Kubernetes API and metrics-server.
// A nil *Clusters answers FAILED_PRECONDITION.
type Clusters struct {
	o       ClusterOptions
	members []*member
}

// member is one cluster and its cached figures (or the error of its last read).
type member struct {
	name    string
	local   bool
	connect func(ctx context.Context) (kubernetes.Interface, dynamic.Interface, error)

	mu      sync.Mutex
	cached  *forgev1.Cluster
	err     error
	fetched time.Time
}

// NewClusters returns the cluster service.
func NewClusters(o ClusterOptions) *Clusters {
	if o.Name == "" {
		o.Name = "default"
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	c := &Clusters{o: o}
	c.members = append(c.members, &member{name: o.Name, local: true, connect: func(context.Context) (kubernetes.Interface, dynamic.Interface, error) {
		return o.Kube, o.Dynamic, nil
	}})
	for _, r := range o.Remotes {
		c.members = append(c.members, &member{name: r.Name, connect: r.Connect})
	}
	return c
}

func (c *Clusters) ListClusters(ctx context.Context, _ *connect.Request[forgev1.ListClustersRequest]) (*connect.Response[forgev1.ListClustersResponse], error) {
	if err := authorizeTeam(ctx, ActClusterRead, ""); err != nil {
		return nil, err
	}
	if c == nil {
		return nil, errClustersOff()
	}
	out := make([]*forgev1.Cluster, len(c.members))
	errs := make([]error, len(c.members))
	var wg sync.WaitGroup
	for i, m := range c.members {
		wg.Go(func() { out[i], errs[i] = c.cluster(ctx, m) })
	}
	wg.Wait()
	failed := 0
	for i, err := range errs {
		if err == nil {
			continue
		}
		failed++
		msg := err.Error()
		var ce *connect.Error
		if errors.As(err, &ce) {
			msg = ce.Message()
		}
		out[i] = &forgev1.Cluster{Name: clusterPrefix + c.members[i].name, UnavailableReason: msg}
	}
	// With nothing to show, the reason is the answer.
	if failed == len(c.members) {
		return nil, errs[0]
	}
	return connect.NewResponse(&forgev1.ListClustersResponse{Clusters: out}), nil
}

func (c *Clusters) GetCluster(ctx context.Context, req *connect.Request[forgev1.GetClusterRequest]) (*connect.Response[forgev1.GetClusterResponse], error) {
	if err := authorizeTeam(ctx, ActClusterRead, ""); err != nil {
		return nil, err
	}
	if c == nil {
		return nil, errClustersOff()
	}
	id := strings.TrimPrefix(req.Msg.GetName(), clusterPrefix)
	if id == "" {
		return nil, invalidArg("name is required: clusters/<name>")
	}
	var names []string
	for _, m := range c.members {
		if m.name == id {
			cl, err := c.cluster(ctx, m)
			if err != nil {
				return nil, err
			}
			return connect.NewResponse(&forgev1.GetClusterResponse{Cluster: cl}), nil
		}
		names = append(names, clusterPrefix+m.name)
	}
	return nil, connect.NewError(connect.CodeNotFound, errors.New("there is no cluster "+clusterPrefix+id+"; this Forge knows "+strings.Join(names, ", ")))
}

func errClustersOff() error {
	return connect.NewError(connect.CodeFailedPrecondition, errors.New("cluster capacity is not configured on this Forge API (--clusters)"))
}

// cluster returns a copy of m's figures, read at most every clustersFresh. A
// remote's failed read is remembered as long, so a cluster that is down does
// not make every list wait for it; this cluster's is not, so a fixed
// ClusterRole shows at once.
func (c *Clusters) cluster(ctx context.Context, m *member) (*forgev1.Cluster, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if (m.cached == nil && m.err == nil) || c.o.Now().Sub(m.fetched) >= clustersFresh {
		cl, err := c.read(ctx, m)
		if err != nil && (m.local || ctx.Err() != nil) {
			return nil, err
		}
		m.cached, m.err, m.fetched = cl, err, c.o.Now()
	}
	if m.err != nil {
		return nil, m.err
	}
	return proto.Clone(m.cached).(*forgev1.Cluster), nil
}

func (c *Clusters) read(ctx context.Context, m *member) (*forgev1.Cluster, error) {
	ctx, cancel := context.WithTimeout(ctx, clustersTimeout)
	defer cancel()
	kube, dyn, err := m.connect(ctx)
	if err != nil {
		c.o.Log.WarnContext(ctx, "connecting to the cluster failed", "cluster", m.name, "error", err)
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("cluster "+m.name+": "+err.Error()))
	}
	nodes, err := kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, c.kubeErr(ctx, m, "nodes", err)
	}
	// Pending pods bound to a node count against its pod limit too.
	pods, err := kube.CoreV1().Pods("").List(ctx, metav1.ListOptions{FieldSelector: "status.phase!=Succeeded,status.phase!=Failed"})
	if err != nil {
		return nil, c.kubeErr(ctx, m, "pods", err)
	}
	onNode := map[string]int32{}
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Spec.NodeName != "" && p.Status.Phase != corev1.PodSucceeded && p.Status.Phase != corev1.PodFailed {
			onNode[p.Spec.NodeName]++
		}
	}
	usage, metricsOK := c.usage(ctx, m, dyn)
	var (
		disk      map[string]nodeDisk
		volumes   []*forgev1.Volume
		storageOK bool
		grafana   string
	)
	if m.local {
		disk, volumes, storageOK = c.storage(ctx, kube, nodes.Items)
		grafana = c.o.GrafanaURL
	}

	out := &forgev1.Cluster{
		Name:             clusterPrefix + m.name,
		GrafanaUrl:       grafana,
		Totals:           &forgev1.Capacity{},
		MetricsAvailable: metricsOK,
		StorageAvailable: storageOK,
		Volumes:          volumes,
	}
	if v, err := kube.Discovery().ServerVersion(); err == nil {
		out.KubernetesVersion = v.GitVersion
	} else {
		c.o.Log.WarnContext(ctx, "reading the Kubernetes version failed", "cluster", m.name, "error", err)
	}
	var oldest time.Time
	for i := range nodes.Items {
		n := &nodes.Items[i]
		if t := n.CreationTimestamp.Time; !t.IsZero() && (oldest.IsZero() || t.Before(oldest)) {
			oldest = t
		}
		alloc := n.Status.Allocatable
		capa := &forgev1.Capacity{
			CpuMillis:       alloc.Cpu().MilliValue(),
			CpuUsedMillis:   usage[n.Name].cpu,
			MemoryBytes:     alloc.Memory().Value(),
			MemoryUsedBytes: usage[n.Name].memory,
			Pods:            onNode[n.Name],
			PodsMax:         int32(alloc.Pods().Value()), //nolint:gosec // kubelet caps pods far below int32
			DiskBytes:       disk[n.Name].size,
			DiskUsedBytes:   disk[n.Name].used,
		}
		node := &forgev1.Node{
			Name:           n.Name,
			Roles:          nodeRoles(n),
			Condition:      nodeCondition(n),
			Schedulable:    !n.Spec.Unschedulable,
			KubeletVersion: n.Status.NodeInfo.KubeletVersion,
			Capacity:       capa,
		}
		out.Nodes = append(out.Nodes, node)
		// Headroom is only where new pods can go.
		if node.Schedulable && node.Condition != "NotReady" {
			t := out.Totals
			t.CpuMillis += capa.CpuMillis
			t.CpuUsedMillis += capa.CpuUsedMillis
			t.MemoryBytes += capa.MemoryBytes
			t.MemoryUsedBytes += capa.MemoryUsedBytes
			t.Pods += capa.Pods
			t.PodsMax += capa.PodsMax
			t.DiskBytes += capa.DiskBytes
			t.DiskUsedBytes += capa.DiskUsedBytes
		}
	}
	if !oldest.IsZero() {
		out.CreateTime = timestamppb.New(oldest)
	}
	slices.SortFunc(out.Nodes, func(a, b *forgev1.Node) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

type nodeUsage struct{ cpu, memory int64 }

// The root filesystem of each node (node-exporter), and every claim's use (kubelet).
const (
	promDiskSize   = `node_filesystem_size_bytes{mountpoint="/",fstype!~"tmpfs|ramfs|overlay|squashfs"}`
	promDiskAvail  = `node_filesystem_avail_bytes{mountpoint="/",fstype!~"tmpfs|ramfs|overlay|squashfs"}`
	promVolumeSize = `kubelet_volume_stats_capacity_bytes`
	promVolumeUsed = `kubelet_volume_stats_used_bytes`
)

type nodeDisk struct{ size, used int64 }

// storage reads node disks and volumes from Prometheus; false when there is
// none or it did not answer.
func (c *Clusters) storage(ctx context.Context, kube kubernetes.Interface, nodes []corev1.Node) (map[string]nodeDisk, []*forgev1.Volume, bool) {
	if c.o.Prometheus == nil {
		return nil, nil, false
	}
	var res [4][]promSample
	for i, q := range []string{promDiskSize, promDiskAvail, promVolumeSize, promVolumeUsed} {
		var err error
		if res[i], err = c.o.Prometheus.query(ctx, q); err != nil {
			c.o.Log.WarnContext(ctx, "reading storage from Prometheus failed", "query", q, "error", err)
			return nil, nil, false
		}
	}

	byIP := map[string]string{}
	for i := range nodes {
		for _, a := range nodes[i].Status.Addresses {
			if a.Type == corev1.NodeInternalIP {
				byIP[a.Address] = nodes[i].Name
			}
		}
	}
	nodeOf := func(l map[string]string) string {
		if n := cmp.Or(l["kubernetes_node"], l["node"]); n != "" {
			return n
		}
		host, _, err := net.SplitHostPort(l["instance"])
		if err != nil {
			host = l["instance"]
		}
		return byIP[host]
	}
	// Size and free space of the same series; a node's disk is its largest root.
	seriesOf := func(l map[string]string) string { return l["instance"] + "|" + l["device"] + "|" + l["job"] }
	avail := map[string]int64{}
	for _, s := range res[1] {
		avail[seriesOf(s.labels)] = int64(s.value)
	}
	disk := map[string]nodeDisk{}
	for _, s := range res[0] {
		n := nodeOf(s.labels)
		if n == "" || int64(s.value) <= disk[n].size {
			continue
		}
		disk[n] = nodeDisk{size: int64(s.value), used: max(int64(s.value)-avail[seriesOf(s.labels)], 0)}
	}

	// On k3s the kubelet's metrics come through the API server: the claim's
	// namespace is then exported_namespace, and each server reports it once.
	type claim struct{ ns, name string }
	claimOf := func(l map[string]string) claim {
		return claim{cmp.Or(l["exported_namespace"], l["namespace"]), l["persistentvolumeclaim"]}
	}
	size, used := map[claim]int64{}, map[claim]int64{}
	for _, s := range res[2] {
		if k := claimOf(s.labels); k.name != "" {
			size[k] = max(size[k], int64(s.value))
		}
	}
	for _, s := range res[3] {
		k := claimOf(s.labels)
		used[k] = max(used[k], int64(s.value))
	}
	owners := c.forgeNamespaces(ctx, kube)
	volumes := make([]*forgev1.Volume, 0, len(size))
	for k, sz := range size {
		v := &forgev1.Volume{Namespace: k.ns, Claim: k.name, CapacityBytes: sz, UsedBytes: used[k]}
		if o, ok := owners[k.ns]; ok {
			v.Service, v.Environment = servicePrefix+o.service, o.env
		}
		volumes = append(volumes, v)
	}
	slices.SortFunc(volumes, func(a, b *forgev1.Volume) int {
		if fa, fb := fullness(a), fullness(b); fa != fb {
			return cmp.Compare(fb, fa)
		}
		return cmp.Or(strings.Compare(a.Namespace, b.Namespace), strings.Compare(a.Claim, b.Claim))
	})
	return disk, volumes, true
}

func fullness(v *forgev1.Volume) float64 {
	if v.CapacityBytes <= 0 {
		return 0
	}
	return float64(v.UsedBytes) / float64(v.CapacityBytes)
}

type nsOwner struct{ service, env string }

// forgeNamespaces maps the namespaces Forge made to their service and
// environment. Without them volumes are still listed, only not tied to a service.
func (c *Clusters) forgeNamespaces(ctx context.Context, kube kubernetes.Interface) map[string]nsOwner {
	list, err := kube.CoreV1().Namespaces().List(ctx, metav1.ListOptions{LabelSelector: labelManagedBy + "=forge"})
	if err != nil {
		c.o.Log.WarnContext(ctx, "listing Forge's namespaces failed", "error", err)
		return nil
	}
	out := map[string]nsOwner{}
	for _, ns := range list.Items {
		if s := ns.Labels[labelService]; s != "" {
			out[ns.Name] = nsOwner{service: s, env: cmp.Or(ns.Labels[labelDeployEnv], ns.Labels[labelEnvironment])}
		}
	}
	return out
}

// usage reads metrics-server; false when it did not answer.
func (c *Clusters) usage(ctx context.Context, m *member, dyn dynamic.Interface) (map[string]nodeUsage, bool) {
	if dyn == nil {
		return nil, false
	}
	list, err := dyn.Resource(NodeMetrics).List(ctx, metav1.ListOptions{})
	if err != nil {
		c.o.Log.WarnContext(ctx, "reading node metrics from metrics-server failed", "cluster", m.name, "error", err)
		return nil, false
	}
	out := make(map[string]nodeUsage, len(list.Items))
	for _, item := range list.Items {
		u, _ := item.Object["usage"].(map[string]any)
		out[item.GetName()] = nodeUsage{cpu: quantity(u["cpu"], true), memory: quantity(u["memory"], false)}
	}
	return out, true
}

func quantity(v any, milli bool) int64 {
	s, _ := v.(string)
	q, err := resource.ParseQuantity(s)
	if err != nil {
		return 0
	}
	if milli {
		return q.MilliValue()
	}
	return q.Value()
}

func (c *Clusters) kubeErr(ctx context.Context, m *member, what string, err error) error {
	c.o.Log.WarnContext(ctx, "reading the cluster failed", "cluster", m.name, "resource", what, "error", err)
	if apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) {
		if m.local {
			return connect.NewError(connect.CodeFailedPrecondition, errors.New("forge-api may not read the cluster's "+what+": apply its ClusterRole forge-api-clusters"))
		}
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("cluster "+m.name+" refuses its credential for listing "+what))
	}
	if !m.local {
		return connect.NewError(connect.CodeUnavailable, errors.New("cluster "+m.name+" did not answer when asked for its "+what+"; try again in a moment"))
	}
	return connect.NewError(connect.CodeUnavailable, errors.New("the Kubernetes API could not be asked for the cluster's "+what+"; try again in a moment"))
}

const nodeRolePrefix = "node-role.kubernetes.io/"

func nodeRoles(n *corev1.Node) []string {
	var roles []string
	for k := range n.Labels {
		if r, ok := strings.CutPrefix(k, nodeRolePrefix); ok && r != "" {
			roles = append(roles, r)
		}
	}
	if len(roles) == 0 {
		return []string{"worker"}
	}
	slices.Sort(roles)
	return roles
}

// nodeProblems are the node conditions that are bad when true, worst first.
var nodeProblems = []corev1.NodeConditionType{
	corev1.NodeMemoryPressure, corev1.NodeDiskPressure, corev1.NodePIDPressure, corev1.NodeNetworkUnavailable,
}

func nodeCondition(n *corev1.Node) string {
	status := map[corev1.NodeConditionType]corev1.ConditionStatus{}
	for _, c := range n.Status.Conditions {
		status[c.Type] = c.Status
	}
	if status[corev1.NodeReady] != corev1.ConditionTrue {
		return "NotReady"
	}
	for _, p := range nodeProblems {
		if status[p] == corev1.ConditionTrue {
			return string(p)
		}
	}
	return "Ready"
}
