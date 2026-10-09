package cli

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/showmino/forge/internal/server"
)

// actionsJWT is what the fake Actions endpoint hands out; the API's static
// authenticator maps it to a CI workload of showmino/payments.
func actionsJWT() string {
	b, _ := json.Marshal(map[string]any{"exp": time.Now().Add(5 * time.Minute).Unix()})
	return "eyJhbGciOiJSUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(b) + ".c2ln"
}

// inActions serves the Actions token endpoint and sets the runner's variables
// (and drops FORGE_TOKEN), returning the request counter.
func (h *harness) inActions(token string, status int) *atomic.Int32 {
	h.t.Helper()
	return h.inActionsBody(status, fmt.Sprintf(`{"value":%q}`, token))
}

// inActionsBody is inActions answering a 200 with body.
func (h *harness) inActionsBody(status int, body string) *atomic.Int32 {
	h.t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer req-token" || r.URL.Query().Get("audience") != "forge" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if status != http.StatusOK {
			http.Error(w, "no", status)
			return
		}
		fmt.Fprint(w, body)
	}))
	h.t.Cleanup(srv.Close)
	delete(h.env, "FORGE_TOKEN")
	h.env["GITHUB_ACTIONS"] = "true"
	h.env["ACTIONS_ID_TOKEN_REQUEST_URL"] = srv.URL + "/token?api-version=2.0"
	h.env["ACTIONS_ID_TOKEN_REQUEST_TOKEN"] = "req-token"
	h.env["GITHUB_SHA"] = cliSHA
	return &calls
}

func TestDeployFromActions(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	jwt := actionsJWT()
	h.startAPI(server.StaticAuthenticator{
		writerToken: {Subject: "ana", Email: "ana@example.com", Groups: []string{"platform"}},
		jwt: {
			Subject: "github:showmino/payments", Permissions: []string{server.PermDeploy("payments", "staging")},
			Workload: &server.Workload{Repository: "showmino/payments", Service: "payments", SHA: cliSHA},
		},
	})
	h.serveDeploys(map[string]string{})
	h.ok("service", "register", "payments", "--team", "billing", "--repo", "https://github.com/showmino/payments")

	// A human token in CI deploys GITHUB_SHA when --ref is not given.
	h.env["GITHUB_SHA"] = cliSHA
	if r := h.ok("deploy", "start", "payments", "--env", "staging", "--dry-run"); !strings.Contains(r.out, "("+cliSHA+" at 1a2b3c4)") {
		t.Errorf("dry run with GITHUB_SHA: %q", r.out)
	}

	calls := h.inActions(jwt, http.StatusOK)
	r := h.ok("deploy", "start", "payments", "--env", "staging")
	if !strings.Contains(r.out, "health          healthy") {
		t.Errorf("printed deployment:\n%s", r.out)
	}
	if calls.Load() != 1 {
		t.Errorf("%d token requests for one deploy, want 1 (the token is reused)", calls.Load())
	}
	if strings.Contains(r.out+r.err, jwt) {
		t.Error("the token was printed")
	}
	h.fails(5, "deploy", "start", "payments", "--env", "production")
	h.fails(5, "deploy", "status", "payments")
	h.fails(5, "service", "list")
}

func TestActionsTokenHints(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	// The endpoint refuses: exit 4, the hint says re-run, no token echoed.
	h.inActions("unused", http.StatusForbidden)
	r := h.fails(4, "deploy", "status", "payments", "--env", "staging")
	if !strings.Contains(r.err, "refused") || strings.Contains(r.err, "id-token") || strings.Contains(r.err, "req-token") {
		t.Errorf("refused token request:\n%s", r.err)
	}

	// GitHub fails: exit 1, retry.
	h.inActions("unused", http.StatusInternalServerError)
	r = h.fails(1, "deploy", "status", "payments", "--env", "staging")
	if !strings.Contains(r.err, "retry the job") || strings.Contains(r.err, "req-token") {
		t.Errorf("GitHub 500:\n%s", r.err)
	}

	// A response that isn't a token: exit 1 too.
	h.inActionsBody(http.StatusOK, "not json")
	r = h.fails(1, "deploy", "status", "payments", "--env", "staging")
	if !strings.Contains(r.err, "retry the job") {
		t.Errorf("invalid JSON:\n%s", r.err)
	}
	h.inActionsBody(http.StatusOK, `{"value":""}`)
	h.fails(1, "deploy", "status", "payments", "--env", "staging")

	// No id-token permission: the runner sets no Actions variables.
	delete(h.env, "ACTIONS_ID_TOKEN_REQUEST_URL")
	delete(h.env, "ACTIONS_ID_TOKEN_REQUEST_TOKEN")
	r = h.fails(4, "deploy", "status", "payments", "--env", "staging")
	if !strings.Contains(r.err, "id-token: write") {
		t.Errorf("no Actions token in GitHub Actions:\n%s", r.err)
	}

	// Outside GitHub Actions the hint stays "forge auth login".
	delete(h.env, "GITHUB_ACTIONS")
	r = h.fails(4, "deploy", "status", "payments", "--env", "staging")
	if !strings.Contains(r.err, "forge auth login") || strings.Contains(r.err, "id-token") {
		t.Errorf("outside Actions:\n%s", r.err)
	}
}
