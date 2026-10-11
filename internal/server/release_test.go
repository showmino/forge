package server

import (
	"context"
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/envtest"
	"github.com/showmino/forge/internal/github"
	"github.com/showmino/forge/internal/semver"
)

func newReleaseFixture(t *testing.T) (*deployFixture, *envtest.Releases) {
	t.Helper()
	rel := &envtest.Releases{}
	return newDeployFixture(t, false, func(o *DeployOptions, _ *envtest.Argo) { o.Releases = rel }), rel
}

// deployRef deploys ref to env and returns the finished operation.
func (f *deployFixture) deployRef(t *testing.T, env, ref string) *forgev1.Operation {
	t.Helper()
	resp, err := f.client.StartDeploy(context.Background(), as("writer", &forgev1.StartDeployRequest{Service: "services/payments", Env: env, Ref: ref}))
	if err != nil {
		t.Fatal(err)
	}
	return wait(t, f.ops, resp.Msg.GetOperation().GetName())
}

func (f *deployFixture) promote(t *testing.T, bump forgev1.Bump) *forgev1.Operation {
	t.Helper()
	resp, err := f.client.PromoteDeploy(context.Background(), as("writer", &forgev1.PromoteDeployRequest{Service: "services/payments", Bump: bump}))
	if err != nil {
		t.Fatal(err)
	}
	return wait(t, f.ops, resp.Msg.GetOperation().GetName())
}

