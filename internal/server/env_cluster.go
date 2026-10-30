package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
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

	forgev1 "github.com/showmino/forge/gen/forge/v1"
)

// Labels and annotations on an environment's namespace. The namespace is
// the environment's only record, so it survives forge-api restarts.
const (
	envPrefix        = "preview-"
	labelManagedBy   = "app.kubernetes.io/managed-by"
	labelEnvironment = "forge.dev/environment"
	labelService     = "forge.dev/service"
	labelDeployEnv   = "forge.dev/deploy-env"
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
	errNoEnv = errors.New("no such environment")
	// errUnlabelled is errNoEnv for a namespace that exists but is not Forge's.
	errUnlabelled = fmt.Errorf("%w: namespace lacks Forge's labels", errNoEnv)
	errEnvExists  = errors.New("environment already exists")
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

// EnvWorkload is what an environment runs.
type EnvWorkload struct {
	Service, Image, Host string
	Port                 int32
	Health               string
	// Writable are forge.yaml's run.writable: folders besides /tmp that get
	// an empty folder of their own.
	Writable []string
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
func (c *Cluster) Deploy(ctx context.Context, id string, w EnvWorkload) error {
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

func deployment(w EnvWorkload) *appsv1.Deployment {
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
	tmpLimit := resource.MustParse("256Mi")
	volumes := []corev1.Volume{{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &tmpLimit}}}}
	mounts := []corev1.VolumeMount{{Name: "tmp", MountPath: "/tmp"}}
	for i, p := range w.Writable {
		name := "writable-" + strconv.Itoa(i)
		volumes = append(volumes, corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &tmpLimit}}})
		mounts = append(mounts, corev1.VolumeMount{Name: name, MountPath: p})
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: w.Service, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: &one,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					ImagePullSecrets:             []corev1.LocalObjectReference{{Name: pullSecretName}},
					AutomountServiceAccountToken: &no,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot:   &yes,
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					// The root filesystem is read-only; /tmp and run.writable are
					// the writable paths.
					Volumes: volumes,
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
						VolumeMounts: mounts,
					}},
				},
			},
		},
	}
}

// Status reports whether the service's Deployment in environment id is
// available and, when it is not, the most telling problem seen.
func (c *Cluster) Status(ctx context.Context, id, service string) (bool, string, error) {
	if _, err := c.Env(ctx, id); err != nil {
		return false, "", err
	}
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
	problem, err := c.podProblem(ctx, ns, service)
	if err != nil {
		return false, "", err
	}
	if problem != "" {
		return false, problem, nil
	}
	return false, fmt.Sprintf("%d/1 pods ready", d.Status.ReadyReplicas), nil
}

// podProblem scans service's pods in ns for why they are not running: a
// container waiting for a reason, restarts, or an unschedulable pod. It is
// "" when none shows a problem.
func (c *Cluster) podProblem(ctx context.Context, ns, service string) (string, error) {
	pods, err := c.Kube.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: "app=" + service})
	if err != nil {
		return "", err
	}
	for _, p := range pods.Items {
		for _, cs := range p.Status.ContainerStatuses {
			if w := cs.State.Waiting; w != nil && w.Reason != "" && w.Reason != "ContainerCreating" {
				return clip(w.Reason + ": " + w.Message), nil
			}
			if cs.RestartCount > 0 {
				return fmt.Sprintf("container restarted %d times", cs.RestartCount), nil
			}
		}
		for _, cond := range p.Status.Conditions {
			if cond.Type == corev1.PodScheduled && cond.Status == corev1.ConditionFalse {
				return clip("not scheduled: " + cond.Message), nil
			}
		}
	}
	return "", nil
}

// ServicePodProblem is why the pods of a deployed service are not running
// ("" when none shows a problem). It only looks into Forge's own namespace
// <service>-<env>.
func (c *Cluster) ServicePodProblem(ctx context.Context, service, env string) (string, error) {
	if err := c.checkServiceNamespace(ctx, service, env); err != nil {
		return "", err
	}
	return c.podProblem(ctx, service+"-"+env, service)
}

