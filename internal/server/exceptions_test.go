package server

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/envtest"
	"github.com/showmino/forge/internal/github"
	"github.com/showmino/forge/internal/repositories"
)

// fakeExceptionGitHub is the deploy repo's pull requests: pulls by commit,
// reviews by pull request, and the admin team's members.
type fakeExceptionGitHub struct {
	repo      *envtest.Repo
	mu        sync.Mutex
	proposals []github.Proposal
	pulls     map[string][]github.Pull
	reviews   map[int][]github.Review
	admins    map[string]bool
}

func (f *fakeExceptionGitHub) ProposeFiles(ctx context.Context, owner, repo string, p github.Proposal) (string, error) {
	if owner != "showmino" || repo != "forge-deploys" {
		return "", errors.New("proposed outside the deploy repo")
	}
	for _, file := range p.Files {
		if _, err := f.repo.Read(ctx, file.Path, ""); err == nil {
			return "", github.ErrFileExists
		}
	}
	for _, path := range p.Remove {
		if _, err := f.repo.Read(ctx, path, ""); err != nil {
			return "", err
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.proposals = append(f.proposals, p)
	return "https://github.com/showmino/forge-deploys/pull/" + p.Branch, nil
}

func (f *fakeExceptionGitHub) PullsForCommit(_ context.Context, _, _, sha string) ([]github.Pull, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pulls[sha], nil
}

func (f *fakeExceptionGitHub) PullReviews(_ context.Context, _, _ string, n int) ([]github.Review, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reviews[n], nil
}

func (f *fakeExceptionGitHub) TeamMember(_ context.Context, org, slug, login string) (bool, error) {
	return org == "showmino" && slug == "admins" && f.admins[login], nil
}

// exceptionYAML is infra-net's exception file, for team, asked now.
func exceptionYAML(team string) []byte {
	return []byte("service: infra-net\nteam: " + team + "\nreason: alone\nrequestedBy: ana\nrequestedAt: " + time.Now().Add(time.Minute).UTC().Format(time.RFC3339) + "\n")
}

// merge squash-merges infra-net's exception as pull request 7 would, its
// last commit h7 reviewed as reviews say.
func (f *fakeExceptionGitHub) merge(reviews ...github.Review) {
	f.mergeAs(exceptionYAML("platform"), true, reviews...)
}

// mergeAs writes data as pull request 7's merge; with squashed false, the
// commit on main is not the pull request's merge commit.
func (f *fakeExceptionGitHub) mergeAs(data []byte, squashed bool, reviews ...github.Review) {
	sha := f.repo.Push("exceptions/infra-net.yaml", data, "Merge pull request #7")
	merge := sha
	if !squashed {
		merge = "m-elsewhere"
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pulls[sha] = []github.Pull{{Number: 7, HTMLURL: "https://github.com/showmino/forge-deploys/pull/7", Merged: time.Now(), Base: "main", HeadSHA: "h7", MergeCommit: merge}}
	f.reviews[7] = reviews
}

func newExceptionEnv(t *testing.T) (*approvalEnv, *fakeExceptionGitHub) {
	t.Helper()
	gh := &fakeExceptionGitHub{repo: &envtest.Repo{}, pulls: map[string][]github.Pull{}, reviews: map[int][]github.Review{}, admins: map[string]bool{"dee": true}}
	e := newApprovalEnvWith(t, false, func(o *ApprovalOptions) {
		o.SelfApproval = nil // the exception decides
		o.Exceptions = &ExceptionOptions{Repo: gh.repo, Owner: "showmino", Name: "forge-deploys", GitHub: gh, AdminTeams: []string{"admins"}}
	})
	rules := repositoryConfig(t)
	data, err := rules.Rules("infra-net", true, true)
	if err != nil {
		t.Fatal(err)
	}
	gh.repo.Push(repositories.Path("infra-net"), data, "Create repository rules")
	e.approvals.o.Exceptions.RepositoryRules = rules
	return e, gh
}

func (e *approvalEnv) exceptions(t *testing.T) []*forgev1.Exception {
	t.Helper()
	resp, err := e.client.ListExceptions(context.Background(), as("cy", &forgev1.ListExceptionsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	return resp.Msg.GetExceptions()
}

func TestExceptionRequest(t *testing.T) {
	t.Parallel()
	e, gh := newExceptionEnv(t)
	ctx := context.Background()
	resp, err := e.client.RequestException(ctx, as("ana", &forgev1.RequestExceptionRequest{Service: "services/infra-net", Reason: "I am the only one on the team"}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resp.Msg.GetPullRequestUrl(), "/pull/forge/exception-infra-net-") || len(gh.proposals) != 1 || !gh.proposals[0].Fresh {
		t.Fatalf("request: %v %v", resp.Msg, gh.proposals)
	}
	p := gh.proposals[0]
	if len(p.Files) != 1 || p.Files[0].Path != "exceptions/infra-net.yaml" || !strings.Contains(string(p.Files[0].Content), "reason: I am the only one on the team") ||
		!strings.Contains(string(p.Files[0].Content), "requestedBy: ana") || len(p.Update) != 1 ||
		p.Update[0].Path != repositories.Path("infra-net") ||
		!strings.Contains(string(p.Update[0].Content), "requiredApprovingReviewCount: 0") ||
		!strings.Contains(p.Body, "admins") || !strings.Contains(p.Body, "waive approving reviews") {
		t.Errorf("proposal %+v", p)
	}
	// Forge itself never writes there: the request is a pull request only.
	if _, err := gh.repo.Read(ctx, "exceptions/infra-net.yaml", ""); err == nil {
		t.Error("the exception was written to the deploy repo")
	}

	for _, tc := range []struct {
		token string
		req   *forgev1.RequestExceptionRequest
		code  connect.Code
		msg   string
	}{
		{"ana", &forgev1.RequestExceptionRequest{Service: "services/infra-net"}, connect.CodeInvalidArgument, "reason is required"},
		{"cy", &forgev1.RequestExceptionRequest{Service: "services/infra-net", Reason: "x"}, connect.CodePermissionDenied, ""},
		{"ana", &forgev1.RequestExceptionRequest{Service: "services/infra-dev", Reason: "x"}, connect.CodeFailedPrecondition, "not an infrastructure repo with production"},
		{"ci", &forgev1.RequestExceptionRequest{Service: "services/infra-net", Reason: "x"}, connect.CodePermissionDenied, ""},
	} {
		_, err := e.client.RequestException(ctx, as(tc.token, tc.req))
		wantCode(t, tc.token+" "+tc.req.GetService(), err, tc.code, tc.msg)
	}

	gh.merge(github.Review{User: "dee", State: "APPROVED", CommitID: "h7"})
	_, err = e.client.RequestException(ctx, as("ana", &forgev1.RequestExceptionRequest{Service: "services/infra-net", Reason: "again"}))
	wantCode(t, "again", err, connect.CodeAlreadyExists, "has an exception already")

	// Revoke: a pull request that removes it.
	rv, err := e.client.RevokeException(ctx, as("bo", &forgev1.RevokeExceptionRequest{Service: "services/infra-net", Reason: "Bo joined"}))
	if err != nil || !strings.Contains(rv.Msg.GetPullRequestUrl(), "/pull/forge/revoke-exception-infra-net-") {
		t.Fatalf("revoke: %v %v", rv, err)
	}
	if last := gh.proposals[len(gh.proposals)-1]; len(last.Remove) != 1 || last.Remove[0] != "exceptions/infra-net.yaml" ||
		len(last.Files) != 0 || len(last.Update) != 1 || last.Update[0].Path != repositories.Path("infra-net") ||
		!strings.Contains(string(last.Update[0].Content), "requiredApprovingReviewCount: 1") {
		t.Errorf("revoke proposal %+v", last)
	}
}

// The exception holds only when merged through a pull request whose last
// commit an admin team member approved; then the team approves its own run.
func TestExceptionGrant(t *testing.T) {
	t.Parallel()
	dee := []github.Review{{User: "dee", State: "APPROVED", CommitID: "h7"}}
	for _, tc := range []struct {
		name    string
		reviews []github.Review
		direct  bool
		data    []byte
		merge   bool // not the pull request's merge commit
		granted bool
		problem string
	}{
		{name: "an admin approved", reviews: []github.Review{{User: "bo", State: "COMMENTED", CommitID: "h7"}, {User: "dee", State: "APPROVED", CommitID: "h7"}}, granted: true},
		{name: "the admin who approved asked for it", reviews: dee, data: []byte(strings.Replace(string(exceptionYAML("platform")), "requestedBy: ana", "requestedBy: Dee", 1)), problem: "other than Dee"},
		{name: "brought back by a merge commit", reviews: dee, merge: true, problem: "is not the merge of"},
		{name: "another team's", reviews: dee, data: exceptionYAML("web"), problem: "it is for team web"},
		{name: "an earlier service's", reviews: dee, data: []byte("service: infra-net\nteam: platform\nrequestedBy: ana\nrequestedAt: 2026-01-01T00:00:00Z\n"), problem: "before services/infra-net was made"},
		{name: "a teammate approved", reviews: []github.Review{{User: "bo", State: "APPROVED", CommitID: "h7"}}, problem: "merged without an approval of its last commit by a member of admins"},
		{name: "an admin's approval was superseded", reviews: []github.Review{{User: "dee", State: "APPROVED", CommitID: "h7"}, {User: "dee", State: "CHANGES_REQUESTED", CommitID: "h7"}}, problem: "merged without an approval of its last commit by a member of admins"},
		{name: "an admin approved an older commit", reviews: []github.Review{{User: "dee", State: "APPROVED", CommitID: "h6"}}, problem: "without an approval"},
		{name: "no review", problem: "without an approval"},
		{name: "committed without a pull request", direct: true, problem: "was not merged through a pull request"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e, gh := newExceptionEnv(t)
			data := tc.data
			if data == nil {
				data = exceptionYAML("platform")
			}
			if tc.direct {
				gh.repo.Push("exceptions/infra-net.yaml", data, "pushed")
			} else {
				gh.mergeAs(data, !tc.merge, tc.reviews...)
			}
			exs := e.exceptions(t)
			if len(exs) != 1 || exs[0].GetService() != "services/infra-net" || exs[0].GetGranted() != tc.granted || !strings.Contains(exs[0].GetProblem(), tc.problem) {
				t.Fatalf("exceptions %v", exs)
			}
			waived, err := e.approvals.ExceptionGranted(context.Background(), "infra-net")
			if err != nil || waived != tc.granted {
				t.Fatalf("review waiver: got %t, want %t (error %v)", waived, tc.granted, err)
			}
			ap := e.request(t, "ci")
			got, err := e.approve("ana", ap.GetName())
			if tc.granted {
				if err != nil || got.GetState() != forgev1.Approval_STATE_APPROVED || exs[0].GetGrantedBy() != "dee" || exs[0].GetPullRequestUrl() == "" {
					t.Fatalf("self-approve with the exception: %v %v %v", got, err, exs[0])
				}
				return
			}
			wantCode(t, "self-approve", err, connect.CodePermissionDenied, "you started this run")
		})
	}
}

func TestExceptionsNotConfigured(t *testing.T) {
	t.Parallel()
	e := newApprovalEnv(t, false)
	_, err := e.client.ListExceptions(context.Background(), as("cy", &forgev1.ListExceptionsRequest{}))
	wantCode(t, "list", err, connect.CodeFailedPrecondition, "exceptions need the deploy repo")
}
