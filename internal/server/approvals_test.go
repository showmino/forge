package server

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/gen/forge/v1/forgev1connect"
	"github.com/showmino/forge/internal/infrastructure"
	"github.com/showmino/forge/internal/store"
)

const sharedWorkflow = "showmino/forge-workflows/.github/workflows/infrastructure.yml@refs/heads/main"

// fakeDispatch records the workflows Forge starts; heads are the default
// branch's head by repo (default envSHA), branches the default branch's name
// (default main).
type fakeDispatch struct {
	mu       sync.Mutex
	calls    []string
	err      error
	heads    map[string]string
	branches map[string]string
	reruns   map[int64]string
}

func (f *fakeDispatch) DefaultBranch(_ context.Context, _, repo string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if b, ok := f.branches[repo]; ok {
		return b, nil
	}
	return "main", nil
}

func (f *fakeDispatch) ResolveRef(_ context.Context, _, repo, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if h, ok := f.heads[repo]; ok {
		return h, nil
	}
	return envSHA, nil
}

// RunActors answers as GitHub does: every run here was started by ana; an
// attempt 2 or later was started by reruns[runID].
func (f *fakeDispatch) RunActors(_ context.Context, _, _ string, runID, attempt int64) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if by, ok := f.reruns[runID]; ok && attempt > 1 {
		return "ana", by, nil
	}
	return "ana", "ana", nil
}

func (f *fakeDispatch) setHead(repo, sha string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.heads == nil {
		f.heads = map[string]string{}
	}
	f.heads[repo] = sha
}

func (f *fakeDispatch) DispatchWorkflow(_ context.Context, owner, repo, workflow, ref string, inputs map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.calls = append(f.calls, owner+"/"+repo+" "+workflow+"@"+ref+" approval="+inputs["approval"])
	return nil
}

func (f *fakeDispatch) all() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

type approvalEnv struct {
	client    forgev1connect.ApprovalServiceClient
	dispatch  *fakeDispatch
	approvals *Approvals
	self      map[string]bool
	mu        sync.Mutex
}

// newApprovalEnv serves approvals for infra-net (an infrastructure repo of
// team platform with production) and payments (a plain service). Tokens:
// ana and bo are of platform, cy of web, admin an admin, nologin of
// platform without a GitHub user; ci-* are CI tokens.
func newApprovalEnv(t *testing.T, noApprovals bool) *approvalEnv {
	t.Helper()
	return newApprovalEnvWith(t, noApprovals, nil)
}

