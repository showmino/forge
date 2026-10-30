package server

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"connectrpc.com/connect"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/github"
)

// A repo the fake GitHub can read: its default branch's files.
func (f *fakeRepos) addRepo(name string, files map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.trees == nil {
		f.trees = map[string]map[string]string{}
	}
	f.trees[name] = files
	f.repos[name] = nil
}

func (f *fakeRepos) DefaultBranch(_ context.Context, owner, repo string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.trees[repo]; !ok || owner != "showmino" {
		return "", fmt.Errorf("%s/%s: %w", owner, repo, github.ErrNotFound)
	}
	return "main", nil
}

func (f *fakeRepos) Tree(_ context.Context, _, repo, treeish string, recursive bool) (*github.Tree, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if treeish != "main" || !recursive {
		return nil, fmt.Errorf("tree %s recursive=%t", treeish, recursive)
	}
	t := &github.Tree{}
	for p := range f.trees[repo] {
		t.Entries = append(t.Entries, github.TreeEntry{Path: p, Type: "blob"})
	}
	return t, nil
}

func (f *fakeRepos) FileAt(_ context.Context, _, repo, path, ref string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.trees[repo][path]
	if !ok || ref != "main" {
		return nil, github.ErrNotFound
	}
	return []byte(c), nil
}

func connectRequest(repo string) *forgev1.CreateServiceRequest {
	return &forgev1.CreateServiceRequest{ServiceId: "tool", RepoUrl: "github.com/showmino/" + repo, OwnerTeam: "billing", RequestId: "c-" + repo}
}

func TestConnectRepo(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repos := newFakeRepos()
	repos.addRepo("tool", map[string]string{"Dockerfile": "FROM scratch", "main.go": ""})
	repos.give("tool", "billing", "push")
	e := newCreateEnv(t, nil, repos)

	// A dry run plans the files and does nothing.
	r := connectRequest("tool")
	r.ValidateOnly, r.RequestId = true, ""
	msgs, err := e.create(ctx, "member", r)
	if err != nil {
		t.Fatal(err)
	}
	if plan := msgs[len(msgs)-1].GetPlan(); plan == nil || strings.Join(plan.GetFiles(), ",") != "forge.yaml,.github/workflows/forge.yml,.github/workflows/forge-release.yml" || plan.GetDockerfile() != "" {
		t.Fatalf("plan: %v", msgs)
	}
	if has, _ := e.catalog.has(ctx, "services/tool"); has || len(repos.proposals) != 0 {
		t.Fatal("a dry run registered or proposed")
	}

	// A member of the team that can write to the repo connects it.
	r = connectRequest("tool")
	r.RunOptions = &forgev1.RunOptions{Port: 9090, Health: "/ready"}
	msgs, err = e.create(ctx, "member", r)
	if err != nil {
		t.Fatal(err)
	}
	got, last := steps(msgs)
	if want := "check_repo:DONE:github.com/showmino/tool on main, its own Dockerfile"; !strings.Contains(strings.Join(got, "\n"), want) {
		t.Errorf("steps: %v", got)
	}
	ch := last.GetChange()
	if ch == nil || ch.GetService().GetName() != "services/tool" || ch.GetService().GetRepoUrl() != "https://github.com/showmino/tool" ||
		ch.GetBranch() != "forge/connect" || ch.GetPullRequestUrl() != "https://github.com/showmino/tool/pull/1" || ch.GetDockerfile() != "" {
		t.Fatalf("change: %v", last)
	}
	s, err := e.catalog.get(ctx, "services/tool")
	if err != nil || s.GetOwnerTeam() != "billing" || s.GetRepoUrl() != "https://github.com/showmino/tool" {
		t.Errorf("catalog: %v %v", s, err)
	}
	p := repos.proposals[0]
	if p.repo != "showmino/tool" || p.Branch != "forge/connect" || !p.Fresh || len(p.Files) != 3 || !strings.Contains(string(p.Files[0].Content), "port: 9090") {
		t.Errorf("proposal: %+v", p)
	}
	// Forge changes no one's access to the repo.
	if repos.access["tool"]["billing"] != "push" {
		t.Errorf("access changed: %v", repos.access["tool"])
	}

	// The same repo again, under another name: it is a service's already.
	r = connectRequest("tool")
	r.ServiceId, r.RequestId = "tool2", "again"
	if _, err := e.create(ctx, "member", r); connect.CodeOf(err) != connect.CodeAlreadyExists || !strings.Contains(err.Error(), "the repo of services/tool") {
		t.Errorf("connect twice: %v", err)
	}
}

