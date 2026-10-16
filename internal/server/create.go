package server

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"strings"
	"sync"

	"connectrpc.com/connect"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/gen/forge/v1/forgev1connect"
	"github.com/showmino/forge/internal/github"
	"github.com/showmino/forge/internal/manifest"
	"github.com/showmino/forge/internal/template"
)

// RepoHost creates, fills and deletes the repos of new services: GitHub, or
// a fake in tests.
type RepoHost interface {
	RepoExists(ctx context.Context, owner, name string) (bool, error)
	CreateOrgRepo(ctx context.Context, org, name, description string) (*github.Repo, error)
	DeleteRepo(ctx context.Context, owner, name string) error
	PushFiles(ctx context.Context, owner, repo, branch, message string, files []github.File) (string, error)
	// ProposeFiles opens a pull request that adds files; for file templates.
	ProposeFiles(ctx context.Context, owner, repo string, p github.Proposal) (string, error)
}

var _ RepoHost = (*github.Client)(nil)

// catalogService adds CreateService to the Catalog's handlers.
type catalogService struct {
	*Catalog
	create *creator
}

var _ forgev1connect.ServiceCatalogServiceHandler = (*catalogService)(nil)

func (s *catalogService) CreateService(ctx context.Context, req *connect.Request[forgev1.CreateServiceRequest], stream *connect.ServerStream[forgev1.CreateServiceResponse]) error {
	return s.create.run(ctx, req.Msg, stream)
}

// createStream is the part of the server stream the pipeline uses.
type createStream interface {
	Send(*forgev1.CreateServiceResponse) error
}

// maxParams caps the number of parameters in one request.
const maxParams = 100

// creator runs CreateService: render a template, create the repo, push the
// first commit and register the service.
type creator struct {
	catalog   *Catalog
	templates TemplateSource
	repos     RepoHost // nil: repos cannot be created; validate_only still works
	org       string
	log       *slog.Logger

	mu sync.Mutex
	// results tracks each create by request_id.
	results map[string]*outcome
}

// outcome is one create's result: done closes when it ends, and final is
// its last message (nil if it ended before any side effect).
type outcome struct {
	done  chan struct{}
	final *forgev1.CreateServiceResponse
}

func validateCreate(req *forgev1.CreateServiceRequest) error {
	if !manifest.ValidID(req.GetServiceId()) {
		return invalid(fmt.Sprintf("service_id %q must be a lowercase DNS label (a-z, 0-9, '-', max 63)", req.GetServiceId()))
	}
	if !manifest.ValidID(strings.TrimPrefix(req.GetTemplate(), templatePrefix)) {
		return invalid(fmt.Sprintf("template %q must look like templates/{template}", req.GetTemplate()))
	}
	if strings.TrimSpace(req.GetOwnerTeam()) == "" {
		return invalid("owner_team is required")
	}
	for _, o := range req.GetOwners() {
		if strings.TrimSpace(o) == "" {
			return invalid("owners must not be empty strings")
		}
	}
	if len(req.GetParameters()) > maxParams {
		return invalid(fmt.Sprintf("at most %d parameters", maxParams))
	}
	for k, v := range req.GetParameters() {
		if len(v) > template.MaxParamLen {
			return invalid(fmt.Sprintf("parameter %q is longer than %d bytes", k, template.MaxParamLen))
		}
	}
	return nil
}

// run checks the caller and the request, applies request_id idempotency and
// runs the pipeline.
func (c *creator) run(ctx context.Context, req *forgev1.CreateServiceRequest, stream createStream) error {
	if _, ok := PrincipalFrom(ctx); !ok {
		return require(ctx, PermServicesWrite)
	}
	if err := validateCreate(req); err != nil {
		return err
	}
	if err := authorizeTeam(ctx, ActServiceCreate, req.GetOwnerTeam()); err != nil {
		return err
	}
	if c.templates == nil {
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("templates are not configured on this Forge API"))
	}
	if c.repos == nil && !req.GetValidateOnly() {
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("creating repos is not configured on this Forge API (no GitHub App)"))
	}
	key := req.GetRequestId()
	if key == "" || req.GetValidateOnly() {
		_, err := c.create(ctx, req, stream)
		return err
	}
	// A retry of a create that is still running waits for it and replays its
	// outcome; one that ended before any side effect is started over.
	var o *outcome
	for o == nil {
		c.mu.Lock()
		running, seen := c.results[key]
		if !seen {
			o = &outcome{done: make(chan struct{})}
			c.results[key] = o
		}
		c.mu.Unlock()
		if !seen {
			break
		}
		select {
		case <-running.done:
		case <-ctx.Done():
			return connect.NewError(connect.CodeDeadlineExceeded, fmt.Errorf("waiting for the create with this request_id: %w", ctx.Err()))
		}
		if running.final != nil {
			noteCreateFailure(ctx, running.final) // a replayed failure is a failure
			return stream.Send(running.final)
		}
	}
	var final *forgev1.CreateServiceResponse
	defer func() { // also when create panics: never leave the key stuck
		c.mu.Lock()
		if final != nil {
			o.final = final
		} else {
			delete(c.results, key) // nothing happened yet: a retry starts over
		}
		c.mu.Unlock()
		close(o.done)
	}()
	final, err := c.create(ctx, req, stream)
	return err
}

