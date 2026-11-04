package cli

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/envtest"
	"github.com/showmino/forge/internal/github"
	"github.com/showmino/forge/internal/infrastructure"
	"github.com/showmino/forge/internal/server"
	"github.com/showmino/forge/internal/store"
)

const testSharedWorkflow = "showmino/forge-workflows/.github/workflows/infrastructure.yml@refs/heads/main"

const testInfraSHA = "1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d"

// noDispatch is GitHub for approvals: main is at testInfraSHA, and starting
// a workflow fails, as without the App's Actions: write.
type noDispatch struct{}

func (noDispatch) DispatchWorkflow(context.Context, string, string, string, string, map[string]string) error {
	return errors.New("403 Resource not accessible by integration")
}
func (noDispatch) DefaultBranch(context.Context, string, string) (string, error) { return "main", nil }
func (noDispatch) ResolveRef(context.Context, string, string, string) (string, error) {
	return testInfraSHA, nil
}

// serveApprovals serves approvals for the infrastructure repo infra-net of
// team billing. Tokens: t-ci (its shared workflow, started by cy), t-team
// (cy, of billing), t-mate (dee, of billing), t-reader.
func serveApprovals(t *testing.T, h *harness) {
	t.Helper()
	serveApprovalsWith(t, h, nil)
}

// serveApprovalsWith is serveApprovals with exceptions when ex is set.
func serveApprovalsWith(t *testing.T, h *harness, ex *server.ExceptionOptions) {
	t.Helper()
	st := store.NewMemory()
	ctx := context.Background()
	if err := st.Write(ctx, func(tx store.Tx) error {
		return tx.Put(ctx, &forgev1.Service{Name: "services/infra-net", OwnerTeam: "billing", RepoUrl: "https://github.com/showmino/infra-net",
			Infrastructure: &forgev1.InfrastructureRepo{Tool: "opentofu", Target: "azure", Environments: []string{"production"}}}, 1)
	}); err != nil {
		t.Fatal(err)
	}
	cfg, err := infrastructure.Parse([]byte("namespace: forge-infrastructure\nproviderConfig: {kind: ClusterProviderConfig, name: github}\nworkflow: " +
		testSharedWorkflow + "\ntargets: [{name: azure, environments: {production: {secret: s}}}]\n"))
	if err != nil {
		t.Fatal(err)
	}
	h.catalog = server.NewCatalogWithStore(st, nil)
	h.approvals = server.NewApprovals(server.ApprovalOptions{Store: st, Catalog: h.catalog, Infra: cfg, Org: "showmino", Dispatch: noDispatch{}, Exceptions: ex})
	ci := func(event string) server.Principal {
		return server.Principal{Subject: "github:showmino/infra-net", Workload: &server.Workload{
			Repository: "showmino/infra-net", Service: "infra-net", SHA: testInfraSHA, Actor: "cy", RunID: 7, JobWorkflowRef: testSharedWorkflow, EventName: event}}
	}
	h.startAPI(server.StaticAuthenticator{
		"t-ci":      ci("push"),
		"t-ci-run":  ci("workflow_dispatch"),
		teamToken:   {Subject: "cy", Login: "cy", Groups: []string{"showmino:billing"}},
		"t-mate":    {Subject: "dee", Login: "dee", Groups: []string{"showmino:billing"}},
		readerToken: {Subject: "bo", Login: "bo", Groups: []string{"dev"}},
	})
}

func TestApprovalCommands(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	serveApprovals(t, h)

	h.env["FORGE_TOKEN"] = "t-ci"
	r := h.run("approval", "request", "-o", "name")
	if r.code != 0 || !strings.HasPrefix(r.out, "approvals/apr_") {
		t.Fatalf("request: %s", r)
	}
	name := strings.TrimSpace(r.out)
	id := strings.TrimPrefix(name, "approvals/")

	h.env["FORGE_TOKEN"] = readerToken
	r = h.run("approval", "list", "--state", "pending")
	if r.code != 0 || !strings.Contains(r.out, id) || !strings.Contains(r.out, "infra-net") || !strings.Contains(r.out, "1a2b3c4") || !strings.Contains(r.out, "pending") {
		t.Fatalf("list: %s", r)
	}
	if r = h.run("approval", "approve", id); r.code != 5 {
		t.Errorf("a reader approves: %s", r)
	}

	// The one who started the run may not approve it.
	h.env["FORGE_TOKEN"] = teamToken
	if r = h.run("approval", "approve", id); r.code != 5 || !strings.Contains(r.err, "you started this run") {
		t.Errorf("self: %s", r)
	}
	h.env["FORGE_TOKEN"] = "t-mate"
	// Starting the run fails here: the approval stands, and says so.
	r = h.run("approval", "approve", name, "--reason", "plan reviewed")
	if r.code == 0 || !strings.Contains(r.err, "could not be started") || !strings.Contains(r.err, "approve "+id) {
		t.Errorf("approve: %s", r)
	}
	r = h.run("approval", "get", id)
	if r.code != 0 || !strings.Contains(r.out, "approved") || !strings.Contains(r.out, "dee") || !strings.Contains(r.out, "plan reviewed") {
		t.Errorf("get: %s", r)
	}

	h.env["FORGE_TOKEN"] = "t-ci-run"
	r = h.run("approval", "claim", id)
	if r.code != 0 || strings.TrimSpace(r.out) != "1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d" {
		t.Errorf("claim: %s", r)
	}
	if r = h.run("approval", "claim", id); r.code == 0 || !strings.Contains(r.err, "is claimed, not approved") {
		t.Errorf("claim twice: %s", r)
	}
	if r = h.run("approval", "get", "nope"); r.code != 2 {
		t.Errorf("bad name: %s", r)
	}
}

