package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/gen/forge/v1/forgev1connect"
	"github.com/showmino/forge/internal/github"
	"github.com/showmino/forge/internal/infrastructure"
	"github.com/showmino/forge/internal/store"
)

const approvalPrefix = "approvals/"

// Dispatcher starts a repo's workflow and reads its default branch and
// that branch's head (*github.Client on GitHub).
type Dispatcher interface {
	DispatchWorkflow(ctx context.Context, owner, repo, workflow, ref string, inputs map[string]string) error
	DefaultBranch(ctx context.Context, owner, repo string) (string, error)
	ResolveRef(ctx context.Context, owner, repo, ref string) (string, error)
	// RunActors returns who started a run and who started its attempt.
	RunActors(ctx context.Context, owner, repo string, runID, attempt int64) (actor, triggering string, err error)
}

var _ Dispatcher = (*github.Client)(nil)

// ApprovalOptions configure approvals.
type ApprovalOptions struct {
	Store   store.ApprovalStore
	Catalog *Catalog
	// Infra names the shared workflow (the only one that may ask and claim)
	// and the repo's workflow Forge starts on approval.
	Infra *infrastructure.Config
	// Dispatch reaches GitHub; without it nothing can be asked for, as the
	// asking commit cannot be checked against its branch.
	Dispatch Dispatcher
	Org      string
	// Exceptions let a team of one approve its own runs; nil: no exceptions.
	Exceptions *ExceptionOptions
	// SelfApproval reports whether the team of the service may approve its
	// own runs; nil: the exception decides (never without Exceptions).
	SelfApproval func(ctx context.Context, service string) (bool, error)
	Now          func() time.Time
	Log          *slog.Logger
}

// Approvals is the ApprovalService: production runs of infrastructure repos
// wait for an approval by the owning team (phase 12).
type Approvals struct {
	o ApprovalOptions
}

var _ forgev1connect.ApprovalServiceHandler = (*Approvals)(nil)

// NewApprovals returns the approval service.
func NewApprovals(o ApprovalOptions) *Approvals {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.Exceptions != nil && o.Exceptions.Branch == "" {
		o.Exceptions.Branch = "main"
	}
	a := &Approvals{o: o}
	if a.o.SelfApproval == nil && o.Exceptions != nil {
		a.o.SelfApproval = a.selfApproval
	}
	return a
}

// ready reports whether this API has approvals.
func (a *Approvals) ready() error {
	if a == nil {
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("approvals are not configured on this Forge API (no --infrastructure-config)"))
	}
	return nil
}

// fromSharedWorkflow checks a CI caller is a job of the shared workflow, in
// an infrastructure repo of the catalog, run by event, and returns its
// service. Only a push asks, so its actor is who pushed the commit; only a
// workflow_dispatch (Forge's, on approval) claims.
func (a *Approvals) fromSharedWorkflow(ctx context.Context, event string) (*Workload, *forgev1.Service, error) {
	w := workloadOf(ctx)
	if w == nil {
		return nil, nil, connect.NewError(connect.CodePermissionDenied, errors.New("only a job of the shared workflow, with its GitHub Actions token, may do this"))
	}
	if w.JobWorkflowRef != a.o.Infra.Workflow {
		return nil, nil, workloadDenied(w, "do this only from the shared workflow %s, not from %q", a.o.Infra.Workflow, w.JobWorkflowRef)
	}
	if w.EventName != event {
		return nil, nil, workloadDenied(w, "do this only in a run started by %s, not by %q", event, w.EventName)
	}
	if a.o.Dispatch == nil {
		return nil, nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("this Forge API cannot reach GitHub (no GitHub App): approvals need it"))
	}
	svc, err := a.o.Catalog.get(ctx, servicePrefix+w.Service)
	if err != nil {
		return nil, nil, err
	}
	if svc.GetInfrastructure() == nil {
		return nil, nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s is not an infrastructure repo: its production runs need no approval", svc.GetName()))
	}
	return w, svc, nil
}

