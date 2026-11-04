package server

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"
	"gopkg.in/yaml.v3"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/github"
	"github.com/showmino/forge/internal/infrastructure"
	"github.com/showmino/forge/internal/store"
)

// ExceptionGitHub is what exceptions need of GitHub (*github.Client).
type ExceptionGitHub interface {
	ProposeFiles(ctx context.Context, owner, repo string, p github.Proposal) (string, error)
	PullsForCommit(ctx context.Context, owner, repo, sha string) ([]github.Pull, error)
	PullReviews(ctx context.Context, owner, repo string, number int) ([]github.Review, error)
	TeamMember(ctx context.Context, org, slug, login string) (bool, error)
}

var _ ExceptionGitHub = (*github.Client)(nil)

// ExceptionOptions configure the exception: a team of one may approve its
// own production runs once the platform team has merged its file.
type ExceptionOptions struct {
	// Repo reads the deploy repo's Branch, Owner/Name.
	Repo        DeployRepo
	Owner, Name string
	Branch      string
	GitHub      ExceptionGitHub
	// AdminTeams are the slugs of the teams (of ApprovalOptions.Org) whose
	// members grant exceptions: the admin groups "<org>:<slug>".
	AdminTeams []string
}

// exceptionDir is the deploy repo's folder of exceptions. forge-api never
// commits there: an exception holds only through a reviewed pull request.
const exceptionDir = "exceptions"

func exceptionPath(svc string) string { return exceptionDir + "/" + svc + ".yaml" }

// exceptionFile is exceptions/<service>.yaml.
type exceptionFile struct {
	Service     string    `yaml:"service"`
	Team        string    `yaml:"team"`
	Reason      string    `yaml:"reason"`
	RequestedBy string    `yaml:"requestedBy"`
	RequestedAt time.Time `yaml:"requestedAt"`
}

func (a *Approvals) exceptionsReady() error {
	if err := a.ready(); err != nil {
		return err
	}
	e := a.o.Exceptions
	switch {
	case e == nil || e.Repo == nil || e.GitHub == nil:
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("exceptions need the deploy repo and the GitHub App on this Forge API"))
	case len(e.AdminTeams) == 0:
		return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("exceptions need admin groups that are teams of %s (--admin-groups %s:<team>): their members grant them", a.o.Org, a.o.Org))
	}
	return nil
}

// exception reads the exception of svc and whether it holds: the file's
// last change on the branch belongs to a merged pull request whose last
// commit a member of an admin team approved. nil, nil without a file.
func (a *Approvals) exception(ctx context.Context, svc string) (*forgev1.Exception, error) {
	e := a.o.Exceptions
	data, err := e.Repo.Read(ctx, exceptionPath(svc), "")
	if errors.Is(err, github.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("read %s in the deploy repo: %w", exceptionPath(svc), err))
	}
	out := &forgev1.Exception{Service: servicePrefix + svc}
	var f exceptionFile
	if err := yaml.Unmarshal(data, &f); err != nil || f.Service != svc {
		out.Problem = fmt.Sprintf("%s is not an exception of %s", exceptionPath(svc), svc)
		return out, nil
	}
	out.Team, out.Reason, out.RequestedBy = f.Team, f.Reason, f.RequestedBy
	if !f.RequestedAt.IsZero() {
		out.RequestTime = timestamppb.New(f.RequestedAt)
	}
	hist, err := e.Repo.History(ctx, exceptionPath(svc), 1)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("read the history of %s: %w", exceptionPath(svc), err))
	}
	if len(hist) == 0 {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("%s has no history in the deploy repo", exceptionPath(svc)))
	}
	pulls, err := e.GitHub.PullsForCommit(ctx, e.Owner, e.Name, hist[0].SHA)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("read the pull request of %s: %w", shortSHA(hist[0].SHA), err))
	}
	out.Problem = fmt.Sprintf("its last change (%s) was not merged through a pull request", shortSHA(hist[0].SHA))
	for _, p := range pulls {
		if p.Merged.IsZero() || p.Base != e.Branch {
			continue
		}
		out.PullRequestUrl = p.HTMLURL
		by, err := a.adminApproval(ctx, p)
		if err != nil {
			return nil, err
		}
		if by == "" {
			out.Problem = fmt.Sprintf("%s was merged without an approval of its last commit by a member of %s", p.HTMLURL, strings.Join(e.AdminTeams, " or "))
			continue
		}
		out.Granted, out.GrantedBy, out.GrantTime, out.Problem = true, by, timestamppb.New(p.Merged), ""
		return out, nil
	}
	return out, nil
}

