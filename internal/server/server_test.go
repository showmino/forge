package server

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/go-jose/go-jose/v4"
	"golang.org/x/oauth2"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/pkg/client"
)

func writerCtx() context.Context {
	return context.WithValue(context.Background(), principalKey{}, &Principal{Subject: "ana", Permissions: []string{PermServicesRead, PermServicesWrite}})
}

func register(t *testing.T, c *Catalog, id string, deps ...string) *forgev1.Service {
	t.Helper()
	resp, err := c.RegisterService(writerCtx(), connect.NewRequest(&forgev1.RegisterServiceRequest{
		ServiceId: id, Service: &forgev1.Service{OwnerTeam: "billing", Dependencies: deps},
	}))
	if err != nil {
		t.Fatalf("register %s: %v", id, err)
	}
	return resp.Msg.GetService()
}

func TestListPagination(t *testing.T) {
	t.Parallel()
	c := NewCatalog(nil)
	for _, id := range []string{"e", "a", "d", "b", "c"} {
		register(t, c, id)
	}
	var got []string
	token := ""
	pages := 0
	for {
		resp, err := c.ListServices(writerCtx(), connect.NewRequest(&forgev1.ListServicesRequest{PageSize: 2, PageToken: token}))
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, s := range resp.Msg.GetServices() {
			got = append(got, s.GetName())
		}
		if token = resp.Msg.GetNextPageToken(); token == "" {
			break
		}
	}
	if strings.Join(got, ",") != "services/a,services/b,services/c,services/d,services/e" || pages != 3 {
		t.Errorf("got %v in %d pages", got, pages)
	}
	if _, err := c.ListServices(writerCtx(), connect.NewRequest(&forgev1.ListServicesRequest{PageToken: "%%%"})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("bad token: %v", err)
	}
}

func TestRegisterIdempotency(t *testing.T) {
	t.Parallel()
	c := NewCatalog(nil)
	req := &forgev1.RegisterServiceRequest{ServiceId: "payments", Service: &forgev1.Service{OwnerTeam: "billing"}, RequestId: "k1"}
	a, err := c.RegisterService(writerCtx(), connect.NewRequest(req))
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.RegisterService(writerCtx(), connect.NewRequest(req))
	if err != nil {
		t.Fatalf("retry with the same request_id must succeed: %v", err)
	}
	if a.Msg.GetService().GetEtag() != b.Msg.GetService().GetEtag() {
		t.Error("retry returned a different result")
	}
	req.RequestId = "k2"
	if _, err := c.RegisterService(writerCtx(), connect.NewRequest(req)); connect.CodeOf(err) != connect.CodeAlreadyExists {
		t.Errorf("new key: %v", err)
	}
}

func TestUpdateEtagAndMask(t *testing.T) {
	t.Parallel()
	c := NewCatalog(nil)
	s := register(t, c, "payments")
	upd := func(etag string, paths ...string) error {
		_, err := c.UpdateService(writerCtx(), connect.NewRequest(&forgev1.UpdateServiceRequest{
			Service:    &forgev1.Service{Name: s.GetName(), Lifecycle: forgev1.Lifecycle_LIFECYCLE_PRODUCTION, Etag: etag},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: paths},
		}))
		return err
	}
	if err := upd("", "lifecycle"); err != nil {
		t.Fatal(err)
	}
	if err := upd(s.GetEtag(), "lifecycle"); connect.CodeOf(err) != connect.CodeAborted {
		t.Errorf("stale etag: %v", err)
	}
	if err := upd("", "name"); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("bad mask: %v", err)
	}
	if err := upd("", "owner_team"); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("clearing required field: %v", err)
	}
}

func TestDependenciesMustExist(t *testing.T) {
	t.Parallel()
	c := NewCatalog(nil)
	reg := func(id string, validateOnly bool, deps ...string) error {
		_, err := c.RegisterService(writerCtx(), connect.NewRequest(&forgev1.RegisterServiceRequest{
			ServiceId: id, Service: &forgev1.Service{OwnerTeam: "billing", Dependencies: deps}, ValidateOnly: validateOnly,
		}))
		return err
	}
	if err := reg("ledger", false, "services/missing"); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("register with unknown dependency: %v", err)
	}
	if err := reg("ledger", true, "services/missing"); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("validate-only register with unknown dependency: %v", err)
	}
	register(t, c, "payments")
	if err := reg("ledger", false, "services/payments"); err != nil {
		t.Fatalf("register with existing dependency: %v", err)
	}

	upd := func(deps ...string) error {
		_, err := c.UpdateService(writerCtx(), connect.NewRequest(&forgev1.UpdateServiceRequest{
			Service:    &forgev1.Service{Name: "services/ledger", Dependencies: deps},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"dependencies"}},
		}))
		return err
	}
	if err := upd("services/missing"); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("update to unknown dependency: %v", err)
	}
	if err := upd("services/ledger"); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("update to depend on itself: %v", err)
	}
	if err := upd("services/payments"); err != nil {
		t.Errorf("update to existing dependency: %v", err)
	}
}

