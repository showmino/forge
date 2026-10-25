package server

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/gen/forge/v1/forgev1connect"
	"github.com/showmino/forge/internal/envtest"
	"github.com/showmino/forge/internal/github"
	"github.com/showmino/forge/policy"
)

// deployFixture is forge-api with deploys on fakes: service "payments"
// (repo showmino/payments, run port 9090 health /ready), main at envSHA,
// its image present, and envtest.Argo playing Argo CD unless noArgo.
type deployFixture struct {
	opsClient forgev1connect.OperationServiceClient
	cluster   *Cluster
	repo      *envtest.Repo
	images    *envtest.Images
	ops       *Operations
	deploys   *Deploys
	client    forgev1connect.DeployServiceClient
	argo      *envtest.Argo
	catalog   *Catalog
}

func newDeployFixture(t *testing.T, noArgo bool, tweak func(*DeployOptions, *envtest.Argo)) *deployFixture {
	t.Helper()
	return newDeployFixtureAuth(t, noArgo, tweak, nil)
}

// newDeployFixtureAuth is newDeployFixture with the authenticator replaced by
// wrap(catalog, the static one) when wrap is set.
func newDeployFixtureAuth(t *testing.T, noArgo bool, tweak func(*DeployOptions, *envtest.Argo), wrap func(*Catalog, Authenticator) Authenticator) *deployFixture {
	t.Helper()
	f := &deployFixture{
		cluster: &Cluster{Kube: envtest.Kube(), Dynamic: envtest.Dynamic(), Gateway: "forge/forge-previews", PullSecret: "forge/mino-ghcr"},
		repo:    &envtest.Repo{},
		images:  &envtest.Images{},
		ops:     NewOperations(nil),
	}
	f.images.Add("ghcr.io/showmino/payments:1a2b3c4")
	f.images.Add("ghcr.io/showmino/payments:9f8e7d6")
	manifest := []byte("apiVersion: forge.dev/v1\nservice:\n  name: payments\n  team: billing\nrun:\n  port: 9090\n  health: /ready\n")
	gh := &envtest.GitHub{
		// "release" points at a commit main does not.
		Refs:  map[string]string{"main": envSHA, "release": ciOldSHA},
		Files: map[string][]byte{envSHA + ":forge.yaml": manifest, ciOldSHA + ":forge.yaml": manifest},
	}
	catalog := NewCatalog(nil)
	f.catalog = catalog
	if _, err := catalog.RegisterService(writerCtx(), connect.NewRequest(&forgev1.RegisterServiceRequest{
		ServiceId: "payments", Service: &forgev1.Service{OwnerTeam: "billing", RepoUrl: "https://github.com/showmino/payments"},
	})); err != nil {
		t.Fatal(err)
	}
	rules, err := policy.New(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	o := DeployOptions{
		Policy:  rules,
		Cluster: f.cluster, GitHub: gh, Images: f.images, Repo: f.repo, Catalog: catalog, Operations: f.ops,
		Org: "showmino", Domain: "10.150.1.222.nip.io",
		ImagePoll: 5 * time.Millisecond, Poll: 5 * time.Millisecond,
		ImageTimeout: 5 * time.Second, SyncTimeout: 5 * time.Second, NamespaceTimeout: 5 * time.Second,
	}
	f.argo = &envtest.Argo{Kube: f.cluster.Kube, Dynamic: f.cluster.Dynamic, Repo: f.repo, Namespace: "argocd", Unhealthy: map[string]string{}}
	if tweak != nil {
		tweak(&o, f.argo)
	}
	f.deploys = NewDeploys(o)
	if !noArgo {
		ctx, cancel := context.WithCancel(context.Background())
		var wg sync.WaitGroup
		wg.Go(func() { f.argo.Run(ctx) })
		t.Cleanup(func() { cancel(); wg.Wait() })
	}
	var auth Authenticator = StaticAuthenticator{
		"writer": {Subject: "ana", Email: "ana@example.com", Groups: []string{"platform"}},
		"reader": {Subject: "bo", Groups: []string{"dev"}},
		"ci": {
			Subject: "github:showmino/payments", Permissions: []string{PermDeploy("payments", "staging")},
			Workload: &Workload{Repository: "showmino/payments", Service: "payments", SHA: envSHA},
		},
		"ci-billing": {
			Subject: "github:showmino/billing", Permissions: []string{PermDeploy("billing", "staging")},
			Workload: &Workload{Repository: "showmino/billing", Service: "billing", SHA: envSHA},
		},
		"ci-old": {
			Subject: "github:showmino/payments", Permissions: []string{PermDeploy("payments", "staging")},
			Workload: &Workload{Repository: "showmino/payments", Service: "payments", SHA: ciOldSHA},
		},
	}
	if wrap != nil {
		auth = wrap(catalog, auth)
	}
	h, err := New(Options{
		Authenticator: auth,
		Authorizer:    Authorizer{WriterGroups: []string{"platform"}},
		Catalog:       catalog, Operations: f.ops, Deploys: f.deploys,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	f.client = forgev1connect.NewDeployServiceClient(srv.Client(), srv.URL)
	f.opsClient = forgev1connect.NewOperationServiceClient(srv.Client(), srv.URL)
	return f
}

func (f *deployFixture) start(t *testing.T, env string) (*forgev1.StartDeployResponse, error) {
	t.Helper()
	resp, err := f.client.StartDeploy(context.Background(), as("writer", &forgev1.StartDeployRequest{Service: "services/payments", Env: env}))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func TestStartDeploy(t *testing.T) {
	t.Parallel()
	f := newDeployFixture(t, false, nil)
	resp, err := f.start(t, "staging")
	if err != nil {
		t.Fatal(err)
	}
	if d := resp.GetDeployment(); d.GetName() != "deployments/payments/staging" || d.GetImage() != "ghcr.io/showmino/payments:1a2b3c4" || d.GetUrl() != "http://payments.staging.10.150.1.222.nip.io" {
		t.Errorf("preview: %v", d)
	}
	op := wait(t, f.ops, resp.GetOperation().GetName())
	if op.GetError() != nil {
		t.Fatalf("op: %v", op.GetError())
	}
	if got := eventSteps(op); got != "resolve,image,commit,namespace,sync" {
		t.Errorf("steps %s", got)
	}
	data, err := f.repo.Read(context.Background(), "services/payments/staging/values.yaml", "")
	if err != nil {
		t.Fatal(err)
	}
	v, err := parseValues(data)
	if err != nil || v.Name != "payments" || v.Env != "staging" || v.Port != 9090 || v.Health != "/ready" || v.Replicas != 1 ||
		v.Host != "payments.staging.10.150.1.222.nip.io" || v.Forge.Kind != "deploy" || v.Forge.Commit != envSHA ||
		v.Forge.Actor != "ana@example.com" || v.Forge.Operation != op.GetName() {
		t.Errorf("values: %+v %v", v, err)
	}
	hist, _ := f.repo.History(context.Background(), "services/payments/staging/values.yaml", 1)
	rec, ok := parseRecord(hist[0].SHA, hist[0].Message, hist[0].Time)
	if !ok || !strings.HasPrefix(hist[0].Message, "deploy payments to staging: ghcr.io/showmino/payments:1a2b3c4 (ana@example.com)\n\n") || rec.Image != v.Image || rec.Operation != op.GetName() {
		t.Errorf("commit message %q -> %+v", hist[0].Message, rec)
	}
	if _, err := f.cluster.Kube.CoreV1().Secrets("payments-staging").Get(context.Background(), "forge-pull", metav1.GetOptions{}); err != nil {
		t.Errorf("pull secret: %v", err)
	}

	got, err := f.client.GetDeployment(context.Background(), as("reader", &forgev1.GetDeploymentRequest{Name: "deployments/payments/staging"}))
	if err != nil {
		t.Fatal(err)
	}
	d := got.Msg.GetDeployment()
	if d.GetSync() != forgev1.Deployment_SYNC_STATUS_SYNCED || d.GetHealth() != forgev1.Deployment_HEALTH_HEALTHY || d.GetCommit() != envSHA ||
		d.GetDeployCommit() != hist[0].SHA || d.GetDeployedBy() != "ana@example.com" || d.GetOperation() != op.GetName() {
		t.Errorf("get: %v", d)
	}
	list, err := f.client.ListDeployments(context.Background(), as("reader", &forgev1.ListDeploymentsRequest{}))
	if err != nil || len(list.Msg.GetDeployments()) != 1 {
		t.Errorf("list: %v %v", list, err)
	}
	for name, code := range map[string]connect.Code{
		"deployments/payments/production": connect.CodeNotFound,
		"deployments/payments/qa":         connect.CodeInvalidArgument,
		"deployments/payments":            connect.CodeInvalidArgument,
	} {
		if _, err := f.client.GetDeployment(context.Background(), as("reader", &forgev1.GetDeploymentRequest{Name: name})); connect.CodeOf(err) != code {
			t.Errorf("get %s: %v, want %v", name, err, code)
		}
	}
}

func TestStartDeployValidation(t *testing.T) {
	t.Parallel()
	f := newDeployFixture(t, true, nil)
	ctx := context.Background()
	for _, tc := range []struct {
		req  *forgev1.StartDeployRequest
		tok  string
		code connect.Code
	}{
		{&forgev1.StartDeployRequest{Service: "services/payments", Env: "qa"}, "writer", connect.CodeInvalidArgument},
		{&forgev1.StartDeployRequest{Service: "services/nope", Env: "staging"}, "writer", connect.CodeNotFound},
		{&forgev1.StartDeployRequest{Service: "services/payments", Env: "staging", Ref: "nope"}, "writer", connect.CodeNotFound},
		{&forgev1.StartDeployRequest{Service: "services/payments", Env: "staging"}, "reader", connect.CodePermissionDenied},
	} {
		if _, err := f.client.StartDeploy(ctx, as(tc.tok, tc.req)); connect.CodeOf(err) != tc.code {
			t.Errorf("%v as %s: %v, want %v", tc.req, tc.tok, err, tc.code)
		}
	}
	dry, err := f.client.StartDeploy(ctx, as("writer", &forgev1.StartDeployRequest{Service: "services/payments", Env: "staging", ValidateOnly: true}))
	if err != nil || dry.Msg.GetOperation() != nil || dry.Msg.GetDeployment().GetImage() == "" {
		t.Errorf("validate_only: %v %v", dry, err)
	}
	if head, _, _ := f.repo.Head(ctx); head != strings.Repeat("0", 40) {
		t.Error("validate_only committed")
	}

	off, err := New(Options{Authenticator: StaticAuthenticator{"writer": {Subject: "ana"}}})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(off)
	t.Cleanup(srv.Close)
	c := forgev1connect.NewDeployServiceClient(srv.Client(), srv.URL)
	if _, err := c.ListDeployments(ctx, as("writer", &forgev1.ListDeploymentsRequest{})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("deploys off: %v", err)
	}
}

func TestStartDeploySyncWaitsForRevision(t *testing.T) {
	t.Parallel()
	f := newDeployFixture(t, false, nil)
	first, err := f.start(t, "staging")
	if err != nil {
		t.Fatal(err)
	}
	if op := wait(t, f.ops, first.GetOperation().GetName()); op.GetError() != nil {
		t.Fatalf("first deploy: %v", op.GetError())
	}
	// Now Argo lags 300ms behind every new head. Until then the Application
	// stays Synced and Healthy at the first deploy's commit: only the
	// revision tells the second deploy it is not done.
	f.argo.SetLag(300 * time.Millisecond)
	start := time.Now()
	second, err := f.start(t, "staging")
	if err != nil {
		t.Fatal(err)
	}
	op := wait(t, f.ops, second.GetOperation().GetName())
	if op.GetError() != nil || time.Since(start) < 300*time.Millisecond {
		t.Errorf("finished after %v: %v", time.Since(start), op.GetError())
	}
	hist, _ := f.repo.History(context.Background(), "services/payments/staging/values.yaml", 1)
	want := "waiting for " + shortSHA(hist[0].SHA)
	found := false
	for _, ev := range op.GetEvents() {
		found = found || (ev.GetStep() == "sync" && strings.Contains(ev.GetMessage(), want))
	}
	if !found {
		t.Errorf("no sync event containing %q in %v", want, op.GetEvents())
	}
}

// countingRepo counts reads at each ref.
type countingRepo struct {
	DeployRepo
	mu    sync.Mutex
	reads map[string]int
}

func (r *countingRepo) Read(ctx context.Context, path, ref string) ([]byte, error) {
	r.mu.Lock()
	r.reads[ref]++
	r.mu.Unlock()
	return r.DeployRepo.Read(ctx, path, ref)
}

func (r *countingRepo) count(ref string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reads[ref]
}

func TestAwaitSyncReadsChartRevisionOnce(t *testing.T) {
	t.Parallel()
	var cr *countingRepo
	f := newDeployFixture(t, false, func(o *DeployOptions, _ *envtest.Argo) {
		cr = &countingRepo{DeployRepo: o.Repo, reads: map[string]int{}}
		o.Repo = cr
	})
	first, err := f.start(t, "staging")
	if err != nil {
		t.Fatal(err)
	}
	if op := wait(t, f.ops, first.GetOperation().GetName()); op.GetError() != nil {
		t.Fatalf("first deploy: %v", op.GetError())
	}
	f.argo.SetLag(300 * time.Millisecond) // many polls at 5ms while Argo reports the old head
	second, err := f.start(t, "staging")
	if err != nil {
		t.Fatal(err)
	}
	if op := wait(t, f.ops, second.GetOperation().GetName()); op.GetError() != nil {
		t.Fatalf("second deploy: %v", op.GetError())
	}
	if n := cr.count("chart"); n > 1 {
		t.Errorf("chart revision read %d times, want at most 1", n)
	}
}

// blankHistoryRepo reports no history once blank is set.
type blankHistoryRepo struct {
	DeployRepo
	blank atomic.Bool
}

func (r *blankHistoryRepo) History(ctx context.Context, path string, limit int) ([]github.CommitInfo, error) {
	if r.blank.Load() {
		return nil, nil
	}
	return r.DeployRepo.History(ctx, path, limit)
}

func TestGetDeploymentEmptyHistory(t *testing.T) {
	t.Parallel()
	var br *blankHistoryRepo
	f := newDeployFixture(t, false, func(o *DeployOptions, _ *envtest.Argo) {
		br = &blankHistoryRepo{DeployRepo: o.Repo}
		o.Repo = br
	})
	resp, err := f.start(t, "staging")
	if err != nil {
		t.Fatal(err)
	}
	if op := wait(t, f.ops, resp.GetOperation().GetName()); op.GetError() != nil {
		t.Fatalf("deploy: %v", op.GetError())
	}
	br.blank.Store(true)
	_, err = f.client.GetDeployment(context.Background(), as("reader", &forgev1.GetDeploymentRequest{Name: "deployments/payments/staging"}))
	if connect.CodeOf(err) != connect.CodeInternal || !strings.Contains(err.Error(), "no history for services/payments/staging/values.yaml") || strings.Contains(err.Error(), "%!w") {
		t.Errorf("got %v", err)
	}
}

func TestStartDeployAfterAnotherCommit(t *testing.T) {
	t.Parallel()
	// Argo reports the deploy repo's head, so another deploy landing before
	// Argo syncs makes it report a newer SHA than ours, forever.
	f := newDeployFixture(t, false, func(o *DeployOptions, a *envtest.Argo) {
		a.Lag = 200 * time.Millisecond
		o.SyncTimeout = 2 * time.Second
	})
	resp, err := f.start(t, "staging")
	if err != nil {
		t.Fatal(err)
	}
	zero := strings.Repeat("0", 40)
	for {
		if head, _, _ := f.repo.Head(context.Background()); head != zero {
			break
		}
		time.Sleep(time.Millisecond)
	}
	f.repo.Push("services/other/staging/values.yaml", []byte("image: o\n"), "another deploy")
	if op := wait(t, f.ops, resp.GetOperation().GetName()); op.GetError() != nil {
		t.Errorf("deploy: %v", op.GetError())
	}
}

func TestStartDeployNotHealthy(t *testing.T) {
	t.Parallel()
	f := newDeployFixture(t, false, func(o *DeployOptions, a *envtest.Argo) {
		o.SyncTimeout = 200 * time.Millisecond
		a.Unhealthy["payments-staging"] = "ImagePullBackOff"
	})
	resp, err := f.start(t, "staging")
	if err != nil {
		t.Fatal(err)
	}
	op := wait(t, f.ops, resp.GetOperation().GetName())
	if op.GetError().GetCode() != "not_healthy" || !strings.Contains(op.GetError().GetMessage(), "ImagePullBackOff") {
		t.Errorf("error: %v", op.GetError())
	}
	if _, err := f.repo.Read(context.Background(), "services/payments/staging/values.yaml", ""); err != nil {
		t.Error("the commit must stay (no automatic rollback)")
	}
}

func TestDeployConcurrencyAndConflict(t *testing.T) {
	t.Parallel()
	// No Argo: the first deploy waits in "namespace" and keeps the target busy.
	f := newDeployFixture(t, true, nil)
	first, err := f.start(t, "staging")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.start(t, "staging"); connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), first.GetOperation().GetName()) {
		t.Errorf("second deploy: %v", err)
	}
	f.ops.Cancel(first.GetOperation().GetName())
	if op := wait(t, f.ops, first.GetOperation().GetName()); op.GetError().GetCode() != OpCodeCancelled || !strings.Contains(op.GetError().GetMessage(), "stands") {
		t.Errorf("cancel after commit: %v", op.GetError())
	}

	// Another writer pushes between our Head and our CommitFile.
	g := newDeployFixture(t, false, nil)
	g.repo.BeforeCommit = func() { g.repo.Push("services/other/staging/values.yaml", []byte("image: o\n"), "outside") }
	resp, err := g.start(t, "staging")
	if err != nil {
		t.Fatal(err)
	}
	if op := wait(t, g.ops, resp.GetOperation().GetName()); op.GetError() != nil {
		t.Fatalf("retry on the new head: %v", op.GetError())
	}
	if _, err := g.repo.Read(context.Background(), "services/other/staging/values.yaml", ""); err != nil {
		t.Error("the other writer's commit was overwritten")
	}

	h := newDeployFixture(t, true, nil)
	n := 0
	var bump func()
	bump = func() { n++; h.repo.Push("x", []byte{byte(n)}, "outside"); h.repo.BeforeCommit = bump }
	h.repo.BeforeCommit = bump
	resp, err = h.start(t, "staging")
	if err != nil {
		t.Fatal(err)
	}
	if op := wait(t, h.ops, resp.GetOperation().GetName()); op.GetError().GetCode() != "git_conflict" || n != 3 {
		t.Errorf("after %d conflicts: %v", n, op.GetError())
	}
}

func TestDeploymentArgoURL(t *testing.T) {
	t.Parallel()
	f := newDeployFixture(t, false, func(o *DeployOptions, _ *envtest.Argo) { o.ArgoURL = "https://argocd.example.com" })
	if op := f.deployRef(t, "staging", ""); op.GetError() != nil {
		t.Fatal(op.GetError())
	}
	d, err := f.client.GetDeployment(context.Background(), as("reader", &forgev1.GetDeploymentRequest{Name: "deployments/payments/staging"}))
	if err != nil || d.Msg.GetDeployment().GetArgoUrl() != "https://argocd.example.com/applications/argocd/payments-staging" {
		t.Errorf("argo_url: %v %v", d, err)
	}
}

// With Registry set, a deploy waits for and runs the image from that
// registry, under the repo's owner, as the services' CI pushes it there.
func TestStartDeployOtherRegistry(t *testing.T) {
	t.Parallel()
	f := newDeployFixture(t, false, func(o *DeployOptions, _ *envtest.Argo) {
		o.Registry = "acrexample.azurecr.io"
	})
	f.images.Add("acrexample.azurecr.io/showmino/payments:1a2b3c4")
	resp, err := f.start(t, "staging")
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.GetDeployment().GetImage(); got != "acrexample.azurecr.io/showmino/payments:1a2b3c4" {
		t.Errorf("image = %q", got)
	}
	if op := wait(t, f.ops, resp.GetOperation().GetName()); op.GetError() != nil {
		t.Fatalf("op: %v", op.GetError())
	}
}

func TestImageName(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ registry, want string }{
		{"", "ghcr.io/acme/app:1a2b3c4"},
		{"ghcr.io", "ghcr.io/acme/app:1a2b3c4"},
		{"acrexample.azurecr.io", "acrexample.azurecr.io/acme/app:1a2b3c4"},
		{"registry.example:5000/team", "registry.example:5000/team/acme/app:1a2b3c4"},
	} {
		if got := imageName(c.registry, "Acme", "app", "1a2b3c4d5e6f"); got != c.want {
			t.Errorf("imageName(%q) = %q, want %q", c.registry, got, c.want)
		}
	}
}
