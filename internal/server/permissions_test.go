package server

import (
	"context"
	"net/http/httptest"
	"slices"
	"testing"

	"connectrpc.com/connect"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/gen/forge/v1/forgev1connect"
)

func TestTestPermissions(t *testing.T) {
	t.Parallel()
	h, err := New(Options{
		Authenticator: StaticAuthenticator{
			"admin":  {Subject: "ana", Groups: []string{"showmino:platform-engineering"}},
			"member": {Subject: "cy", Groups: []string{"showmino:billing", "showmino:test-team"}},
			"reader": {Subject: "bo"},
		},
		Authorizer: Authorizer{AdminGroups: []string{"showmino:platform-engineering"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	ctx := context.Background()
	cat := forgev1connect.NewServiceCatalogServiceClient(srv.Client(), srv.URL)
	if _, err := cat.RegisterService(ctx, as("admin", &forgev1.RegisterServiceRequest{ServiceId: "pay", Service: &forgev1.Service{OwnerTeam: "billing"}})); err != nil {
		t.Fatal(err)
	}
	auth := forgev1connect.NewAuthServiceClient(srv.Client(), srv.URL)
	test := func(token string, req *forgev1.TestPermissionsRequest) (*forgev1.TestPermissionsResponse, error) {
		resp, err := auth.TestPermissions(ctx, as(token, req))
		if err != nil {
			return nil, err
		}
		return resp.Msg, nil
	}

	asked := []string{ActDeployPromote, ActServiceDelete, ActResourceApplyProduction, ActServiceUpdate}
	for _, tc := range []struct {
		token, role string
		want        []string
	}{
		{"admin", "admin", asked},
		{"member", "team", []string{ActDeployPromote, ActServiceUpdate}},
		{"reader", "reader", nil},
	} {
		got, err := test(tc.token, &forgev1.TestPermissionsRequest{Service: "services/pay", Actions: asked})
		if err != nil {
			t.Fatalf("%s: %v", tc.token, err)
		}
		if !slices.Equal(got.GetAllowed(), tc.want) || got.GetRole() != tc.role {
			t.Errorf("%s: allowed %v role %q, want %v %q", tc.token, got.GetAllowed(), got.GetRole(), tc.want, tc.role)
		}
	}

	// No actions asks for every action about the service.
	got, err := test("member", &forgev1.TestPermissionsRequest{Service: "services/pay"})
	if err != nil || !slices.Contains(got.GetAllowed(), ActDeployRollback) || slices.Contains(got.GetAllowed(), ActServiceDelete) {
		t.Errorf("every action: %v %v", got, err)
	}
	// Without a service: create for an own team, not for any team.
	got, err = test("member", &forgev1.TestPermissionsRequest{Actions: []string{ActServiceCreate, ActServiceCreateAnyTeam, ActServiceRegister}, OwnerTeam: "test-team"})
	if err != nil || !slices.Equal(got.GetAllowed(), []string{ActServiceCreate}) {
		t.Errorf("no service: %v %v", got, err)
	}
	got, err = test("member", &forgev1.TestPermissionsRequest{Actions: []string{ActServiceCreate}, OwnerTeam: "frontend"})
	if err != nil || len(got.GetAllowed()) != 0 {
		t.Errorf("create for another team: %v %v", got, err)
	}
	if _, err := test("member", &forgev1.TestPermissionsRequest{Service: "services/pay", Actions: []string{"deploy.everything"}}); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("unknown action: %v", err)
	}
	if _, err := test("member", &forgev1.TestPermissionsRequest{Service: "services/nope"}); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("missing service: %v", err)
	}

	st, err := auth.GetStatus(ctx, as("member", &forgev1.GetStatusRequest{}))
	if err != nil || st.Msg.GetAdmin() || !slices.Equal(st.Msg.GetTeams(), []string{"billing", "test-team"}) {
		t.Errorf("status: %v %v", st, err)
	}
	if st, err := auth.GetStatus(ctx, as("admin", &forgev1.GetStatusRequest{})); err != nil || !st.Msg.GetAdmin() {
		t.Errorf("admin status: %v %v", st, err)
	}
}
