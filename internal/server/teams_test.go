package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/github"
	"github.com/showmino/forge/internal/store"
)

type fakeTeamSource struct {
	teams []github.Team
	err   error
	calls int
}

func (f *fakeTeamSource) ListTeams(context.Context, string) ([]github.Team, error) {
	f.calls++
	return f.teams, f.err
}

func TestListTeams(t *testing.T) {
	t.Parallel()
	reader := context.WithValue(context.Background(), principalKey{}, &Principal{Subject: "ana", Permissions: []string{PermServicesRead}})
	list := func(s *teamService, ctx context.Context) ([]*forgev1.Team, error) {
		resp, err := s.ListTeams(ctx, connect.NewRequest(&forgev1.ListTeamsRequest{}))
		if err != nil {
			return nil, err
		}
		return resp.Msg.GetTeams(), nil
	}
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	src := &fakeTeamSource{teams: []github.Team{{Slug: "billing", Name: "Billing", Description: "Payments"}, {Slug: "platform-engineering", Name: "Platform Engineering"}}}
	s := &teamService{src: src, org: "showmino", log: slog.New(slog.DiscardHandler), now: func() time.Time { return now }}

	teams, err := list(s, reader)
	if err != nil {
		t.Fatal(err)
	}
	if len(teams) != 2 || teams[0].GetName() != "teams/billing" || teams[0].GetDisplayName() != "Billing" || teams[0].GetDescription() != "Payments" || teams[1].GetName() != "teams/platform-engineering" {
		t.Errorf("teams: %v", teams)
	}

	// A form that offers teams asks often; GitHub is asked once a minute.
	if _, err := list(s, reader); err != nil || src.calls != 1 {
		t.Errorf("second call: %v, GitHub asked %d times", err, src.calls)
	}
	now = now.Add(teamsFresh + time.Second)
	src.teams = append(src.teams, github.Team{Slug: "risk", Name: "Risk"})
	if teams, err := list(s, reader); err != nil || src.calls != 2 || len(teams) != 3 {
		t.Errorf("after a minute: %v, GitHub asked %d times, %d teams", err, src.calls, len(teams))
	}

	// Someone without a login gets nothing.
	if _, err := list(s, context.Background()); connect.CodeOf(err) != connect.CodeUnauthenticated && connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("no principal: %v", err)
	}

	t.Run("no GitHub App", func(t *testing.T) {
		_, err := list(&teamService{org: "showmino", log: slog.New(slog.DiscardHandler), now: time.Now}, reader)
		if connect.CodeOf(err) != connect.CodeFailedPrecondition {
			t.Errorf("got %v", err)
		}
	})
	t.Run("the App may not read teams", func(t *testing.T) {
		denied := &fakeTeamSource{err: &github.StatusError{Method: "GET", Path: "/orgs/showmino/teams", Status: http.StatusForbidden, Body: `{"message":"Resource not accessible by integration"}`}}
		_, err := list(&teamService{src: denied, org: "showmino", log: slog.New(slog.DiscardHandler), now: time.Now}, reader)
		if connect.CodeOf(err) != connect.CodeFailedPrecondition || !contains(err, "Members: read") || contains(err, "integration") || contains(err, "/orgs/") {
			t.Errorf("got %v: it must name the permission and keep GitHub's own text out", err)
		}
	})
	t.Run("GitHub is down, and a failure is not remembered", func(t *testing.T) {
		flaky := &fakeTeamSource{err: errors.New("dial tcp: connection refused")}
		s := &teamService{src: flaky, org: "showmino", log: slog.New(slog.DiscardHandler), now: time.Now}
		if _, err := list(s, reader); connect.CodeOf(err) != connect.CodeUnavailable || contains(err, "dial tcp") {
			t.Errorf("got %v", err)
		}
		flaky.err, flaky.teams = nil, []github.Team{{Slug: "billing"}}
		if teams, err := list(s, reader); err != nil || len(teams) != 1 {
			t.Errorf("after GitHub came back: %v %v", teams, err)
		}
	})
}

func contains(err error, text string) bool {
	return err != nil && strings.Contains(err.Error(), text)
}

// countingTeams also counts each team's members, as *github.Client does.
type countingTeams struct {
	fakeTeamSource
	members map[string]int
}

func (c *countingTeams) TeamMemberCount(_ context.Context, _, slug string) (int, error) {
	n, ok := c.members[slug]
	if !ok {
		return 0, errors.New("GitHub is away")
	}
	return n, nil
}

// A team's on-call rotation is an admin's to set; ListTeams shows it with
// how many members the team has on GitHub.
func TestTeamOncall(t *testing.T) {
	t.Parallel()
	reader := context.WithValue(context.Background(), principalKey{}, &Principal{Subject: "bo", Permissions: []string{PermServicesRead}})
	src := &countingTeams{
		fakeTeamSource: fakeTeamSource{teams: []github.Team{{Slug: "billing", Name: "Billing"}, {Slug: "risk", Name: "Risk"}}},
		members:        map[string]int{"billing": 6},
	}
	st := store.NewMemory()
	now := time.Date(2026, 11, 9, 9, 0, 0, 0, time.UTC)
	s := &teamService{src: src, settings: st, org: "showmino", log: slog.New(slog.DiscardHandler), now: func() time.Time { return now }}
	update := func(ctx context.Context, name, oncall string, validate bool) (*forgev1.Team, error) {
		resp, err := s.UpdateTeam(ctx, connect.NewRequest(&forgev1.UpdateTeamRequest{
			Team:         &forgev1.Team{Name: name, Oncall: oncall},
			UpdateMask:   &fieldmaskpb.FieldMask{Paths: []string{"oncall"}},
			ValidateOnly: validate,
		}))
		if err != nil {
			return nil, err
		}
		return resp.Msg.GetTeam(), nil
	}

	if _, err := update(reader, "teams/billing", "billing-primary", false); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("a reader: %v", err)
	}
	if _, err := update(adminCtx(), "teams/nobody", "x", false); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("an unknown team: %v", err)
	}
	if _, err := update(adminCtx(), "teams/billing", "billing primary!", false); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("a bad name: %v", err)
	}
	if got, err := update(adminCtx(), "teams/billing", "billing-primary", true); err != nil || got.GetOncall() != "billing-primary" {
		t.Errorf("dry run: %v %v", got, err)
	}
	if set, _ := st.TeamSettings(context.Background()); len(set) != 0 {
		t.Errorf("a dry run wrote %v", set)
	}
	got, err := update(adminCtx(), "teams/billing", "billing-primary", false)
	if err != nil || got.GetOncall() != "billing-primary" || got.GetDisplayName() != "Billing" {
		t.Fatalf("update: %v %v", got, err)
	}

	resp, err := s.ListTeams(reader, connect.NewRequest(&forgev1.ListTeamsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	teams := resp.Msg.GetTeams()
	if teams[0].GetOncall() != "billing-primary" || teams[0].GetMemberCount() != 6 {
		t.Errorf("billing %v", teams[0])
	}
	// GitHub could not count risk's members: 0, and the list still answers.
	if teams[1].GetOncall() != "" || teams[1].GetMemberCount() != 0 {
		t.Errorf("risk %v", teams[1])
	}

	// An empty rotation removes it.
	if got, err := update(adminCtx(), "teams/billing", "", false); err != nil || got.GetOncall() != "" {
		t.Errorf("clear: %v %v", got, err)
	}
}
