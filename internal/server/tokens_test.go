package server

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/gen/forge/v1/forgev1connect"
	"github.com/showmino/forge/internal/store"
)

func (f *sessionFixture) token(caller, name string, days int32, scopes ...string) (*forgev1.CreateTokenResponse, error) {
	resp, err := f.auth.CreateToken(context.Background(), as(caller, &forgev1.CreateTokenRequest{DisplayName: name, Scopes: scopes, ExpireDays: days}))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func (f *sessionFixture) register(token, id string) error {
	c := forgev1connect.NewServiceCatalogServiceClient(http.DefaultClient, f.url)
	_, err := c.RegisterService(context.Background(), as(token, &forgev1.RegisterServiceRequest{ServiceId: id, Service: &forgev1.Service{OwnerTeam: "platform"}}))
	return err
}

// A token acts as its person, within its scopes, until it is revoked or
// ends; only a login makes or revokes one.
func TestPersonalTokens(t *testing.T) {
	t.Parallel()
	f := newSessionFixture(t)
	login, _ := f.login("cli-login", "laptop")

	read, err := f.token(login, "ci-read", 30)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(read.GetSecret(), "fgp_") || read.GetToken().GetExpireTime() == nil || strings.Join(read.GetToken().GetScopes(), ",") != "read" {
		t.Fatalf("made %v", read)
	}
	// It is ana, with her role, but may only read.
	st, err := f.status(read.GetSecret())
	if err != nil || st.GetEmail() != "ana@example.com" || !canWrite(st) {
		t.Fatalf("status: %v %v", st, err)
	}
	if err := f.register(read.GetSecret(), "one"); connect.CodeOf(err) != connect.CodePermissionDenied || !strings.Contains(err.Error(), "ci-read") {
		t.Errorf("a read token writes: %v", err)
	}
	settings, err := f.token(login, "admin-script", 0, "settings")
	if err != nil || settings.GetToken().GetExpireTime() != nil {
		t.Fatalf("a token that never expires: %v %v", settings, err)
	}
	if err := f.register(settings.GetSecret(), "two"); err != nil {
		t.Errorf("a settings token: %v", err)
	}
	// The audit log names the token.
	evs, _ := f.store.ListAuditEvents(context.Background(), store.AuditQuery{Action: "service.register", Limit: 10})
	if len(evs) == 0 || evs[0].GetDetails()["token"] != "admin-script" {
		t.Errorf("audit: %v", evs)
	}

	// A token neither makes a token or a session, nor revokes one.
	for _, tok := range []string{read.GetSecret(), settings.GetSecret()} {
		if _, err := f.token(tok, "more", 1, "settings"); connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Errorf("a token makes a token: %v", err)
		}
		if _, err := f.auth.CreateSession(context.Background(), as(tok, &forgev1.CreateSessionRequest{Label: "x"})); connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Errorf("a token makes a session: %v", err)
		}
		if _, err := f.auth.DeleteToken(context.Background(), as(tok, &forgev1.DeleteTokenRequest{Name: read.GetToken().GetName()})); connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Errorf("a token revokes a token: %v", err)
		}
	}
	// Nor does a CI job.
	if _, err := f.token("ci", "x", 1); err == nil {
		t.Error("a CI job made a token")
	}

	// Refused: a name taken, a scope or an expiry Forge does not know.
	if _, err := f.token(login, "ci-read", 30); connect.CodeOf(err) != connect.CodeAlreadyExists {
		t.Errorf("same name: %v", err)
	}
	if _, err := f.token(login, "x", 30, "everything"); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("bad scope: %v", err)
	}
	if _, err := f.token(login, "x", 400); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("400 days: %v", err)
	}
	if _, err := f.token(login, "Bad Name", 1); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("bad name: %v", err)
	}

	// Listed apart from the sessions; another person sees none of them.
	list, err := f.auth.ListTokens(context.Background(), as(login, &forgev1.ListTokensRequest{}))
	if err != nil || len(list.Msg.GetTokens()) != 2 {
		t.Fatalf("tokens: %v %v", list, err)
	}
	ses, _ := f.auth.ListSessions(context.Background(), as(login, &forgev1.ListSessionsRequest{}))
	for _, s := range ses.Msg.GetSessions() {
		if strings.Contains(s.GetName(), "tok_") {
			t.Errorf("a token among the sessions: %v", s)
		}
	}
	bo, _ := f.login("bo-login", "laptop")
	if _, err := f.auth.DeleteToken(context.Background(), as(bo, &forgev1.DeleteTokenRequest{Name: read.GetToken().GetName()})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("bo revokes ana's token: %v", err)
	}
	// A session's name cannot end a token.
	if _, err := f.auth.DeleteSession(context.Background(), as(login, &forgev1.DeleteSessionRequest{Name: "sessions/" + strings.TrimPrefix(read.GetToken().GetName(), "tokens/")})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("a token as a session: %v", err)
	}

	// Revoked: it stops at once.
	if _, err := f.auth.DeleteToken(context.Background(), as(login, &forgev1.DeleteTokenRequest{Name: read.GetToken().GetName()})); err != nil {
		t.Fatal(err)
	}
	if _, err := f.status(read.GetSecret()); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("a revoked token: %v", err)
	}
}