// adminApproval returns who of the admin teams approved p's last commit, or
// "" when none did.
func (a *Approvals) adminApproval(ctx context.Context, p github.Pull) (string, error) {
	e := a.o.Exceptions
	reviews, err := e.GitHub.PullReviews(ctx, e.Owner, e.Name, p.Number)
	if err != nil {
		return "", connect.NewError(connect.CodeUnavailable, fmt.Errorf("read the reviews of %s: %w", p.HTMLURL, err))
	}
	for _, r := range slices.Backward(reviews) {
		if r.State != "APPROVED" || r.CommitID != p.HeadSHA || r.User == "" {
			continue
		}
		for _, team := range e.AdminTeams {
			ok, err := e.GitHub.TeamMember(ctx, a.o.Org, team, r.User)
			if err != nil {
				return "", connect.NewError(connect.CodeUnavailable, fmt.Errorf("check whether %s is in %s: %w", r.User, team, err))
			}
			if ok {
				return r.User, nil
			}
		}
	}
	return "", nil
}

// selfApproval is ApprovalOptions.SelfApproval when exceptions are set up.
func (a *Approvals) selfApproval(ctx context.Context, svc string) (bool, error) {
	ex, err := a.exception(ctx, svc)
	if err != nil {
		return false, err
	}
	return ex != nil && ex.GetGranted(), nil
}

// exceptionService checks the caller may ask for or revoke the exception of
// name and returns its service, an infrastructure repo with production.
func (a *Approvals) exceptionService(ctx context.Context, name string) (*forgev1.Service, error) {
	if err := a.exceptionsReady(); err != nil {
		return nil, err
	}
	sid, ok := strings.CutPrefix(name, servicePrefix)
	if !ok || sid == "" {
		return nil, invalid(fmt.Sprintf("service %q must be services/<name>", name))
	}
	noteAuditService(ctx, name)
	if err := authorize(ctx, ActApprovalException, name); err != nil {
		return nil, err
	}
	svc, err := a.o.Catalog.get(ctx, name)
	if err != nil {
		return nil, err
	}
	if i := svc.GetInfrastructure(); i == nil || !slices.Contains(i.GetEnvironments(), infrastructure.Production) {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s is not an infrastructure repo with production: there is nothing to approve", name))
	}
	return svc, nil
}

func (a *Approvals) RequestException(ctx context.Context, req *connect.Request[forgev1.RequestExceptionRequest]) (*connect.Response[forgev1.RequestExceptionResponse], error) {
	svc, err := a.exceptionService(ctx, req.Msg.GetService())
	if err != nil {
		return nil, err
	}
	reason := strings.TrimSpace(req.Msg.GetReason())
	if reason == "" {
		return nil, invalid("reason is required: say why the team needs to approve its own runs")
	}
	sid := strings.TrimPrefix(svc.GetName(), servicePrefix)
	p, _ := PrincipalFrom(ctx)
	f := exceptionFile{Service: sid, Team: svc.GetOwnerTeam(), Reason: reason, RequestedBy: p.Login, RequestedAt: a.o.Now().UTC().Truncate(time.Second)}
	if f.RequestedBy == "" {
		f.RequestedBy = p.Subject
	}
	data, err := yaml.Marshal(f)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	e := a.o.Exceptions
	url, err := e.GitHub.ProposeFiles(ctx, e.Owner, e.Name, github.Proposal{
		Branch:  "forge/exception-" + sid,
		Title:   fmt.Sprintf("Exception: team %s approves its own production runs of %s", svc.GetOwnerTeam(), sid),
		Message: fmt.Sprintf("exception for %s, asked by %s\n\n%s", sid, f.RequestedBy, reason),
		Body: fmt.Sprintf("%s asks that team **%s** may approve its own production runs of **%s**, which Forge otherwise refuses "+
			"(the person who started a run may not approve it).\n\n> %s\n\nMerge this only if the team has nobody else who could approve. "+
			"It holds once merged with an approval by a member of %s (Forge checks on GitHub); revoke it with `forge approval exception revoke %s`.",
			f.RequestedBy, svc.GetOwnerTeam(), sid, reason, strings.Join(e.AdminTeams, " or "), sid),
		Files: []github.File{{Path: exceptionPath(sid), Content: data}},
	})
	if errors.Is(err, github.ErrFileExists) {
		return nil, connect.NewError(connect.CodeAlreadyExists, fmt.Errorf("%s has an exception already (%s); see forge approval exception list", svc.GetName(), exceptionPath(sid)))
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("open the pull request in %s/%s: %w", e.Owner, e.Name, err))
	}
	noteAuditDetail(ctx, "pull_request", url)
	return connect.NewResponse(&forgev1.RequestExceptionResponse{PullRequestUrl: url}), nil
}