// versions is what each environment shows, "staging=v… production=v…".
func (f *deployFixture) versions(t *testing.T) string {
	t.Helper()
	list, err := f.client.ListDeployments(context.Background(), as("reader", &forgev1.ListDeploymentsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, d := range list.Msg.GetDeployments() {
		out = append(out, d.GetEnv()+"="+d.GetVersion())
	}
	return strings.Join(out, " ")
}

func TestReleaseVersions(t *testing.T) {
	t.Parallel()
	f, rel := newReleaseFixture(t)
	ctx := context.Background()
	main7, old7 := shortSHA(envSHA), shortSHA(ciOldSHA)

	// Every build that reaches staging is a release candidate.
	op := f.deployRef(t, "staging", "")
	if op.GetError() != nil || eventSteps(op) != "resolve,image,release,commit,namespace,sync" {
		t.Fatalf("first deploy: %v %s", op.GetError(), eventSteps(op))
	}
	if got := rel.Names(); got != "v0.1.0-rc.1@"+main7 {
		t.Fatalf("tags %q", got)
	}
	if r, ok := rel.Release("v0.1.0-rc.1"); !ok || !r.Prerelease || r.Latest || r.Commit != envSHA || r.Previous != "" {
		t.Errorf("candidate release %+v %v", r, ok)
	}
	hist, _ := f.repo.History(ctx, valuesPath("payments", "staging"), 1)
	if rec, ok := parseRecord(hist[0].SHA, hist[0].Message, hist[0].Time); !ok || rec.Version != "v0.1.0-rc.1" ||
		!strings.HasPrefix(hist[0].Message, "deploy payments v0.1.0-rc.1 to staging: ") {
		t.Errorf("commit message %q -> %+v", hist[0].Message, rec)
	}

	// The same commit again keeps its candidate.
	if op := f.deployRef(t, "staging", ""); op.GetError() != nil {
		t.Fatal(op.GetError())
	}
	if got := rel.Names(); got != "v0.1.0-rc.1@"+main7 {
		t.Errorf("after a redeploy: tags %q", got)
	}
	// Another commit is the next candidate, with notes since the one before.
	if op := f.deployRef(t, "staging", "release"); op.GetError() != nil {
		t.Fatal(op.GetError())
	}
	if r, ok := rel.Release("v0.1.0-rc.2"); !ok || r.Commit != ciOldSHA || r.Previous != "v0.1.0-rc.1" {
		t.Errorf("second candidate %+v %v", r, ok)
	}
	if got := f.versions(t); got != "staging=v0.1.0-rc.2" {
		t.Errorf("versions %q", got)
	}

	// A dry run says which version the promotion would cut, and cuts nothing.
	dry, err := f.client.PromoteDeploy(ctx, as("writer", &forgev1.PromoteDeployRequest{Service: "services/payments", ValidateOnly: true}))
	if err != nil || dry.Msg.GetDeployment().GetVersion() != "v0.1.0" || len(rel.Tags()) != 2 {
		t.Fatalf("dry run: %v %v, tags %q", dry, err, rel.Names())
	}

	// Promoting cuts the final version on the same commit.
	op = f.promote(t, forgev1.Bump_BUMP_UNSPECIFIED)
	if op.GetError() != nil || eventSteps(op) != "release,commit,namespace,sync" {
		t.Fatalf("promote: %v %s", op.GetError(), eventSteps(op))
	}
	if got := rel.Names(); !strings.HasSuffix(got, " v0.1.0@"+old7) {
		t.Errorf("tags %q", got)
	}
	if r, ok := rel.Release("v0.1.0"); !ok || r.Prerelease || !r.Latest || r.Commit != ciOldSHA || r.Previous != "" {
		t.Errorf("final release %+v %v", r, ok)
	}
	// Staging runs the released commit, so it shows the final version too;
	// its history keeps what it was deployed as.
	if got := f.versions(t); got != "staging=v0.1.0 production=v0.1.0" {
		t.Errorf("versions %q", got)
	}
	if got, err := f.client.GetDeployment(ctx, as("reader", &forgev1.GetDeploymentRequest{Name: "deployments/payments/staging"})); err != nil || got.Msg.GetDeployment().GetVersion() != "v0.1.0" {
		t.Errorf("get staging: %v %v", got, err)
	}
	stagingHist, _ := f.client.ListDeployHistory(ctx, as("reader", &forgev1.ListDeployHistoryRequest{Name: "deployments/payments/staging", Limit: 1}))
	if r := stagingHist.Msg.GetRecords(); len(r) != 1 || r[0].GetVersion() != "v0.1.0-rc.2" {
		t.Errorf("staging history %v", r)
	}

	// Back to main on staging. Its old candidate was for v0.1.0, which was
	// released from the other commit: it is now a candidate for the next one.
	if op := f.deployRef(t, "staging", ""); op.GetError() != nil {
		t.Fatal(op.GetError())
	}
	if got := f.versions(t); got != "staging=v0.1.1-rc.1 production=v0.1.0" {
		t.Errorf("versions %q", got)
	}
	// A bump is picked for production only.
	if _, err := f.client.StartDeploy(ctx, as("writer", &forgev1.StartDeployRequest{Service: "services/payments", Env: "staging", Bump: forgev1.Bump_BUMP_MINOR})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("a bump on a staging deploy: %v", err)
	}
	// A minor is picked at the promotion; notes run from the last final.
	if op := f.promote(t, forgev1.Bump_BUMP_MINOR); op.GetError() != nil {
		t.Fatal(op.GetError())
	}
	if r, ok := rel.Release("v0.2.0"); !ok || r.Commit != envSHA || r.Previous != "v0.1.0" {
		t.Errorf("minor release %+v %v", r, ok)
	}
	// A released commit keeps its version, whatever is picked.
	tags := rel.Names()
	if op := f.promote(t, forgev1.Bump_BUMP_MAJOR); op.GetError() != nil {
		t.Fatal(op.GetError())
	}
	if got := rel.Names(); got != tags {
		t.Errorf("promoting a released commit cut %q, had %q", got, tags)
	}
	if got := f.versions(t); got != "staging=v0.2.0 production=v0.2.0" {
		t.Errorf("versions %q", got)
	}
	// The next build after a release is a candidate for the next patch.
	next := planVersion(rel.Tags(), strings.Repeat("c", 40), false, semver.Patch)
	if next.version.String() != "v0.2.1-rc.1" || next.previous != "v0.2.0" {
		t.Errorf("next candidate %+v", next)
	}

	// A rollback brings the version of the deploy it restores, and cuts nothing.
	rb, err := f.client.RollbackDeploy(ctx, as("writer", &forgev1.RollbackDeployRequest{Name: "deployments/payments/production", To: firstDeploy(t, f, "production")}))
	if err != nil {
		t.Fatal(err)
	}
	if op := wait(t, f.ops, rb.Msg.GetOperation().GetName()); op.GetError() != nil || eventSteps(op) != "commit,namespace,sync" {
		t.Fatalf("rollback: %v %s", op.GetError(), eventSteps(op))
	}
	// Staging no longer runs what production runs, so it shows what it was
	// deployed as.
	if got := f.versions(t); got != "staging=v0.1.1-rc.1 production=v0.1.0" {
		t.Errorf("after the rollback: versions %q", got)
	}
	if got := rel.Names(); got != tags {
		t.Errorf("the rollback cut %q, had %q", got, tags)
	}

	if _, err := f.client.PromoteDeploy(ctx, as("writer", &forgev1.PromoteDeployRequest{Service: "services/payments", Bump: 9})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("unknown bump: %v", err)
	}
}

// stepMessage is the message of op's last event for step.
func stepMessage(op *forgev1.Operation, step string) string {
	msg := ""
	for _, e := range op.GetEvents() {
		if e.GetStep() == step {
			msg = e.GetMessage()
		}
	}
	return msg
}

// firstDeploy is the oldest deploy commit of payments in env.
func firstDeploy(t *testing.T, f *deployFixture, env string) string {
	t.Helper()
	hist, err := f.client.ListDeployHistory(context.Background(), as("reader", &forgev1.ListDeployHistoryRequest{Name: "deployments/payments/" + env}))
	if err != nil || len(hist.Msg.GetRecords()) == 0 {
		t.Fatalf("history of %s: %v %v", env, hist, err)
	}
	r := hist.Msg.GetRecords()
	return r[len(r)-1].GetDeployCommit()
}

func TestReleaseVersionFailures(t *testing.T) {
	t.Parallel()
	f, rel := newReleaseFixture(t)
	ctx := context.Background()

	// GitHub refuses the tag: the deploy fails before it changes anything.
	rel.Fail(errors.New("502 Bad Gateway"), nil)
	op := f.deployRef(t, "staging", "")
	if e := op.GetError(); e.GetCode() != "release_failed" || !strings.Contains(e.GetMessage(), "v0.1.0-rc.1") || !strings.Contains(e.GetMessage(), "github.com/showmino/payments") {
		t.Fatalf("op error %v", e)
	}
	if _, err := f.repo.Read(ctx, valuesPath("payments", "staging"), ""); !errors.Is(err, github.ErrNotFound) {
		t.Errorf("a deploy without a version was committed: %v", err)
	}

	// The tag is made but the release is not: the deploy goes on and says
	// so, and the next deploy of the commit makes the release.
	rel.Fail(nil, errors.New("503"))
	op = f.deployRef(t, "staging", "")
	if op.GetError() != nil || rel.Names() != "v0.1.0-rc.1@"+shortSHA(envSHA) || !strings.Contains(stepMessage(op, "release"), "could not be made") {
		t.Fatalf("op error %v, tags %q, release step %q", op.GetError(), rel.Names(), stepMessage(op, "release"))
	}
	if got := f.versions(t); got != "staging=v0.1.0-rc.1" {
		t.Errorf("versions %q", got)
	}
	rel.Fail(nil, nil)
	if op := f.deployRef(t, "staging", ""); op.GetError() != nil {
		t.Fatal(op.GetError())
	}
	if _, ok := rel.Release("v0.1.0-rc.1"); !ok || rel.Names() != "v0.1.0-rc.1@"+shortSHA(envSHA) {
		t.Errorf("after the next deploy: tags %q", rel.Names())
	}

	// A promotion that cannot tag changes nothing in production.
	rel.Fail(errors.New("502 Bad Gateway"), nil)
	if e := f.promote(t, forgev1.Bump_BUMP_UNSPECIFIED).GetError(); e.GetCode() != "release_failed" {
		t.Fatalf("promote: %v", e)
	}
	if _, err := f.repo.Read(ctx, valuesPath("payments", "production"), ""); !errors.Is(err, github.ErrNotFound) {
		t.Errorf("a promotion without a version was committed: %v", err)
	}
	// It tags but cannot make the release: production runs the version.
	rel.Fail(nil, errors.New("503"))
	if op := f.promote(t, forgev1.Bump_BUMP_UNSPECIFIED); op.GetError() != nil || !strings.Contains(stepMessage(op, "release"), "could not be made") {
		t.Fatalf("promote: %v, release step %q", op.GetError(), stepMessage(op, "release"))
	}
	if got := f.versions(t); got != "staging=v0.1.0 production=v0.1.0" {
		t.Errorf("versions %q", got)
	}
	// A deploy to staging never makes the release of a final version (it
	// would be CI making it, and as a candidate).
	rel.Fail(nil, nil)
	if op := f.deployRef(t, "staging", ""); op.GetError() != nil || stepMessage(op, "release") != shortSHA(envSHA)+" is v0.1.0" {
		t.Fatalf("staging redeploy: %v, release step %q", op.GetError(), stepMessage(op, "release"))
	}
	if _, ok := rel.Release("v0.1.0"); ok {
		t.Error("a staging deploy made the release of a final version")
	}
	// The next promotion of the commit makes it, as a final release.
	if op := f.promote(t, forgev1.Bump_BUMP_MAJOR); op.GetError() != nil {
		t.Fatal(op.GetError())
	}
	if r, ok := rel.Release("v0.1.0"); !ok || r.Prerelease || !r.Latest {
		t.Errorf("repaired release %+v %v", r, ok)
	}

	// Someone tags the next number first: Forge takes the one after it.
	rel.Race = &github.Tag{Name: "v0.1.1-rc.1", Commit: strings.Repeat("d", 40)}
	if op := f.deployRef(t, "staging", "release"); op.GetError() != nil {
		t.Fatal(op.GetError())
	}
	if got := rel.Names(); !strings.HasSuffix(got, " v0.1.1-rc.2@"+shortSHA(ciOldSHA)) {
		t.Errorf("after a race: tags %q", got)
	}
}

func TestPlanVersion(t *testing.T) {
	t.Parallel()
	a, b := strings.Repeat("a", 40), strings.Repeat("b", 40)
	tags := []github.Tag{
		{Name: "v0.3.0", Commit: a}, {Name: "v0.3.1-rc.1", Commit: b}, {Name: "nightly", Commit: b},
		{Name: "v1.2", Commit: b}, {Name: "v0.3.0-rc.4", Commit: a},
	}
	// The candidate of b was for v0.1.1, which was released from a.
	stale := []github.Tag{{Name: "v0.1.1-rc.1", Commit: a}, {Name: "v0.1.1-rc.2", Commit: b}, {Name: "v0.1.1", Commit: a}, {Name: "v0.1.0", Commit: b}}
	for _, tc := range []struct {
		commit         string
		final          bool
		want           string
		tagged, latest bool
	}{
		{b, false, "v0.1.0", true, false}, // staging shows the final version b has
		{b, true, "v0.1.0", true, false},  // an old release is not the latest
		{a, true, "v0.1.1", true, true},   // the highest one is
		{strings.Repeat("c", 40), false, "v0.1.2-rc.1", false, false},
		{strings.Repeat("c", 40), true, "v0.1.2", false, true},
	} {
		p := planVersion(stale, tc.commit, tc.final, semver.Patch)
		if p.version.String() != tc.want || p.tagged != tc.tagged || p.latest != tc.latest {
			t.Errorf("%s final=%v: got %s tagged=%v latest=%v", shortSHA(tc.commit), tc.final, p.version, p.tagged, p.latest)
		}
	}
	// Without b's final version its stale candidate gives way to a new one.
	if p := planVersion(stale[:3], b, false, semver.Patch); p.version.String() != "v0.1.2-rc.1" || p.tagged {
		t.Errorf("a stale candidate: got %s tagged=%v", p.version, p.tagged)
	}

	for _, tc := range []struct {
		name, commit string
		final        bool
		bump         semver.Bump
		want         string
		tagged       bool
		previous     string
	}{
		{"a released commit on staging shows its final version", a, false, semver.Patch, "v0.3.0", true, ""},
		{"a released commit keeps its version in production", a, true, semver.Major, "v0.3.0", true, ""},
		{"a candidate keeps its number on staging", b, false, semver.Patch, "v0.3.1-rc.1", true, "v0.3.0"},
		{"a candidate becomes the next patch", b, true, semver.Patch, "v0.3.1", false, "v0.3.0"},
		{"or the next minor", b, true, semver.Minor, "v0.4.0", false, "v0.3.0"},
		{"a new commit is the next candidate", strings.Repeat("c", 40), false, semver.Patch, "v0.3.1-rc.2", false, "v0.3.1-rc.1"},
	} {
		p := planVersion(tags, tc.commit, tc.final, tc.bump)
		if p.version.String() != tc.want || p.tagged != tc.tagged || p.previous != tc.previous {
			t.Errorf("%s: got %s tagged=%v previous=%q", tc.name, p.version, p.tagged, p.previous)
		}
	}
}