// A deploy token deploys, nothing more; an expired token ends; a person who
// leaves the organization takes their tokens with them.
func TestPersonalTokenScopesAndEnd(t *testing.T) {
	t.Parallel()
	f := newSessionFixture(t)
	login, _ := f.login("cli-login", "laptop")
	deploy, err := f.token(login, "ci-deploys", 7, "deploy")
	if err != nil || strings.Join(deploy.GetToken().GetScopes(), ",") != "read,deploy" {
		t.Fatalf("%v %v", deploy, err)
	}
	if !tokenMayCall(deploy.GetToken().GetScopes(), forgev1connect.DeployServicePromoteDeployProcedure) ||
		tokenMayCall(deploy.GetToken().GetScopes(), forgev1connect.ServiceCatalogServiceDeleteServiceProcedure) ||
		tokenMayCall([]string{ScopeRead, ScopeDeploy, ScopeSettings}, forgev1connect.AuthServiceCreateTokenProcedure) ||
		tokenMayCall([]string{ScopeRead, ScopeDeploy, ScopeSettings}, "/forge.v1.NoSuchService/Do") {
		t.Error("the scopes reach too far or not far enough")
	}
	if err := f.register(deploy.GetSecret(), "x"); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("a deploy token registers: %v", err)
	}
	keep, _ := f.token(login, "keep", 0, "read")

	f.advance(7*24*time.Hour + time.Minute)
	if _, err := f.status(deploy.GetSecret()); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("an expired token: %v", err)
	}
	if _, err := f.status(keep.GetSecret()); err != nil {
		t.Errorf("a token that never expires: %v", err)
	}
	// Ana leaves the organization: her token ends at the next check.
	f.members.set(func(m *fakeMembers) { m.org["ana"] = false })
	f.advance(61 * time.Minute)
	if _, err := f.status(keep.GetSecret()); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("after leaving: %v", err)
	}
}

// A session's token does not pass as a personal token, nor the other way.
func TestTokenKindsDoNotMix(t *testing.T) {
	t.Parallel()
	f := newSessionFixture(t)
	login, _ := f.login("cli-login", "laptop")
	tok, err := f.token(login, "ci", 1)
	if err != nil {
		t.Fatal(err)
	}
	swapped := "fgs_" + strings.TrimPrefix(tok.GetSecret(), "fgp_")
	if _, err := f.status(swapped); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("a personal token with a session's prefix: %v", err)
	}
	if _, err := f.status("fgp_" + strings.TrimPrefix(login, "fgs_")); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("a session with a token's prefix: %v", err)
	}
}