func TestPermissions(t *testing.T) {
	t.Parallel()
	c := NewCatalog(nil)
	reader := context.WithValue(context.Background(), principalKey{}, &Principal{Subject: "bo", Permissions: []string{PermServicesRead}})
	_, err := c.RegisterService(reader, connect.NewRequest(&forgev1.RegisterServiceRequest{ServiceId: "x", Service: &forgev1.Service{OwnerTeam: "t"}}))
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("reader register: %v", err)
	}
	if _, err := c.ListServices(context.Background(), connect.NewRequest(&forgev1.ListServicesRequest{})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("anonymous list: %v", err)
	}
	a := Authorizer{WriterGroups: []string{"platform"}}
	p := &Principal{Groups: []string{"platform"}}
	a.Grant(p)
	if len(p.Permissions) != 2 {
		t.Errorf("writer group: %v", p.Permissions)
	}
}

// oidcProvider is a minimal issuer that signs real RS256 ID tokens.
type oidcProvider struct {
	srv *httptest.Server
	key *rsa.PrivateKey
}

func newOIDCProvider(t *testing.T) *oidcProvider {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := &oidcProvider{key: key}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"issuer":%q,"jwks_uri":"%[1]s/keys","authorization_endpoint":"%[1]s/auth","token_endpoint":"%[1]s/token","id_token_signing_alg_values_supported":["RS256"]}`, p.srv.URL)
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	})
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	return p
}

func (p *oidcProvider) token(t *testing.T, aud string, exp time.Time) string {
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: p.key}, (&jose.SignerOptions{}).WithHeader("kid", "k1"))
	if err != nil {
		t.Fatal(err)
	}
	claims, _ := json.Marshal(map[string]any{
		"iss": p.srv.URL, "sub": "ana", "aud": aud, "exp": exp.Unix(), "iat": time.Now().Unix(),
		"email": "ana@example.com", "groups": []string{"platform"},
	})
	obj, err := signer.Sign(claims)
	if err != nil {
		t.Fatal(err)
	}
	s, err := obj.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestOIDCAuthenticator(t *testing.T) {
	t.Parallel()
	p := newOIDCProvider(t)
	a, err := NewOIDCAuthenticator(context.Background(), p.srv.URL, "forge-cli")
	if err != nil {
		t.Fatal(err)
	}
	pr, err := a.Authenticate(context.Background(), p.token(t, "forge-cli", time.Now().Add(time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	if pr.Subject != "ana" || pr.Email != "ana@example.com" || len(pr.Groups) != 1 {
		t.Errorf("principal: %+v", pr)
	}
	for name, tok := range map[string]string{
		"wrong audience": p.token(t, "someone-else", time.Now().Add(time.Hour)),
		"expired":        p.token(t, "forge-cli", time.Now().Add(-time.Hour)),
		"garbage":        "not.a.jwt",
	} {
		if _, err := a.Authenticate(context.Background(), tok); err == nil {
			t.Errorf("%s: token accepted", name)
		}
	}
}

// TestProtocols checks that one handler serves Connect, gRPC and JSON,
// including gRPC over cleartext HTTP/2.
func TestProtocols(t *testing.T) {
	t.Parallel()
	h, err := New(Options{Version: "0.1.0", Authenticator: StaticAuthenticator{"tok": {Subject: "ana"}}})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := HTTPServer("", h)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	base := "http://" + ln.Addr().String()

	h2c := &http.Client{Transport: &http.Transport{Protocols: func() *http.Protocols {
		p := new(http.Protocols)
		p.SetUnencryptedHTTP2(true)
		return p
	}()}}
	for name, o := range map[string]client.Options{
		"connect": {},
		"grpc":    {GRPC: true, HTTPClient: h2c},
	} {
		o.TokenSource = oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "tok"})
		o.ClientVersion = "0.1.0"
		c, err := client.New(base, o)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := c.Auth.GetStatus(context.Background(), connect.NewRequest(&forgev1.GetStatusRequest{}))
		if err != nil || resp.Msg.GetSubject() != "ana" {
			t.Errorf("%s: %v %v", name, resp, err)
		}
	}

	// Plain JSON over HTTP/1.1, as curl or a browser would send it.
	req, _ := http.NewRequest(http.MethodPost, base+"/forge.v1.AuthService/GetServerInfo", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var body map[string]any
	_ = json.NewDecoder(res.Body).Decode(&body)
	if res.StatusCode != 200 || body["serverVersion"] != "0.1.0" || res.Header.Get(client.HeaderServerVersion) != "0.1.0" {
		t.Errorf("json: %d %v %v", res.StatusCode, body, res.Header)
	}
}
