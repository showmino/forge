package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/sync/errgroup"
	"google.golang.org/protobuf/proto"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/github"
	"github.com/showmino/forge/internal/store"
)

const teamPrefix = "teams/"

// teamsFresh is how long a list of teams is served from memory. Teams change
// rarely, and every form that offers them asks.
const teamsFresh = time.Minute

// TeamSource lists the teams of a GitHub organization; *github.Client is one.
type TeamSource interface {
	ListTeams(ctx context.Context, org string) ([]github.Team, error)
}

// TeamCounter counts a team's members; *github.Client is one. A
// TeamSource that is not one leaves member_count 0.
type TeamCounter interface {
	TeamMemberCount(ctx context.Context, org, slug string) (int, error)
}

// teamService serves TeamService: the teams from GitHub, and what Forge
// keeps about them (their on-call rotation) from settings.
type teamService struct {
	src      TeamSource
	settings store.TeamStore
	org      string
	log      *slog.Logger
	now      func() time.Time

	mu      sync.Mutex
	teams   []*forgev1.Team
	fetched time.Time
}

func (s *teamService) ListTeams(ctx context.Context, _ *connect.Request[forgev1.ListTeamsRequest]) (*connect.Response[forgev1.ListTeamsResponse], error) {
	if err := require(ctx, PermServicesRead); err != nil {
		return nil, err
	}
	if s.src == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("teams come from GitHub: this Forge has no GitHub App configured"))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.teams != nil && s.now().Sub(s.fetched) < teamsFresh {
		return s.withSettings(ctx, s.teams)
	}
	got, err := s.src.ListTeams(ctx, s.org)
	if err != nil {
		// GitHub's own text names the App and its paths: log it, do not send it.
		s.log.WarnContext(ctx, "listing the organization's teams failed", "org", s.org, "error", err)
		var status *github.StatusError
		if errors.As(err, &status) && status.Status == http.StatusForbidden {
			return nil, connect.NewError(connect.CodeFailedPrecondition,
				errors.New("the Forge GitHub App may not read the organization's teams: grant it the organization permission \"Members: read\""))
		}
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("GitHub could not be asked for the organization's teams; try again in a moment"))
	}
	teams := make([]*forgev1.Team, 0, len(got))
	for _, t := range got {
		teams = append(teams, &forgev1.Team{Name: teamPrefix + t.Slug, DisplayName: t.Name, Description: t.Description})
	}
	s.countMembers(ctx, teams)
	s.teams, s.fetched = teams, s.now()
	return s.withSettings(ctx, teams)
}

// countMembers asks GitHub how many members each team has, a few teams at
// a time. A team GitHub could not count keeps 0: the list still answers.
func (s *teamService) countMembers(ctx context.Context, teams []*forgev1.Team) {
	counter, ok := s.src.(TeamCounter)
	if !ok {
		return
	}
	var g errgroup.Group
	g.SetLimit(8)
	var failed sync.Once
	for _, t := range teams {
		g.Go(func() error {
			n, err := counter.TeamMemberCount(ctx, s.org, strings.TrimPrefix(t.GetName(), teamPrefix))
			if err != nil {
				failed.Do(func() { s.log.WarnContext(ctx, "counting a team's members failed", "team", t.GetName(), "error", err) })
				return nil
			}
			t.MemberCount = int32(min(n, 1<<30)) //nolint:gosec // bounded above
			return nil
		})
	}
	_ = g.Wait()
}

// withSettings answers with the teams and what Forge keeps about them. The
// settings are read on every call: an admin's change shows at once.
func (s *teamService) withSettings(ctx context.Context, teams []*forgev1.Team) (*connect.Response[forgev1.ListTeamsResponse], error) {
	if s.settings == nil {
		return connect.NewResponse(&forgev1.ListTeamsResponse{Teams: teams}), nil
	}
	set, err := s.settings.TeamSettings(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("read the teams' settings: %w", err))
	}
	out := make([]*forgev1.Team, 0, len(teams))
	for _, t := range teams {
		c := proto.Clone(t).(*forgev1.Team)
		c.Oncall = set[strings.TrimPrefix(t.GetName(), teamPrefix)].Oncall
		out = append(out, c)
	}
	return connect.NewResponse(&forgev1.ListTeamsResponse{Teams: out}), nil
}

// oncallName is a rotation's name as people write it in a paging tool:
// letters, digits, '.', '_', '/', '-', at most 100.
var oncallName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,99}$`)

// UpdateTeam sets a team's on-call rotation. Admins only: a team's members
// are GitHub's, and who is paged is the platform's to keep straight.
func (s *teamService) UpdateTeam(ctx context.Context, req *connect.Request[forgev1.UpdateTeamRequest]) (*connect.Response[forgev1.UpdateTeamResponse], error) {
	in := req.Msg.GetTeam()
	slug := strings.TrimPrefix(in.GetName(), teamPrefix)
	if !strings.HasPrefix(in.GetName(), teamPrefix) || slug == "" {
		return nil, invalid(fmt.Sprintf("team.name %q must look like teams/{team}", in.GetName()))
	}
	if err := authorizeTeam(ctx, ActTeamUpdate, ""); err != nil {
		return nil, err
	}
	if paths := req.Msg.GetUpdateMask().GetPaths(); !slices.Equal(paths, []string{"oncall"}) {
		return nil, invalid(fmt.Sprintf("update_mask must be [oncall], got %q", paths))
	}
	if in.GetOncall() != "" && !oncallName.MatchString(in.GetOncall()) {
		return nil, invalid(fmt.Sprintf("oncall %q must be a rotation's name: letters, digits, '.', '_', '/' and '-', at most 100", in.GetOncall()))
	}
	if s.settings == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("this Forge keeps no team settings"))
	}
	list, err := s.ListTeams(ctx, connect.NewRequest(&forgev1.ListTeamsRequest{}))
	if err != nil {
		return nil, err
	}
	i := slices.IndexFunc(list.Msg.GetTeams(), func(t *forgev1.Team) bool { return t.GetName() == teamPrefix+slug })
	if i < 0 {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("teams/%s is not a team of %s on GitHub", slug, s.org))
	}
	out := proto.Clone(list.Msg.GetTeams()[i]).(*forgev1.Team)
	out.Oncall = in.GetOncall()
	if req.Msg.GetValidateOnly() {
		return connect.NewResponse(&forgev1.UpdateTeamResponse{Team: out}), nil
	}
	if err := s.settings.SetTeamOncall(ctx, slug, in.GetOncall(), actorOf(ctx), s.now()); err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("save the team's on-call rotation: %w", err))
	}
	return connect.NewResponse(&forgev1.UpdateTeamResponse{Team: out}), nil
}