// exceptionGitHub opens every pull request, and plays #7 as merged with
// dee's approval of its last commit.
type exceptionGitHub struct{ repo *envtest.Repo }

func (g exceptionGitHub) ProposeFiles(context.Context, string, string, github.Proposal) (string, error) {
	return "https://github.com/showmino/forge-deploys/pull/7", nil
}
func (g exceptionGitHub) PullsForCommit(_ context.Context, _, _, sha string) ([]github.Pull, error) {
	return []github.Pull{{Number: 7, HTMLURL: "https://github.com/showmino/forge-deploys/pull/7", Merged: time.Now(), Base: "main", HeadSHA: "h7", MergeCommit: sha}}, nil
}
func (g exceptionGitHub) PullReviews(context.Context, string, string, int) ([]github.Review, error) {
	return []github.Review{{User: "dee", State: "APPROVED", CommitID: "h7"}}, nil
}
func (g exceptionGitHub) TeamMember(_ context.Context, _, slug, login string) (bool, error) {
	return slug == "admins" && login == "dee", nil
}

func TestApprovalExceptionCommands(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	repo := &envtest.Repo{}
	serveApprovalsWith(t, h, &server.ExceptionOptions{Repo: repo, Owner: "showmino", Name: "forge-deploys", GitHub: exceptionGitHub{repo}, AdminTeams: []string{"admins"}})

	h.env["FORGE_TOKEN"] = teamToken
	if r := h.run("approval", "exception", "request", "infra-net"); r.code != 2 || !strings.Contains(r.err, "--reason is required") {
		t.Errorf("no reason: %s", r)
	}
	r := h.run("approval", "exception", "request", "infra-net", "--reason", "I am alone")
	if r.code != 0 || !strings.Contains(r.out, "/pull/7") {
		t.Fatalf("request: %s", r)
	}
	if r = h.run("approval", "exception", "list"); r.code != 0 || !strings.Contains(r.err, "No exceptions") {
		t.Errorf("list before the merge: %s", r)
	}
	repo.Push("exceptions/infra-net.yaml", []byte("service: infra-net\nteam: billing\nreason: I am alone\nrequestedBy: cy\nrequestedAt: "+
		time.Now().UTC().Format(time.RFC3339)+"\n"), "Merge pull request #7")
	r = h.run("approval", "exception", "list")
	if r.code != 0 || !strings.Contains(r.out, "infra-net") || !strings.Contains(r.out, "yes") || !strings.Contains(r.out, "dee") {
		t.Errorf("list: %s", r)
	}
	r = h.run("service", "get", "infra-net")
	if r.code != 0 || !strings.Contains(r.out, "the team approves its own runs (granted by dee)") {
		t.Errorf("service get: %s", r)
	}
	// With it, the one who started the run approves it.
	h.env["FORGE_TOKEN"] = "t-ci"
	id := strings.TrimPrefix(strings.TrimSpace(h.run("approval", "request", "-o", "name").out), "approvals/")
	h.env["FORGE_TOKEN"] = teamToken
	if r = h.run("approval", "approve", id); !strings.Contains(r.err, "could not be started") {
		t.Errorf("self-approve with the exception: %s", r)
	}
}

func TestTemplateTargetsNone(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	r := h.run("template", "targets")
	if r.code != 0 || !strings.Contains(r.err, "No targets") {
		t.Errorf("targets: %s", r)
	}
	if r = h.run("template", "targets", "-o", "json"); r.code != 0 || !strings.Contains(r.out, `"targets": []`) {
		t.Errorf("json: %s", r)
	}
}
