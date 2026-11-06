package server

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"gopkg.in/yaml.v3"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/gen/forge/v1/forgev1connect"
	"github.com/showmino/forge/internal/envtest"
	"github.com/showmino/forge/internal/github"
	"github.com/showmino/forge/internal/infrastructure"
	"github.com/showmino/forge/internal/repositories"
	"github.com/showmino/forge/internal/store"
)

type repositoryReviewGitHub struct {
	*fakeRepos
	pulls   map[string][]github.Pull
	reviews map[int][]github.Review
	admins  map[string]bool
	members map[string]bool
}

func newRepositoryReviewGitHub(names ...string) *repositoryReviewGitHub {
	return &repositoryReviewGitHub{
		fakeRepos: newFakeRepos(names...), pulls: map[string][]github.Pull{},
		reviews: map[int][]github.Review{}, admins: map[string]bool{"dee": true}, members: map[string]bool{},
	}
}

func (g *repositoryReviewGitHub) PullsForCommit(_ context.Context, _, _, sha string) ([]github.Pull, error) {
	return g.pulls[sha], nil
}

func (g *repositoryReviewGitHub) PullReviews(_ context.Context, _, _ string, number int) ([]github.Review, error) {
	return g.reviews[number], nil
}

func (g *repositoryReviewGitHub) TeamMember(_ context.Context, org, slug, login string) (bool, error) {
	if org != "showmino" {
		return false, nil
	}
	if slug == "admins" {
		return g.admins[login], nil
	}
	return g.members[slug+"/"+login], nil
}

func repositoryConfig(t *testing.T) *repositories.Config {
	t.Helper()
	c, err := repositories.Parse([]byte("namespace: forge-repositories\nproviderConfig: {kind: ClusterProviderConfig, name: github}"))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestReconcileRepositoryReviewException(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name         string
		role         string
		expectedTeam string
		prAuthor     string
		wantPR       bool
	}{
		{name: "alternate lacks maintain", role: "pull", expectedTeam: "platform", prAuthor: "ana"},
		{name: "approved alternate", role: "maintain", expectedTeam: "alternate", prAuthor: "ana", wantPR: true},
		{name: "requester is not the PR author", role: "maintain", expectedTeam: "platform", prAuthor: "someone-else"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			audit := store.NewMemory()
			catalog := NewCatalog(nil)
			svc := &forgev1.Service{OwnerTeam: "platform", RepoUrl: "https://github.com/showmino/app"}
			if _, err := catalog.register(writerCtx(), connect.NewRequest(&forgev1.RegisterServiceRequest{ServiceId: "app", Service: svc}), false); err != nil {
				t.Fatal(err)
			}
			if _, err := audit.AppendAuditEvent(ctx, &forgev1.AuditEvent{
				Action: "service.create", Service: "services/app", Result: &forgev1.AuditResult{Code: "ok"},
				Details: details("template", "templates/svc"),
			}); err != nil {
				t.Fatal(err)
			}

			deployRepo := &envtest.Repo{}
			now := time.Now().UTC().Truncate(time.Second)
			exception, err := yaml.Marshal(reviewExceptionFile{
				Service: "app", Team: "platform", AlternateTeam: "alternate",
				Reason: "temporary reviewer coverage", RequestedBy: "ana", RequestedAt: now.Add(time.Minute),
			})
			if err != nil {
				t.Fatal(err)
			}
			sha := deployRepo.Push(repositories.ReviewExceptionPath("app"), exception, "Merge review exception")
			gh := newRepositoryReviewGitHub("app")
			gh.writeFile("app", "CODEOWNERS", []byte("* @showmino/stale\n"))
			gh.writeFile("app", ".github/CODEOWNERS", []byte("* @showmino/platform\n"))
			gh.give("app", "platform", "maintain")
			gh.give("app", "alternate", tc.role)
			gh.pulls[sha] = []github.Pull{{
				Number: 7, HTMLURL: "https://github.com/showmino/forge-deploys/pull/7",
				User: tc.prAuthor, Merged: now, Base: "main", HeadSHA: "head7", MergeCommit: sha,
			}}
			gh.reviews[7] = []github.Review{{User: "dee", State: "APPROVED", CommitID: "head7"}}

			o := RepositoryOptions{
				Catalog: catalog, Deploys: NewDeploys(DeployOptions{Catalog: catalog, Repo: deployRepo}),
				Config: repositoryConfig(t), Audit: audit, GitHub: gh, Org: "showmino",
				DeployOwner: "showmino", DeployName: "forge-deploys", ReviewAdminTeams: []string{"admins"},
			}
			changed, err := ReconcileRepositories(ctx, o)
			if err != nil {
				t.Fatal(err)
			}
			codeowners, err := gh.FileAt(ctx, "showmino", "app", ".github/CODEOWNERS", "main")
			if err != nil {
				t.Fatal(err)
			}
			currentTeam := tc.expectedTeam
			if tc.wantPR {
				currentTeam = "platform"
			}
			if got, want := string(codeowners), "* @showmino/"+currentTeam+"\n"; got != want {
				t.Fatalf("CODEOWNERS = %q, want %q", got, want)
			}
			if tc.wantPR {
				if len(changed) != 0 || len(gh.proposals) != 1 ||
					gh.proposals[0].Update[0].Path != ".github/CODEOWNERS" ||
					string(gh.proposals[0].Update[0].Content) != "* @showmino/"+tc.expectedTeam+"\n" {
					t.Fatalf("alternate CODEOWNERS proposal: changed=%v proposals=%+v", changed, gh.proposals)
				}
				if _, err := deployRepo.Read(ctx, repositories.Path("app"), ""); err == nil {
					t.Fatal("enabled review rules before the CODEOWNERS PR merged")
				}
			} else if strings.Join(changed, ",") != "app" || len(gh.proposals) != 0 {
				t.Fatalf("unqualified exception was applied: changed=%v proposals=%+v", changed, gh.proposals)
			}
		})
	}
}

