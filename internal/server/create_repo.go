package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"connectrpc.com/connect"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	forgeconnect "github.com/showmino/forge/internal/connect"
	"github.com/showmino/forge/internal/github"
	"github.com/showmino/forge/internal/manifest"
)

// A connected repo (phase 10) is an existing repo of the organization that
// Forge registers, and whose CI it adds in a pull request: forge.yaml,
// Forge's build-and-deploy workflow and, when the repo has none, a
// Dockerfile. Merging the pull request is the first deploy.

// RepoReader reads an existing repo. *github.Client implements it; a
// RepoHost that does too can connect repos.
type RepoReader interface {
	DefaultBranch(ctx context.Context, owner, repo string) (string, error)
	Tree(ctx context.Context, owner, repo, treeish string, recursive bool) (*github.Tree, error)
	FileAt(ctx context.Context, owner, repo, path, ref string) ([]byte, error)
}

var _ RepoReader = (*github.Client)(nil)

// githubName is a GitHub owner or repo name.
var githubName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)

// connectBranch is the branch of the pull request that connects a repo.
const connectBranch = "forge/connect"

// parseRepoURL reads "github.com/<owner>/<repo>" (with or without https://
// and .git) as the owner and the repo.
func parseRepoURL(s string) (owner, repo string, ok bool) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(strings.TrimPrefix(s, "https://"), "http://")
	rest, ok := strings.CutPrefix(s, "github.com/")
	if !ok {
		return "", "", false
	}
	owner, repo, ok = strings.Cut(strings.TrimSuffix(strings.TrimSuffix(rest, "/"), ".git"), "/")
	if !ok || owner == "" || repo == "" || strings.Contains(repo, "/") || !githubName.MatchString(owner) || !githubName.MatchString(repo) {
		return "", "", false
	}
	return owner, repo, true
}

// connectReady says why this Forge API cannot connect repos, or nil.
func (c *creator) connectReady() (RepoReader, error) {
	reader, ok := c.repos.(RepoReader)
	if c.repos == nil || !ok || c.org == "" {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("connecting repos is not configured on this Forge API (no GitHub App)"))
	}
	return reader, nil
}

