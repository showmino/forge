package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/run"
)

// runnerServer serves the fixture's runner API over HTTP.
func runnerServer(t *testing.T, f *resourceFixture) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(RunnerHandler(f.res, nil))
	t.Cleanup(srv.Close)
	return srv
}

// The whole plan and apply cycle, with the fake runner talking to forge-api
// over HTTP as the real one does.
func TestRunnerOverHTTP(t *testing.T) {
	t.Parallel()
	f := newResourceFixture(t, nil)
	f.runner.API = &run.Client{BaseURL: runnerServer(t, f).URL}

	p, op := f.plan(t, "writer", "staging", database("5Gi", 0), false)
	if op.GetError() != nil || p.GetState() != forgev1.ResourcePlan_STATE_PLANNED || len(p.GetChanges()) != 1 {
		t.Fatalf("plan over HTTP: %v %v", p, op.GetError())
	}
	if steps := eventSteps(op); !strings.Contains(steps, "init") || !strings.Contains(steps, "policy") {
		t.Errorf("events did not arrive over HTTP: %s", steps)
	}
	if op := f.apply(t, p.GetName()); op.GetError() != nil {
		t.Fatalf("apply over HTTP: %v", op.GetError())
	}
	if got := f.runner.Databases(); !slices.Equal(got, []string{"payments-staging/main 5Gi x1"}) {
		t.Errorf("databases: %v", got)
	}
	res := f.getResource(t, "resources/payments/staging/main")
	if res.GetState() != forgev1.Resource_STATE_READY || res.GetSecret() != "main-app" {
		t.Errorf("resource: %v", res)
	}
	if st, _ := f.store.GetResourceState(context.Background(), "payments/staging"); st.Version != 1 || st.LockID != "" {
		t.Errorf("state after an apply over HTTP: %+v", st)
	}
	// A failed run reports its error through Complete too.
	f.runner.FailPlan = "tofu: boom"
	if p, op := f.plan(t, "writer", "production", database("5Gi", 2), false); op.GetError().GetCode() != "run_failed" || p.GetMessage() != "tofu: boom" {
		t.Errorf("a failed plan over HTTP: %v %v", p, op.GetError())
	}
}

// startedToken starts a hanging plan and returns its run token.
func startedToken(t *testing.T, f *resourceFixture) string {
	t.Helper()
	f.runner.Hang = true // the test plays the runner itself
	f.startPlan(t, "writer", "services/payments", "staging", database("5Gi", 0), false)
	for end := time.Now().Add(5 * time.Second); len(f.runner.Started()) == 0; time.Sleep(time.Millisecond) {
		if time.Now().After(end) {
			t.Fatal("the run did not start")
		}
	}
	return f.runner.Started()[0].Token
}

