package server

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/envtest"
	"github.com/showmino/forge/internal/infrastructure"
	"github.com/showmino/forge/internal/manifest"
	"github.com/showmino/forge/policy"
)

// infraRelease has the infrastructure template "tofu" and the service
// template "svc".
var infraRelease = staticSource{
	versions: []string{"v1.0.0"},
	trees: map[string]fs.FS{"v1.0.0": fstest.MapFS{
		"tofu/template.yaml": {Data: []byte(templateYAML("tofu", "iac", "type: infrastructure\n"))},
		"tofu/skeleton/forge.yaml": {Data: []byte("apiVersion: forge.dev/v1\nservice:\n  name: {{ .Service.Name }}\n  team: {{ .Service.Team }}\n" +
			"infrastructure:\n  tool: opentofu\n")},
		"tofu/skeleton/infrastructure/terraform/main.tf": {Data: []byte("terraform {}\n")},
		"svc/template.yaml":       {Data: []byte(templateYAML("svc", "go", ""))},
		"svc/skeleton/forge.yaml": {Data: []byte("apiVersion: forge.dev/v1\nservice:\n  name: {{ .Service.Name }}\n  team: {{ .Service.Team }}\n")},
	}},
}

const infraConfig = `
namespace: forge-infrastructure
providerConfig: {kind: ClusterProviderConfig, name: github}
workflow: showmino/forge-workflows/.github/workflows/infrastructure.yml@refs/heads/main
accessKinds: [managedidentity.azure.m.upbound.io/FederatedIdentityCredential]
targets:
  - name: azure
    environments:
      staging:
        secret: target-azure-staging
        githubSecrets: {AZURE_CLIENT_ID: clientId}
      production:
        secret: target-azure-production
        githubSecrets: {AZURE_CLIENT_ID: clientId}
        maxRepos: 1
        access: |
          apiVersion: managedidentity.azure.m.upbound.io/v1beta1
          kind: FederatedIdentityCredential
          metadata:
            name: {{ .Repo }}-{{ .Environment }}
          spec:
            forProvider:
              subject: {{ .Subject }}
  - name: web-only
    teams: [web]
    environments:
      production: {secret: target-web}
`

type infraEnv struct {
	*createEnv
	repo    *envtest.Repo
	deploys *Deploys
}

// newInfraEnv is a create env with deploys (the deploy repo, envtest's Argo
// CD) and the targets of infraConfig, unless noConfig.
func newInfraEnv(t *testing.T, noConfig bool) *infraEnv {
	t.Helper()
	rules, err := policy.New(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cluster := &Cluster{Kube: envtest.Kube(), Dynamic: envtest.Dynamic()}
	repo := &envtest.Repo{}
	catalog := NewCatalog(nil)
	deploys := NewDeploys(DeployOptions{
		Policy: rules, Cluster: cluster, Repo: repo, Catalog: catalog, Org: "showmino",
		Poll: 5 * time.Millisecond, SyncTimeout: 5 * time.Second,
	})
	argo := &envtest.Argo{Kube: cluster.Kube, Dynamic: cluster.Dynamic, Repo: repo, Namespace: "argocd", Unhealthy: map[string]string{}}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() { argo.Run(ctx) })
	t.Cleanup(func() { cancel(); wg.Wait() })
	var cfg *infrastructure.Config
	if !noConfig {
		if cfg, err = infrastructure.Parse([]byte(infraConfig)); err != nil {
			t.Fatal(err)
		}
	}
	e := newCreateEnvOpts(t, infraRelease, newFakeRepos(), deploys, func(o *Options) {
		o.Catalog = catalog
		o.Infrastructure = cfg
	})
	e.catalog = catalog
	return &infraEnv{createEnv: e, repo: repo, deploys: deploys}
}

func infraRequest(id string, envs ...string) *forgev1.CreateServiceRequest {
	return &forgev1.CreateServiceRequest{ServiceId: id, Template: "templates/tofu", OwnerTeam: "platform", Target: "azure", Environments: envs}
}