func (a *Approvals) RequestApproval(ctx context.Context, _ *connect.Request[forgev1.RequestApprovalRequest]) (*connect.Response[forgev1.RequestApprovalResponse], error) {
	if err := a.ready(); err != nil {
		return nil, err
	}
	w, svc, err := a.fromSharedWorkflow(ctx, "push")
	if err != nil {
		return nil, err
	}
	noteAuditService(ctx, svc.GetName())
	if !slices.Contains(svc.GetInfrastructure().GetEnvironments(), infrastructure.Production) {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s has no production environment", svc.GetName()))
	}
	if w.Actor == "" {
		return nil, workloadDenied(w, "ask for an approval only with a token that names who started the run (actor)")
	}
	// Only the branch's newest commit: a re-run of an old push must not ask
	// to deploy an older commit over a newer one.
	owner, repo, _ := strings.Cut(w.Repository, "/")
	branch, head, err := a.head(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	if head != w.SHA {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(
			"%s is not the newest commit of %s (%s is): only the newest asks for production's approval", shortSHA(w.SHA), branch, shortSHA(head)))
	}
	// Who started this attempt: GitHub's OIDC tokens do not say, so ask. A
	// re-run's is the person who re-ran it, who may not approve it either.
	triggering := w.TriggeringActor
	if w.RunID == 0 || w.RunAttempt == 0 {
		return nil, workloadDenied(w, "ask for an approval only with a token that names its run (run_id, run_attempt)")
	}
	_, byGitHub, err := a.o.Dispatch.RunActors(ctx, owner, repo, w.RunID, w.RunAttempt)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("read who started run %d (attempt %d) of %s: %w", w.RunID, w.RunAttempt, w.Repository, err))
	}
	if byGitHub != "" {
		triggering = byGitHub
	}
	// A re-run of the same job by the same person asks again: it gets the
	// approval it has. Someone else's re-run gets a new one, naming them.
	pending, err := a.o.Store.ListApprovals(ctx, store.ApprovalQuery{Service: w.Service, State: store.ApprovalPending, Limit: 1})
	if err != nil {
		return nil, storeErr(err)
	}
	if len(pending) == 1 && pending[0].Commit == w.SHA && pending[0].RunID == w.RunID && strings.EqualFold(pending[0].TriggeredBy, triggering) {
		noteAuditTarget(ctx, approvalPrefix+pending[0].ID)
		return connect.NewResponse(&forgev1.RequestApprovalResponse{Approval: a.proto(pending[0], svc)}), nil
	}
	ap := store.Approval{
		ID: newApprovalID(), Service: w.Service, Environment: infrastructure.Production, Commit: w.SHA,
		RunID: w.RunID, RequestedBy: w.Actor, TriggeredBy: triggering, State: store.ApprovalPending, CreatedAt: a.o.Now().UTC(),
	}
	if w.RunID != 0 {
		ap.RunURL = fmt.Sprintf("https://github.com/%s/actions/runs/%d", w.Repository, w.RunID)
	}
	if err := a.o.Store.CreateApproval(ctx, ap); err != nil {
		return nil, storeErr(err)
	}
	noteAuditTarget(ctx, approvalPrefix+ap.ID)
	noteAuditDetail(ctx, "commit", ap.Commit)
	a.o.Log.InfoContext(ctx, "approval asked", "approval", ap.ID, "service", ap.Service, "commit", ap.Commit, "by", ap.RequestedBy)
	return connect.NewResponse(&forgev1.RequestApprovalResponse{Approval: a.proto(ap, svc)}), nil
}

// head returns the default branch of owner/repo and its newest commit.
func (a *Approvals) head(ctx context.Context, owner, repo string) (branch, sha string, err error) {
	if branch, err = a.o.Dispatch.DefaultBranch(ctx, owner, repo); err != nil {
		return "", "", connect.NewError(connect.CodeUnavailable, fmt.Errorf("read the default branch of github.com/%s/%s: %w", owner, repo, err))
	}
	if sha, err = a.o.Dispatch.ResolveRef(ctx, owner, repo, branch); err != nil {
		return "", "", connect.NewError(connect.CodeUnavailable, fmt.Errorf("read the head of %s in github.com/%s/%s: %w", branch, owner, repo, err))
	}
	return branch, sha, nil
}

