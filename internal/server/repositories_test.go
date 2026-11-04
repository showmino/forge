package server

import (
	"context"
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/gen/forge/v1/forgev1connect"
	"github.com/showmino/forge/internal/envtest"
	"github.com/showmino/forge/internal/github"
	"github.com/showmino/forge/internal/repositories"
	"github.com/showmino/forge/internal/store"
)

func repositoryConfig(t *testing.T) *repositories.Config {
	t.Helper()
	c, err := repositories.Parse([]byte("namespace: forge-repositories\nproviderConfig: {kind: ClusterProviderConfig, name: github}"))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCreatedRepository(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	svc := &forgev1.Service{Name: "services/app"}
	add := func(template, validate, result string) {
		t.Helper()
		if _, err := st.AppendAuditEvent(ctx, &forgev1.AuditEvent{
			Action: "service.create", Service: svc.Name, Result: &forgev1.AuditResult{Code: result},
			Details: map[string]string{"template": template, "validate_only": validate},
		}); err != nil {
			t.Fatal(err)
		}
	}
	add("", "false", "ok") // A connected repository.
	add("templates/svc", "true", "ok")
	add("templates/svc", "invalid", "ok")
	add("templates/svc", "false", "failed")
	if got, err := createdRepository(ctx, st, svc); err != nil || got {
		t.Fatalf("adopted without a successful create: %v %v", got, err)
	}
	add("templates/svc", "false", "ok")
	for range listAllPage {
		add("templates/svc", "true", "ok")
	}
	if got, err := createdRepository(ctx, st, svc); err != nil || !got {
		t.Fatalf("did not find paginated successful create: %v %v", got, err)
	}
}

func TestCreatedRepositoryWithAuditedRequest(t *testing.T) {
	for _, validate := range []bool{false, true} {
		t.Run(map[bool]string{false: "create", true: "dry-run"}[validate], func(t *testing.T) {
			ctx := context.Background()
			st := store.NewMemory()
			_, _, det := auditActions[forgev1connect.ServiceCatalogServiceCreateServiceProcedure].describe(&forgev1.CreateServiceRequest{
				ServiceId: "app", Template: "templates/svc", ValidateOnly: validate,
			})
			if _, err := st.AppendAuditEvent(ctx, &forgev1.AuditEvent{
				Action: "service.create", Service: "services/app", Result: &forgev1.AuditResult{Code: "ok"},
				Details: det,
			}); err != nil {
				t.Fatal(err)
			}
			got, err := createdRepository(ctx, st, &forgev1.Service{Name: "services/app"})
			if err != nil || got == validate {
				t.Fatalf("audited request (validate_only=%v): created=%v, error=%v", validate, got, err)
			}
		})
	}
}

func TestCreateRepositoryProtection(t *testing.T) {
	ctx := context.Background()
	for _, validate := range []bool{true, false} {
		t.Run(map[bool]string{true: "dry-run", false: "create"}[validate], func(t *testing.T) {
			repo := &envtest.Repo{}
			catalog := NewCatalog(nil)
			deploys := NewDeploys(DeployOptions{Repo: repo, Catalog: catalog})
			e := newCreateEnvOpts(t, createRelease, newFakeRepos(), deploys, func(o *Options) {
				o.Repositories = repositoryConfig(t)
				o.Catalog = catalog
			})
			msgs, err := e.create(ctx, "writer", &forgev1.CreateServiceRequest{
				ServiceId: "app", Template: "templates/svc", OwnerTeam: "platform",
				Parameters: map[string]string{"tier": "gold"}, ValidateOnly: validate,
			})
			if err != nil {
				t.Fatal(err)
			}
			last := msgs[len(msgs)-1]
			if validate {
				if plan := last.GetPlan(); plan == nil || !strings.Contains(strings.Join(plan.GetFiles(), ","), repositories.Path("app")) {
					t.Fatalf("no protection in dry-run: %v", last)
				}
				if e.repos.createCount() != 0 {
					t.Fatal("dry run created a repo")
				}
				return
			}
			if last.GetService() == nil {
				t.Fatalf("create failed: %v", last)
			}
			data, err := repo.Read(ctx, repositories.Path("app"), "")
			if err != nil || !strings.Contains(string(data), "kind: RepositoryRuleset") || strings.Contains(string(data), "requiredStatusChecks") {
				t.Fatalf("application protection: %s %v", data, err)
			}
		})
	}
}

func TestReconcileRepositories(t *testing.T) {
	ctx := context.Background()
	audit := store.NewMemory()
	catalog := NewCatalog(nil)
	repo := &envtest.Repo{}
	repos := newFakeRepos("app", "infra", "connected")
	for _, id := range []string{"app", "infra", "connected"} {
		svc := &forgev1.Service{OwnerTeam: "platform", RepoUrl: "https://github.com/showmino/" + id}
		if id == "infra" {
			svc.Infrastructure = &forgev1.InfrastructureRepo{Tool: "bicep", Target: "azure", Environments: []string{"production"}}
		}

		if _, err := catalog.register(writerCtx(), connect.NewRequest(&forgev1.RegisterServiceRequest{ServiceId: id, Service: svc}), false); err != nil {
			t.Fatal(err)
		}
		if id != "connected" {
			if _, err := audit.AppendAuditEvent(ctx, &forgev1.AuditEvent{
				Action: "service.create", Service: servicePrefix + id, Result: &forgev1.AuditResult{Code: "ok"},
				Details: details("template", "templates/svc"),
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	o := RepositoryOptions{Catalog: catalog, Deploys: NewDeploys(DeployOptions{Catalog: catalog, Repo: repo}), Config: repositoryConfig(t), Audit: audit, GitHub: repos, Org: "showmino"}
	changed, err := ReconcileRepositories(ctx, o)
	if err != nil || strings.Join(changed, ",") != "app,infra" {
		t.Fatalf("backfill: %v %v", changed, err)
	}
	data, err := repo.Read(ctx, repositories.Path("infra"), "")
	if err != nil || !strings.Contains(string(data), repositories.InfrastructureCheck) {
		t.Fatalf("no required infrastructure gate: %s %v", data, err)
	}
	if _, err := repo.Read(ctx, repositories.Path("connected"), ""); !errors.Is(err, github.ErrNotFound) {
		t.Fatalf("connected repo was adopted: %v", err)
	}
	if changed, err := ReconcileRepositories(ctx, o); err != nil || len(changed) != 0 {
		t.Fatalf("backfill not idempotent: %v %v", changed, err)
	}
	catalog.markDeleting("services/app", "operations/op_delete")
	if _, err := ReconcileRepositories(ctx, o); err == nil || !strings.Contains(err.Error(), "being deleted") {
		t.Fatalf("deleting repo: %v", err)
	}
}

func TestInfrastructureRepositoryProtectionReady(t *testing.T) {
	e := newInfraEnv(t, false, func(o *Options) {
		o.Repositories = repositoryConfig(t)
	})
	msgs, err := e.create(context.Background(), "writer", infraRequest("infra-net", "production"))
	if err != nil {
		t.Fatal(err)
	}
	if msgs[len(msgs)-1].GetService() == nil {
		t.Fatalf("create failed: %v", msgs[len(msgs)-1])
	}
	ready := false
	for _, msg := range msgs {
		if s := msg.GetStep(); s.GetName() == "repository_ready" && s.GetState() == forgev1.CreateStep_STATE_DONE {
			ready = true
		}
	}
	if !ready {
		t.Fatal("repo succeeded without waiting for protection")
	}
}

func TestInfrastructureRequiresPlanIdentity(t *testing.T) {
	e := newInfraEnv(t, false, func(o *Options) {
		env := o.Infrastructure.Targets[0].Environments["production"]
		env.Plan = nil
		o.Infrastructure.Targets[0].Environments["production"] = env
	})
	_, err := e.create(context.Background(), "writer", infraRequest("infra-net", "production"))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "no plan identity") {
		t.Fatalf("missing plan identity: %v", err)
	}
	if e.repos.createCount() != 0 {
		t.Fatal("created a repository before validating its plan identity")
	}
}
