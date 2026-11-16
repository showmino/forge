package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/envtest"
	"github.com/showmino/forge/internal/store"
)

// An application is promoted and rolled back as one, each time recording a
// release; a part that fails is put back to what it ran.
func TestApplicationPromoteAndRollback(t *testing.T) {
	t.Parallel()
	f := newDeployFixture(t, false, func(o *DeployOptions, _ *envtest.Argo) { o.SyncTimeout = time.Second })
	ctx := writerCtx()
	if _, err := f.catalog.UpdateService(ctx, connect.NewRequest(&forgev1.UpdateServiceRequest{
		Service:    &forgev1.Service{Name: "services/payments", Application: "shop"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"application"}},
	})); err != nil {
		t.Fatal(err)
	}
	s := &applicationService{catalog: f.catalog, deploys: f.deploys, releases: store.NewMemory(), now: time.Now}
	promote := func() *forgev1.Operation {
		t.Helper()
		resp, err := s.PromoteApplication(ctx, connect.NewRequest(&forgev1.PromoteApplicationRequest{Name: "applications/shop"}))
		if err != nil {
			t.Fatal(err)
		}
		return wait(t, f.ops, resp.Msg.GetOperation().GetName())
	}
	releases := func() []*forgev1.ApplicationRelease {
		t.Helper()
		resp, err := s.ListApplicationReleases(ctx, connect.NewRequest(&forgev1.ListApplicationReleasesRequest{Name: "applications/shop"}))
		if err != nil {
			t.Fatal(err)
		}
		return resp.Msg.GetReleases()
	}
	production := func() string { t.Helper(); return f.values(t, "production").Forge.Commit }

	// Nothing in staging: nothing to promote.
	if _, err := s.PromoteApplication(ctx, connect.NewRequest(&forgev1.PromoteApplicationRequest{Name: "applications/shop"})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("an empty promotion: %v", err)
	}
	if _, err := s.PromoteApplication(ctx, connect.NewRequest(&forgev1.PromoteApplicationRequest{Name: "applications/nope"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("an unknown application: %v", err)
	}

	// Release 1: main.
	if op := f.deployRef(t, "staging", ""); op.GetError() != nil {
		t.Fatal(op.GetError())
	}
	dry, err := s.PromoteApplication(ctx, connect.NewRequest(&forgev1.PromoteApplicationRequest{Name: "applications/shop", ValidateOnly: true}))
	if err != nil || dry.Msg.GetOperation() != nil || len(dry.Msg.GetParts()) != 1 || dry.Msg.GetParts()[0].GetService() != "services/payments" {
		t.Fatalf("dry run: %v %v", dry, err)
	}
	if op := promote(); op.GetError() != nil || op.GetKind() != kindApplicationPromote || op.GetTarget() != "applications/shop" {
		t.Fatalf("promote: %v", op)
	}
	if production() != envSHA {
		t.Fatalf("production runs %s", production())
	}
	rs := releases()
	if len(rs) != 1 || rs[0].GetNumber() != 1 || rs[0].GetName() != "applications/shop/releases/1" || len(rs[0].GetParts()) != 1 ||
		rs[0].GetParts()[0].GetCommit() != envSHA || rs[0].GetParts()[0].GetDeployCommit() == "" {
		t.Fatalf("release 1: %v", rs)
	}
	// Production runs what staging runs.
	if _, err := s.PromoteApplication(ctx, connect.NewRequest(&forgev1.PromoteApplicationRequest{Name: "applications/shop"})); connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "nothing to promote") {
		t.Fatalf("a second promotion: %v", err)
	}
	if _, err := s.RollbackApplication(ctx, connect.NewRequest(&forgev1.RollbackApplicationRequest{Name: "applications/shop"})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("a rollback with one release: %v", err)
	}

	// A restart is a commit of the values that is no deploy: what the part
	// ran before the next promotion is that commit.
	restart, err := f.client.RestartDeployment(context.Background(), as("writer", &forgev1.RestartDeploymentRequest{Name: "deployments/payments/production"}))
	if err != nil {
		t.Fatal(err)
	}
	if op := wait(t, f.ops, restart.Msg.GetOperation().GetName()); op.GetError() != nil {
		t.Fatal(op.GetError())
	}
	restarted := f.values(t, "production").RestartedAt
	if restarted == "" {
		t.Fatal("the restart left no mark")
	}

	// A part that fails in production is put back to what it ran.
	if op := f.deployRef(t, "staging", "release"); op.GetError() != nil {
		t.Fatal(op.GetError())
	}
	// The new version is unhealthy; what ran before it is healthy again.
	f.argo.SetUnhealthyOnce("payments-production", "the new version crashes")
	started, err := s.PromoteApplication(ctx, connect.NewRequest(&forgev1.PromoteApplicationRequest{Name: "applications/shop"}))
	if err != nil {
		t.Fatal(err)
	}
	op := wait(t, f.ops, started.Msg.GetOperation().GetName())
	if op.GetError() == nil || op.GetError().GetCode() != "promote_failed" || !strings.Contains(op.GetError().GetMessage(), "put back to what they ran: payments") {
		t.Fatalf("a failed promotion: %v", op.GetError())
	}
	if production() != envSHA {
		t.Errorf("after the put back production runs %s", production())
	}
	if len(releases()) != 1 {
		t.Errorf("a failed promotion recorded a release")
	}
	if strings.Contains(op.GetError().GetMessage(), "NOT put back") {
		t.Errorf("the part was not put back: %s", op.GetError().GetMessage())
	}
	// The locks are free again, and a reader may do none of this.
	reader := context.WithValue(context.Background(), principalKey{}, &Principal{Subject: "bo", Permissions: []string{PermServicesRead}})
	if _, err := s.PromoteApplication(reader, connect.NewRequest(&forgev1.PromoteApplicationRequest{Name: "applications/shop", ValidateOnly: true})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("a reader promoting: %v", err)
	}
	if _, err := s.RollbackApplication(reader, connect.NewRequest(&forgev1.RollbackApplicationRequest{Name: "applications/shop", ValidateOnly: true})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("a reader rolling back: %v", err)
	}
	// Without a store of releases nothing can be rolled back.
	bare := &applicationService{catalog: f.catalog, deploys: f.deploys, now: time.Now}
	if _, err := bare.RollbackApplication(ctx, connect.NewRequest(&forgev1.RollbackApplicationRequest{Name: "applications/shop"})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("a rollback without releases: %v", err)
	}

	// Release 2: the other commit.
	if op := promote(); op.GetError() != nil {
		t.Fatal(op.GetError())
	}
	if production() != ciOldSHA || len(releases()) != 2 {
		t.Fatalf("release 2: production %s, %d releases", production(), len(releases()))
	}

	// Back to release 1, which is recorded as release 3.
	rb, err := s.RollbackApplication(ctx, connect.NewRequest(&forgev1.RollbackApplicationRequest{Name: "applications/shop"}))
	if err != nil || rb.Msg.GetRelease().GetNumber() != 1 || len(rb.Msg.GetParts()) != 1 {
		t.Fatalf("rollback: %v %v", rb, err)
	}
	if op := wait(t, f.ops, rb.Msg.GetOperation().GetName()); op.GetError() != nil || op.GetKind() != kindApplicationRollback {
		t.Fatalf("rollback: %v", op)
	}
	if production() != envSHA {
		t.Errorf("after the rollback production runs %s", production())
	}
	rs = releases()
	if len(rs) != 3 || rs[0].GetNote() != "rollback to release 1" || rs[0].GetParts()[0].GetCommit() != envSHA {
		t.Errorf("release 3: %v", rs[0])
	}
	// And forward again, by number.
	rb, err = s.RollbackApplication(ctx, connect.NewRequest(&forgev1.RollbackApplicationRequest{Name: "applications/shop", Release: 2}))
	if err != nil {
		t.Fatal(err)
	}
	if op := wait(t, f.ops, rb.Msg.GetOperation().GetName()); op.GetError() != nil || production() != ciOldSHA {
		t.Errorf("to release 2: %v, production %s", op.GetError(), production())
	}
	if _, err := s.RollbackApplication(context.Background(), connect.NewRequest(&forgev1.RollbackApplicationRequest{Name: "applications/shop"})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("without a caller: %v", err)
	}
}