// load returns the approval name and its service.
func (a *Approvals) load(ctx context.Context, name string) (store.Approval, *forgev1.Service, error) {
	id, ok := strings.CutPrefix(name, approvalPrefix)
	if !ok || id == "" {
		return store.Approval{}, nil, invalid(fmt.Sprintf("approval %q must be approvals/<id>", name))
	}
	ap, err := a.o.Store.GetApproval(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return ap, nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("%s not found", name))
	}
	if err != nil {
		return ap, nil, storeErr(err)
	}
	svc, err := a.o.Catalog.get(ctx, servicePrefix+ap.Service)
	if connect.CodeOf(err) == connect.CodeNotFound {
		svc, err = &forgev1.Service{Name: servicePrefix + ap.Service}, nil // the service was deleted since
	}
	return ap, svc, err
}

func (a *Approvals) GetApproval(ctx context.Context, req *connect.Request[forgev1.GetApprovalRequest]) (*connect.Response[forgev1.GetApprovalResponse], error) {
	if err := a.ready(); err != nil {
		return nil, err
	}
	if w := workloadOf(ctx); w == nil {
		if err := require(ctx, PermServicesRead); err != nil {
			return nil, err
		}
	}
	ap, svc, err := a.load(ctx, req.Msg.GetName())
	if err != nil {
		return nil, err
	}
	if w := workloadOf(ctx); w != nil && w.Service != ap.Service {
		return nil, workloadDenied(w, "only read the approvals of %s%s", servicePrefix, w.Service)
	}
	return connect.NewResponse(&forgev1.GetApprovalResponse{Approval: a.proto(ap, svc)}), nil
}

func (a *Approvals) ListApprovals(ctx context.Context, req *connect.Request[forgev1.ListApprovalsRequest]) (*connect.Response[forgev1.ListApprovalsResponse], error) {
	if err := a.ready(); err != nil {
		return nil, err
	}
	if err := require(ctx, PermServicesRead); err != nil {
		return nil, err
	}
	q := store.ApprovalQuery{Limit: int(req.Msg.GetPageSize())}
	switch {
	case q.Limit <= 0:
		q.Limit = 50
	case q.Limit > 200:
		q.Limit = 200
	}
	if s := req.Msg.GetService(); s != "" {
		id, ok := strings.CutPrefix(s, servicePrefix)
		if !ok || id == "" {
			return nil, invalid(fmt.Sprintf("service %q must be services/<name>", s))
		}
		q.Service = id
	}
	if st := req.Msg.GetState(); st != forgev1.Approval_STATE_UNSPECIFIED {
		q.State = approvalState(st)
	}
	aps, err := a.o.Store.ListApprovals(ctx, q)
	if err != nil {
		return nil, storeErr(err)
	}
	out := &forgev1.ListApprovalsResponse{}
	svcs := map[string]*forgev1.Service{}
	for _, ap := range aps {
		svc, ok := svcs[ap.Service]
		if !ok {
			svc, _ = a.o.Catalog.get(ctx, servicePrefix+ap.Service) // nil when deleted
			svcs[ap.Service] = svc
		}
		out.Approvals = append(out.Approvals, a.proto(ap, svc))
	}
	return connect.NewResponse(out), nil
}

// decider checks the caller may decide ap: of the owning team or an admin,
// with a GitHub user in their login. It returns that user.
func (a *Approvals) decider(ctx context.Context, ap store.Approval) (string, error) {
	if err := authorize(ctx, ActApprovalDecide, servicePrefix+ap.Service); err != nil {
		return "", err
	}
	p, _ := PrincipalFrom(ctx)
	if p.Login == "" {
		return "", connect.NewError(connect.CodeFailedPrecondition, errors.New("your login does not say which GitHub user you are, so Forge cannot tell whether you started the run; sign in again with 'forge auth login'"))
	}
	return p.Login, nil
}

