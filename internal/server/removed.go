package server

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
)

// Calls removed in Forge 0.57.0 (the owner's choice, 2026-11-14). They stay
// in the protos, deprecated, so the API stays compatible, and answer
// UNIMPLEMENTED with what happened to them.

// ListServiceChecks listed the golden-path checks.
func (s *catalogService) ListServiceChecks(context.Context, *connect.Request[forgev1.ListServiceChecksRequest]) (*connect.Response[forgev1.ListServiceChecksResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("the golden-path checks were removed in Forge 0.57.0"))
}

// UpdateTeam set a team's on-call rotation.
func (s *teamService) UpdateTeam(context.Context, *connect.Request[forgev1.UpdateTeamRequest]) (*connect.Response[forgev1.UpdateTeamResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("a team's on-call rotation was removed in Forge 0.57.0"))
}
