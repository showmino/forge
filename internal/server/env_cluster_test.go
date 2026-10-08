package server

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/showmino/forge/internal/envtest"
)

func testCluster(objs ...*corev1.Namespace) *Cluster {
	kube := envtest.Kube()
	for _, ns := range objs {
		_, _ = kube.CoreV1().Namespaces().Create(context.Background(), ns, metav1.CreateOptions{})
	}
	return &Cluster{Kube: kube, Dynamic: envtest.Dynamic(), Gateway: "forge/forge-previews", PullSecret: "forge/mino-ghcr"}
}

func TestClusterLifecycle(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := testCluster()
	if err := c.CreateEnv(ctx, "pr-1", map[string]string{labelService: "payments"}, map[string]string{annState: stateCreating}); err != nil {
		t.Fatal(err)
	}
	if err := c.CreateEnv(ctx, "pr-1", nil, nil); !errors.Is(err, errEnvExists) {
		t.Errorf("second create: %v", err)
	}
	ns, err := c.Env(ctx, "pr-1")
	if err != nil || ns.Name != "preview-pr-1" || ns.Labels[labelManagedBy] != "forge" || ns.Labels[labelEnvironment] != "pr-1" || ns.Labels[labelService] != "payments" {
		t.Fatalf("env: %v %v", ns, err)
	}
	if err := c.Annotate(ctx, "pr-1", map[string]string{annState: stateReady}); err != nil {
		t.Fatal(err)
	}
	if ns, _ := c.Env(ctx, "pr-1"); ns.Annotations[annState] != stateReady {
		t.Errorf("annotate: %v", ns.Annotations)
	}

	w := Workload{Service: "payments", Image: "ghcr.io/showmino/payments:1a2b3c4", Host: "pr-1.preview.example", Port: 9090, Health: "/ready"}
	if err := c.Deploy(ctx, "pr-1", w); err != nil {
		t.Fatal(err)
	}
	d, err := c.Kube.AppsV1().Deployments("preview-pr-1").Get(ctx, "payments", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pod := d.Spec.Template.Spec
	ctr := pod.Containers[0]
	switch {
	case *d.Spec.Replicas != 1:
		t.Error("replicas")
	case ctr.Image != w.Image || ctr.Ports[0].ContainerPort != 9090:
		t.Errorf("container: %+v", ctr)
	case ctr.ReadinessProbe.HTTPGet.Path != "/ready" || ctr.LivenessProbe.HTTPGet.Path != "/ready" || ctr.ReadinessProbe.HTTPGet.Port.IntValue() != 9090:
		t.Errorf("probes: %+v", ctr.ReadinessProbe)
	case len(ctr.Env) != 1 || ctr.Env[0].Name != "PORT" || ctr.Env[0].Value != "9090":
		t.Errorf("env: %+v", ctr.Env)
	case pod.SecurityContext == nil || !*pod.SecurityContext.RunAsNonRoot:
		t.Error("runAsNonRoot")
	case pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken:
		t.Error("service account token mounted")
	case ctr.SecurityContext == nil || !*ctr.SecurityContext.ReadOnlyRootFilesystem || *ctr.SecurityContext.AllowPrivilegeEscalation || ctr.SecurityContext.Capabilities.Drop[0] != "ALL":
		t.Errorf("container security: %+v", ctr.SecurityContext)
	case len(pod.ImagePullSecrets) != 1 || pod.ImagePullSecrets[0].Name != "forge-pull":
		t.Errorf("pull secrets: %+v", pod.ImagePullSecrets)
	}
	s, err := c.Kube.CoreV1().Secrets("preview-pr-1").Get(ctx, "forge-pull", metav1.GetOptions{})
	if err != nil || s.Type != corev1.SecretTypeDockerConfigJson || string(s.Data[corev1.DockerConfigJsonKey]) != string(envtest.PullSecretJSON) {
		t.Errorf("pull secret copy: %v %v", s, err)
	}
	svc, err := c.Kube.CoreV1().Services("preview-pr-1").Get(ctx, "payments", metav1.GetOptions{})
	if err != nil || svc.Spec.Ports[0].Port != 80 || svc.Spec.Ports[0].TargetPort.IntValue() != 9090 {
		t.Errorf("service: %v %v", svc, err)
	}
	q, err := c.Kube.CoreV1().ResourceQuotas("preview-pr-1").Get(ctx, "forge", metav1.GetOptions{})
	if err != nil || q.Spec.Hard.Pods().Value() != 10 || q.Spec.Hard.Name("limits.memory", "").String() != "2Gi" {
		t.Errorf("quota: %v %v", q, err)
	}
	lr, err := c.Kube.CoreV1().LimitRanges("preview-pr-1").Get(ctx, "forge", metav1.GetOptions{})
	if err != nil || lr.Spec.Limits[0].Default.Memory().String() != "512Mi" || lr.Spec.Limits[0].DefaultRequest.Cpu().String() != "50m" {
		t.Errorf("limit range: %v %v", lr, err)
	}
	route, err := c.Dynamic.Resource(envtest.HTTPRoutes).Namespace("preview-pr-1").Get(ctx, "payments", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	hosts, _, _ := unstructured.NestedStringSlice(route.Object, "spec", "hostnames")
	parents, _, _ := unstructured.NestedSlice(route.Object, "spec", "parentRefs")
	if len(hosts) != 1 || hosts[0] != "pr-1.preview.example" || len(parents) != 1 ||
		parents[0].(map[string]any)["name"] != "forge-previews" || parents[0].(map[string]any)["namespace"] != "forge" {
		t.Errorf("route: %v", route.Object)
	}

	if ready, problem, err := c.Status(ctx, "pr-1", "payments"); err != nil || ready || problem != "0/1 pods ready" {
		t.Errorf("status before ready: %v %q %v", ready, problem, err)
	}
	_, _ = c.Kube.CoreV1().Pods("preview-pr-1").Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "payments-1", Labels: map[string]string{"app": "payments"}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: "pull access denied"}},
		}}},
	}, metav1.CreateOptions{})
	if _, problem, _ := c.Status(ctx, "pr-1", "payments"); problem != "ImagePullBackOff: pull access denied" {
		t.Errorf("pod problem: %q", problem)
	}
	d.Status.AvailableReplicas = 1
	_, _ = c.Kube.AppsV1().Deployments("preview-pr-1").UpdateStatus(ctx, d, metav1.UpdateOptions{})
	if ready, _, err := c.Status(ctx, "pr-1", "payments"); !ready || err != nil {
		t.Errorf("status ready: %v %v", ready, err)
	}

	envs, err := c.Envs(ctx)
	if err != nil || len(envs) != 1 {
		t.Errorf("envs: %v %v", envs, err)
	}
	if err := c.DeleteEnv(ctx, "pr-1"); err != nil {
		t.Fatal(err)
	}
	if gone, err := c.EnvGone(ctx, "pr-1"); !gone || err != nil {
		t.Errorf("gone: %v %v", gone, err)
	}
	if _, err := c.Env(ctx, "pr-1"); !errors.Is(err, errNoEnv) {
		t.Errorf("after delete: %v", err)
	}
}

