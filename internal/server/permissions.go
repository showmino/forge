package server

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
)

// globalActions are the actions about no one service.
var globalActions = []string{ActServiceCreate, ActServiceCreateAnyTeam, ActServiceRegister}

// TestPermissions answers which actions the caller may do. It decides as
// authorize does, so a client that hides what is not allowed hides exactly
// what the server would refuse.
func (a *authService) TestPermissions(ctx context.Context, req *connect.Request[forgev1.TestPermissionsRequest]) (*connect.Response[forgev1.TestPermissionsResponse], error) {
	p, ok := PrincipalFrom(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("not authenticated"))
	}
	m := req.Msg
	org := a.o.Authorizer.Org
	actions := m.GetActions()
	if len(actions) == 0 {
		actions = globalActions
		if m.GetService() != "" {
			actions = serviceActions
		}
	}
	for _, act := range actions {
		if _, known := actionRoles[act]; !known {
			return nil, invalidArg("unknown action %q; the actions are %v", act, Actions())
		}
	}
	team := m.GetOwnerTeam()
	if m.GetService() != "" {
		name, err := parseName(m.GetService())
		if err != nil {
			return nil, err
		}
		if team, err = a.o.Catalog.teamOf(ctx, name); err != nil {
			return nil, err
		}
	}
	out := &forgev1.TestPermissionsResponse{Role: roleFor(p, org, team).String()}
	for _, act := range actions {
		ok := allowed(p, org, act, team)
		// Without a team, a person may create a service if they are in any.
		if act == ActServiceCreate && team == "" && m.GetService() == "" {
			ok = isAdmin(p) || len(teamsOf(p, org)) > 0
		}
		if ok {
			out.Allowed = append(out.Allowed, act)
		}
	}
	return connect.NewResponse(out), nil
}
