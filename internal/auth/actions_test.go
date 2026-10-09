package auth

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// actionsFakeJWT is a JWT-shaped string whose payload says exp.
func actionsFakeJWT(exp time.Time) string {
	b, _ := json.Marshal(map[string]any{"exp": exp.Unix()})
	return "eyJhbGciOiJSUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(b) + ".c2ln"
}

// actionsEndpoint serves tokens from next and counts requests; it insists on
// the request token and the forge audience, and keeps the URL's own query.
func actionsEndpoint(t *testing.T, next func() string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer req-token" {
			http.Error(w, "bad request token", http.StatusForbidden)
			return
		}
		if r.URL.Query().Get("audience") != "forge" || r.URL.Query().Get("api-version") != "2.0" {
			http.Error(w, "bad query "+r.URL.RawQuery, http.StatusBadRequest)
			return
		}
		fmt.Fprintf(w, `{"count":1,"value":%q}`, next())
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func TestActionsTokenReused(t *testing.T) {
	t.Parallel()
	jwt := actionsFakeJWT(time.Now().Add(5 * time.Minute))
	srv, calls := actionsEndpoint(t, func() string { return jwt })
	ts := ActionsToken(srv.Client(), srv.URL+"/token?api-version=2.0", "req-token", ActionsAudience)
	for range 3 {
		tok, err := ts.Token()
		if err != nil {
			t.Fatal(err)
		}
		if tok.AccessToken != jwt {
			t.Fatalf("token %q", tok.AccessToken)
		}
	}
	if calls.Load() != 1 {
		t.Errorf("%d requests for a token valid five more minutes, want 1", calls.Load())
	}
}

func TestActionsTokenRefetchesNearExpiry(t *testing.T) {
	t.Parallel()
	srv, calls := actionsEndpoint(t, func() string { return actionsFakeJWT(time.Now().Add(30 * time.Second)) })
	ts := ActionsToken(srv.Client(), srv.URL+"/token?api-version=2.0", "req-token", ActionsAudience)
	for range 2 {
		if _, err := ts.Token(); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 2 {
		t.Errorf("%d requests for tokens within a minute of expiry, want 2", calls.Load())
	}
}

func TestActionsTokenUnparseableExpiry(t *testing.T) {
	t.Parallel()
	srv, calls := actionsEndpoint(t, func() string { return "not-a-jwt" })
	ts := ActionsToken(srv.Client(), srv.URL+"/token?api-version=2.0", "req-token", ActionsAudience)
	for range 2 {
		if _, err := ts.Token(); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 2 {
		t.Errorf("a token without a readable exp must not be cached: %d requests", calls.Load())
	}
}

func TestActionsTokenErrors(t *testing.T) {
	t.Parallel()
	srv, _ := actionsEndpoint(t, func() string { return "" })
	// Wrong request token: 403, and the error must not echo either token.
	_, err := ActionsToken(srv.Client(), srv.URL+"/token?api-version=2.0", "wrong", ActionsAudience).Token()
	if err == nil || !strings.Contains(err.Error(), "403") || strings.Contains(err.Error(), "wrong") {
		t.Errorf("403: %v", err)
	}
	if !errors.Is(err, ErrActionsTokenRefused) {
		t.Errorf("403 should be ErrActionsTokenRefused: %v", err)
	}
	// A server error is not a refusal.
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "boom", http.StatusInternalServerError) }))
	t.Cleanup(down.Close)
	_, err = ActionsToken(down.Client(), down.URL+"/token", "req-token", ActionsAudience).Token()
	if err == nil || errors.Is(err, ErrActionsTokenRefused) || !strings.Contains(err.Error(), "500") {
		t.Errorf("500: %v", err)
	}
	// An empty value.
	_, err = ActionsToken(srv.Client(), srv.URL+"/token?api-version=2.0", "req-token", ActionsAudience).Token()
	if err == nil || !strings.Contains(err.Error(), "no token") {
		t.Errorf("empty value: %v", err)
	}
	// A request URL that isn't one.
	if _, err := ActionsToken(srv.Client(), "://bad", "req-token", ActionsAudience).Token(); err == nil {
		t.Error("bad URL: want an error")
	}
}