func TestConnectRepoProposesADockerfile(t *testing.T) {
	t.Parallel()
	repos := newFakeRepos()
	repos.addRepo("tool", map[string]string{"go.mod": "module x\ngo 1.25\n", "main.go": ""})
	repos.give("tool", "billing", "maintain")
	e := newCreateEnv(t, nil, repos)
	msgs, err := e.create(context.Background(), "member", connectRequest("tool"))
	if err != nil {
		t.Fatal(err)
	}
	ch := msgs[len(msgs)-1].GetChange()
	if ch.GetDockerfile() != "go" || !strings.Contains(strings.Join(ch.GetFiles(), ","), "Dockerfile") {
		t.Errorf("change: %v", ch)
	}
	if !strings.Contains(repos.proposals[0].Body, "proposes one for go") {
		t.Errorf("body: %s", repos.proposals[0].Body)
	}
}

func TestConnectRepoRefuses(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repos := newFakeRepos()
	repos.addRepo("tool", map[string]string{"Dockerfile": ""})
	repos.addRepo("theirs", map[string]string{"Dockerfile": ""})
	repos.addRepo("done", map[string]string{"Dockerfile": "", "forge.yaml": ""})
	repos.addRepo("docs", map[string]string{"README.md": ""})
	for _, r := range []string{"tool", "done", "docs"} {
		repos.give(r, "billing", "push")
	}
	repos.give("theirs", "billing", "pull")
	e := newCreateEnv(t, nil, repos)
	with := func(repo string, mut func(*forgev1.CreateServiceRequest)) *forgev1.CreateServiceRequest {
		r := connectRequest(repo)
		r.RequestId = ""
		if mut != nil {
			mut(r)
		}
		return r
	}
	for _, tc := range []struct {
		name, token string
		req         *forgev1.CreateServiceRequest
		code        connect.Code
		msg         string
	}{
		{"a reader", "reader", with("tool", nil), connect.CodePermissionDenied, "service.create"},
		{"a team that cannot write to it", "member", with("theirs", nil), connect.CodePermissionDenied, "cannot write to github.com/showmino/theirs"},
		{"outside the org", "member", with("tool", func(r *forgev1.CreateServiceRequest) { r.RepoUrl = "github.com/acme/tool" }), connect.CodeFailedPrecondition, "outside the showmino organization"},
		{"not a GitHub repo", "member", with("tool", func(r *forgev1.CreateServiceRequest) { r.RepoUrl = "gitlab.com/showmino/tool" }), connect.CodeInvalidArgument, "github.com/<owner>/<repo>"},
		{"a repo the App cannot see", "writer", with("missing", nil), connect.CodeFailedPrecondition, "install the App"},
		{"connected already", "member", with("done", nil), connect.CodeFailedPrecondition, "register it with forge service register"},
		{"nothing to build", "member", with("docs", nil), connect.CodeFailedPrecondition, "add a Dockerfile"},
		{"a template too", "member", with("tool", func(r *forgev1.CreateServiceRequest) { r.Template = "templates/svc" }), connect.CodeInvalidArgument, "one of a template, an image, a repo or a chart"},
		{"run options", "member", with("tool", func(r *forgev1.CreateServiceRequest) { r.RunOptions = &forgev1.RunOptions{Replicas: 2} }), connect.CodeInvalidArgument, "only port and health"},
	} {
		_, err := e.create(ctx, tc.token, tc.req)
		if connect.CodeOf(err) != tc.code || !strings.Contains(err.Error(), tc.msg) {
			t.Errorf("%s: %v, want %v with %q", tc.name, err, tc.code, tc.msg)
		}
	}
	if has, _ := e.catalog.has(ctx, "services/tool"); has || len(repos.proposals) != 0 {
		t.Error("a refused connect registered or proposed")
	}

	// An admin may connect a repo the team cannot write to.
	if _, err := e.create(ctx, "writer", with("theirs", nil)); err != nil {
		t.Errorf("an admin's connect: %v", err)
	}
}