func TestReconcileRepositoryReviewExceptionRevocation(t *testing.T) {
	ctx := context.Background()
	audit := store.NewMemory()
	catalog := NewCatalog(nil)
	svc := &forgev1.Service{OwnerTeam: "platform", RepoUrl: "https://github.com/showmino/app"}
	if _, err := catalog.register(writerCtx(), connect.NewRequest(&forgev1.RegisterServiceRequest{ServiceId: "app", Service: svc}), false); err != nil {
		t.Fatal(err)
	}
	if _, err := audit.AppendAuditEvent(ctx, &forgev1.AuditEvent{
		Action: "service.create", Service: "services/app", Result: &forgev1.AuditResult{Code: "ok"},
		Details: details("template", "templates/svc"),
	}); err != nil {
		t.Fatal(err)
	}

	deployRepo := &envtest.Repo{}
	gh := newRepositoryReviewGitHub("app")
	gh.writeFile("app", "CODEOWNERS", []byte("* @showmino/alternate\n"))
	gh.give("app", "platform", "maintain")
	gh.give("app", "alternate", "maintain")
	o := RepositoryOptions{
		Catalog: catalog, Deploys: NewDeploys(DeployOptions{Catalog: catalog, Repo: deployRepo}),
		Config: repositoryConfig(t), Audit: audit, GitHub: gh, Org: "showmino",
		DeployOwner: "showmino", DeployName: "forge-deploys",
	}

	changed, err := ReconcileRepositories(ctx, o)
	if err != nil || len(changed) != 0 || len(gh.proposals) != 1 {
		t.Fatalf("revocation should first propose restoring CODEOWNERS: changed=%v proposals=%+v err=%v", changed, gh.proposals, err)
	}
	if file := gh.proposals[0].Update[0]; file.Path != "CODEOWNERS" || string(file.Content) != "* @showmino/platform\n" {
		t.Fatalf("unexpected revocation proposal: %+v", file)
	}
	if _, err := deployRepo.Read(ctx, repositories.Path("app"), ""); err == nil {
		t.Fatal("enabled repository rules before the alternate code-owner restoration PR merged")
	}

	gh.writeFile("app", "CODEOWNERS", []byte("* @showmino/platform\n"))
	changed, err = ReconcileRepositories(ctx, o)
	if err != nil || strings.Join(changed, ",") != "app" {
		t.Fatalf("did not enable rules after CODEOWNERS restoration: changed=%v err=%v", changed, err)
	}
}

