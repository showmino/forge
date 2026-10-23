package server

import (
	"context"
	"errors"
	"log/slog"
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
	Log     *slog.Logger
	Now     func() time.Time
}

// Clusters serves ClusterService from the Kubernetes API and metrics-server.
// A nil *Clusters answers FAILED_PRECONDITION.
type Clusters struct {
	o ClusterOptions

	mu      sync.Mutex
	cached  *forgev1.Cluster
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
	return &Clusters{o: o}
}

func (c *Clusters) ListClusters(ctx context.Context, _ *connect.Request[forgev1.ListClustersRequest]) (*connect.Response[forgev1.ListClustersResponse], error) {
	if err := authorizeTeam(ctx, ActClusterRead, ""); err != nil {
		return nil, err
	}
	if c == nil {
		return nil, errClustersOff
	}
	cl, err := c.cluster(ctx)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&forgev1.ListClustersResponse{Clusters: []*forgev1.Cluster{cl}}), nil
}

func (c *Clusters) GetCluster(ctx context.Context, req *connect.Request[forgev1.GetClusterRequest]) (*connect.Response[forgev1.GetClusterResponse], error) {
	if err := authorizeTeam(ctx, ActClusterRead, ""); err != nil {
		return nil, err
	}
	if c == nil {
		return nil, errClustersOff
	}
	id := strings.TrimPrefix(req.Msg.GetName(), clusterPrefix)
	if id == "" {
		return nil, invalidArg("name is required: clusters/<name>")
	}
	if id != c.o.Name {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("there is no cluster "+clusterPrefix+id+"; this Forge runs on "+clusterPrefix+c.o.Name))
	}
	cl, err := c.cluster(ctx)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&forgev1.GetClusterResponse{Cluster: cl}), nil
}

var errClustersOff = connect.NewError(connect.CodeFailedPrecondition, errors.New("cluster capacity is not configured on this Forge API (--clusters)"))

// cluster returns a copy of the cluster's figures, read at most every clustersFresh.
func (c *Clusters) cluster(ctx context.Context) (*forgev1.Cluster, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cached == nil || c.o.Now().Sub(c.fetched) >= clustersFresh {
		cl, err := c.read(ctx)
		if err != nil {
			return nil, err
		}
		c.cached, c.fetched = cl, c.o.Now()
	}
	return proto.Clone(c.cached).(*forgev1.Cluster), nil
}

func (c *Clusters) read(ctx context.Context) (*forgev1.Cluster, error) {
	nodes, err := c.o.Kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, c.kubeErr(ctx, "nodes", err)
	}
	pods, err := c.o.Kube.CoreV1().Pods("").List(ctx, metav1.ListOptions{FieldSelector: "status.phase=Running"})
	if err != nil {
		return nil, c.kubeErr(ctx, "pods", err)
	}
	running := map[string]int32{}
	for i := range pods.Items {
		if p := &pods.Items[i]; p.Status.Phase == corev1.PodRunning && p.Spec.NodeName != "" {
			running[p.Spec.NodeName]++
		}
	}
	usage, metricsOK := c.usage(ctx)

	out := &forgev1.Cluster{
		Name:             clusterPrefix + c.o.Name,
		GrafanaUrl:       c.o.GrafanaURL,
		Totals:           &forgev1.Capacity{},
		MetricsAvailable: metricsOK,
	}
	if v, err := c.o.Kube.Discovery().ServerVersion(); err == nil {
		out.KubernetesVersion = v.GitVersion
	} else {
		c.o.Log.WarnContext(ctx, "reading the Kubernetes version failed", "error", err)
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
			Pods:            running[n.Name],
			PodsMax:         int32(alloc.Pods().Value()), //nolint:gosec // kubelet caps pods far below int32
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
		if node.Schedulable {
			t := out.Totals
			t.CpuMillis += capa.CpuMillis
			t.CpuUsedMillis += capa.CpuUsedMillis
			t.MemoryBytes += capa.MemoryBytes
			t.MemoryUsedBytes += capa.MemoryUsedBytes
			t.Pods += capa.Pods
			t.PodsMax += capa.PodsMax
		}
	}
	if !oldest.IsZero() {
		out.CreateTime = timestamppb.New(oldest)
	}
	slices.SortFunc(out.Nodes, func(a, b *forgev1.Node) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

type nodeUsage struct{ cpu, memory int64 }

// usage reads metrics-server; false when it did not answer.
func (c *Clusters) usage(ctx context.Context) (map[string]nodeUsage, bool) {
	if c.o.Dynamic == nil {
		return nil, false
	}
	list, err := c.o.Dynamic.Resource(NodeMetrics).List(ctx, metav1.ListOptions{})
	if err != nil {
		c.o.Log.WarnContext(ctx, "reading node metrics from metrics-server failed", "error", err)
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

func (c *Clusters) kubeErr(ctx context.Context, what string, err error) error {
	c.o.Log.WarnContext(ctx, "reading the cluster failed", "resource", what, "error", err)
	if apierrors.IsForbidden(err) {
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("forge-api may not read the cluster's "+what+": apply its ClusterRole forge-api-clusters"))
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