// newApprovalEnvWith is newApprovalEnv with the approvals' options changed by
// tweak when it is set.
func newApprovalEnvWith(t *testing.T, noApprovals bool, tweak func(*ApprovalOptions)) *approvalEnv {
	t.Helper()
	cat := NewCatalog(nil)
	for _, s := range []*forgev1.Service{
		{OwnerTeam: "platform", RepoUrl: "https://github.com/showmino/infra-net", Infrastructure: &forgev1.InfrastructureRepo{Tool: "opentofu", Target: "azure", Environments: []string{"production"}}},
		{OwnerTeam: "platform", RepoUrl: "https://github.com/showmino/infra-dev", Infrastructure: &forgev1.InfrastructureRepo{Tool: "bicep", Target: "azure", Environments: []string{"staging"}}},
		{OwnerTeam: "billing", RepoUrl: "https://github.com/showmino/payments"},
	} {
		id := strings.TrimPrefix(s.GetRepoUrl(), "https://github.com/showmino/")
		if _, err := cat.register(writerCtx(), connect.NewRequest(&forgev1.RegisterServiceRequest{ServiceId: id, Service: s}), false); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := infrastructure.Parse([]byte(`
namespace: forge-infrastructure
providerConfig: {kind: ClusterProviderConfig, name: github}
workflow: ` + sharedWorkflow + `
targets: [{name: azure, environments: {production: {secret: s}}}]
`))
	if err != nil {
		t.Fatal(err)
	}
	// Bo re-runs run 41 (attempt 2).
	e := &approvalEnv{dispatch: &fakeDispatch{reruns: map[int64]string{41: "Bo"}}, self: map[string]bool{}}
	var aps *Approvals
	if !noApprovals {
		o := ApprovalOptions{
			Store: store.NewMemory(), Catalog: cat, Infra: cfg, Dispatch: e.dispatch, Org: "showmino",
			SelfApproval: func(_ context.Context, svc string) (bool, error) {
				e.mu.Lock()
				defer e.mu.Unlock()
				return e.self[svc], nil
			},
		}
		if tweak != nil {
			tweak(&o)
		}
		aps = NewApprovals(o)
	}
	ci := func(repo, sha, actor, wf string, run int64, event string) *Principal {
		return &Principal{Subject: "github:showmino/" + repo, Permissions: []string{PermDeploy(repo, "staging")},
			Workload: &Workload{Repository: "showmino/" + repo, Service: repo, SHA: sha, Actor: actor, RunID: run, JobWorkflowRef: wf,
				EventName: event, TriggeringActor: actor, RunAttempt: 1}}
	}
	rerunBy := func(p *Principal, who string) Principal {
		w := *p.Workload
		w.TriggeringActor, w.RunAttempt = who, 2 // GitHub, not the token, says who
		q := *p
		q.Workload = &w
		return q
	}
	h, err := New(Options{
		Authenticator: StaticAuthenticator{
			"ana":      {Subject: "ana", Login: "ana", Groups: []string{"showmino:platform"}},
			"bo":       {Subject: "bo", Login: "Bo", Groups: []string{"showmino:platform"}},
			"cy":       {Subject: "cy", Login: "cy", Groups: []string{"showmino:web"}},
			"admin":    {Subject: "dee", Login: "dee", Groups: []string{"showmino:admins"}},
			"nologin":  {Subject: "eve", Groups: []string{"showmino:platform"}},
			"ci":       *ci("infra-net", envSHA, "ana", sharedWorkflow, 41, "push"),
			"ci-rerun": *ci("infra-net", envSHA, "ana", sharedWorkflow, 41, "push"),
			"ci-rerun-attempt-2": func() Principal {
				p := *ci("infra-net", envSHA, "ana", sharedWorkflow, 41, "push")
				w := *p.Workload
				w.RunAttempt = 2
				p.Workload = &w
				return p
			}(),
			"ci-rerun-bo":  rerunBy(ci("infra-net", envSHA, "ana", sharedWorkflow, 41, "push"), "Bo"),
			"ci-newer":     *ci("infra-net", ciOldSHA, "ana", sharedWorkflow, 42, "push"),
			"ci-comment":   *ci("infra-net", envSHA, "cy", sharedWorkflow, 46, "issue_comment"),
			"ci-own-wf":    *ci("infra-net", envSHA, "ana", "showmino/infra-net/.github/workflows/deploy.yml@refs/heads/main", 43, "push"),
			"ci-dev":       *ci("infra-dev", envSHA, "ana", sharedWorkflow, 44, "push"),
			"ci-pay":       *ci("payments", envSHA, "ana", sharedWorkflow, 45, "push"),
			"ci-run":       *ci("infra-net", envSHA, "ana", sharedWorkflow, 50, "workflow_dispatch"),
			"ci-run-newer": *ci("infra-net", ciOldSHA, "ana", sharedWorkflow, 51, "workflow_dispatch"),
			"ci-run-own":   *ci("infra-net", envSHA, "ana", "showmino/infra-net/.github/workflows/deploy.yml@refs/heads/main", 52, "workflow_dispatch"),
			"ci-run-dev":   *ci("infra-dev", envSHA, "ana", sharedWorkflow, 53, "workflow_dispatch"),
		},
		Authorizer: Authorizer{AdminGroups: []string{"showmino:admins"}, Org: "showmino"},
		Catalog:    cat,
		Approvals:  aps,
	})
	if err != nil {
		t.Fatal(err)
	}
	e.approvals = aps
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	e.client = forgev1connect.NewApprovalServiceClient(srv.Client(), srv.URL)
	return e
}

func (e *approvalEnv) request(t *testing.T, token string) *forgev1.Approval {
	t.Helper()
	resp, err := e.client.RequestApproval(context.Background(), as(token, &forgev1.RequestApprovalRequest{}))
	if err != nil {
		t.Fatalf("request as %s: %v", token, err)
	}
	return resp.Msg.GetApproval()
}

func (e *approvalEnv) approve(token, name string) (*forgev1.Approval, error) {
	resp, err := e.client.ApproveApproval(context.Background(), as(token, &forgev1.ApproveApprovalRequest{Name: name, Reason: "plan reviewed"}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetApproval(), nil
}

func (e *approvalEnv) claim(token, name string) (*forgev1.Approval, error) {
	resp, err := e.client.ClaimApproval(context.Background(), as(token, &forgev1.ClaimApprovalRequest{Name: name}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetApproval(), nil
}

func (e *approvalEnv) get(t *testing.T, name string) *forgev1.Approval {
	t.Helper()
	resp, err := e.client.GetApproval(context.Background(), as("cy", &forgev1.GetApprovalRequest{Name: name}))
	if err != nil {
		t.Fatal(err)
	}
	return resp.Msg.GetApproval()
}

func TestApprovalFlow(t *testing.T) {
	t.Parallel()
	e := newApprovalEnv(t, false)
	ap := e.request(t, "ci")
	if ap.GetState() != forgev1.Approval_STATE_PENDING || ap.GetService() != "services/infra-net" || ap.GetEnvironment() != "production" ||
		ap.GetCommit() != envSHA || ap.GetRequestedBy() != "ana" || ap.GetOwnerTeam() != "platform" ||
		ap.GetRunUrl() != "https://github.com/showmino/infra-net/actions/runs/41" {
		t.Fatalf("asked: %v", ap)
	}
	// A re-run of the same job gets the same approval.
	if again := e.request(t, "ci-rerun"); again.GetName() != ap.GetName() {
		t.Errorf("re-run: %s, want %s", again.GetName(), ap.GetName())
	}

	// The person who started the run cannot approve it; nor can another team.
	_, err := e.approve("ana", ap.GetName())
	wantCode(t, "self", err, connect.CodePermissionDenied, "you started this run, so another member of team platform must approve it")
	_, err = e.approve("cy", ap.GetName())
	wantCode(t, "other team", err, connect.CodePermissionDenied, "")
	_, err = e.approve("nologin", ap.GetName())
	wantCode(t, "no GitHub user", err, connect.CodeFailedPrecondition, "does not say which GitHub user")
	if got := e.dispatch.all(); len(got) != 0 {
		t.Fatalf("started before an approval: %v", got)
	}

	// The claim of a pending approval is refused: nothing deploys yet.
	_, err = e.claim("ci-run", ap.GetName())
	wantCode(t, "claim pending", err, connect.CodeFailedPrecondition, "is pending, not approved")

	// A teammate approves (the login's case does not matter), and Forge
	// starts the production run with the approval.
	got, err := e.approve("bo", ap.GetName())
	if err != nil || got.GetState() != forgev1.Approval_STATE_APPROVED || got.GetDecidedBy() != "Bo" || got.GetReason() != "plan reviewed" || got.GetDispatchError() != "" {
		t.Fatalf("approve: %v %v", got, err)
	}
	if calls := e.dispatch.all(); len(calls) != 1 || calls[0] != "showmino/infra-net deploy.yml@main approval="+ap.GetName() {
		t.Errorf("dispatched %v", calls)
	}

	// The claim: only from the shared workflow, only the repo's own, once.
	_, err = e.claim("ci-run-own", ap.GetName())
	wantCode(t, "claim from the repo's workflow", err, connect.CodePermissionDenied, "only from the shared workflow")
	_, err = e.claim("ci-run-dev", ap.GetName())
	wantCode(t, "claim another repo's", err, connect.CodePermissionDenied, "only claim the approvals of services/infra-dev")
	_, err = e.claim("ci", ap.GetName())
	wantCode(t, "claim from a push", err, connect.CodePermissionDenied, "only in a run started by workflow_dispatch")
	_, err = e.claim("admin", ap.GetName())
	wantCode(t, "a person claims", err, connect.CodePermissionDenied, "only a job of the shared workflow")
	_, err = e.claim("ci-run-newer", ap.GetName())
	wantCode(t, "claim at another commit", err, connect.CodeFailedPrecondition, "the branch moved on")
	got, err = e.claim("ci-run", ap.GetName())
	if err != nil || got.GetState() != forgev1.Approval_STATE_CLAIMED || got.GetCommit() != envSHA || got.GetClaimTime() == nil {
		t.Fatalf("claim: %v %v", got, err)
	}
	_, err = e.claim("ci-run", ap.GetName())
	wantCode(t, "claim twice", err, connect.CodeFailedPrecondition, "is claimed, not approved")
	_, err = e.approve("bo", ap.GetName())
	wantCode(t, "approve a claimed one", err, connect.CodeFailedPrecondition, "is claimed")
}

func TestApprovalRequestRefuses(t *testing.T) {
	t.Parallel()
	e := newApprovalEnv(t, false)
	for _, tc := range []struct {
		token string
		code  connect.Code
		msg   string
	}{
		{"ci-own-wf", connect.CodePermissionDenied, "only from the shared workflow " + sharedWorkflow},
		{"ci-comment", connect.CodePermissionDenied, `only in a run started by push, not by "issue_comment"`},
		{"ci-newer", connect.CodeFailedPrecondition, "is not the newest commit of main"},
		{"ci-pay", connect.CodeFailedPrecondition, "services/payments is not an infrastructure repo"},
		{"ci-dev", connect.CodeFailedPrecondition, "has no production environment"},
		{"ana", connect.CodePermissionDenied, "only a job of the shared workflow"},
	} {
		_, err := e.client.RequestApproval(context.Background(), as(tc.token, &forgev1.RequestApprovalRequest{}))
		wantCode(t, tc.token, err, tc.code, tc.msg)
	}
	// A CI token may not list, approve or reject: the interceptor stops it.
	_, err := e.client.ListApprovals(context.Background(), as("ci", &forgev1.ListApprovalsRequest{}))
	wantCode(t, "CI lists", err, connect.CodePermissionDenied, "")
}

func TestApprovalSupersededRejectedAndListed(t *testing.T) {
	t.Parallel()
	e := newApprovalEnv(t, false)
	old := e.request(t, "ci")
	e.dispatch.setHead("infra-net", ciOldSHA) // a newer push
	newer := e.request(t, "ci-newer")
	if got := e.get(t, old.GetName()); got.GetState() != forgev1.Approval_STATE_SUPERSEDED {
		t.Errorf("older: %v", got.GetState())
	}
	_, err := e.approve("bo", old.GetName())
	wantCode(t, "approve a superseded one", err, connect.CodeFailedPrecondition, "is superseded")

	// The person who started it may reject (take back) their own run.
	resp, err := e.client.RejectApproval(context.Background(), as("ana", &forgev1.RejectApprovalRequest{Name: newer.GetName(), Reason: "wrong plan"}))
	if err != nil || resp.Msg.GetApproval().GetState() != forgev1.Approval_STATE_REJECTED || resp.Msg.GetApproval().GetDecidedBy() != "ana" {
		t.Fatalf("reject: %v %v", resp, err)
	}

	list, err := e.client.ListApprovals(context.Background(), as("cy", &forgev1.ListApprovalsRequest{Service: "services/infra-net"}))
	if err != nil || len(list.Msg.GetApprovals()) != 2 || list.Msg.GetApprovals()[0].GetName() != newer.GetName() {
		t.Fatalf("list: %v %v", list, err)
	}
	list, _ = e.client.ListApprovals(context.Background(), as("cy", &forgev1.ListApprovalsRequest{State: forgev1.Approval_STATE_PENDING}))
	if n := len(list.Msg.GetApprovals()); n != 0 {
		t.Errorf("pending: %d", n)
	}
	_, err = e.client.GetApproval(context.Background(), as("cy", &forgev1.GetApprovalRequest{Name: "approvals/apr_missing"}))
	wantCode(t, "missing", err, connect.CodeNotFound, "")
	_, err = e.client.GetApproval(context.Background(), as("ci-dev", &forgev1.GetApprovalRequest{Name: newer.GetName()}))
	wantCode(t, "CI reads another repo's", err, connect.CodePermissionDenied, "only read the approvals of services/infra-dev")
}

// When starting the run fails, the approval says why, and approving again
// starts it.
func TestApprovalDispatchRetry(t *testing.T) {
	t.Parallel()
	e := newApprovalEnv(t, false)
	ap := e.request(t, "ci")
	e.dispatch.mu.Lock()
	e.dispatch.err = errors.New("403 Resource not accessible by integration")
	e.dispatch.mu.Unlock()
	got, err := e.approve("bo", ap.GetName())
	if err != nil || got.GetState() != forgev1.Approval_STATE_APPROVED || !strings.Contains(got.GetDispatchError(), "403 Resource not accessible") {
		t.Fatalf("approve: %v %v", got, err)
	}
	e.dispatch.mu.Lock()
	e.dispatch.err = nil
	e.dispatch.mu.Unlock()
	got, err = e.approve("admin", ap.GetName())
	if err != nil || got.GetDispatchError() != "" || len(e.dispatch.all()) != 1 {
		t.Fatalf("again: %v %v %v", got, err, e.dispatch.all())
	}
	if got.GetDecidedBy() != "Bo" {
		t.Errorf("the retry changed who approved: %s", got.GetDecidedBy())
	}
}

// With the exception, the team may approve its own runs.
func TestApprovalSelfWithException(t *testing.T) {
	t.Parallel()
	e := newApprovalEnv(t, false)
	e.mu.Lock()
	e.self["infra-net"] = true
	e.mu.Unlock()
	ap := e.request(t, "ci")
	if got, err := e.approve("ana", ap.GetName()); err != nil || got.GetState() != forgev1.Approval_STATE_APPROVED {
		t.Fatalf("self: %v %v", got, err)
	}
}

func TestApprovalsNotConfigured(t *testing.T) {
	t.Parallel()
	e := newApprovalEnv(t, true)
	_, err := e.client.ListApprovals(context.Background(), as("cy", &forgev1.ListApprovalsRequest{}))
	wantCode(t, "list", err, connect.CodeFailedPrecondition, "approvals are not configured")
}

// Who re-ran the asking job may not approve it either.
func TestApprovalTriggeringActor(t *testing.T) {
	t.Parallel()
	e := newApprovalEnv(t, false)
	ap := e.request(t, "ci-rerun-bo")
	_, err := e.approve("bo", ap.GetName())
	wantCode(t, "the one who re-ran it", err, connect.CodePermissionDenied, "you started this run")
	if _, err := e.approve("admin", ap.GetName()); err != nil {
		t.Errorf("an admin: %v", err)
	}
}

// An approved run that never claimed is superseded by a newer commit's
// request: it can neither be dispatched again nor claimed.
func TestApprovalApprovedIsSuperseded(t *testing.T) {
	t.Parallel()
	e := newApprovalEnv(t, false)
	old := e.request(t, "ci")
	if _, err := e.approve("bo", old.GetName()); err != nil {
		t.Fatal(err)
	}
	e.dispatch.setHead("infra-net", ciOldSHA)
	e.request(t, "ci-newer")
	if got := e.get(t, old.GetName()); got.GetState() != forgev1.Approval_STATE_SUPERSEDED {
		t.Fatalf("old: %v", got.GetState())
	}
	_, err := e.approve("bo", old.GetName())
	wantCode(t, "dispatch again", err, connect.CodeFailedPrecondition, "is superseded")
	_, err = e.claim("ci-run", old.GetName())
	wantCode(t, "claim", err, connect.CodeFailedPrecondition, "is superseded, not approved")
}

// Forge starts the run on the repo's default branch, whatever it is called.
func TestApprovalDispatchesTheDefaultBranch(t *testing.T) {
	t.Parallel()
	e := newApprovalEnv(t, false)
	e.dispatch.mu.Lock()
	e.dispatch.branches = map[string]string{"infra-net": "trunk"}
	e.dispatch.mu.Unlock()
	ap := e.request(t, "ci")
	if _, err := e.approve("bo", ap.GetName()); err != nil {
		t.Fatal(err)
	}
	if calls := e.dispatch.all(); len(calls) != 1 || !strings.Contains(calls[0], "deploy.yml@trunk") {
		t.Errorf("dispatched %v", calls)
	}
}

// GitHub's tokens do not carry the triggering actor: Forge asks GitHub who
// started the attempt, so the one who re-ran a job cannot approve it.
func TestApprovalRerunByGitHub(t *testing.T) {
	t.Parallel()
	e := newApprovalEnv(t, false)
	first := e.request(t, "ci") // attempt 1, pushed by ana
	// Bo re-runs it (the fake's run 41, attempt 2); the token says nothing about Bo.
	rerun := e.request(t, "ci-rerun-attempt-2")
	if rerun.GetName() == first.GetName() {
		t.Fatalf("Bo's re-run got ana's approval %s", first.GetName())
	}
	_, err := e.approve("bo", rerun.GetName())
	wantCode(t, "the one who re-ran it", err, connect.CodePermissionDenied, "you started this run")
	if got := e.get(t, first.GetName()); got.GetState() != forgev1.Approval_STATE_SUPERSEDED {
		t.Errorf("first: %v", got.GetState())
	}
}

// When GitHub does not say who started the attempt, nothing is asked.
func TestApprovalUnknownTriggeringActor(t *testing.T) {
	t.Parallel()
	e := newApprovalEnv(t, false)
	e.dispatch.mu.Lock()
	e.dispatch.reruns = map[int64]string{41: ""}
	e.dispatch.mu.Unlock()
	_, err := e.client.RequestApproval(context.Background(), as("ci-rerun-attempt-2", &forgev1.RequestApprovalRequest{}))
	wantCode(t, "unknown", err, connect.CodeUnavailable, "GitHub did not say who started")
}