func TestReconcileRepositoryAdoptionUsesImmutableIDs(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name        string
		repoID      int64
		requestedAt time.Duration
		wantAdopted bool
	}{
		{name: "matching immutable IDs", repoID: 1445, requestedAt: time.Minute, wantAdopted: true},
		{name: "different repository ID", repoID: 9876, requestedAt: time.Minute},
		{name: "stale adoption request", repoID: 1445, requestedAt: -time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			audit := store.NewMemory()
			catalog := NewCatalog(nil)
			svc := &forgev1.Service{OwnerTeam: "web", RepoUrl: "https://github.com/showmino/forge-web"}
			if _, err := catalog.register(writerCtx(), connect.NewRequest(&forgev1.RegisterServiceRequest{ServiceId: "forge-web", Service: svc}), false); err != nil {
				t.Fatal(err)
			}
			deployRepo := &envtest.Repo{}
			now := time.Now().UTC().Truncate(time.Second)
			adoption, err := yaml.Marshal(adoptionFile{
				Service: "forge-web", OwnerID: 155, RepoID: tc.repoID,
				Reason: "historical forge create event", RequestedBy: "saitargoed", RequestedAt: now.Add(tc.requestedAt),
			})
			if err != nil {
				t.Fatal(err)
			}
			sha := deployRepo.Push(repositories.AdoptionPath("forge-web"), adoption, "Merge repository adoption")
			gh := newRepositoryReviewGitHub("forge-web")
			gh.writeFile("forge-web", "CODEOWNERS", []byte("* @showmino/web\n"))
			gh.give("forge-web", "web", "maintain")
			gh.pulls[sha] = []github.Pull{{
				Number: 8, HTMLURL: "https://github.com/showmino/forge-deploys/pull/8",
				User: "saitargoed", Merged: now, Base: "main", HeadSHA: "head8", MergeCommit: sha,
			}}
			gh.reviews[8] = []github.Review{{User: "dee", State: "APPROVED", CommitID: "head8"}}
			gh.members["web/saitargoed"] = true
			o := RepositoryOptions{
				Catalog: catalog, Deploys: NewDeploys(DeployOptions{Catalog: catalog, Repo: deployRepo}),
				Config: repositoryConfig(t), Audit: audit, GitHub: gh, Org: "showmino",
				DeployOwner: "showmino", DeployName: "forge-deploys", ReviewAdminTeams: []string{"admins"},
			}
			changed, err := ReconcileRepositories(ctx, o)
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantAdopted && strings.Join(changed, ",") != "forge-web" {
				t.Fatalf("matching immutable IDs were not adopted: %v", changed)
			}
			if !tc.wantAdopted && len(changed) != 0 {
				t.Fatalf("adopted an invalid repository record: %v", changed)
			}
		})
	}
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
	repo := &github.Repo{ID: 1445, CreatedAt: time.Unix(1, 0)}
	repo.Owner.ID = 155
	if got, err := createdRepository(ctx, st, svc, repo); err != nil || got {
		t.Fatalf("adopted without a successful create: %v %v", got, err)
	}
	add("templates/svc", "false", "ok")
	for range listAllPage {
		add("templates/svc", "true", "ok")
	}
	if got, err := createdRepository(ctx, st, svc, repo); err != nil || !got {
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
			repo := &github.Repo{ID: 1445, CreatedAt: time.Unix(1, 0)}
			repo.Owner.ID = 155
			got, err := createdRepository(ctx, st, &forgev1.Service{Name: "services/app"}, repo)
			if err != nil || got == validate {
				t.Fatalf("audited request (validate_only=%v): created=%v, error=%v", validate, got, err)
			}
		})
	}
}

func TestCreatedRepositoryRejectsRecreatedRepo(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	if _, err := st.AppendAuditEvent(ctx, &forgev1.AuditEvent{
		Action: "service.create", Service: "services/app", Result: &forgev1.AuditResult{Code: "ok"},
		Details: details("template", "templates/svc"),
	}); err != nil {
		t.Fatal(err)
	}
	repo := &github.Repo{ID: 1445, CreatedAt: time.Now().Add(time.Hour)}
	repo.Owner.ID = 155
	if got, err := createdRepository(ctx, st, &forgev1.Service{Name: "services/app"}, repo); err != nil || got {
		t.Fatalf("adopted a repository created after its audit provenance: %v %v", got, err)
	}
}