func (a *Approvals) ApproveApproval(ctx context.Context, req *connect.Request[forgev1.ApproveApprovalRequest]) (*connect.Response[forgev1.ApproveApprovalResponse], error) {
	if err := a.ready(); err != nil {
		return nil, err
	}
	ap, svc, err := a.load(ctx, req.Msg.GetName())
	if err != nil {
		return nil, err
	}
	noteAuditService(ctx, servicePrefix+ap.Service)
	login, err := a.decider(ctx, ap)
	if err != nil {
		return nil, err
	}
	// Neither who pushed the commit nor who started this attempt of the run.
	if strings.EqualFold(login, ap.RequestedBy) || (ap.TriggeredBy != "" && strings.EqualFold(login, ap.TriggeredBy)) {
		self := false
		if a.o.SelfApproval != nil {
			if self, err = a.o.SelfApproval(ctx, ap.Service); err != nil {
				return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("check the exceptions of %s%s: %w", servicePrefix, ap.Service, err))
			}
		}
		if !self {
			return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf(
				"you started this run, so another member of team %s must approve it (a team of one can ask the platform team for an exception)", svc.GetOwnerTeam()))
		}
		noteAuditDetail(ctx, "self_approved", "true")
	}
	switch ap.State {
	case store.ApprovalPending:
		if ap, err = a.o.Store.DecideApproval(ctx, ap.ID, store.ApprovalApproved, login, req.Msg.GetReason(), a.o.Now().UTC()); err != nil {
			return nil, a.decideErr(req.Msg.GetName(), ap, err)
		}
	case store.ApprovalApproved:
		// Starting the run failed before: start it again.
	default:
		return nil, a.decideErr(req.Msg.GetName(), ap, store.ErrNotPending)
	}
	// The run is started even if the caller goes away now.
	ap = a.dispatch(context.WithoutCancel(ctx), ap, svc)
	a.o.Log.InfoContext(ctx, "approval approved", "approval", ap.ID, "service", ap.Service, "commit", ap.Commit, "by", login, "dispatch_error", ap.DispatchError)
	return connect.NewResponse(&forgev1.ApproveApprovalResponse{Approval: a.proto(ap, svc)}), nil
}

// dispatch starts the production run of ap and records whether it could.
func (a *Approvals) dispatch(ctx context.Context, ap store.Approval, svc *forgev1.Service) store.Approval {
	msg := ""
	owner, repo, ok := repoOf(svc.GetRepoUrl(), a.o.Org)
	switch {
	case a.o.Dispatch == nil:
		msg = "this Forge API cannot start workflows (no GitHub App)"
	case !ok:
		msg = fmt.Sprintf("%s has no GitHub repo in %s", svc.GetName(), a.o.Org)
	default:
		branch, err := a.o.Dispatch.DefaultBranch(ctx, owner, repo)
		if err == nil {
			err = a.o.Dispatch.DispatchWorkflow(ctx, owner, repo, a.o.Infra.RepoWorkflowFile(), branch, map[string]string{"approval": approvalPrefix + ap.ID})
		}
		if err != nil {
			msg = fmt.Sprintf("start %s in github.com/%s/%s: %v", a.o.Infra.RepoWorkflowFile(), owner, repo, err)
		}
	}
	if msg != "" {
		noteAuditDetail(ctx, "dispatch_error", msg)
	}
	if msg != ap.DispatchError {
		if err := a.o.Store.SetDispatchError(ctx, ap.ID, msg); err != nil {
			a.o.Log.WarnContext(ctx, "record a dispatch error", "approval", ap.ID, "error", err)
		}
		ap.DispatchError = msg
	}
	return ap
}

func (a *Approvals) decideErr(name string, ap store.Approval, err error) error {
	if errors.Is(err, store.ErrNotPending) {
		return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s is %s: there is nothing to decide", name, ap.State))
	}
	return storeErr(err)
}

