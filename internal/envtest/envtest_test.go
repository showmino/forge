package envtest

import (
	"context"
	"errors"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/showmino/forge/internal/github"
)

func TestRepo(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := &Repo{}
	if _, err := r.Read(ctx, "a", ""); !errors.Is(err, github.ErrNotFound) {
		t.Errorf("empty read: %v", err)
	}
	head, tree, _ := r.Head(ctx)
	c1, err := r.CommitFile(ctx, head, tree, "a", []byte("1"), "one")
	if err != nil {
		t.Fatal(err)
	}
	r.Push("b", []byte("x"), "outside")
	if _, err := r.CommitFile(ctx, c1, tree, "a", []byte("2"), "two"); !errors.Is(err, github.ErrNotFastForward) {
		t.Errorf("stale parent: %v", err)
	}
	head, tree, _ = r.Head(ctx)
	c3, _ := r.CommitFile(ctx, head, tree, "a", []byte("3"), "three")
	if got, _ := r.Read(ctx, "a", ""); string(got) != "3" {
		t.Errorf("head read %q", got)
	}
	if got, _ := r.Read(ctx, "a", c1); string(got) != "1" {
		t.Errorf("read at c1 %q", got)
	}
	if got, _ := r.Read(ctx, "b", c3); string(got) != "x" {
		t.Errorf("b kept: %q", got)
	}
	hist, _ := r.History(ctx, "a", 10)
	if len(hist) != 2 || hist[0].SHA != c3 || hist[1].SHA != c1 || hist[0].Message != "three" {
		t.Errorf("history %+v", hist)
	}
}

func TestArgo(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	kube, dyn, repo := Kube(), Dynamic(), &Repo{}
	a := &Argo{Kube: kube, Dynamic: dyn, Repo: repo, Namespace: "argocd", Unhealthy: map[string]string{"bad-staging": "ImagePullBackOff"}}
	go a.Run(ctx)
	repo.Push("services/payments/staging/values.yaml", []byte("image: x\n"), "deploy")
	repo.Push("services/bad/staging/values.yaml", []byte("image: y\n"), "deploy")
	deadline := time.Now().Add(5 * time.Second)
	for {
		app, err := dyn.Resource(Applications).Namespace("argocd").Get(ctx, "payments-staging", metav1.GetOptions{})
		bad, berr := dyn.Resource(Applications).Namespace("argocd").Get(ctx, "bad-staging", metav1.GetOptions{})
		if err == nil && berr == nil {
			revs, _, _ := unstructured.NestedStringSlice(app.Object, "status", "sync", "revisions")
			health, _, _ := unstructured.NestedString(bad.Object, "status", "health", "status")
			if len(revs) == 2 && health == "Degraded" {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("argo never synced")
		}
		time.Sleep(5 * time.Millisecond)
	}
	pod, err := kube.CoreV1().Pods("bad-staging").Get(ctx, "bad-0", metav1.GetOptions{})
	if err != nil || pod.Labels["app"] != "bad" || len(pod.Status.ContainerStatuses) != 1 || pod.Status.ContainerStatuses[0].State.Waiting.Reason != "ImagePullBackOff" {
		t.Errorf("unhealthy pod: %v %v", pod, err)
	}
	bad, _ := dyn.Resource(Applications).Namespace("argocd").Get(ctx, "bad-staging", metav1.GetOptions{})
	if m, _, _ := unstructured.NestedString(bad.Object, "status", "health", "message"); m != "" {
		t.Errorf("health.message %q: Argo CD 3.x leaves it empty", m)
	}
	ns, err := kube.CoreV1().Namespaces().Get(ctx, "payments-staging", metav1.GetOptions{})
	if err != nil || ns.Labels["app.kubernetes.io/managed-by"] != "forge" || ns.Labels["forge.dev/deploy-env"] != "staging" || ns.Labels["forge.dev/service"] != "payments" {
		t.Errorf("namespace: %v %v", ns, err)
	}
}

func TestArgoLag(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dyn, repo := Dynamic(), &Repo{}
	a := &Argo{Kube: Kube(), Dynamic: dyn, Repo: repo, Namespace: "argocd", Lag: 300 * time.Millisecond}
	go a.Run(ctx)
	revs := func() []string {
		app, err := dyn.Resource(Applications).Namespace("argocd").Get(ctx, "payments-staging", metav1.GetOptions{})
		if err != nil {
			return nil
		}
		r, _, _ := unstructured.NestedStringSlice(app.Object, "status", "sync", "revisions")
		return r
	}
	head := repo.Push("services/payments/staging/values.yaml", []byte("image: x\n"), "one")
	start := time.Now()
	time.Sleep(100 * time.Millisecond)
	if r := revs(); r != nil {
		t.Errorf("before the lag the app is absent, got %v", r)
	}
	for revs() == nil {
		if time.Since(start) > 5*time.Second {
			t.Fatal("never synced")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if time.Since(start) < 300*time.Millisecond {
		t.Errorf("synced after %v, before the lag", time.Since(start))
	}
	if r := revs(); len(r) != 2 || r[1] != head {
		t.Errorf("revisions %v, want head %s", r, head)
	}

	// A new head: until the lag passes the app stays on the old one.
	a.SetLag(300 * time.Millisecond)
	head2 := repo.Push("services/payments/staging/values.yaml", []byte("image: y\n"), "two")
	time.Sleep(100 * time.Millisecond)
	if r := revs(); len(r) != 2 || r[1] != head {
		t.Errorf("during the lag: %v, want old head %s", r, head)
	}
	for r := revs(); r[1] != head2; r = revs() {
		if time.Since(start) > 5*time.Second {
			t.Fatal("never synced the new head")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