// clip keeps a problem message short enough for an annotation and a table.
func clip(s string) string {
	s = strings.TrimSuffix(strings.TrimSpace(s), ":")
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}

var applications = schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "applications"}

// AppStatus is what Argo CD reports for one Application.
type AppStatus struct {
	Sync, Health, Message string
	// Revisions are the synced revisions, one per source.
	Revisions []string
	// Refused is what Argo CD refused to sync in its last sync, which failed,
	// and RefusedAt that sync's revisions: an object the project does not
	// allow is named per resource, not in the sync's own message.
	Refused   string
	RefusedAt []string
}

// Application reads Argo CD's status for name in argoNamespace. A missing
// Application (the ApplicationSet has not generated it yet) is Missing.
func (c *Cluster) Application(ctx context.Context, argoNamespace, name string) (AppStatus, error) {
	u, err := c.Dynamic.Resource(applications).Namespace(argoNamespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return AppStatus{Sync: "Unknown", Health: "Missing"}, nil
	}
	if err != nil {
		return AppStatus{}, err
	}
	st := AppStatus{}
	st.Sync, _, _ = unstructured.NestedString(u.Object, "status", "sync", "status")
	st.Health, _, _ = unstructured.NestedString(u.Object, "status", "health", "status")
	st.Revisions, _, _ = unstructured.NestedStringSlice(u.Object, "status", "sync", "revisions")
	if rev, _, _ := unstructured.NestedString(u.Object, "status", "sync", "revision"); rev != "" {
		st.Revisions = append(st.Revisions, rev)
	}
	var msgs []string
	conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, cond := range conds {
		cm, _ := cond.(map[string]any)
		if m, ok := cm["message"].(string); ok && m != "" {
			msgs = append(msgs, m)
		}
	}
	if m, _, _ := unstructured.NestedString(u.Object, "status", "health", "message"); m != "" {
		msgs = append(msgs, m)
	}
	// Argo CD 3.x leaves health.message empty: a failed sync explains itself
	// in operationState, an unhealthy resource in its own health.
	if phase, _, _ := unstructured.NestedString(u.Object, "status", "operationState", "phase"); phase == "Failed" || phase == "Error" {
		if m, _, _ := unstructured.NestedString(u.Object, "status", "operationState", "message"); m != "" {
			msgs = append(msgs, m)
		}
		var refused []string
		rs, _, _ := unstructured.NestedSlice(u.Object, "status", "operationState", "syncResult", "resources")
		for _, r := range rs {
			rm, _ := r.(map[string]any)
			if status, _ := rm["status"].(string); status != "SyncFailed" {
				continue
			}
			if m, _ := rm["message"].(string); m != "" {
				refused = append(refused, m)
			}
		}
		if len(refused) > 0 {
			st.Refused = strings.Join(refused, "; ")
			msgs = append(msgs, st.Refused)
			st.RefusedAt, _, _ = unstructured.NestedStringSlice(u.Object, "status", "operationState", "syncResult", "revisions")
			if rev, _, _ := unstructured.NestedString(u.Object, "status", "operationState", "syncResult", "revision"); rev != "" {
				st.RefusedAt = append(st.RefusedAt, rev)
			}
		}
	}
	res, _, _ := unstructured.NestedSlice(u.Object, "status", "resources")
	for _, r := range res {
		rm, _ := r.(map[string]any)
		h, _ := rm["health"].(map[string]any)
		if hs, _ := h["status"].(string); hs == "" || hs == "Healthy" {
			continue
		}
		if m, _ := h["message"].(string); m != "" {
			kind, _ := rm["kind"].(string)
			name, _ := rm["name"].(string)
			msgs = append(msgs, strings.TrimSpace(kind+" "+name)+": "+m)
		}
	}
	st.Message = clip(strings.Join(msgs, "; "))
	if st.Sync == "" {
		st.Sync = "Unknown"
	}
	if st.Health == "" {
		st.Health = "Unknown"
	}
	return st, nil
}

