package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/store"
)

// A rule set to warn lets the action through, and the audit log keeps the
// warning; set back to enforce, it refuses again.
func TestWarnedRuleLetsItThrough(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newImageFixture(t, nil)
	req := func(id string) *forgev1.CreateServiceRequest {
		return &forgev1.CreateServiceRequest{ServiceId: id, Image: "nginx:latest", OwnerTeam: "billing"}
	}
	if _, err := f.create(t, req("web")); err == nil || !strings.Contains(err.Error(), "the tag latest moves") {
		t.Fatalf("enforced: %v", err)
	}
	if err := f.store.SetRuleMode(ctx, "images.pinned-version", "warn", "ana@example.com", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.create(t, req("web")); err != nil {
		t.Fatalf("warned: %v", err)
	}
	events, err := f.store.ListAuditEvents(ctx, store.AuditQuery{Action: "service.create", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	var warned bool
	for _, e := range events {
		if strings.Contains(e.GetDetails()["policy_warnings"], "the tag latest moves") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("no audit event keeps the warning: %v", events)
	}
}

// Only an admin sets a mode, and only of a switchable rule.
func TestUpdateRule(t *testing.T) {
	t.Parallel()
	st := store.NewMemory()
	s := &policyService{st: st, now: time.Now}
	set := func(ctx context.Context, name, mode string) (*forgev1.Rule, error) {
		resp, err := s.UpdateRule(ctx, connect.NewRequest(&forgev1.UpdateRuleRequest{
			Rule: &forgev1.Rule{Name: name, Mode: mode}, UpdateMask: fieldMask("mode"),
		}))
		if err != nil {
			return nil, err
		}
		return resp.Msg.GetRule(), nil
	}
	member := context.WithValue(context.Background(), principalKey{}, &Principal{Subject: "bo", Groups: []string{"showmino:billing"}, Permissions: []string{PermServicesRead}})
	ci := context.WithValue(context.Background(), principalKey{}, &Principal{Subject: "repo:showmino/payments", Permissions: []string{PermServicesRead, PermServicesWrite}, Workload: &Workload{}})
	for who, ctx := range map[string]context.Context{"a reader": readerCtx(), "a team member": member, "a CI workload": ci} {
		if _, err := set(ctx, "rules/images.pinned-version", "off"); connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Errorf("%s: %v", who, err)
		}
	}
	if _, err := set(writerCtx(), "rules/promotion.checked-deploy", "off"); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("a rule that guards Forge: %v", err)
	}
	if _, err := set(writerCtx(), "rules/images.pinned-version", "loud"); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("a bad mode: %v", err)
	}
	if r, err := set(writerCtx(), "rules/resources.staging-single", "off"); err != nil || r.GetMode() != "off" || r.GetDefaultMode() != "warn" || r.GetUpdatedBy() == "" {
		t.Errorf("off: %v %v", r, err)
	}
	// Back to the default: nothing is kept, so a later default applies.
	if r, err := set(writerCtx(), "rules/resources.staging-single", "warn"); err != nil || r.GetMode() != "warn" || r.GetUpdatedBy() != "" {
		t.Errorf("default: %v %v", r, err)
	}
	if m, _ := st.RuleModes(context.Background()); len(m) != 0 {
		t.Errorf("stored %v", m)
	}
	resp, err := s.ListRules(readerCtx(), connect.NewRequest(&forgev1.ListRulesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	var switchable, always int
	for _, r := range resp.Msg.GetRules() {
		if r.GetSwitchable() {
			switchable++
		} else if r.GetMode() == "enforce" {
			always++
		}
	}
	if switchable != 8 || always != 7 {
		t.Errorf("%d switchable, %d always", switchable, always)
	}
}