func TestCreateInfrastructure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newInfraEnv(t, false)
	msgs, err := e.create(ctx, "writer", infraRequest("infra-net", "production"))
	if err != nil {
		t.Fatal(err)
	}
	var steps []string
	for _, m := range msgs {
		if s := m.GetStep(); s != nil && s.GetState() == forgev1.CreateStep_STATE_DONE {
			steps = append(steps, s.GetName())
		}
	}
	if got := strings.Join(steps, ","); got != "fetch,render,create_repo,grant_team,oidc_subject,push,write_infrastructure,register,setup" {
		t.Errorf("steps %s", got)
	}
	svc := msgs[len(msgs)-1].GetService()
	if svc == nil {
		t.Fatalf("last message %v", msgs[len(msgs)-1])
	}
	if i := svc.GetInfrastructure(); i.GetTool() != "opentofu" || i.GetTarget() != "azure" || strings.Join(i.GetEnvironments(), ",") != "production" {
		t.Errorf("infrastructure %v", i)
	}
	if got := strings.Join(e.repos.oidc["infra-net"], ","); got != "job_workflow_ref,repo,context" {
		t.Errorf("OIDC claims %q", got)
	}
	rec, err := e.deploys.readInfrastructure(ctx, "infra-net")
	if err != nil || rec == nil || rec.Target != "azure" || strings.Join(rec.Environments, ",") != "production" {
		t.Fatalf("record %+v %v", rec, err)
	}
	objs, err := e.repo.Read(ctx, "infrastructure/infra-net/production.yaml", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"kind: Environment", "kind: EnvironmentDeploymentPolicy", "secretName: AZURE_CLIENT_ID",
		"subject: job_workflow_ref:showmino/forge-workflows/.github/workflows/infrastructure.yml@refs/heads/main:repo:showmino@155/infra-net@1445:environment:production"} {
		if !strings.Contains(string(objs), want) {
			t.Errorf("production.yaml has no %q:\n%s", want, objs)
		}
	}
	if _, err := e.repo.Read(ctx, "infrastructure/infra-net/staging.yaml", ""); err == nil {
		t.Error("staging was set up, but only production was asked for")
	}

	// A Forge deploy or preview of it is refused: it deploys itself.
	if _, _, err := e.deploys.resolve(ctx, "services/infra-net", "", nil); connect.CodeOf(err) != connect.CodeFailedPrecondition || !errors.Is(err, manifest.ErrDeploysItself) {
		t.Errorf("deploy: %v", err)
	}

	// production's maxRepos is 1: a second repo there is refused, by name.
	_, err = e.create(ctx, "writer", infraRequest("infra-dns", "production"))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "target azure's production has no room") {
		t.Errorf("full: %v", err)
	}
	// staging has room.
	if _, err := e.create(ctx, "writer", infraRequest("infra-dns", "staging")); err != nil {
		t.Errorf("staging: %v", err)
	}

	// The delete's part: the folder goes, and so the objects.
	if err := e.deploys.removeInfrastructure(ctx, "infra-net"); err != nil {
		t.Fatal(err)
	}
	if rec, err := e.deploys.readInfrastructure(ctx, "infra-net"); err != nil || rec != nil {
		t.Errorf("after remove: %+v %v", rec, err)
	}
	if _, err := e.repo.Read(ctx, "infrastructure/infra-net/production.yaml", ""); err == nil {
		t.Error("production.yaml is still there")
	}
	if err := e.deploys.removeInfrastructure(ctx, "infra-net"); err != nil {
		t.Errorf("again: %v", err)
	}
}

