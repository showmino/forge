package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/gen/forge/v1/forgev1connect"
	"github.com/showmino/forge/internal/github"
)

type fakeOrgNamer struct {
	name  string
	err   error
	calls int
}

func (f *fakeOrgNamer) OrgName(context.Context, string) (string, error) {
	f.calls++
	return f.name, f.err
}

func TestOrgDisplayName(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	src := &fakeOrgNamer{name: "My Org"}
	o := &orgName{src: src, org: "my-org", log: slog.New(slog.DiscardHandler), now: func() time.Time { return now }}

	if got := o.get(ctx); got != "My Org" {
		t.Errorf("got %q", got)
	}
	// Every page of the dashboard asks for the status; GitHub is asked once an hour.
	if got := o.get(ctx); got != "My Org" || src.calls != 1 {
		t.Errorf("second call: %q, GitHub asked %d times", got, src.calls)
	}
	now = now.Add(orgNameFresh + time.Second)
	src.name = "My Renamed Org"
	if got := o.get(ctx); got != "My Renamed Org" || src.calls != 2 {
		t.Errorf("after an hour: %q, GitHub asked %d times", got, src.calls)
	}

	// GitHub failing keeps the name it gave last, and asks again a minute later.
	now = now.Add(orgNameFresh + time.Second)
	src.err = errors.New("boom")
	if got := o.get(ctx); got != "My Renamed Org" || src.calls != 3 {
		t.Errorf("failing: %q, GitHub asked %d times", got, src.calls)
	}
	if got := o.get(ctx); got != "My Renamed Org" || src.calls != 3 {
		t.Errorf("failing again at once: %q, GitHub asked %d times", got, src.calls)
	}
	now = now.Add(orgNameRetry + time.Second)
	src.err, src.name = nil, ""
	// An organization without a display name is called by its slug.
	if got := o.get(ctx); got != "my-org" || src.calls != 4 {
		t.Errorf("unnamed: %q, GitHub asked %d times", got, src.calls)
	}

	// Without GitHub, or when it never answered, the slug; without an org, nothing.
	if got := (&orgName{org: "my-org"}).get(ctx); got != "my-org" {
		t.Errorf("no GitHub: %q", got)
	}
	failing := &orgName{src: &fakeOrgNamer{err: errors.New("boom")}, org: "my-org", log: slog.New(slog.DiscardHandler), now: time.Now}
	if got := failing.get(ctx); got != "my-org" {
		t.Errorf("never answered: %q", got)
	}
	if got := (&orgName{src: src}).get(ctx); got != "" {
		t.Errorf("no org: %q", got)
	}
}

// namedTeams is a TeamSource that also gives the organization's name, as
// *github.Client does.
type namedTeams struct{ fakeOrgNamer }

func (*namedTeams) ListTeams(context.Context, string) ([]github.Team, error) { return nil, nil }

func TestStatusOrganization(t *testing.T) {
	t.Parallel()
	status := func(o Options) *forgev1.GetStatusResponse {
		t.Helper()
		o.Authenticator = StaticAuthenticator{"tok": {Subject: "ana"}}
		h, err := New(o)
		if err != nil {
			t.Fatal(err)
		}
		srv := httptest.NewServer(h)
		t.Cleanup(srv.Close)
		req := connect.NewRequest(&forgev1.GetStatusRequest{})
		req.Header().Set("Authorization", "Bearer tok")
		resp, err := forgev1connect.NewAuthServiceClient(http.DefaultClient, srv.URL).GetStatus(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		return resp.Msg
	}
	if got := status(Options{GitHubOrg: "my-org", Teams: &namedTeams{fakeOrgNamer{name: "My Org"}}}); got.GetOrganization() != "my-org" || got.GetOrganizationDisplayName() != "My Org" {
		t.Errorf("with GitHub: %q %q", got.GetOrganization(), got.GetOrganizationDisplayName())
	}
	if got := status(Options{GitHubOrg: "my-org"}); got.GetOrganization() != "my-org" || got.GetOrganizationDisplayName() != "my-org" {
		t.Errorf("without GitHub: %q %q", got.GetOrganization(), got.GetOrganizationDisplayName())
	}
	if got := status(Options{}); got.GetOrganization() != "" || got.GetOrganizationDisplayName() != "" {
		t.Errorf("without an org: %q %q", got.GetOrganization(), got.GetOrganizationDisplayName())
	}
}
