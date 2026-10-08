package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

// Labels and annotations on an environment's namespace. The namespace is
// the environment's only record, so it survives forge-api restarts.
const (
	envPrefix        = "preview-"
	labelManagedBy   = "app.kubernetes.io/managed-by"
	labelEnvironment = "forge.dev/environment"
	labelService     = "forge.dev/service"
	annRef           = "forge.dev/ref"
	annCommit        = "forge.dev/commit"
	annImage         = "forge.dev/image"
	annCreator       = "forge.dev/creator"
	annExpire        = "forge.dev/expire-time"
	annState         = "forge.dev/state"
	annStateMessage  = "forge.dev/state-message"

	stateCreating   = "creating"
	stateReady      = "ready"
	stateFailed     = "failed"
	stateDestroying = "destroying"

	pullSecretName = "forge-pull"
)

var httpRoutes = schema.GroupVersionResource{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "httproutes"}

var (
	errNoEnv     = errors.New("no such environment")
	errEnvExists = errors.New("environment already exists")
)

func envNamespace(id string) string { return envPrefix + id }

// Cluster is where preview environments live. Every method goes through the
// guard: it reads and changes only namespaces Forge made for an environment.
type Cluster struct {
	Kube    kubernetes.Interface
	Dynamic dynamic.Interface
	// Gateway is the "namespace/name" of the Gateway that routes previews.
	Gateway string
	// PullSecret is the "namespace/name" of a dockerconfigjson Secret
	// copied into each environment and used to check images.
	PullSecret string
}

// Workload is what an environment runs.
type Workload struct {
	Service, Image, Host string
	Port                 int32
	Health               string
}

// owned is the guard: a namespace is an environment's only if Forge made it.
func owned(ns *corev1.Namespace) bool {
	id, ok := strings.CutPrefix(ns.Name, envPrefix)
	return ok && ns.Labels[labelManagedBy] == "forge" && ns.Labels[labelEnvironment] == id
}

// Env returns the namespace of environment id, or errNoEnv.
func (c *Cluster) Env(ctx context.Context, id string) (*corev1.Namespace, error) {
	ns, err := c.Kube.CoreV1().Namespaces().Get(ctx, envNamespace(id), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, errNoEnv
	}
	if err != nil {
		return nil, err
	}
	if !owned(ns) {
		return nil, errNoEnv
	}
	return ns, nil
}

// Envs returns every environment's namespace.
func (c *Cluster) Envs(ctx context.Context) ([]corev1.Namespace, error) {
	list, err := c.Kube.CoreV1().Namespaces().List(ctx, metav1.ListOptions{LabelSelector: labelManagedBy + "=forge"})
	if err != nil {
		return nil, err
	}
	var out []corev1.Namespace
	for i := range list.Items {
		if owned(&list.Items[i]) {
			out = append(out, list.Items[i])
		}
	}
	return out, nil
}

// CreateEnv creates environment id's namespace, or returns errEnvExists.
func (c *Cluster) CreateEnv(ctx context.Context, id string, labels, annotations map[string]string) error {
	l := map[string]string{labelManagedBy: "forge", labelEnvironment: id}
	for k, v := range labels {
		l[k] = v
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: envNamespace(id), Labels: l, Annotations: annotations}}
	_, err := c.Kube.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return errEnvExists
	}
	return err
}

// Annotate merges annotations into environment id's namespace.
func (c *Cluster) Annotate(ctx context.Context, id string, annotations map[string]string) error {
	if _, err := c.Env(ctx, id); err != nil {
		return err
	}
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": annotations}})
	if err != nil {
		return err
	}
	_, err = c.Kube.CoreV1().Namespaces().Patch(ctx, envNamespace(id), types.MergePatchType, patch, metav1.PatchOptions{})
	if apierrors.IsNotFound(err) {
		return errNoEnv
	}
	return err
}