// create runs the pipeline. It returns the last message (nil when it ended
// with an error before any side effect).
func (c *creator) create(ctx context.Context, req *forgev1.CreateServiceRequest, stream createStream) (*forgev1.CreateServiceResponse, error) {
	return c.scaffold(ctx, req, stream)
}

// checkFree checks that a new service's name is free in the catalog and on GitHub.
func (c *creator) checkFree(ctx context.Context, id string) error {
	exists, err := c.catalog.has(ctx, servicePrefix+id)
	if err != nil {
		return err
	}
	if exists {
		return connect.NewError(connect.CodeAlreadyExists, fmt.Errorf("%s%s already exists", servicePrefix, id))
	}
	if c.repos != nil {
		exists, err = c.repos.RepoExists(ctx, c.org, id)
		if err != nil {
			return connect.NewError(connect.CodeUnavailable, fmt.Errorf("check github.com/%s/%s: %w", c.org, id, err))
		}
		if exists {
			return connect.NewError(connect.CodeAlreadyExists, fmt.Errorf("github.com/%s/%s already exists", c.org, id))
		}
	}
	return nil
}

// sender sends stream messages and remembers the first send error: once
// side effects began, the pipeline continues even if the client is gone.
type sender struct {
	s   createStream
	err error
}

func (w *sender) send(m *forgev1.CreateServiceResponse) {
	if w.err == nil {
		w.err = w.s.Send(m)
	}
}

func stepMsg(name string, state forgev1.CreateStep_State, detail string) *forgev1.CreateServiceResponse {
	return &forgev1.CreateServiceResponse{Event: &forgev1.CreateServiceResponse_Step{Step: &forgev1.CreateStep{Name: name, State: state, Detail: detail}}}
}

