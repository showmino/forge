package server

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	jose "github.com/go-jose/go-jose/v4"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
)

// actionsIssuer plays token.actions.githubusercontent.com: a JWKS endpoint at
// /.well-known/jwks and the key that signs test tokens.
type actionsIssuer struct {
	srv *httptest.Server
	key *rsa.PrivateKey
}

func newActionsIssuer(t *testing.T) *actionsIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	i := &actionsIssuer{key: key}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/jwks", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "gh1", Algorithm: "RS256", Use: "sig"}}})
	})
	i.srv = httptest.NewServer(mux)
	t.Cleanup(i.srv.Close)
	return i
}

// claims are a push to main of showmino/payments at envSHA, valid for five
// minutes from now, changed by over; a nil value in over deletes the claim.
func (i *actionsIssuer) claims(now time.Time, over map[string]any) map[string]any {
	c := map[string]any{
		"iss": i.srv.URL, "aud": "forge", "iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix(),
		"sub":              "repo:showmino/payments:ref:refs/heads/main",
		"repository":       "showmino/payments",
		"repository_owner": "showmino",
		"ref":              "refs/heads/main",
		"sha":              envSHA,
	}
	for k, v := range over {
		if v == nil {
			delete(c, k)
		} else {
			c[k] = v
		}
	}
	return c
}

