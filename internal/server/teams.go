package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"connectrpc.com/connect"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/github"
)

const teamPrefix = "teams/"

// teamsFresh is how long a list of teams is served from memory. Teams change
// rarely, and every form that offers them asks.
const teamsFresh = time.Minute

// TeamSource lists the teams of a GitHub organization; *github.Client is one.
type TeamSource interface {
	ListTeams(ctx context.Context, org string) ([]github.Team, error)
}

// teamService serves TeamService from GitHub.
type teamService struct {
	src TeamSource
	org string
	log *slog.Logger
	now func() time.Time

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
		return connect.NewResponse(&forgev1.ListTeamsResponse{Teams: s.teams}), nil
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
	s.teams, s.fetched = teams, s.now()
	return connect.NewResponse(&forgev1.ListTeamsResponse{Teams: teams}), nil
}