func TestCreatedRepositoryRequiresMatchingImmutableIDs(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	if _, err := st.AppendAuditEvent(ctx, &forgev1.AuditEvent{
		Action: "service.create", Service: "services/app", Result: &forgev1.AuditResult{Code: "ok"},
		Details: map[string]string{
			"template": "templates/svc", "repository_id": "9876", "repository_owner_id": "155",
		},
	}); err != nil {
		t.Fatal(err)
	}
	repo := &github.Repo{ID: 1445, CreatedAt: time.Unix(1, 0)}
	repo.Owner.ID = 155
	if got, err := createdRepository(ctx, st, &forgev1.Service{Name: "services/app"}, repo); err != nil || got {
		t.Fatalf("adopted a different immutable repository ID: %v %v", got, err)
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
			events, err := e.audit.ListAuditEvents(ctx, store.AuditQuery{
				Action: "service.create", Service: "services/app", Limit: 1,
			})
			if err != nil || len(events) != 1 {
				t.Fatalf("create audit event: %v %v", events, err)
			}
			details := events[0].GetDetails()
			if details["repository_id"] != "1445" || details["repository_owner_id"] != "155" {
				t.Fatalf("create audit lacks immutable repository IDs: %v", details)
			}
			if last.GetService() == nil {
				t.Fatalf("create failed: %v", last)
			}
			data, err := repo.Read(ctx, repositories.Path("app"), "")
			if err != nil || !strings.Contains(string(data), "kind: RepositoryRuleset") || strings.Contains(string(data), "requiredStatusChecks") {
				t.Fatalf("application protection: %s %v", data, err)
			}
			files, ok := e.repos.files("app")
			if !ok {
				t.Fatal("repository files were not pushed")
			}
			found := false
			for _, file := range files {
				if file.Path == "CODEOWNERS" && string(file.Content) == "* @showmino/platform\n" {
					found = true
				}
			}
			if !found {
				t.Fatalf("team CODEOWNERS was not scaffolded: %+v", files)
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
		repos.give(id, "platform", "maintain")
	}
	o := RepositoryOptions{
		Catalog: catalog, Deploys: NewDeploys(DeployOptions{Catalog: catalog, Repo: repo}),
		Config: repositoryConfig(t), Audit: audit, GitHub: repos, Org: "showmino",
		DeployOwner: "showmino", DeployName: "forge-deploys",
		ReviewWaiver: func(_ context.Context, id string) (bool, error) { return id == "infra", nil },
	}
	changed, err := ReconcileRepositories(ctx, o)
	if err != nil || len(changed) != 0 {
		t.Fatalf("CODEOWNERS proposals: %v %v", changed, err)
	}
	if len(repos.proposals) != 2 {
		t.Fatalf("expected CODEOWNERS proposals, got %+v", repos.proposals)
	}
	for _, id := range []string{"app", "infra"} {
		repos.writeFile(id, "CODEOWNERS", []byte("* @showmino/platform\n"))
	}
	changed, err = ReconcileRepositories(ctx, o)
	if err != nil || strings.Join(changed, ",") != "app,infra" {
		t.Fatalf("backfill: %v %v", changed, err)
	}
	data, err := repo.Read(ctx, repositories.Path("infra"), "")
	if err != nil || !strings.Contains(string(data), repositories.InfrastructureCheck) ||
		!strings.Contains(string(data), "requiredApprovingReviewCount: 0") ||
		!strings.Contains(string(data), "requireCodeOwnerReview: false") {
		t.Fatalf("incorrect infrastructure review waiver: %s %v", data, err)
	}
	data, err = repo.Read(ctx, repositories.Path("app"), "")
	if err != nil || !strings.Contains(string(data), "requiredApprovingReviewCount: 1") ||
		!strings.Contains(string(data), "requireCodeOwnerReview: true") {
		t.Fatalf("application review rules: %s %v", data, err)
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

// With objects: template the access template makes the plan access, so a
// target needs no plan block, and the environment's file holds only the
// template's objects.
func TestInfrastructureTemplateObjectsNeedNoPlanBlock(t *testing.T) {
	e := newInfraEnv(t, false, func(o *Options) {
		tg := &o.Infrastructure.Targets[0]
		tg.Objects = infrastructure.ObjectsTemplate
		env := tg.Environments["production"]
		env.Plan = nil
		tg.Environments["production"] = env
	})
	msgs, err := e.create(context.Background(), "writer", infraRequest("infra-net", "production"))
	if err != nil {
		t.Fatal(err)
	}
	if msgs[len(msgs)-1].GetService() == nil {
		t.Fatalf("create failed: %v", msgs[len(msgs)-1])
	}
	prod, err := e.repo.Read(context.Background(), "infrastructure/infra-net/production.yaml", "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(prod), "kind: Environment\n") || strings.Contains(string(prod), "kind: EnvironmentSecret") {
		t.Fatalf("Forge wrote its own GitHub objects with objects: template:\n%s", prod)
	}
}