// sign signs claims with key (nil: the issuer's own key).
func (i *actionsIssuer) sign(t *testing.T, key *rsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	if key == nil {
		key = i.key
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithHeader("kid", "gh1"))
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	obj, err := signer.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	s, err := obj.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// ciCatalog registers services (id → repo_url).
func ciCatalog(t *testing.T, services map[string]string) *Catalog {
	t.Helper()
	c := NewCatalog(nil)
	for id, repo := range services {
		if _, err := c.RegisterService(writerCtx(), connect.NewRequest(&forgev1.RegisterServiceRequest{
			ServiceId: id, Service: &forgev1.Service{OwnerTeam: "billing", RepoUrl: repo},
		})); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

// ciClock is a settable clock.
type ciClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *ciClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *ciClock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// fakeBranches answers DefaultBranch from branch ("owner/repo" → name, default
// "main") or with err, counting calls.
type fakeBranches struct {
	mu     sync.Mutex
	branch map[string]string
	err    error
	calls  int
}

func (f *fakeBranches) DefaultBranch(_ context.Context, owner, repo string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return "", f.err
	}
	if b, ok := f.branch[owner+"/"+repo]; ok {
		return b, nil
	}
	return "main", nil
}

func (f *fakeBranches) set(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *fakeBranches) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func newCIAuth(t *testing.T, iss *actionsIssuer, cat *Catalog, br *fakeBranches, clk *ciClock) *GitHubOIDCAuthenticator {
	t.Helper()
	return NewGitHubOIDCAuthenticator(t.Context(), GitHubOIDCOptions{
		Issuer: iss.srv.URL, Org: "showmino", Catalog: cat, Branches: br, Now: clk.Now,
	})
}

func TestGitHubOIDCClaims(t *testing.T) {
	t.Parallel()
	iss := newActionsIssuer(t)
	cat := ciCatalog(t, map[string]string{
		"payments": "https://github.com/showmino/payments",
		"trunked":  "https://github.com/showmino/trunked",
		"twin-a":   "https://github.com/showmino/twin",
		"twin-b":   "https://github.com/showmino/twin",
	})
	br := &fakeBranches{branch: map[string]string{"showmino/trunked": "trunk"}}
	clk := &ciClock{t: time.Now()}
	a := newCIAuth(t, iss, cat, br, clk)
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}

	// code is the result's class: CodeUnknown means a plain error, which the
	// interceptor turns into Unauthenticated; 0 means success.
	for _, tc := range []struct {
		name string
		over map[string]any
		key  *rsa.PrivateKey
		code connect.Code
		want string // in the error message, or the Principal's service
	}{
		{name: "push to main", want: "payments"},
		{name: "case-insensitive repo and owner", over: map[string]any{"repository": "Showmino/Payments", "repository_owner": "ShowMino"}, want: "payments"},
		{name: "other default branch", over: map[string]any{"repository": "showmino/trunked", "ref": "refs/heads/trunk"}, want: "trunked"},
		{name: "wrong audience", over: map[string]any{"aud": "sts.amazonaws.com"}, code: connect.CodeUnknown},
		{name: "wrong issuer", over: map[string]any{"iss": "https://evil.example"}, code: connect.CodeUnknown},
		{name: "expired", over: map[string]any{"exp": clk.Now().Add(-time.Minute).Unix()}, code: connect.CodeUnknown},
		{name: "other key", key: otherKey, code: connect.CodeUnknown},
		{name: "other owner", over: map[string]any{"repository_owner": "evil", "repository": "evil/payments"}, code: connect.CodePermissionDenied, want: "repository_owner"},
		{name: "repository in another org", over: map[string]any{"repository": "evil/payments"}, code: connect.CodePermissionDenied, want: "repository"},
		{name: "unknown repository", over: map[string]any{"repository": "showmino/nope"}, code: connect.CodePermissionDenied, want: "not a Forge service"},
		{name: "repo of two services", over: map[string]any{"repository": "showmino/twin"}, code: connect.CodePermissionDenied, want: "twin-a, twin-b"},
		{name: "feature branch", over: map[string]any{"ref": "refs/heads/feature"}, code: connect.CodePermissionDenied, want: "ref"},
		{name: "tag", over: map[string]any{"ref": "refs/tags/v1.0.0"}, code: connect.CodePermissionDenied, want: "ref"},
		{name: "pull request", over: map[string]any{"ref": "refs/pull/7/merge"}, code: connect.CodePermissionDenied, want: "ref"},
		{name: "no ref", over: map[string]any{"ref": nil}, code: connect.CodePermissionDenied, want: "ref"},
		{name: "no sha", over: map[string]any{"sha": nil}, code: connect.CodePermissionDenied, want: "sha"},
		{name: "sha not a commit", over: map[string]any{"sha": "main"}, code: connect.CodePermissionDenied, want: "sha"},
		{name: "no repository", over: map[string]any{"repository": nil}, code: connect.CodePermissionDenied, want: "repository"},
		{name: "no owner", over: map[string]any{"repository_owner": nil}, code: connect.CodePermissionDenied, want: "repository_owner"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			token := iss.sign(t, tc.key, iss.claims(clk.Now(), tc.over))
			p, err := a.Authenticate(context.Background(), token)
			if tc.code == 0 {
				if err != nil {
					t.Fatal(err)
				}
				if p.Workload == nil || p.Workload.Service != tc.want || p.Workload.SHA != envSHA {
					t.Fatalf("workload: %+v", p.Workload)
				}
				if !strings.HasPrefix(p.Subject, "github:") || len(p.Permissions) != 1 || p.Permissions[0] != PermDeploy(tc.want, "staging") {
					t.Fatalf("principal: %+v", p)
				}
				return
			}
			if err == nil {
				t.Fatalf("want an error, got %+v", p)
			}
			if got := connect.CodeOf(err); got != tc.code {
				t.Fatalf("code %v, want %v: %v", got, tc.code, err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name %q", err, tc.want)
			}
			if strings.Contains(err.Error(), token) {
				t.Error("the error contains the token")
			}
		})
	}
}

func TestGitHubOIDCSubject(t *testing.T) {
	t.Parallel()
	iss := newActionsIssuer(t)
	clk := &ciClock{t: time.Now()}
	a := newCIAuth(t, iss, ciCatalog(t, map[string]string{"payments": "https://github.com/showmino/payments"}), &fakeBranches{}, clk)
	p, err := a.Authenticate(context.Background(), iss.sign(t, nil, iss.claims(clk.Now(), nil)))
	if err != nil {
		t.Fatal(err)
	}
	want := Workload{Repository: "showmino/payments", Service: "payments", SHA: envSHA}
	if p.Subject != "github:showmino/payments" || p.Email != "" || len(p.Groups) != 0 || *p.Workload != want {
		t.Errorf("principal: %+v %+v", p, p.Workload)
	}
}

func TestGitHubOIDCBranchLookup(t *testing.T) {
	t.Parallel()
	iss := newActionsIssuer(t)
	br := &fakeBranches{}
	clk := &ciClock{t: time.Now()}
	a := newCIAuth(t, iss, ciCatalog(t, map[string]string{"payments": "https://github.com/showmino/payments"}), br, clk)
	auth := func() error {
		_, err := a.Authenticate(context.Background(), iss.sign(t, nil, iss.claims(clk.Now(), nil)))
		return err
	}

	br.set(errors.New("connection refused"))
	if err := auth(); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("GitHub down: %v (want Unavailable)", err)
	}
	br.set(nil)
	if err := auth(); err != nil {
		t.Fatalf("GitHub back, failure not cached: %v", err)
	}
	calls := br.count()
	if err := auth(); err != nil || br.count() != calls {
		t.Fatalf("second token within 10 minutes: %v, %d lookups (want %d)", err, br.count(), calls)
	}
	clk.Add(11 * time.Minute)
	if err := auth(); err != nil || br.count() != calls+1 {
		t.Fatalf("after 11 minutes: %v, %d lookups (want %d)", err, br.count(), calls+1)
	}
}

func TestGitHubOIDCDefaults(t *testing.T) {
	t.Parallel()
	a := NewGitHubOIDCAuthenticator(t.Context(), GitHubOIDCOptions{Org: "showmino", Catalog: NewCatalog(nil), Branches: &fakeBranches{}})
	got := map[string]any{"issuer": a.o.Issuer, "jwks": a.o.JWKSURL, "audience": a.o.Audience, "ttl": a.o.BranchTTL}
	want := map[string]any{
		"issuer": GitHubIssuer, "jwks": GitHubIssuer + "/.well-known/jwks", "audience": "forge", "ttl": 10 * time.Minute,
	}
	if !maps.Equal(got, want) {
		t.Errorf("defaults %v, want %v", got, want)
	}
}