// The state endpoints, driven the way OpenTofu's "http" backend drives them.
func TestRunnerStateProtocol(t *testing.T) {
	t.Parallel()
	f := newResourceFixture(t, nil)
	srv := runnerServer(t, f)
	token := startedToken(t, f)

	// do sends one request as OpenTofu does: basic auth, the token as password.
	do := func(method, query, password, body string) (int, string) {
		t.Helper()
		req, err := http.NewRequest(method, srv.URL+run.StatePath+query, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if password != "" {
			req.SetBasicAuth(run.StateUser, password)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		data, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(data)
	}
	lockA := `{"ID":"lock-a","Operation":"OperationTypePlan","Who":"runner@pod"}`

	if code, body := do("GET", "", token, ""); code != http.StatusNotFound || body != "" {
		t.Errorf("no state yet: %d %q, want 404 and no body", code, body)
	}
	if code, _ := do("LOCK", "", token, lockA); code != http.StatusOK {
		t.Errorf("lock: %d", code)
	}
	// Another id is told who holds the lock, as OpenTofu expects.
	if code, body := do("LOCK", "", token, `{"ID":"lock-b"}`); code != http.StatusLocked || body != lockA {
		t.Errorf("second lock: %d %q, want 423 and the holder's info", code, body)
	}
	// A plan run may not write the state.
	if code, body := do("POST", "?ID=lock-a", token, `{"version":4,"serial":1}`); code != http.StatusConflict || !strings.Contains(body, "a plan run may not write the state") {
		t.Errorf("a plan run writing state: %d %q", code, body)
	}
	if code, _ := do("UNLOCK", "", token, `{"ID":"lock-b"}`); code != http.StatusLocked {
		t.Errorf("unlock by another id: %d, want 423", code)
	}
	if code, _ := do("UNLOCK", "", token, lockA); code != http.StatusOK {
		t.Errorf("unlock: %d", code)
	}
	for name, tc := range map[string]struct {
		method, password, body string
		want                   int
	}{
		"a wrong token":           {"GET", "nope", "", http.StatusUnauthorized},
		"no token":                {"GET", "", "", http.StatusUnauthorized},
		"a lock with no id":       {"LOCK", token, `{}`, http.StatusBadRequest},
		"a lock that is no JSON":  {"LOCK", token, `lock`, http.StatusBadRequest},
		"an unlock with no body":  {"UNLOCK", token, ``, http.StatusBadRequest},
		"a delete":                {"DELETE", token, "", http.StatusMethodNotAllowed},
		"a state that is too big": {"POST", token, strings.Repeat("x", maxRunnerState+1), http.StatusBadRequest},
	} {
		code, body := do(tc.method, "", tc.password, tc.body)
		if code != tc.want {
			t.Errorf("%s: %d %q, want %d", name, code, body, tc.want)
		}
		if strings.Contains(body, token) {
			t.Errorf("%s: the answer holds the token", name)
		}
	}
	var e struct {
		Error string `json:"error"`
	}
	if _, body := do("GET", "", "nope", ""); json.Unmarshal([]byte(body), &e) != nil || e.Error != "the run token is not valid" {
		t.Errorf("error body: %q", body)
	}
}

func TestRunnerHTTPCalls(t *testing.T) {
	t.Parallel()
	f := newResourceFixture(t, nil)
	srv := runnerServer(t, f)
	token := startedToken(t, f)
	c := &run.Client{BaseURL: srv.URL + "/"} // a trailing slash is fine
	ctx := context.Background()

	work, err := c.Claim(ctx, token)
	if err != nil || work.Mode != run.ModePlan || work.Variables.Namespace != "payments-staging" || len(work.Variables.Resources) != 1 {
		t.Fatalf("claim: %+v %v", work, err)
	}
	if err := c.Event(ctx, token, "plan", "planning"); err != nil {
		t.Errorf("event: %v", err)
	}
	if _, err := c.Claim(ctx, "nope"); !run.IsStatus(err, http.StatusUnauthorized) || !strings.Contains(err.Error(), "the run token is not valid") {
		t.Errorf("a wrong token: %v", err)
	}
	if c.StateURL() != srv.URL+run.StatePath {
		t.Errorf("state URL %q", c.StateURL())
	}
	// A body that is not the expected JSON, and one that is too large.
	for name, body := range map[string]string{"not JSON": "{", "too large": `{"step":"` + strings.Repeat("x", maxRunnerSmall) + `"}`} {
		req, _ := http.NewRequest("POST", srv.URL+run.EventPath, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", name, resp.StatusCode)
		}
	}
	// The token is checked before a body is read: a caller without a good
	// one is refused whatever it sends, and never gets a word on its body.
	huge := strings.Repeat("x", maxRunnerSmall+1)
	for _, call := range []struct{ method, path string }{
		{"POST", run.EventPath}, {"POST", run.CompletePath}, {"POST", run.StatePath}, {"LOCK", run.StatePath}, {"UNLOCK", run.StatePath},
	} {
		for _, auth := range []string{"", "Bearer nope"} {
			req, _ := http.NewRequest(call.method, srv.URL+call.path, strings.NewReader(huge))
			if auth != "" {
				req.Header.Set("Authorization", auth)
			}
			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("%s %s with %q: %d, want 401", call.method, call.path, auth, resp.StatusCode)
			}
		}
	}
	// Only POST on the calls; the health check needs no token.
	for path, want := range map[string]int{run.ClaimPath: http.StatusMethodNotAllowed, "/healthz": http.StatusOK} {
		resp, err := srv.Client().Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("GET %s: %d, want %d", path, resp.StatusCode, want)
		}
	}
	// Completing over HTTP ends the run.
	planJSON := []byte(`{"resource_changes":[]}`)
	if err := c.Complete(ctx, token, run.Result{PlanFile: planJSON, PlanJSON: planJSON}); err != nil {
		t.Fatal(err)
	}
	if err := c.Complete(ctx, token, run.Result{Error: "late"}); !run.IsStatus(err, http.StatusUnauthorized) {
		t.Errorf("a second complete: %v", err)
	}
}