// checkServiceNamespace returns errNoEnv unless Argo CD made <service>-<env> for Forge.
func (c *Cluster) checkServiceNamespace(ctx context.Context, service, env string) error {
	ns, err := c.Kube.CoreV1().Namespaces().Get(ctx, service+"-"+env, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return errNoEnv
	}
	if err != nil {
		return err
	}
	l := ns.Labels
	if l[labelManagedBy] != "forge" || l[labelService] != service || l[labelDeployEnv] != env {
		return errUnlabelled
	}
	return nil
}

// ServiceNamespaceFree reports whether Forge may deploy into <service>-<env>:
// it must not exist yet, or be one Argo CD made for Forge. On the one
// production cluster, a namespace with that name that isn't Forge's is
// someone else's, and Argo CD would otherwise take it over.
func (c *Cluster) ServiceNamespaceFree(ctx context.Context, service, env string) error {
	err := c.checkServiceNamespace(ctx, service, env)
	switch {
	case err == nil, errors.Is(err, errNoEnv) && !errors.Is(err, errUnlabelled):
		return nil
	case errors.Is(err, errUnlabelled):
		return connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("namespace %s-%s exists and is not Forge's (it lacks Forge's labels); pick another service name or remove that namespace", service, env))
	default:
		return connect.NewError(connect.CodeUnavailable, fmt.Errorf("check namespace %s-%s: %w", service, env, err))
	}
}

// EnsureServiceNamespace makes <service>-<env> when it is missing, with the
// labels Argo CD's ApplicationSet gives it, so a database can exist before
// the first deploy and Argo CD finds the namespace as it expects it. A
// namespace of that name that is not Forge's is refused.
func (c *Cluster) EnsureServiceNamespace(ctx context.Context, service, env string) error {
	if err := c.ServiceNamespaceFree(ctx, service, env); err != nil {
		return err
	}
	_, err := c.Kube.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   service + "-" + env,
		Labels: map[string]string{labelManagedBy: "forge", labelService: service, labelDeployEnv: env},
	}}, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

// ForgeApplicationExists reports whether Argo CD has an Application of that
// name that is Forge's (the ApplicationSet labels them). One that is not
// Forge's is someone else's: it does not count and is never waited for.
func (c *Cluster) ForgeApplicationExists(ctx context.Context, argoNamespace, name string) (bool, error) {
	app, err := c.Dynamic.Resource(applications).Namespace(argoNamespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return app.GetLabels()[labelManagedBy] == "forge", nil
}

// DeleteServiceNamespace starts deleting <service>-<env>. A namespace that is
// already gone is fine; one that is not Forge's is refused with
// errUnlabelled and never touched. The delete names the namespace it looked
// at by UID, so a namespace recreated in between is not the one deleted.
func (c *Cluster) DeleteServiceNamespace(ctx context.Context, service, env string) error {
	ns, err := c.Kube.CoreV1().Namespaces().Get(ctx, service+"-"+env, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if l := ns.Labels; l[labelManagedBy] != "forge" || l[labelService] != service || l[labelDeployEnv] != env {
		return errUnlabelled
	}
	err = c.Kube.CoreV1().Namespaces().Delete(ctx, ns.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &ns.UID}})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

var cnpgClusters = schema.GroupVersionResource{Group: "postgresql.cnpg.io", Version: "v1", Resource: "clusters"}

// DatabaseHealth reads what CloudNativePG reports for the database called
// name in <service>-<env>. It gives up after five seconds, so a slow API
// server cannot stall a list of resources.
func (c *Cluster) DatabaseHealth(ctx context.Context, service, env, name string) (*forgev1.ResourceHealth, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	u, err := c.Dynamic.Resource(cnpgClusters).Namespace(service+"-"+env).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	phase, _, _ := unstructured.NestedString(u.Object, "status", "phase")
	if phase == "" {
		phase = "unknown"
	}
	ready, _, _ := unstructured.NestedInt64(u.Object, "status", "readyInstances")
	want, _, _ := unstructured.NestedInt64(u.Object, "spec", "instances")
	return &forgev1.ResourceHealth{Phase: phase, ReadyInstances: int32(ready), Instances: int32(want)}, nil //nolint:gosec // instance counts are small
}

// The cluster is what resources check namespaces and health with.
var (
	_ ServiceNamespaces = (*Cluster)(nil)
	_ DatabaseHealth    = (*Cluster)(nil)
)

// ApplicationFree reports whether Forge may use the Argo CD Application name:
// it must not exist yet, or be one the ApplicationSet made (labelled
// managed-by forge). Otherwise forge-api would report a foreign
// Application's status and time out waiting for its own.
func (c *Cluster) ApplicationFree(ctx context.Context, argoNamespace, name string) error {
	u, err := c.Dynamic.Resource(applications).Namespace(argoNamespace).Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return nil
	case err != nil:
		return connect.NewError(connect.CodeUnavailable, fmt.Errorf("check Argo CD Application %s/%s: %w", argoNamespace, name, err))
	case u.GetLabels()[labelManagedBy] == "forge":
		return nil
	}
	return connect.NewError(connect.CodeFailedPrecondition,
		fmt.Errorf("Argo CD Application %s/%s exists and is not Forge's", argoNamespace, name)) //nolint:staticcheck // Argo CD is a proper noun
}