func (a *Approvals) RevokeException(ctx context.Context, req *connect.Request[forgev1.RevokeExceptionRequest]) (*connect.Response[forgev1.RevokeExceptionResponse], error) {
	svc, err := a.exceptionService(ctx, req.Msg.GetService())
	if err != nil {
		return nil, err
	}
	sid := strings.TrimPrefix(svc.GetName(), servicePrefix)
	p, _ := PrincipalFrom(ctx)
	by := p.Login
	if by == "" {
		by = p.Subject
	}
	reason := strings.TrimSpace(req.Msg.GetReason())
	e := a.o.Exceptions
	url, err := e.GitHub.ProposeFiles(ctx, e.Owner, e.Name, github.Proposal{
		Branch:  "forge/revoke-exception-" + sid,
		Title:   fmt.Sprintf("Revoke the exception of %s", sid),
		Message: fmt.Sprintf("revoke the exception of %s, asked by %s\n\n%s", sid, by, reason),
		Body:    fmt.Sprintf("%s asks to remove the exception of **%s**: once merged, its team may no longer approve its own production runs.\n\n> %s", by, sid, reason),
		Remove:  []string{exceptionPath(sid)},
	})
	if errors.Is(err, github.ErrNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("%s has no exception", svc.GetName()))
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("open the pull request in %s/%s: %w", e.Owner, e.Name, err))
	}
	noteAuditDetail(ctx, "pull_request", url)
	return connect.NewResponse(&forgev1.RevokeExceptionResponse{PullRequestUrl: url}), nil
}

func (a *Approvals) ListExceptions(ctx context.Context, req *connect.Request[forgev1.ListExceptionsRequest]) (*connect.Response[forgev1.ListExceptionsResponse], error) {
	if err := a.exceptionsReady(); err != nil {
		return nil, err
	}
	if err := require(ctx, PermServicesRead); err != nil {
		return nil, err
	}
	var ids []string
	if s := req.Msg.GetService(); s != "" {
		id, ok := strings.CutPrefix(s, servicePrefix)
		if !ok || id == "" {
			return nil, invalid(fmt.Sprintf("service %q must be services/<name>", s))
		}
		ids = []string{id}
	} else {
		var err error
		if ids, err = a.o.Catalog.infrastructureIDs(ctx); err != nil {
			return nil, err
		}
	}
	out := &forgev1.ListExceptionsResponse{}
	for _, id := range ids {
		ex, err := a.exception(ctx, id)
		if err != nil {
			return nil, err
		}
		if ex != nil {
			out.Exceptions = append(out.Exceptions, ex)
		}
	}
	return connect.NewResponse(out), nil
}

// infrastructureIDs are the ids of the catalog's infrastructure repos, sorted.
func (c *Catalog) infrastructureIDs(ctx context.Context) ([]string, error) {
	var ids []string
	after := ""
	for {
		svcs, err := c.store.List(ctx, store.ListQuery{After: after, Limit: listAllPage})
		if err != nil {
			return nil, storeErr(err)
		}
		for _, s := range svcs {
			if s.GetInfrastructure() != nil {
				ids = append(ids, strings.TrimPrefix(s.GetName(), servicePrefix))
			}
		}
		if len(svcs) < listAllPage {
			return ids, nil
		}
		after = svcs[len(svcs)-1].GetName()
	}
}