// fromRepo connects an existing repo. Steps: check_repo, render, then
// (unless validate_only) register and pull_request.
func (c *creator) fromRepo(ctx context.Context, req *forgev1.CreateServiceRequest, stream createStream) (*forgev1.CreateServiceResponse, error) {
	w := &sender{s: stream}
	started := func(step string) { w.send(stepMsg(step, forgev1.CreateStep_STATE_STARTED, "")) }
	done := func(step, detail string) { w.send(stepMsg(step, forgev1.CreateStep_STATE_DONE, detail)) }
	id := req.GetServiceId()
	reader, err := c.connectReady()
	if err != nil {
		return nil, err
	}
	owner, repo, ok := parseRepoURL(req.GetRepoUrl())
	if !ok {
		return nil, invalid(fmt.Sprintf("repo_url %q must be github.com/<owner>/<repo>", req.GetRepoUrl()))
	}
	if !strings.EqualFold(owner, c.org) {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("github.com/%s/%s is outside the %s organization: Forge connects only its repos", owner, repo, c.org))
	}
	owner = c.org
	repoURL := "https://github.com/" + owner + "/" + repo
	// The team goes into forge.yaml as it is: only a GitHub team slug.
	if !manifest.ValidID(req.GetOwnerTeam()) {
		return nil, invalid(fmt.Sprintf("owner_team %q must be a GitHub team slug (a-z, 0-9, '-')", req.GetOwnerTeam()))
	}
	release, err := c.claimName(id)
	if err != nil {
		return nil, err
	}
	defer release()
	releaseRepo, err := c.claimRepo(owner, repo)
	if err != nil {
		return nil, err
	}
	defer releaseRepo()
	if exists, err := c.catalog.has(ctx, servicePrefix+id); err != nil {
		return nil, err
	} else if exists {
		return nil, connect.NewError(connect.CodeAlreadyExists, fmt.Errorf("%s%s already exists", servicePrefix, id))
	}
	if names, err := c.catalog.servicesForRepo(ctx, owner, repo); err != nil {
		return nil, err
	} else if len(names) > 0 {
		return nil, connect.NewError(connect.CodeAlreadyExists, fmt.Errorf("github.com/%s/%s is the repo of %s%s already", owner, repo, servicePrefix, names[0]))
	}

	started("check_repo")
	// Only a repo the owner team can write to, unless an admin connects it:
	// the service is the team's, and its deploys run what the repo holds.
	// Forge changes no one's access to the repo.
	p, _ := PrincipalFrom(ctx)
	admin := isAdmin(p)
	if !admin {
		role, err := c.repos.TeamRepoPermission(ctx, c.org, req.GetOwnerTeam(), owner, repo)
		if err != nil {
			return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("read team %s's access to github.com/%s/%s: %w", req.GetOwnerTeam(), owner, repo, err))
		}
		if slices.Index(repoRoles, role) < slices.Index(repoRoles, "push") {
			return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf(
				"the team %s cannot write to github.com/%s/%s: only a repo the team can write to may become its service; ask a Forge admin to connect it", req.GetOwnerTeam(), owner, repo))
		}
	}
	branch, err := reader.DefaultBranch(ctx, owner, repo)
	if errors.Is(err, github.ErrNotFound) {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("github.com/%s/%s is not there, or the Forge GitHub App cannot read it: install the App on the repo", owner, repo))
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("read github.com/%s/%s: %w", owner, repo, err))
	}
	tree, err := reader.Tree(ctx, owner, repo, branch, true)
	var se *github.StatusError
	if errors.As(err, &se) && (se.Status == http.StatusConflict || se.Status == http.StatusNotFound) {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("github.com/%s/%s has no commits on %s: push its code first", owner, repo, branch))
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("read github.com/%s/%s: %w", owner, repo, err))
	}
	paths := make([]string, 0, len(tree.Entries))
	for _, e := range tree.Entries {
		if e.Type == "blob" {
			paths = append(paths, e.Path)
		}
	}
	var registry string
	if c.deploys != nil {
		registry = c.deploys.o.Registry
	}
	ro := req.GetRunOptions()
	plan, err := forgeconnect.Plan(forgeconnect.Input{
		Service: id, Team: req.GetOwnerTeam(), Owners: req.GetOwners(),
		Owner: owner, RepoURL: repoURL,
		Port: int(ro.GetPort()), Health: ro.GetHealth(),
		Registry: registry, Runner: c.ciRunner, DeployRunner: c.ciDeployRunner,
		Paths: paths,
		Read:  func(path string) ([]byte, error) { return reader.FileAt(ctx, owner, repo, path, branch) },
	})
	switch {
	case errors.Is(err, forgeconnect.ErrConnected):
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("github.com/%s/%s: %w; register it with forge service register", owner, repo, err))
	case errors.Is(err, forgeconnect.ErrNoDockerfile):
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("github.com/%s/%s: %w", owner, repo, err))
	case err != nil:
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	dockerfile := "its own Dockerfile"
	if plan.Language != "" {
		dockerfile = "no Dockerfile: Forge proposes one for " + plan.Language
	}
	if plan.UserNote != "" {
		dockerfile += "; " + plan.UserNote
	}
	done("check_repo", fmt.Sprintf("github.com/%s/%s on %s, %s", owner, repo, branch, dockerfile))

	started("render")
	files := make([]string, len(plan.Files))
	push := make([]github.File, len(plan.Files))
	for i, f := range plan.Files {
		files[i] = f.Path
		push[i] = github.File{Path: f.Path, Content: f.Content}
	}
	m, err := manifest.Parse(plan.Files[0].Content)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	svc := serviceFromManifest(m)
	if _, err := c.catalog.register(ctx, connect.NewRequest(&forgev1.RegisterServiceRequest{ServiceId: id, Service: svc, ValidateOnly: true}), false); err != nil {
		return nil, err
	}
	done("render", fmt.Sprintf("%d files", len(files)))

	if req.GetValidateOnly() {
		final := &forgev1.CreateServiceResponse{Event: &forgev1.CreateServiceResponse_Plan{Plan: &forgev1.CreatePlan{RepoUrl: repoURL, Files: files, Dockerfile: plan.Language}}}
		w.send(final)
		return final, w.err
	}

	// From here on the work outlives the client, as a template's create does.
	work := context.WithoutCancel(ctx)
	started("register")
	resp, err := c.catalog.register(work, connect.NewRequest(&forgev1.RegisterServiceRequest{ServiceId: id, Service: svc}), false)
	if err != nil {
		return nil, err
	}
	done("register", servicePrefix+id)

	started("pull_request")
	body := fmt.Sprintf("Connects this repo to Forge as %s%s, for the team %s (asked by %s).\n\n"+
		"Merging it is the first deploy: the workflow `forge` builds the image, and on main deploys it to staging. "+
		"Production is a promotion (`forge deploy promote %s`).\n\nFiles:\n- %s\n",
		servicePrefix, id, req.GetOwnerTeam(), actorOf(ctx), id, strings.Join(files, "\n- "))
	if plan.Language != "" {
		body += fmt.Sprintf("\nThe repo has no Dockerfile, so Forge proposes one for %s. Check it before merging: it runs as a non-root user with a read-only filesystem (only /tmp is writable), and the app must listen on $PORT.\n", plan.Language)
	}
	if plan.UserNote != "" {
		body += "\n**Before merging:** " + plan.UserNote + ". Forge runs every service as a non-root user with a read-only filesystem (only /tmp is writable); a deploy of an image that runs as root is refused.\n"
	}
	if plan.RegistrySecrets {
		body += fmt.Sprintf("\nThe workflow logs in to %s with the repo secrets FORGE_REGISTRY_USERNAME and FORGE_REGISTRY_PASSWORD: add them before merging.\n", registry)
	}
	body += "\nThe deploy job needs the organization variable FORGE_API_URL, and a runner that reaches the Forge API there.\n"
	url, err := c.repos.ProposeFiles(work, owner, repo, github.Proposal{
		Branch: connectBranch, Title: "Connect " + id + " to Forge", Body: body,
		Message: "Connect " + id + " to Forge\n\nAdded by Forge: forge.yaml and its build-and-deploy workflow.",
		Files:   push,
		// Anyone with push may have made forge/connect: never take it as ours.
		Fresh: true,
	})
	if err != nil {
		// Without the pull request the service cannot deploy: take it out
		// of the catalog again, so that the next connect starts clean.
		delErr := c.catalog.forget(work, servicePrefix+id)
		c.log.WarnContext(ctx, "connect failed", "service", id, "repo", owner+"/"+repo, "error", err, "rolled_back", delErr == nil, "rollback_error", delErr)
		msg := err.Error()
		if errors.Is(err, github.ErrBranchExists) {
			msg = fmt.Sprintf("the branch %s is in github.com/%s/%s already: merge or delete its pull request, then connect again", connectBranch, owner, repo)
		}
		final := &forgev1.CreateServiceResponse{Event: &forgev1.CreateServiceResponse_Failure{Failure: &forgev1.CreateFailure{
			Step: "pull_request", Message: msg, RolledBack: delErr == nil,
		}}}
		noteCreateFailure(ctx, final)
		w.send(final)
		return final, nil
	}
	done("pull_request", url)

	c.log.InfoContext(ctx, "repo connected", "service", id, "repo", owner+"/"+repo, "pull_request", url, "dockerfile", plan.Language, "by", p.Subject)
	final := &forgev1.CreateServiceResponse{Event: &forgev1.CreateServiceResponse_Change{Change: &forgev1.ProposedChange{
		RepoUrl: repoURL, Branch: connectBranch, PullRequestUrl: url, Files: files,
		Service: resp.Msg.GetService(), Dockerfile: plan.Language, NeedsRegistrySecrets: plan.RegistrySecrets,
	}}}
	w.send(final)
	return final, nil
}

// runnerLabel is one GitHub Actions runner label, written into a connected
// repo's workflow as runs-on.
var runnerLabel = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)