// DeleteEnv starts deleting environment id's namespace.
func (c *Cluster) DeleteEnv(ctx context.Context, id string) error {
	if _, err := c.Env(ctx, id); err != nil {
		return err
	}
	err := c.Kube.CoreV1().Namespaces().Delete(ctx, envNamespace(id), metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// EnvGone reports whether environment id's namespace no longer exists.
func (c *Cluster) EnvGone(ctx context.Context, id string) (bool, error) {
	_, err := c.Kube.CoreV1().Namespaces().Get(ctx, envNamespace(id), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	return false, err
}

// PullSecretData returns the .dockerconfigjson of the configured pull secret.
func (c *Cluster) PullSecretData(ctx context.Context) ([]byte, error) {
	ns, name, ok := strings.Cut(c.PullSecret, "/")
	if !ok {
		return nil, fmt.Errorf("pull secret %q must be namespace/name", c.PullSecret)
	}
	s, err := c.Kube.CoreV1().Secrets(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("pull secret %s: %w", c.PullSecret, err)
	}
	data, ok := s.Data[corev1.DockerConfigJsonKey]
	if !ok {
		return nil, fmt.Errorf("pull secret %s has no %s", c.PullSecret, corev1.DockerConfigJsonKey)
	}
	return data, nil
}

// Deploy creates what environment id runs: the pull secret copy, quota and
// limits, the Deployment, its Service and an HTTPRoute for w.Host. The
// namespace is new, so every object is created once.
func (c *Cluster) Deploy(ctx context.Context, id string, w Workload) error {
	if _, err := c.Env(ctx, id); err != nil {
		return err
	}
	ns := envNamespace(id)
	pull, err := c.PullSecretData(ctx)
	if err != nil {
		return err
	}
	core := c.Kube.CoreV1()
	meta := metav1.ObjectMeta{Name: "forge"}
	if _, err := core.Secrets(ns).Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: pullSecretName},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data:       map[string][]byte{corev1.DockerConfigJsonKey: pull},
	}, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("pull secret: %w", err)
	}
	if _, err := core.ResourceQuotas(ns).Create(ctx, &corev1.ResourceQuota{ObjectMeta: meta, Spec: corev1.ResourceQuotaSpec{
		Hard: corev1.ResourceList{
			"requests.cpu": resource.MustParse("1"), "requests.memory": resource.MustParse("1Gi"),
			"limits.cpu": resource.MustParse("2"), "limits.memory": resource.MustParse("2Gi"),
			corev1.ResourcePods: resource.MustParse("10"),
		},
	}}, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("resource quota: %w", err)
	}
	if _, err := core.LimitRanges(ns).Create(ctx, &corev1.LimitRange{ObjectMeta: meta, Spec: corev1.LimitRangeSpec{
		Limits: []corev1.LimitRangeItem{{
			Type:           corev1.LimitTypeContainer,
			DefaultRequest: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m"), corev1.ResourceMemory: resource.MustParse("64Mi")},
			Default:        corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("512Mi")},
		}},
	}}, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("limit range: %w", err)
	}
	if _, err := c.Kube.AppsV1().Deployments(ns).Create(ctx, deployment(w), metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("deployment: %w", err)
	}
	if _, err := core.Services(ns).Create(ctx, &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: w.Service},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": w.Service},
			Ports:    []corev1.ServicePort{{Name: "http", Port: 80, TargetPort: intstr.FromInt32(w.Port)}},
		},
	}, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("service: %w", err)
	}
	gwNS, gwName, ok := strings.Cut(c.Gateway, "/")
	if !ok {
		return fmt.Errorf("gateway %q must be namespace/name", c.Gateway)
	}
	route := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "gateway.networking.k8s.io/v1",
		"kind":       "HTTPRoute",
		"metadata":   map[string]any{"name": w.Service, "namespace": ns},
		"spec": map[string]any{
			"parentRefs": []any{map[string]any{"name": gwName, "namespace": gwNS}},
			"hostnames":  []any{w.Host},
			"rules":      []any{map[string]any{"backendRefs": []any{map[string]any{"name": w.Service, "port": int64(80)}}}},
		},
	}}
	if _, err := c.Dynamic.Resource(httpRoutes).Namespace(ns).Create(ctx, route, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("route: %w", err)
	}
	return nil
}

func deployment(w Workload) *appsv1.Deployment {
	yes, no := true, false
	one := int32(1)
	labels := map[string]string{"app": w.Service}
	probe := func(delay int32) *corev1.Probe {
		return &corev1.Probe{
			ProbeHandler:        corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: w.Health, Port: intstr.FromInt32(w.Port)}},
			InitialDelaySeconds: delay,
			PeriodSeconds:       5,
		}
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: w.Service, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: &one,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					ImagePullSecrets: []corev1.LocalObjectReference{{Name: pullSecretName}},
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot:   &yes,
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Containers: []corev1.Container{{
						Name:           w.Service,
						Image:          w.Image,
						Ports:          []corev1.ContainerPort{{Name: "http", ContainerPort: w.Port}},
						Env:            []corev1.EnvVar{{Name: "PORT", Value: strconv.Itoa(int(w.Port))}},
						ReadinessProbe: probe(0),
						LivenessProbe:  probe(10),
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: &no,
							ReadOnlyRootFilesystem:   &yes,
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
					}},
				},
			},
		},
	}
}

// Status reports whether the service's Deployment in environment id is
// available and, when it is not, the most telling problem seen.
func (c *Cluster) Status(ctx context.Context, id, service string) (bool, string, error) {
	ns := envNamespace(id)
	d, err := c.Kube.AppsV1().Deployments(ns).Get(ctx, service, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, "deployment " + service + " is missing", nil
	}
	if err != nil {
		return false, "", err
	}
	if d.Status.AvailableReplicas >= 1 {
		return true, "", nil
	}
	pods, err := c.Kube.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: "app=" + service})
	if err != nil {
		return false, "", err
	}
	for _, p := range pods.Items {
		for _, cs := range p.Status.ContainerStatuses {
			if w := cs.State.Waiting; w != nil && w.Reason != "" && w.Reason != "ContainerCreating" {
				return false, clip(w.Reason + ": " + w.Message), nil
			}
			if cs.RestartCount > 0 {
				return false, fmt.Sprintf("container restarted %d times", cs.RestartCount), nil
			}
		}
		for _, cond := range p.Status.Conditions {
			if cond.Type == corev1.PodScheduled && cond.Status == corev1.ConditionFalse {
				return false, clip("not scheduled: " + cond.Message), nil
			}
		}
	}
	return false, fmt.Sprintf("%d/1 pods ready", d.Status.ReadyReplicas), nil
}

// clip keeps a problem message short enough for an annotation and a table.
func clip(s string) string {
	s = strings.TrimSuffix(strings.TrimSpace(s), ":")
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}
