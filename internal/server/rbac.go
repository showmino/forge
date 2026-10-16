package server

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"connectrpc.com/connect"
)

// Roles, from least to most. Every member of the organization is a reader;
// the members of a service's owner team work on it; the members of an admin
// group may do everything on every service.
type role int

const (
	roleReader role = iota
	roleTeam
	roleAdmin
)

func (r role) String() string {
	switch r {
	case roleTeam:
		return "team"
	case roleAdmin:
		return "admin"
	default:
		return "reader"
	}
}

// Actions a caller can be authorized for. Their names are the audit log's.
const (
	ActServiceCreate           = "service.create"
	ActServiceCreateAnyTeam    = "service.create.any_team"
	ActServiceUpdate           = "service.update"
	ActServiceUpdateTeam       = "service.update.team"
	ActServiceRegister         = "service.register"
	ActServiceDelete           = "service.delete"
	ActServiceAddress          = "service.address"
	ActDeployStart             = "deploy.start"
	ActDeployPromote           = "deploy.promote"
	ActDeployRollback          = "deploy.rollback"
	ActEnvironmentCreate       = "environment.create"
	ActEnvironmentExtend       = "environment.extend"
	ActEnvironmentDestroy      = "environment.destroy"
	ActOperationCancel         = "operation.cancel"
	ActResourcePlan            = "resource.plan"
	ActResourceApply           = "resource.apply"
	ActResourceApplyProduction = "resource.apply.production"
)

// actionRoles is the lowest role that may do each action. An action that is
// not here is refused to everyone but admins.
var actionRoles = map[string]role{
	ActServiceCreate:           roleTeam,
	ActServiceCreateAnyTeam:    roleAdmin,
	ActServiceUpdate:           roleTeam,
	ActServiceUpdateTeam:       roleAdmin,
	ActServiceRegister:         roleAdmin,
	ActServiceDelete:           roleAdmin,
	ActServiceAddress:          roleAdmin,
	ActDeployStart:             roleTeam,
	ActDeployPromote:           roleTeam,
	ActDeployRollback:          roleTeam,
	ActEnvironmentCreate:       roleTeam,
	ActEnvironmentExtend:       roleTeam,
	ActEnvironmentDestroy:      roleTeam,
	ActOperationCancel:         roleTeam,
	ActResourcePlan:            roleTeam,
	ActResourceApply:           roleTeam,
	ActResourceApplyProduction: roleAdmin,
}

// Actions lists every action, sorted.
func Actions() []string {
	out := make([]string, 0, len(actionRoles))
	for a := range actionRoles {
		out = append(out, a)
	}
	slices.Sort(out)
	return out
}

// serviceActions are the actions about one existing service, in the order
// clients show them.
var serviceActions = []string{
	ActServiceUpdate, ActServiceUpdateTeam, ActServiceAddress, ActServiceDelete,
	ActDeployStart, ActDeployPromote, ActDeployRollback,
	ActEnvironmentCreate, ActEnvironmentExtend, ActEnvironmentDestroy,
	ActOperationCancel, ActResourcePlan, ActResourceApply, ActResourceApplyProduction,
}

// access is what authorize needs besides the caller: put in the context by
// the auth interceptor.
type access struct {
	org string
	// teamOf returns the owner team of a service ("services/<id>").
	teamOf func(ctx context.Context, service string) (string, error)
}

type accessKey struct{}

func accessFrom(ctx context.Context) *access {
	a, _ := ctx.Value(accessKey{}).(*access)
	return a
}

// isAdmin reports whether p may do everything.
func isAdmin(p *Principal) bool {
	return p.Workload == nil && slices.Contains(p.Permissions, PermServicesWrite)
}

// inTeam reports whether p is in the GitHub team with the slug team of org.
// Without an org, a group of any organization with that slug counts.
func inTeam(p *Principal, org, team string) bool {
	if team == "" || p.Workload != nil {
		return false
	}
	return slices.ContainsFunc(p.Groups, func(g string) bool {
		o, slug, ok := strings.Cut(g, ":")
		if !ok {
			return org == "" && g == team
		}
		return slug == team && (org == "" || strings.EqualFold(o, org))
	})
}

// teamsOf returns the slugs of org's teams p is in, sorted.
func teamsOf(p *Principal, org string) []string {
	var out []string
	for _, g := range p.Groups {
		if o, slug, ok := strings.Cut(g, ":"); ok && slug != "" && (org == "" || strings.EqualFold(o, org)) {
			out = append(out, slug)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// allowed reports whether p may do action on something owned by team.
func allowed(p *Principal, org, action, team string) bool {
	if isAdmin(p) {
		return true
	}
	need, ok := actionRoles[action]
	if !ok || need == roleAdmin {
		return false
	}
	if need == roleReader {
		return slices.Contains(p.Permissions, PermServicesRead)
	}
	return inTeam(p, org, team)
}

// authorizeTeam checks that the caller may do action on something owned by
// team (a GitHub team slug).
func authorizeTeam(ctx context.Context, action, team string) error {
	p, ok := PrincipalFrom(ctx)
	if !ok {
		return connect.NewError(connect.CodeUnauthenticated, errors.New("not authenticated"))
	}
	org := ""
	if a := accessFrom(ctx); a != nil {
		org = a.org
	}
	if allowed(p, org, action, team) {
		return nil
	}
	return denied(p, org, action, team, "")
}

// authorize checks that the caller may do action on service
// ("services/<id>"). An admin passes without the service being looked up,
// so the handler reports a missing service as it always has.
func authorize(ctx context.Context, action, service string) error {
	p, ok := PrincipalFrom(ctx)
	if !ok {
		return connect.NewError(connect.CodeUnauthenticated, errors.New("not authenticated"))
	}
	if isAdmin(p) {
		return nil
	}
	a := accessFrom(ctx)
	if a == nil || a.teamOf == nil || actionRoles[action] == roleAdmin || service == "" {
		org := ""
		if a != nil {
			org = a.org
		}
		return denied(p, org, action, "", service)
	}
	team, err := a.teamOf(ctx, service)
	if err != nil {
		return err
	}
	if allowed(p, a.org, action, team) {
		return nil
	}
	return denied(p, a.org, action, team, service)
}

func denied(p *Principal, org, action, team, service string) error {
	on := ""
	if service != "" {
		on = " on " + service
	}
	if actionRoles[action] == roleTeam && team != "" {
		group := team
		if org != "" {
			group = org + ":" + team
		}
		return connect.NewError(connect.CodePermissionDenied, fmt.Errorf("%s may not %s%s: it needs the team %s or an admin", p.Subject, action, on, group))
	}
	return connect.NewError(connect.CodePermissionDenied, fmt.Errorf("%s may not %s%s: only an admin may", p.Subject, action, on))
}

// roleFor is the caller's strongest role toward team.
func roleFor(p *Principal, org, team string) role {
	switch {
	case isAdmin(p):
		return roleAdmin
	case inTeam(p, org, team):
		return roleTeam
	default:
		return roleReader
	}
}
