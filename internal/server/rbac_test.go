package server

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"
)

func rbacCtx(p *Principal, teams map[string]string) context.Context {
	ctx := context.WithValue(context.Background(), principalKey{}, p)
	return context.WithValue(ctx, accessKey{}, &access{org: "showmino", teamOf: func(_ context.Context, svc string) (string, error) {
		t, ok := teams[svc]
		if !ok {
			return "", connect.NewError(connect.CodeNotFound, nil)
		}
		return t, nil
	}})
}

func TestAuthorizeRoles(t *testing.T) {
	t.Parallel()
	teams := map[string]string{"services/pay": "billing", "services/web": "frontend"}
	admin := &Principal{Subject: "ana", Groups: []string{"showmino:platform-engineering"}, Permissions: []string{PermServicesRead, PermServicesWrite}}
	member := &Principal{Subject: "cy", Groups: []string{"showmino:billing", "other:frontend"}, Permissions: []string{PermServicesRead}}
	reader := &Principal{Subject: "bo", Groups: []string{"showmino:dev"}, Permissions: []string{PermServicesRead}}
	ci := &Principal{Subject: "repo:showmino/pay", Workload: &Workload{Repository: "showmino/pay", Service: "pay"}, Permissions: []string{PermServicesWrite}}

	for _, tc := range []struct {
		who     *Principal
		action  string
		service string
		ok      bool
	}{
		{admin, ActServiceDelete, "services/pay", true},
		{admin, ActDeployPromote, "services/missing", true},
		{member, ActDeployStart, "services/pay", true},
		{member, ActDeployPromote, "services/pay", true},
		{member, ActDeployRollback, "services/pay", true},
		{member, ActServiceUpdate, "services/pay", true},
		{member, ActEnvironmentCreate, "services/pay", true},
		{member, ActResourceApply, "services/pay", true},
		{member, ActResourceApplyProduction, "services/pay", false},
		{member, ActServiceDelete, "services/pay", false},
		{member, ActServiceAddress, "services/pay", false},
		{member, ActServiceUpdateTeam, "services/pay", false},
		// frontend is a team of another organization, not of showmino.
		{member, ActDeployStart, "services/web", false},
		{reader, ActDeployStart, "services/pay", false},
		{ci, ActDeployStart, "services/pay", false},
		{member, "no.such.action", "services/pay", false},
	} {
		err := authorize(rbacCtx(tc.who, teams), tc.action, tc.service)
		if (err == nil) != tc.ok {
			t.Errorf("%s %s %s: %v, want ok=%v", tc.who.Subject, tc.action, tc.service, err, tc.ok)
		}
		if err != nil && connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Errorf("%s %s %s: code %v", tc.who.Subject, tc.action, tc.service, connect.CodeOf(err))
		}
	}

	err := authorize(rbacCtx(member, teams), ActDeployStart, "services/web")
	if !strings.Contains(err.Error(), "showmino:frontend") {
		t.Errorf("a refusal names the team that may: %v", err)
	}
	if err := authorize(rbacCtx(member, teams), ActDeployStart, "services/missing"); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("a missing service is not found for a non-admin: %v", err)
	}
}

func TestAuthorizeTeam(t *testing.T) {
	t.Parallel()
	member := &Principal{Subject: "cy", Groups: []string{"showmino:billing"}, Permissions: []string{PermServicesRead}}
	ctx := rbacCtx(member, nil)
	if err := authorizeTeam(ctx, ActServiceCreate, "billing"); err != nil {
		t.Errorf("create for own team: %v", err)
	}
	if err := authorizeTeam(ctx, ActServiceCreate, "frontend"); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("create for another team: %v", err)
	}
	if got := teamsOf(&Principal{Groups: []string{"showmino:b", "showmino:a", "x:c", "plain"}}, "showmino"); strings.Join(got, ",") != "a,b" {
		t.Errorf("teamsOf = %v", got)
	}
}

func TestAuthorizerAdminGroups(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		a      Authorizer
		groups []string
		admin  bool
	}{
		{Authorizer{AdminGroups: []string{"showmino:pe"}}, []string{"showmino:pe"}, true},
		{Authorizer{AdminGroups: []string{"showmino:pe"}}, []string{"showmino:dev"}, false},
		// Without admin groups the writer groups are the admins.
		{Authorizer{WriterGroups: []string{"showmino:pe"}}, []string{"showmino:pe"}, true},
		{Authorizer{WriterGroups: []string{"showmino:pe"}}, []string{"showmino:dev"}, false},
		{Authorizer{AdminGroups: []string{"showmino:pe"}, WriterGroups: []string{"showmino:dev"}}, []string{"showmino:dev"}, false},
		// Neither: every caller is an admin (a development setup).
		{Authorizer{}, nil, true},
	} {
		p := &Principal{Groups: tc.groups}
		tc.a.Grant(p)
		if isAdmin(p) != tc.admin {
			t.Errorf("%+v with %v: admin=%v", tc.a, tc.groups, isAdmin(p))
		}
	}
}