// A pull request that cannot be opened takes the service out of the catalog again.
func TestConnectRepoRollsBack(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repos := newFakeRepos()
	repos.addRepo("tool", map[string]string{"Dockerfile": ""})
	repos.give("tool", "billing", "push")
	repos.proposeErr = fmt.Errorf("open: %w", github.ErrBranchExists)
	e := newCreateEnv(t, nil, repos)
	msgs, err := e.create(ctx, "member", connectRequest("tool"))
	if err != nil {
		t.Fatal(err)
	}
	f := msgs[len(msgs)-1].GetFailure()
	if f == nil || f.GetStep() != "pull_request" || !f.GetRolledBack() || !strings.Contains(f.GetMessage(), "forge/connect is in github.com/showmino/tool already") {
		t.Fatalf("failure: %v", msgs[len(msgs)-1])
	}
	if has, _ := e.catalog.has(ctx, "services/tool"); has {
		t.Error("the service stayed in the catalog")
	}
	// Fixed, the same connect works.
	repos.mu.Lock()
	repos.proposeErr = nil
	repos.mu.Unlock()
	r := connectRequest("tool")
	r.RequestId = "next"
	if msgs, err := e.create(ctx, "member", r); err != nil || msgs[len(msgs)-1].GetChange() == nil {
		t.Errorf("connect again: %v %v", msgs, err)
	}
}

// Two connects of one repo at once: the second waits its turn, so that no
// repo becomes the repo of two services.
func TestConnectClaimsTheRepo(t *testing.T) {
	t.Parallel()
	c := &creator{}
	release, err := c.claimRepo("showmino", "Tool")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.claimRepo("Showmino", "tool"); connect.CodeOf(err) != connect.CodeAborted || !strings.Contains(err.Error(), "being connected already") {
		t.Errorf("second claim: %v", err)
	}
	release()
	if _, err := c.claimRepo("showmino", "tool"); err != nil {
		t.Errorf("after release: %v", err)
	}
}

// A runner label goes into every connected repo's workflow: one label, plain.
func TestConnectRunnerLabels(t *testing.T) {
	t.Parallel()
	for label, ok := range map[string]bool{"ubuntu-latest": true, "kube-runners": true, "self-hosted, linux": false, "a\nb": false, "${{ x }}": false} {
		_, err := New(Options{Authenticator: StaticAuthenticator{}, CIRunner: label})
		if (err == nil) != ok {
			t.Errorf("%q: %v", label, err)
		}
	}
}

// A repo whose own Dockerfile runs as root is connected, with a note in the
// step and the pull request: the deploy would refuse its image.
func TestConnectRepoNotesARootDockerfile(t *testing.T) {
	t.Parallel()
	repos := newFakeRepos()
	repos.addRepo("tool", map[string]string{"Dockerfile": "FROM alpine\nCMD [\"./app\"]\n"})
	repos.give("tool", "billing", "push")
	e := newCreateEnv(t, nil, repos)
	msgs, err := e.create(context.Background(), "member", connectRequest("tool"))
	if err != nil {
		t.Fatal(err)
	}
	got, last := steps(msgs)
	if last.GetChange() == nil || !strings.Contains(strings.Join(got, "\n"), "sets no USER") {
		t.Errorf("steps: %v", got)
	}
	if !strings.Contains(repos.proposals[0].Body, "**Before merging:** its Dockerfile's last stage sets no USER") {
		t.Errorf("body: %s", repos.proposals[0].Body)
	}
}