func (a *Approvals) RejectApproval(ctx context.Context, req *connect.Request[forgev1.RejectApprovalRequest]) (*connect.Response[forgev1.RejectApprovalResponse], error) {
	if err := a.ready(); err != nil {
		return nil, err
	}
	ap, svc, err := a.load(ctx, req.Msg.GetName())
	if err != nil {
		return nil, err
	}
	noteAuditService(ctx, servicePrefix+ap.Service)
	login, err := a.decider(ctx, ap)
	if err != nil {
		return nil, err
	}
	if ap, err = a.o.Store.DecideApproval(ctx, ap.ID, store.ApprovalRejected, login, req.Msg.GetReason(), a.o.Now().UTC()); err != nil {
		return nil, a.decideErr(req.Msg.GetName(), ap, err)
	}
	return connect.NewResponse(&forgev1.RejectApprovalResponse{Approval: a.proto(ap, svc)}), nil
}

func (a *Approvals) ClaimApproval(ctx context.Context, req *connect.Request[forgev1.ClaimApprovalRequest]) (*connect.Response[forgev1.ClaimApprovalResponse], error) {
	if err := a.ready(); err != nil {
		return nil, err
	}
	w, svc, err := a.fromSharedWorkflow(ctx, "workflow_dispatch")
	if err != nil {
		return nil, err
	}
	noteAuditService(ctx, svc.GetName())
	ap, _, err := a.load(ctx, req.Msg.GetName())
	if err != nil {
		return nil, err
	}
	if ap.Service != w.Service {
		return nil, workloadDenied(w, "only claim the approvals of %s%s", servicePrefix, w.Service)
	}
	// The run runs the default branch's workflow as it is now: only the
	// approved commit's, so nothing unapproved deploys.
	if w.SHA != ap.Commit {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(
			"%s approves %s, but this run is at %s: the branch moved on, and its newest commit asks for its own approval", req.Msg.GetName(), shortSHA(ap.Commit), shortSHA(w.SHA)))
	}
	if ap, err = a.o.Store.ClaimApproval(ctx, ap.ID, a.o.Now().UTC()); err != nil {
		if errors.Is(err, store.ErrNotPending) {
			return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s is %s, not approved: this run may not deploy to production", req.Msg.GetName(), ap.State))
		}
		return nil, storeErr(err)
	}
	noteAuditDetail(ctx, "commit", ap.Commit)
	return connect.NewResponse(&forgev1.ClaimApprovalResponse{Approval: a.proto(ap, svc)}), nil
}

// proto is ap as the API shows it; svc may be nil when it was deleted.
func (a *Approvals) proto(ap store.Approval, svc *forgev1.Service) *forgev1.Approval {
	out := &forgev1.Approval{
		Name: approvalPrefix + ap.ID, Service: servicePrefix + ap.Service, Environment: ap.Environment,
		Commit: ap.Commit, RunId: ap.RunID, RunUrl: ap.RunURL, RequestedBy: ap.RequestedBy,
		State: approvalStates[ap.State], DecidedBy: ap.DecidedBy, Reason: ap.Reason,
		CreateTime: timestamppb.New(ap.CreatedAt), DispatchError: ap.DispatchError, OwnerTeam: svc.GetOwnerTeam(),
	}
	if !ap.DecidedAt.IsZero() {
		out.DecideTime = timestamppb.New(ap.DecidedAt)
	}
	if !ap.ClaimedAt.IsZero() {
		out.ClaimTime = timestamppb.New(ap.ClaimedAt)
	}
	return out
}

var approvalStates = map[string]forgev1.Approval_State{
	store.ApprovalPending:    forgev1.Approval_STATE_PENDING,
	store.ApprovalApproved:   forgev1.Approval_STATE_APPROVED,
	store.ApprovalRejected:   forgev1.Approval_STATE_REJECTED,
	store.ApprovalSuperseded: forgev1.Approval_STATE_SUPERSEDED,
	store.ApprovalClaimed:    forgev1.Approval_STATE_CLAIMED,
}

func approvalState(s forgev1.Approval_State) string {
	for k, v := range approvalStates {
		if v == s {
			return k
		}
	}
	return "unknown"
}

func newApprovalID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "apr_" + hex.EncodeToString(b)
}
