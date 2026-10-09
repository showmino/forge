// Package envtest holds fakes for preview environment tests: a Kubernetes
// clientset with forge-api's pull secret, a dynamic client that knows
// HTTPRoutes, GitHub and a registry. Only tests import it.
package envtest

import (
	"context"
	"encoding/base64"
	"fmt"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/showmino/forge/internal/github"
)

// HTTPRoutes is the Gateway API HTTPRoute resource.
var HTTPRoutes = schema.GroupVersionResource{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "httproutes"}

// PullSecretJSON is the .dockerconfigjson of the fake pull secret forge/mino-ghcr.
var PullSecretJSON = []byte(`{"auths":{"ghcr.io":{"auth":"` + base64.StdEncoding.EncodeToString([]byte("bot:s3cret")) + `"}}}`)

// Kube returns a fake clientset holding objs and the pull secret forge/mino-ghcr.
func Kube(objs ...runtime.Object) *fake.Clientset {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "mino-ghcr", Namespace: "forge"},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data:       map[string][]byte{corev1.DockerConfigJsonKey: PullSecretJSON},
	}
	return fake.NewClientset(append([]runtime.Object{secret}, objs...)...)
}

// Dynamic returns a fake dynamic client that knows HTTPRoutes.
func Dynamic() *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{HTTPRoutes: "HTTPRouteList"})
}

// GitHub is an in-memory GitHub: refs, files and Actions runs.
type GitHub struct {
	mu sync.Mutex
	// Refs maps a ref to its commit SHA.
	Refs map[string]string
	// Files maps "<sha>:<path>" to content.
	Files map[string][]byte
	// Runs maps a SHA to its runs, newest first.
	Runs map[string][]github.WorkflowRun
}

func (g *GitHub) ResolveRef(_ context.Context, _, _, ref string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if sha, ok := g.Refs[ref]; ok {
		return sha, nil
	}
	return "", fmt.Errorf("ref %s: %w", ref, github.ErrNotFound)
}

func (g *GitHub) FileAt(_ context.Context, _, _, path, ref string) ([]byte, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if data, ok := g.Files[ref+":"+path]; ok {
		return data, nil
	}
	return nil, fmt.Errorf("%s at %s: %w", path, ref, github.ErrNotFound)
}

func (g *GitHub) RunsFor(_ context.Context, _, _, sha string) ([]github.WorkflowRun, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]github.WorkflowRun(nil), g.Runs[sha]...), nil
}

// SetRuns replaces the runs of sha.
func (g *GitHub) SetRuns(sha string, runs []github.WorkflowRun) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.Runs == nil {
		g.Runs = map[string][]github.WorkflowRun{}
	}
	g.Runs[sha] = runs
}

// Images is an in-memory registry.
type Images struct {
	mu      sync.Mutex
	present map[string]bool
}

// Add makes image exist.
func (i *Images) Add(image string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.present == nil {
		i.present = map[string]bool{}
	}
	i.present[image] = true
}

func (i *Images) Exists(_ context.Context, image string) (bool, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.present[image], nil
}

// MarkAvailable plays the Deployment controller until ctx ends: every
// Deployment outside the namespaces skip selects becomes available.
func MarkAvailable(ctx context.Context, kube kubernetes.Interface, skip func(namespace string) bool) {
	t := time.NewTicker(5 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		list, err := kube.AppsV1().Deployments("").List(ctx, metav1.ListOptions{})
		if err != nil {
			continue
		}
		for i := range list.Items {
			d := &list.Items[i]
			if d.Status.AvailableReplicas > 0 || (skip != nil && skip(d.Namespace)) {
				continue
			}
			d.Status.AvailableReplicas, d.Status.ReadyReplicas = 1, 1
			_, _ = kube.AppsV1().Deployments(d.Namespace).UpdateStatus(ctx, d, metav1.UpdateOptions{})
		}
	}
}