// scaffold runs fetch, render and (unless validate_only) create_repo, push
// and register.
func (c *creator) scaffold(ctx context.Context, req *forgev1.CreateServiceRequest, stream createStream) (*forgev1.CreateServiceResponse, error) {
	w := &sender{s: stream}
	started := func(step string) { w.send(stepMsg(step, forgev1.CreateStep_STATE_STARTED, "")) }
	done := func(step, detail string) { w.send(stepMsg(step, forgev1.CreateStep_STATE_DONE, detail)) }
	id := req.GetServiceId()
	tname := strings.TrimPrefix(req.GetTemplate(), templatePrefix)
	repoURL := "https://github.com/" + c.org + "/" + id

	started("fetch")
	version, tree, err := resolveRelease(ctx, c.templates, req.GetTemplateVersion())
	if err != nil {
		return nil, err
	}
	t, err := templateFromRelease(tree, tname, version)
	if err != nil {
		return nil, err
	}
	skeleton, err := fs.Sub(tree, path.Join(tname, template.SkeletonDir))
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if t.IsFile() {
		return c.propose(ctx, req, t, version, skeleton, w)
	}
	if err := c.checkFree(ctx, id); err != nil {
		return nil, err
	}
	done("fetch", tname+"@"+version)

	started("render")
	v := template.Values{
		Service:  template.ServiceValues{Name: id, Team: req.GetOwnerTeam(), Owners: req.GetOwners()},
		Repo:     template.RepoValues{Owner: c.org, Name: id, URL: repoURL},
		Template: template.TemplateValues{Name: tname, Version: version},
	}
	params, err := t.ResolveParams(v, req.GetParameters())
	var pe *template.ParamError
	if errors.As(err, &pe) {
		return nil, invalid(pe.Error())
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	v.Params = params
	files, err := t.Render(skeleton, v)
	if err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s@%s does not render: %w", tname, version, err))
	}
	if err := template.CheckManifest(files, id, req.GetOwnerTeam()); err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s@%s: %w", tname, version, err))
	}
	if ro := req.GetRunOptions(); ro != nil {
		if files, err = applyRunOptions(files, t, ro); err != nil {
			return nil, err
		}
	}
	var m *manifest.Manifest
	for _, f := range files {
		if f.Path == manifest.FileName {
			m, _ = manifest.Parse(f.Data) // CheckManifest parsed it already
		}
	}
	// What registration would refuse (an unknown dependency, an invalid
	// field) is found now, before anything is created.
	svc := serviceFromManifest(m)
	svc.RepoUrl = repoURL
	if _, err := c.catalog.register(ctx, connect.NewRequest(&forgev1.RegisterServiceRequest{ServiceId: id, Service: svc, ValidateOnly: true})); err != nil {
		return nil, err
	}
	done("render", fmt.Sprintf("%d files", len(files)))

	if req.GetValidateOnly() {
		plan := &forgev1.CreatePlan{TemplateVersion: version, RepoUrl: repoURL}
		for _, f := range files {
			plan.Files = append(plan.Files, f.Path)
		}
		final := &forgev1.CreateServiceResponse{Event: &forgev1.CreateServiceResponse_Plan{Plan: plan}}
		w.send(final)
		return final, w.err
	}

	// From here on the work outlives the client: the outcome is stored for
	// its retry. The HTTP client's timeout bounds every GitHub call.
	work := context.WithoutCancel(ctx)
	started("create_repo")
	repo, err := c.repos.CreateOrgRepo(work, c.org, id, fmt.Sprintf("%s, created by forge create from %s@%s", t.Title, tname, version))
	if errors.Is(err, github.ErrRepoExists) {
		return nil, connect.NewError(connect.CodeAlreadyExists, fmt.Errorf("github.com/%s/%s already exists", c.org, id))
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("create github.com/%s/%s: %w", c.org, id, err))
	}
	done("create_repo", "github.com/"+c.org+"/"+id)

	fail := func(step string, cause error) (*forgev1.CreateServiceResponse, error) {
		delErr := c.repos.DeleteRepo(work, c.org, id)
		c.log.WarnContext(ctx, "create failed", "service", id, "step", step, "error", cause, "rolled_back", delErr == nil, "rollback_error", delErr)
		final := &forgev1.CreateServiceResponse{Event: &forgev1.CreateServiceResponse_Failure{Failure: &forgev1.CreateFailure{
			Step: step, Message: cause.Error(), RolledBack: delErr == nil,
		}}}
		noteCreateFailure(ctx, final)
		w.send(final)
		return final, nil
	}

	started("push")
	pushFiles := make([]github.File, 0, len(files))
	for _, f := range files {
		pushFiles = append(pushFiles, github.File{Path: f.Path, Executable: f.Mode&0o111 != 0, Content: f.Data})
	}
	sha, err := c.repos.PushFiles(work, c.org, id, repo.DefaultBranch, fmt.Sprintf("Scaffold %s from %s@%s\n\nCreated with forge create.", id, tname, version), pushFiles)
	if err != nil {
		return fail("push", err)
	}
	done("push", shortSHA(sha)+" (CI started)")

	started("register")
	resp, err := c.catalog.register(work, connect.NewRequest(&forgev1.RegisterServiceRequest{ServiceId: id, Service: svc}))
	if err != nil {
		return fail("register", err)
	}
	done("register", servicePrefix+id)

	p, _ := PrincipalFrom(ctx)
	c.log.InfoContext(ctx, "service created", "service", id, "template", tname, "version", version, "repo", repoURL, "by", p.Subject)
	final := &forgev1.CreateServiceResponse{Event: &forgev1.CreateServiceResponse_Service{Service: resp.Msg.GetService()}}
	w.send(final)
	return final, nil
}

