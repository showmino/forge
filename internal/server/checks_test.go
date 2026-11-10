package server

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/envtest"
	"github.com/showmino/forge/internal/store"
)

type checksFixture struct {
	c   *checker
	gh  *envtest.DocsGitHub
	st  *store.Memory
	now time.Time
}

// newChecksFixture is four services far apart on the golden path:
// payments has everything; ledger only its team's on-call; web runs an
// image, so it has no repo to check; api's CI deployed it, with no workflow
// in its repo.
func newChecksFixture(t *testing.T) *checksFixture {
	t.Helper()
	ctx := context.Background()
	cat := NewCatalog(nil)
	for _, reg := range []*forgev1.RegisterServiceRequest{
		{ServiceId: "payments", Service: &forgev1.Service{OwnerTeam: "billing", RepoUrl: "https://github.com/showmino/payments", Slo: "99.9%", Tracing: true}},
		{ServiceId: "ledger", Service: &forgev1.Service{OwnerTeam: "billing", RepoUrl: "https://github.com/showmino/ledger"}},
		{ServiceId: "web", Service: &forgev1.Service{OwnerTeam: "growth"}},
		{ServiceId: "api", Service: &forgev1.Service{OwnerTeam: "growth", RepoUrl: "https://github.com/showmino/api"}},
	} {
		if _, err := cat.RegisterService(writerCtx(), connect.NewRequest(reg)); err != nil {
			t.Fatal(err)
		}
	}
	f := &checksFixture{gh: &envtest.DocsGitHub{}, st: store.NewMemory(), now: time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)}
	f.gh.Push("showmino", "payments", map[string]string{
		".github/workflows/ci.yml": "on: push",
		"docs/index.md":            "# Payments",
		"docs/runbook.md":          "# Runbook",
	}, f.now)
	f.gh.Push("showmino", "ledger", map[string]string{"README.md": "# ledger", ".github/CODEOWNERS": "* @billing"}, f.now)
	f.gh.Push("showmino", "api", map[string]string{"README.md": "# api"}, f.now)
	if err := f.st.SetTeamOncall(ctx, "billing", "billing-primary", "ana@example.com", f.now); err != nil {
		t.Fatal(err)
	}
	ci := &forgev1.AuditEvent{
		Time: timestamppb.New(f.now.Add(-48 * time.Hour)), Kind: "request", Action: "deploy.start", Service: "services/api", Target: "deployments/api/staging",
		Actor: &forgev1.AuditActor{Kind: "workload", Repository: "showmino/api"}, Result: &forgev1.AuditResult{Code: "ok"},
	}
	if _, err := f.st.AppendAuditEvent(ctx, ci); err != nil {
		t.Fatal(err)
	}
	f.c = newChecker(cat, f.st, f.st, f.gh, "showmino", slog.New(slog.DiscardHandler))
	f.c.now = func() time.Time { return f.now }
	return f
}

func (f *checksFixture) list(t *testing.T, service string) map[string]*forgev1.ServiceChecks {
	t.Helper()
	resp, err := f.c.ListServiceChecks(readerCtx(), connect.NewRequest(&forgev1.ListServiceChecksRequest{Service: service}))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*forgev1.ServiceChecks{}
	for _, s := range resp.Msg.GetServices() {
		out[strings.TrimPrefix(s.GetService(), servicePrefix)] = s
	}
	return out
}

// passing words a service's checks as "ci slo -tracing ~runbook": a name
// passes, -name fails, ~name does not apply.
func passing(s *forgev1.ServiceChecks) string {
	var w []string
	for _, c := range s.GetChecks() {
		switch {
		case !c.GetApplies():
			w = append(w, "~"+c.GetName())
		case c.GetPassed():
			w = append(w, c.GetName())
		default:
			w = append(w, "-"+c.GetName())
		}
	}
	return strings.Join(w, " ")
}

func TestServiceChecks(t *testing.T) {
	t.Parallel()
	f := newChecksFixture(t)
	got := f.list(t, "")
	for id, want := range map[string]string{
		"payments": "ci slo tracing runbook oncall",
		"ledger":   "-ci -slo -tracing -runbook oncall",
		"web":      "~ci -slo -tracing ~runbook -oncall",
		"api":      "ci -slo -tracing -runbook -oncall",
	} {
		if passing(got[id]) != want {
			t.Errorf("%s: %s, want %s", id, passing(got[id]), want)
		}
	}
	if p := got["payments"]; p.GetPassed() != 5 || p.GetApplies() != 5 {
		t.Errorf("payments %d of %d", p.GetPassed(), p.GetApplies())
	}
	if w := got["web"]; w.GetPassed() != 0 || w.GetApplies() != 3 {
		t.Errorf("web %d of %d", w.GetPassed(), w.GetApplies())
	}
	// What to do is in the detail.
	for _, c := range got["ledger"].GetChecks() {
		if c.GetName() == "runbook" && !strings.Contains(c.GetDetail(), "docs/runbook.md") {
			t.Errorf("runbook detail %q", c.GetDetail())
		}
		if c.GetName() == "ci" && !strings.Contains(c.GetDetail(), ".github/workflows") {
			t.Errorf("ci detail %q", c.GetDetail())
		}
	}

	// One service.
	if one := f.list(t, "services/ledger"); len(one) != 1 || one["ledger"] == nil {
		t.Errorf("one: %v", one)
	}
	if _, err := f.c.ListServiceChecks(readerCtx(), connect.NewRequest(&forgev1.ListServiceChecksRequest{Service: "services/nope"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("unknown service: %v", err)
	}
	if _, err := f.c.ListServiceChecks(context.Background(), connect.NewRequest(&forgev1.ListServiceChecksRequest{})); err == nil {
		t.Error("no login: allowed")
	}
}

// A change in the catalog or a team's on-call shows at once; what is read
// from the repo is kept for a few minutes.
func TestServiceChecksFreshness(t *testing.T) {
	t.Parallel()
	f := newChecksFixture(t)
	f.list(t, "services/ledger")
	f.gh.Push("showmino", "ledger", map[string]string{"docs/runbook.md": "# Runbook", ".github/workflows/ci.yml": "on: push"}, f.now)
	if _, err := f.c.catalog.UpdateService(writerCtx(), connect.NewRequest(&forgev1.UpdateServiceRequest{
		Service:    &forgev1.Service{Name: "services/ledger", Slo: "99.5%", Tracing: true},
		UpdateMask: fieldMask("slo", "tracing"),
	})); err != nil {
		t.Fatal(err)
	}
	if got := passing(f.list(t, "services/ledger")["ledger"]); got != "-ci slo tracing -runbook oncall" {
		t.Errorf("at once: %s", got)
	}
	f.now = f.now.Add(checksFresh + time.Second)
	if got := passing(f.list(t, "services/ledger")["ledger"]); got != "ci slo tracing runbook oncall" {
		t.Errorf("after the repo is read again: %s", got)
	}
}
