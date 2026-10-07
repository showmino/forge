package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func fakeJWT(sub string, exp time.Time) string {
	payload, _ := json.Marshal(map[string]any{"sub": sub, "email": sub + "@example.com", "exp": exp.Unix()})
	return "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

// fakeIdP implements discovery, device authorization and the token endpoint.
type fakeIdP struct {
	srv      *httptest.Server
	polls    atomic.Int32
	refreshs atomic.Int32
	deny     bool
}

func newFakeIdP(t *testing.T) *fakeIdP {
	f := &fakeIdP{}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":                        f.srv.URL,
			"authorization_endpoint":        f.srv.URL + "/auth",
			"token_endpoint":                f.srv.URL + "/token",
			"device_authorization_endpoint": f.srv.URL + "/device/code",
		})
	})
	mux.HandleFunc("/device/code", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("client_id") != "forge-cli" {
			http.Error(w, `{"error":"invalid_client"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"device_code": "dc-1", "user_code": "ABCD-EFGH",
			"verification_uri": f.srv.URL + "/device", "verification_uri_complete": f.srv.URL + "/device?user_code=ABCD-EFGH",
			"expires_in": 60, "interval": 1,
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		switch r.Form.Get("grant_type") {
		case "urn:ietf:params:oauth:grant-type:device_code":
			if f.polls.Add(1) == 1 {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, `{"error":"authorization_pending"}`)
				return
			}
			if f.deny {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, `{"error":"access_denied"}`)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "at-1", "token_type": "bearer", "expires_in": 3600,
				"refresh_token": "rt-1", "id_token": fakeJWT("ana", time.Now().Add(time.Hour)),
			})
		case "refresh_token":
			if r.Form.Get("refresh_token") != "rt-1" {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, `{"error":"invalid_grant"}`)
				return
			}
			f.refreshs.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "at-2", "token_type": "bearer", "expires_in": 3600,
				"id_token": fakeJWT("ana", time.Now().Add(2*time.Hour)),
			})
		default:
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":"unsupported_grant_type"}`)
		}
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func TestDeviceLogin(t *testing.T) {
	t.Parallel()
	idp := newFakeIdP(t)
	var shown DeviceCode
	creds, err := DeviceLogin(context.Background(), Config{Issuer: idp.srv.URL, ClientID: "forge-cli", Scopes: []string{"openid"}}, func(d DeviceCode) { shown = d })
	if err != nil {
		t.Fatal(err)
	}
	if shown.UserCode != "ABCD-EFGH" || shown.VerificationURIComplete == "" {
		t.Errorf("shown = %+v", shown)
	}
	if creds.Subject != "ana" || creds.Email != "ana@example.com" || creds.RefreshToken != "rt-1" || !creds.Valid(time.Now(), time.Minute) {
		t.Errorf("creds = %+v", creds)
	}
	if idp.polls.Load() != 2 {
		t.Errorf("polls = %d, want 2 (one pending, one success)", idp.polls.Load())
	}
}

func TestDeviceLoginDenied(t *testing.T) {
	t.Parallel()
	idp := newFakeIdP(t)
	idp.deny = true
	_, err := DeviceLogin(context.Background(), Config{Issuer: idp.srv.URL, ClientID: "forge-cli"}, func(DeviceCode) {})
	if err == nil || err.Error() != "device login: login was denied" {
		t.Fatalf("err = %v", err)
	}
}

func TestDiscoverIssuerMismatch(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"issuer":"https://evil.example.com","device_authorization_endpoint":"x"}`)
	}))
	defer srv.Close()
	if _, err := Discover(context.Background(), nil, srv.URL); err == nil {
		t.Fatal("expected issuer mismatch error")
	}
}

func TestTokenSourceRefreshesAndSaves(t *testing.T) {
	t.Parallel()
	idp := newFakeIdP(t)
	store := &MemoryStore{}
	_ = store.Set("api", &Credentials{
		Issuer: idp.srv.URL, ClientID: "forge-cli", IDToken: fakeJWT("ana", time.Now().Add(-time.Minute)),
		RefreshToken: "rt-1", Expiry: time.Now().Add(-time.Minute),
	})
	ts := TokenSource(context.Background(), store, "api", nil, nil)
	tok, err := ts.Token()
	if err != nil {
		t.Fatal(err)
	}
	if idp.refreshs.Load() != 1 {
		t.Fatalf("refreshes = %d", idp.refreshs.Load())
	}
	saved, _ := store.Get("api")
	if saved.IDToken != tok.AccessToken || saved.RefreshToken != "rt-1" || !saved.Valid(time.Now(), time.Hour) {
		t.Errorf("saved = %+v", saved)
	}
	// A second call uses the cached, valid token.
	if _, err := ts.Token(); err != nil || idp.refreshs.Load() != 1 {
		t.Errorf("unexpected refresh: %v %d", err, idp.refreshs.Load())
	}
}

func TestTokenSourceNotLoggedIn(t *testing.T) {
	t.Parallel()
	_, err := TokenSource(context.Background(), &MemoryStore{}, "api", nil, nil).Token()
	if !errors.Is(err, ErrLoginRequired) {
		t.Fatalf("err = %v", err)
	}
	store := &MemoryStore{}
	_ = store.Set("api", &Credentials{IDToken: "x", Expiry: time.Now().Add(-time.Hour)})
	_, err = TokenSource(context.Background(), store, "api", nil, nil).Token()
	if !errors.Is(err, ErrLoginRequired) {
		t.Fatalf("expired without refresh token: err = %v", err)
	}
}

func TestFileStore(t *testing.T) {
	t.Parallel()
	fs := &FileStore{Path: filepath.Join(t.TempDir(), "creds.json")}
	if _, err := fs.Get("a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty get: %v", err)
	}
	if err := fs.Set("a", &Credentials{IDToken: "t1"}); err != nil {
		t.Fatal(err)
	}
	if err := fs.Set("b", &Credentials{IDToken: "t2"}); err != nil {
		t.Fatal(err)
	}
	if c, err := fs.Get("a"); err != nil || c.IDToken != "t1" {
		t.Fatalf("get a: %v %+v", err, c)
	}
	if err := fs.Delete("a"); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.Get("a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
	if c, _ := fs.Get("b"); c.IDToken != "t2" {
		t.Fatal("b lost")
	}
}