// WaitServiceNamespace waits until Argo CD has created <service>-<env>.
func (c *Cluster) WaitServiceNamespace(ctx context.Context, service, env string, poll time.Duration) error {
	for {
		err := c.checkServiceNamespace(ctx, service, env)
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			ns := service + "-" + env
			switch {
			case errors.Is(err, errUnlabelled):
				return fmt.Errorf("namespace %s exists without Forge's labels: %w", ns, ctx.Err())
			case !errors.Is(err, errNoEnv):
				return fmt.Errorf("namespace %s: %w (%w)", ns, err, ctx.Err())
			}
			return fmt.Errorf("namespace %s did not appear: %w", ns, ctx.Err())
		case <-time.After(poll):
		}
	}
}

// Pod Security Admission levels of a namespace that runs a published chart:
// enforced, pods may not be privileged, share the node's network, PIDs or
// files, or add capabilities beyond the defaults; Kubernetes warns about
// anything short of the restricted level.
const (
	podSecurityEnforce = "pod-security.kubernetes.io/enforce"
	podSecurityWarn    = "pod-security.kubernetes.io/warn"
)

// RestrictPodSecurity labels <service>-<env>, which must be Forge's, so
// that Kubernetes refuses privileged pods in it: a chart's pods are not
// Forge's, and run as the chart says.
func (c *Cluster) RestrictPodSecurity(ctx context.Context, service, env string) error {
	if err := c.checkServiceNamespace(ctx, service, env); err != nil {
		return err
	}
	patch := []byte(`{"metadata":{"labels":{"` + podSecurityEnforce + `":"baseline","` + podSecurityWarn + `":"restricted"}}}`)
	_, err := c.Kube.CoreV1().Namespaces().Patch(ctx, service+"-"+env, types.MergePatchType, patch, metav1.PatchOptions{})
	return err
}

// WriteServicePullSecret copies the pull secret into <service>-<env>.
func (c *Cluster) WriteServicePullSecret(ctx context.Context, service, env string) error {
	if err := c.checkServiceNamespace(ctx, service, env); err != nil {
		return err
	}
	pull, err := c.PullSecretData(ctx)
	if err != nil {
		return err
	}
	_, err = c.Kube.CoreV1().Secrets(service+"-"+env).Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: pullSecretName},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data:       map[string][]byte{corev1.DockerConfigJsonKey: pull},
	}, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}