func TestGuardIgnoresForeignNamespaces(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := testCluster(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "preview-x"}}, // not labelled by Forge
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "preview-y", Labels: map[string]string{labelManagedBy: "forge", labelEnvironment: "other"}}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "finance", Labels: map[string]string{labelManagedBy: "forge", labelEnvironment: "finance"}}},
	)
	for _, id := range []string{"x", "y"} {
		if _, err := c.Env(ctx, id); !errors.Is(err, errNoEnv) {
			t.Errorf("Env(%s): %v", id, err)
		}
		if err := c.Annotate(ctx, id, map[string]string{annState: stateReady}); !errors.Is(err, errNoEnv) {
			t.Errorf("Annotate(%s): %v", id, err)
		}
		if err := c.DeleteEnv(ctx, id); !errors.Is(err, errNoEnv) {
			t.Errorf("DeleteEnv(%s): %v", id, err)
		}
		if _, _, err := c.Status(ctx, id, "a"); !errors.Is(err, errNoEnv) {
			t.Errorf("Status(%s): %v", id, err)
		}
		if err := c.Deploy(ctx, id, Workload{Service: "a", Image: "ghcr.io/a/a:1", Host: "h", Port: 80, Health: "/"}); !errors.Is(err, errNoEnv) {
			t.Errorf("Deploy(%s): %v", id, err)
		}
	}
	if envs, _ := c.Envs(ctx); len(envs) != 0 {
		t.Errorf("Envs listed foreign namespaces: %v", envs)
	}
	for _, name := range []string{"preview-x", "preview-y", "finance"} {
		if _, err := c.Kube.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{}); err != nil {
			t.Errorf("%s was touched: %v", name, err)
		}
	}
}