// propose runs a file template: render, then open a pull request that adds
// the files to the template's target repo. Nothing is registered.
func (c *creator) propose(ctx context.Context, req *forgev1.CreateServiceRequest, t *template.Template, version string, skeleton fs.FS, w *sender) (*forgev1.CreateServiceResponse, error) {
	started := func(step string) { w.send(stepMsg(step, forgev1.CreateStep_STATE_STARTED, "")) }
	done := func(step, detail string) { w.send(stepMsg(step, forgev1.CreateStep_STATE_DONE, detail)) }
	id := req.GetServiceId()
	owner, repo, _ := strings.Cut(t.Target, "/")
	if !strings.EqualFold(owner, c.org) {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s@%s targets %s, outside the %s organization", t.Name, version, t.Target, c.org))
	}
	// The team goes into the file as it is: only a GitHub team slug.
	if !manifest.ValidID(req.GetOwnerTeam()) {
		return nil, invalid(fmt.Sprintf("owner_team %q must be a GitHub team slug (a-z, 0-9, '-')", req.GetOwnerTeam()))
	}
	if req.GetRunOptions() != nil {
		return nil, invalid("run_options are only for a service template")
	}
	repoURL := "https://github.com/" + t.Target
	done("fetch", t.Name+"@"+version)

	started("render")
	v := template.Values{
		Service:  template.ServiceValues{Name: id, Team: req.GetOwnerTeam(), Owners: req.GetOwners()},
		Repo:     template.RepoValues{Owner: owner, Name: repo, URL: repoURL},
		Template: template.TemplateValues{Name: t.Name, Version: version},
	}
	params, err := t.ResolveParams(v, req.GetParameters())
	var pe *template.ParamError
	if errors.As(err, &pe) {
		return nil, invalid(pe.Error())
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	v.Params = params
	files, err := t.Render(skeleton, v)
	if err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s@%s does not render: %w", t.Name, version, err))
	}
	if len(files) == 0 {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s@%s renders no files with these parameters", t.Name, version))
	}
	if err := template.CheckFilePaths(files); err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s@%s: %w", t.Name, version, err))
	}
	paths := make([]string, len(files))
	for i, f := range files {
		paths[i] = f.Path
	}
	done("render", fmt.Sprintf("%d files", len(files)))

	if req.GetValidateOnly() {
		final := &forgev1.CreateServiceResponse{Event: &forgev1.CreateServiceResponse_Plan{Plan: &forgev1.CreatePlan{TemplateVersion: version, RepoUrl: repoURL, Files: paths}}}
		w.send(final)
		return final, w.err
	}

	started("pull_request")
	branch := "forge/" + t.Name + "/" + id
	p, _ := PrincipalFrom(ctx)
	pushFiles := make([]github.File, 0, len(files))
	for _, f := range files {
		pushFiles = append(pushFiles, github.File{Path: f.Path, Executable: f.Mode&0o111 != 0, Content: f.Data})
	}
	url, err := c.repos.ProposeFiles(context.WithoutCancel(ctx), owner, repo, github.Proposal{
		Branch:  branch,
		Title:   fmt.Sprintf("%s: %s", t.Title, id),
		Body:    fmt.Sprintf("Made with Forge from %s@%s by %s, for the team %s.\n\nFiles:\n- %s\n", t.Name, version, actorOf(ctx), req.GetOwnerTeam(), strings.Join(paths, "\n- ")),
		Message: fmt.Sprintf("Add %s from %s@%s\n\nMade with Forge.", id, t.Name, version),
		Files:   pushFiles,
	})
	if errors.Is(err, github.ErrFileExists) || errors.Is(err, github.ErrBranchExists) {
		return nil, connect.NewError(connect.CodeAlreadyExists, err)
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("open the pull request in %s: %w", t.Target, err))
	}
	done("pull_request", url)

	c.log.InfoContext(ctx, "change proposed", "name", id, "template", t.Name, "version", version, "repo", t.Target, "pull_request", url, "by", p.Subject)
	final := &forgev1.CreateServiceResponse{Event: &forgev1.CreateServiceResponse_Change{Change: &forgev1.ProposedChange{
		RepoUrl: repoURL, Branch: branch, PullRequestUrl: url, Files: paths,
	}}}
	w.send(final)
	return final, nil
}

// noteCreateFailure notes a Failure message on the call's audit record, as
// "<step>: <message> (rolled back: true|false)"; other messages are ignored.
func noteCreateFailure(ctx context.Context, m *forgev1.CreateServiceResponse) {
	if f := m.GetFailure(); f != nil {
		noteAuditFailure(ctx, fmt.Sprintf("%s: %s (rolled back: %t)", f.GetStep(), f.GetMessage(), f.GetRolledBack()))
	}
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// serviceFromManifest is the catalog entry a rendered forge.yaml describes.
func serviceFromManifest(m *manifest.Manifest) *forgev1.Service {
	s := &forgev1.Service{
		DisplayName: m.Service.DisplayName,
		OwnerTeam:   m.Service.Team,
		Owners:      m.Service.Owners,
		RepoUrl:     m.Service.Repo,
		Labels:      m.Service.Labels,
	}
	switch m.Service.Lifecycle {
	case "experimental":
		s.Lifecycle = forgev1.Lifecycle_LIFECYCLE_EXPERIMENTAL
	case "production":
		s.Lifecycle = forgev1.Lifecycle_LIFECYCLE_PRODUCTION
	case "deprecated":
		s.Lifecycle = forgev1.Lifecycle_LIFECYCLE_DEPRECATED
	}
	for _, d := range m.Service.Dependencies {
		s.Dependencies = append(s.Dependencies, servicePrefix+strings.TrimPrefix(d, servicePrefix))
	}
	return s
}