func TestCreateInfrastructureEveryEnvironment(t *testing.T) {
	t.Parallel()
	e := newInfraEnv(t, false)
	msgs, err := e.create(context.Background(), "writer", infraRequest("infra-net"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(msgs[len(msgs)-1].GetService().GetInfrastructure().GetEnvironments(), ","); got != "staging,production" {
		t.Errorf("environments %s, want every one of the target's", got)
	}
}

func TestCreateInfrastructureRefuses(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newInfraEnv(t, false)
	with := func(mut func(*forgev1.CreateServiceRequest)) *forgev1.CreateServiceRequest {
		r := infraRequest("infra-net")
		mut(r)
		return r
	}
	for _, tt := range []struct {
		name string
		env  *infraEnv
		req  *forgev1.CreateServiceRequest
		code connect.Code
		msg  string
	}{
		{"no target", e, with(func(r *forgev1.CreateServiceRequest) { r.Target = "" }), connect.CodeInvalidArgument, "needs a target: one of azure, web-only"},
		{"unknown target", e, with(func(r *forgev1.CreateServiceRequest) { r.Target = "aws" }), connect.CodeInvalidArgument, `no target "aws"`},
		{"team not allowed", e, with(func(r *forgev1.CreateServiceRequest) { r.Target = "web-only" }), connect.CodeFailedPrecondition, "target web-only is for the teams web, not platform"},
		{"environment not offered", e, with(func(r *forgev1.CreateServiceRequest) { r.Environments = []string{"qa"} }), connect.CodeFailedPrecondition, "target azure has no qa"},
		{"target on a service template", e, with(func(r *forgev1.CreateServiceRequest) { r.Template = "templates/svc" }), connect.CodeInvalidArgument, "svc is not an infrastructure template"},
		{"target with an image", e, with(func(r *forgev1.CreateServiceRequest) { r.Template, r.Image = "", "nginx:1.27" }), connect.CodeInvalidArgument, "target and environments are for an infrastructure template"},
		{"not configured", newInfraEnv(t, true), infraRequest("infra-net"), connect.CodeFailedPrecondition, "no --infrastructure-config"},
		{"run options", e, with(func(r *forgev1.CreateServiceRequest) { r.RunOptions = &forgev1.RunOptions{} }), connect.CodeInvalidArgument, "run options are for a service"},
		{"team not a slug", e, with(func(r *forgev1.CreateServiceRequest) { r.OwnerTeam = "Platform Team" }), connect.CodeInvalidArgument, "must be a team's slug"},
	} {
		_, err := tt.env.create(ctx, "writer", tt.req)
		if connect.CodeOf(err) != tt.code || !strings.Contains(err.Error(), tt.msg) {
			t.Errorf("%s: got %v, want %v with %q", tt.name, err, tt.code, tt.msg)
		}
		if n := e.repos.createCount(); n != 0 {
			t.Errorf("%s: %d repos created", tt.name, n)
		}
	}

	// Only CreateService sets infrastructure: a register may not.
	_, err := e.catalog.RegisterService(writerCtx(), connect.NewRequest(&forgev1.RegisterServiceRequest{
		ServiceId: "fake", Service: &forgev1.Service{OwnerTeam: "platform", Infrastructure: &forgev1.InfrastructureRepo{Target: "azure"}},
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "output only") {
		t.Errorf("register with infrastructure: %v", err)
	}
}

// A failure after the repo was made takes back the repo and the deploy
// repo's part.
func TestCreateInfrastructureRollsBack(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newInfraEnv(t, false)
	e.repos.oidcErr = errors.New("403 Resource not accessible by integration")
	msgs, err := e.create(ctx, "writer", infraRequest("infra-net"))
	if err != nil {
		t.Fatal(err)
	}
	f := msgs[len(msgs)-1].GetFailure()
	if f == nil || f.GetStep() != "oidc_subject" || !strings.Contains(f.GetMessage(), "Resource not accessible") || !f.GetRolledBack() {
		t.Fatalf("failure %v", msgs[len(msgs)-1])
	}
	if ok, _ := e.repos.RepoExists(ctx, "showmino", "infra-net"); ok {
		t.Error("the repo is still there")
	}
	if rec, _ := e.deploys.readInfrastructure(ctx, "infra-net"); rec != nil {
		t.Errorf("the deploy repo still has %+v", rec)
	}
	if ok, _ := e.catalog.has(ctx, "services/infra-net"); ok {
		t.Error("it is in the catalog")
	}
}

// A folder an earlier repo of the same name left behind refuses the name:
// it would hand the new repo its environments and cloud access.
func TestCreateInfrastructureLeftoverFolder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newInfraEnv(t, false)
	e.repo.Push("infrastructure/infra-net/staging.yaml", []byte("kind: Environment\n"), "left behind")
	_, err := e.create(ctx, "writer", infraRequest("infra-net"))
	if connect.CodeOf(err) != connect.CodeAlreadyExists || !strings.Contains(err.Error(), "still has infrastructure/infra-net") {
		t.Fatalf("leftover: %v", err)
	}
	// removeInfrastructure takes every file Forge may have written, with or
	// without a record.
	if err := e.deploys.removeInfrastructure(ctx, "infra-net"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.create(ctx, "writer", infraRequest("infra-net")); err != nil {
		t.Errorf("after the cleanup: %v", err)
	}
}

// A failure after the deploy repo was written takes the folder back, and
// says when it could not.
func TestCreateInfrastructureRollsBackTheFolder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newInfraEnv(t, false)
	// Someone registers the name while the repo is made: register fails.
	e.repos.onCreate = func() { register(t, e.catalog, "infra-net") }
	msgs, err := e.create(ctx, "writer", infraRequest("infra-net"))
	if err != nil {
		t.Fatal(err)
	}
	f := msgs[len(msgs)-1].GetFailure()
	if f == nil || f.GetStep() != "register" || !f.GetRolledBack() {
		t.Fatalf("failure %v", msgs[len(msgs)-1])
	}
	if left, _ := e.deploys.infrastructureLeft(ctx, "infra-net"); len(left) != 0 {
		t.Errorf("left in the deploy repo: %v", left)
	}
}

// An infrastructure repo cannot move to another team: its target's rules and
// its cloud access are its team's.
func TestInfrastructureRepoKeepsItsTeam(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newInfraEnv(t, false)
	if _, err := e.create(ctx, "writer", infraRequest("infra-net")); err != nil {
		t.Fatal(err)
	}
	_, err := e.catalog.UpdateService(writerCtx(), connect.NewRequest(&forgev1.UpdateServiceRequest{
		Service:    &forgev1.Service{Name: "services/infra-net", OwnerTeam: "web"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"owner_team"}},
	}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "cannot move to another team") {
		t.Errorf("move: %v", err)
	}
}

func TestListTargets(t *testing.T) {
	t.Parallel()
	cfg, err := infrastructure.Parse([]byte(infraConfig))
	if err != nil {
		t.Fatal(err)
	}
	s := &templateService{infra: cfg}
	resp, err := s.ListTargets(writerCtx(), connect.NewRequest(&forgev1.ListTargetsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	ts := resp.Msg.GetTargets()
	if len(ts) != 2 || ts[0].GetName() != "azure" || strings.Join(ts[0].GetEnvironments(), ",") != "staging,production" ||
		ts[1].GetName() != "web-only" || strings.Join(ts[1].GetTeams(), ",") != "web" {
		t.Fatalf("targets %v", ts)
	}
	none, err := (&templateService{}).ListTargets(writerCtx(), connect.NewRequest(&forgev1.ListTargetsRequest{}))
	if err != nil || len(none.Msg.GetTargets()) != 0 {
		t.Errorf("without infrastructure: %v %v", none, err)
	}
	if _, err := s.ListTargets(context.Background(), connect.NewRequest(&forgev1.ListTargetsRequest{})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("anonymous: %v", err)
	}
}

// ReconcileInfrastructure brings a folder written before the repos' IDs were
// recorded up to date, once.
func TestReconcileInfrastructure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newInfraEnv(t, false)
	if _, err := e.create(ctx, "writer", infraRequest("infra-net", "staging")); err != nil {
		t.Fatal(err)
	}
	// An older repo: no IDs in its record, a subject without them.
	e.repos.mu.Lock()
	e.repos.repos["infra-old"] = nil
	e.repos.mu.Unlock()
	if _, err := e.catalog.register(writerCtx(), connect.NewRequest(&forgev1.RegisterServiceRequest{ServiceId: "infra-old", Service: &forgev1.Service{
		OwnerTeam: "platform", RepoUrl: "https://github.com/showmino/infra-old",
		Infrastructure: &forgev1.InfrastructureRepo{Tool: "opentofu", Target: "azure", Environments: []string{"production"}},
	}}), false); err != nil {
		t.Fatal(err)
	}
	e.repo.Push("infrastructure/infra-old/target.yaml", []byte("target: azure\nteam: platform\nenvironments: [production]\n"), "old")
	e.repo.Push("infrastructure/infra-old/production.yaml", []byte("subject: repo:showmino/infra-old\n"), "old")
	cfg, _ := infrastructure.Parse([]byte(infraConfig))
	o := ReconcileOptions{Catalog: e.catalog, Deploys: e.deploys, Infra: cfg, GitHub: e.repos, OIDC: e.repos, Org: "showmino"}
	changed, err := ReconcileInfrastructure(ctx, o)
	if err != nil || strings.Join(changed, ",") != "infra-old" {
		t.Fatalf("changed %v %v, want infra-old only", changed, err)
	}
	prod, _ := e.repo.Read(ctx, "infrastructure/infra-old/production.yaml", "")
	rec, _ := e.deploys.readInfrastructure(ctx, "infra-old")
	if !strings.Contains(string(prod), "repo:showmino@155/infra-old@1445:environment:production") || rec.RepoID != 1445 || rec.OwnerID != 155 {
		t.Errorf("after: %s %+v", prod, rec)
	}
	if _, err := e.repo.Read(ctx, "infrastructure/infra-old/repo.yaml", ""); err != nil {
		t.Errorf("repo.yaml: %v", err)
	}
	if got := strings.Join(e.repos.oidc["infra-old"], ","); got != "job_workflow_ref,repo,context" {
		t.Errorf("OIDC claims %q", got)
	}
	if changed, err := ReconcileInfrastructure(ctx, o); err != nil || len(changed) != 0 {
		t.Errorf("again: %v %v", changed, err)
	}
}
