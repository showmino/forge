// Package envtest holds fakes for preview environment tests: a Kubernetes
// clientset with forge-api's pull secret, a dynamic client that knows
// HTTPRoutes, GitHub and a registry. Only tests import it.
package envtest

import (
	"context"
	"encoding/base64"
	"fmt"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/showmino/forge/internal/github"
)

// HTTPRoutes is the Gateway API HTTPRoute resource.
var HTTPRoutes = schema.GroupVersionResource{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "httproutes"}

// Applications is the Argo CD Application resource.
var Applications = schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "applications"}

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

// Dynamic returns a fake dynamic client that knows HTTPRoutes and Argo CD Applications.
func Dynamic() *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{HTTPRoutes: "HTTPRouteList", Applications: "ApplicationList"})
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

type repoCommit struct {
	sha     string
	files   map[string][]byte
	message string
	time    time.Time
}

// Repo is an in-memory git repo with one branch, shaped like
// github.RepoFiles. Commit n has SHA %040x of n; commit 0 is empty. It is
// safe for concurrent use.
type Repo struct {
	mu      sync.Mutex
	commits []repoCommit // commits[0] is the empty initial state, added lazily
	// BeforeCommit, when set, runs once before the next CommitFile, so a test
	// can push as a racing writer. It runs without the repo's lock held.
	BeforeCommit func()
}

func (r *Repo) init() {
	if len(r.commits) == 0 {
		r.commits = []repoCommit{{sha: fmt.Sprintf("%040x", 0), files: map[string][]byte{}}}
	}
}

func (r *Repo) find(ref string) (repoCommit, bool) {
	r.init()
	if ref == "" || ref == "main" {
		return r.commits[len(r.commits)-1], true
	}
	for _, c := range r.commits {
		if c.sha == ref {
			return c, true
		}
	}
	return repoCommit{}, false
}

func (r *Repo) push(p string, content []byte, message string) string {
	r.init()
	head := r.commits[len(r.commits)-1]
	files := make(map[string][]byte, len(head.files)+1)
	for k, v := range head.files {
		files[k] = v
	}
	files[p] = append([]byte(nil), content...)
	sha := fmt.Sprintf("%040x", len(r.commits))
	r.commits = append(r.commits, repoCommit{sha: sha, files: files, message: message, time: time.Now()})
	return sha
}

// Read returns path at ref ("" or "main" is the head).
func (r *Repo) Read(_ context.Context, p, ref string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.find(ref)
	if !ok {
		return nil, fmt.Errorf("commit %s: %w", ref, github.ErrNotFound)
	}
	data, ok := c.files[p]
	if !ok {
		return nil, fmt.Errorf("%s at %s: %w", p, ref, github.ErrNotFound)
	}
	return append([]byte(nil), data...), nil
}

// Head returns the head commit and its tree, which is the same id.
func (r *Repo) Head(_ context.Context) (string, string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, _ := r.find("")
	return c.sha, c.sha, nil
}

// CommitFile commits path on parent, which must be the head.
func (r *Repo) CommitFile(_ context.Context, parent, _, p string, content []byte, message string) (string, error) {
	r.mu.Lock()
	hook := r.BeforeCommit
	r.BeforeCommit = nil
	r.mu.Unlock()
	if hook != nil {
		hook()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if head, _ := r.find(""); head.sha != parent {
		return "", github.ErrNotFastForward
	}
	return r.push(p, content, message), nil
}

// History returns the commits that changed path, newest first.
func (r *Repo) History(_ context.Context, p string, limit int) ([]github.CommitInfo, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.init()
	var out []github.CommitInfo
	for i := len(r.commits) - 1; i > 0 && len(out) < limit; i-- {
		cur, hasCur := r.commits[i].files[p]
		prev, hasPrev := r.commits[i-1].files[p]
		if hasCur && (!hasPrev || string(cur) != string(prev)) {
			out = append(out, github.CommitInfo{SHA: r.commits[i].sha, Message: r.commits[i].message, Time: r.commits[i].time})
		}
	}
	return out, nil
}

// Push commits path on the head as an outside writer and returns the commit.
func (r *Repo) Push(p string, content []byte, message string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.push(p, content, message)
}

// files returns the head commit and a snapshot of its paths.
func (r *Repo) files() (string, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, _ := r.find("")
	paths := make([]string, 0, len(c.files))
	for p := range c.files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return c.sha, paths
}

// Argo plays Argo CD until Run's ctx ends: for every
// services/<svc>/<env>/values.yaml at the repo head it makes namespace
// <svc>-<env> and, Lag after first seeing that head, marks Application
// <svc>-<env> synced to it.
type Argo struct {
	Kube      kubernetes.Interface
	Dynamic   dynamic.Interface
	Repo      *Repo
	Namespace string
	// Unhealthy maps "<svc>-<env>" to the message of its Degraded health.
	Unhealthy map[string]string
	Lag       time.Duration
}

// Run blocks until ctx ends.
func (a *Argo) Run(ctx context.Context) {
	t := time.NewTicker(5 * time.Millisecond)
	defer t.Stop()
	seen := map[string]time.Time{} // head -> first seen
	synced := map[string]string{}  // app -> head last written
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		head, paths := a.Repo.files()
		if _, ok := seen[head]; !ok {
			seen[head] = time.Now()
		}
		for _, p := range paths {
			parts := strings.Split(p, "/")
			if len(parts) != 4 || parts[0] != "services" || path.Base(p) != "values.yaml" {
				continue
			}
			svc, env := parts[1], parts[2]
			name := svc + "-" + env
			a.ensureNamespace(ctx, name, svc, env)
			if time.Since(seen[head]) < a.Lag || synced[name] == head {
				continue
			}
			if a.sync(ctx, name, head) == nil {
				synced[name] = head
			}
		}
	}
}

func (a *Argo) ensureNamespace(ctx context.Context, name, svc, env string) {
	_, err := a.Kube.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: name,
		Labels: map[string]string{
			"app.kubernetes.io/managed-by": "forge",
			"forge.dev/service":            svc,
			"forge.dev/deploy-env":         env,
		},
	}}, metav1.CreateOptions{})
	_ = err // AlreadyExists is the steady state
}

func (a *Argo) sync(ctx context.Context, name, head string) error {
	health := map[string]any{"status": "Healthy"}
	if msg, bad := a.Unhealthy[name]; bad {
		health = map[string]any{"status": "Degraded", "message": msg}
	}
	status := map[string]any{
		"sync":   map[string]any{"status": "Synced", "revisions": []any{"chart", head}},
		"health": health,
	}
	apps := a.Dynamic.Resource(Applications).Namespace(a.Namespace)
	app := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "argoproj.io/v1alpha1", "kind": "Application",
		"metadata": map[string]any{"name": name, "namespace": a.Namespace},
		"status":   status,
	}}
	_, err := apps.Create(ctx, app, metav1.CreateOptions{})
	if !apierrors.IsAlreadyExists(err) {
		return err
	}
	cur, err := apps.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	cur.Object["status"] = status
	_, err = apps.Update(ctx, cur, metav1.UpdateOptions{})
	return err
}
