package envtest

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/version"
	fakediscovery "k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// NodeMetrics is metrics-server's node resource.
var NodeMetrics = schema.GroupVersionResource{Group: "metrics.k8s.io", Version: "v1beta1", Resource: "nodes"}

// Nodes returns a cluster of three nodes, as the Kubernetes API and
// metrics-server see it: srv-kube (control plane, Ready), srv-cron (worker,
// Ready) and srv-lab (cordoned, MemoryPressure). Without metrics, the dynamic
// client has no node metrics, as when metrics-server is down.
func Nodes(metrics bool) (*fake.Clientset, *dynamicfake.FakeDynamicClient) {
	created := time.Date(2026, 4, 22, 9, 0, 0, 0, time.UTC)
	node := func(name, ip string, age time.Duration, labels map[string]string, cpu, mem string, cond corev1.NodeConditionType, cordoned bool) *corev1.Node {
		conds := []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
		if cond != corev1.NodeReady {
			conds = append(conds, corev1.NodeCondition{Type: cond, Status: corev1.ConditionTrue})
		}
		return &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels, CreationTimestamp: metav1.NewTime(created.Add(age))},
			Spec:       corev1.NodeSpec{Unschedulable: cordoned},
			Status: corev1.NodeStatus{
				Conditions: conds,
				Addresses:  []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: ip}},
				NodeInfo:   corev1.NodeSystemInfo{KubeletVersion: "v1.37.0+k3s1"},
				Allocatable: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse(cpu), corev1.ResourceMemory: resource.MustParse(mem), corev1.ResourcePods: resource.MustParse("110"),
				},
			},
		}
	}
	pod := func(name, nodeName string, phase corev1.PodPhase) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}, Spec: corev1.PodSpec{NodeName: nodeName}, Status: corev1.PodStatus{Phase: phase}}
	}
	kube := fake.NewClientset(
		node("srv-kube", "10.150.0.10", 0, map[string]string{"node-role.kubernetes.io/control-plane": "true", "node-role.kubernetes.io/etcd": "true"}, "4", "8Gi", corev1.NodeReady, false),
		node("srv-cron", "10.150.0.15", 24*time.Hour, nil, "8", "16Gi", corev1.NodeReady, false),
		node("srv-lab", "10.150.0.13", 48*time.Hour, nil, "4", "8Gi", corev1.NodeMemoryPressure, true),
		pod("a", "srv-kube", corev1.PodRunning), pod("b", "srv-cron", corev1.PodRunning), pod("c", "srv-cron", corev1.PodRunning),
		pod("d", "srv-cron", corev1.PodSucceeded), pod("e", "srv-lab", corev1.PodRunning),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "payments-production", Labels: map[string]string{
			"app.kubernetes.io/managed-by": "forge", "forge.dev/service": "payments", "forge.dev/deploy-env": "production",
		}}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "monitoring"}},
	)
	kube.Discovery().(*fakediscovery.FakeDiscovery).FakedServerVersion = &version.Info{GitVersion: "v1.37.0+k3s1"}

	scheme := runtime.NewScheme()
	kinds := map[schema.GroupVersionResource]string{NodeMetrics: "NodeMetricsList"}
	if !metrics {
		dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, kinds)
		dyn.PrependReactor("list", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewServiceUnavailable("the server is currently unable to handle the request")
		})
		return kube, dyn
	}
	usage := func(name, cpu, mem string) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "metrics.k8s.io/v1beta1", "kind": "NodeMetrics",
			"metadata": map[string]any{"name": name},
			"usage":    map[string]any{"cpu": cpu, "memory": mem},
		}}
	}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, kinds)
	// Created through the resource: from its kind alone the fake would guess "nodemetrics".
	for _, u := range []*unstructured.Unstructured{usage("srv-kube", "1", "6Gi"), usage("srv-cron", "2500m", "4Gi"), usage("srv-lab", "500m", "7Gi")} {
		if _, err := dyn.Resource(NodeMetrics).Create(context.Background(), u, metav1.CreateOptions{}); err != nil {
			panic(err)
		}
	}
	return kube, dyn
}
