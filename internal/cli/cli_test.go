package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/showmino/forge/internal/auth"
	"github.com/showmino/forge/internal/server"
	"github.com/showmino/forge/internal/store"
)

// harness runs the real command tree against an in-process Forge API.
type harness struct {
	t      *testing.T
	api    *httptest.Server
	dir    string
	env    map[string]string
	store  *auth.MemoryStore
	stdin  string
	tty    bool
	issuer string

	authn     server.Authenticator
	templates server.TemplateSource
	repos     server.RepoHost
	ops       *server.Operations
	catalog   *server.Catalog
	envs      *server.Environments
	deploys   *server.Deploys
	audit     *store.Memory
}

const (
	writerToken = "t-writer"
	readerToken = "t-reader"
)

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, dir: t.TempDir(), store: &auth.MemoryStore{}, ops: server.NewOperations(nil), catalog: server.NewCatalog(nil), audit: store.NewMemory()}
	h.startAPI(server.StaticAuthenticator{
		writerToken: {Subject: "ana", Email: "ana@example.com", Groups: []string{"platform"}},
		readerToken: {Subject: "bo", Email: "bo@example.com", Groups: []string{"dev"}},
	})
	h.env = map[string]string{
		"FORGE_CONFIG":  filepath.Join(h.dir, "config.yaml"),
		"FORGE_API_URL": h.api.URL,
		"FORGE_TOKEN":   writerToken,
	}
	return h
}

func (h *harness) startAPI(authn server.Authenticator) {
	h.authn = authn
	if h.api != nil {
		h.api.Close()
	}
	handler, err := server.New(server.Options{
		Version:       "0.1.0",
		OIDC:          server.OIDC{Issuer: h.issuer, ClientID: "forge-cli"},
		Authenticator: authn,
		Authorizer:    server.Authorizer{WriterGroups: []string{"platform"}},
		Templates:     h.templates,
		Repos:         h.repos,
		Operations:    h.ops,
		Catalog:       h.catalog,
		Environments:  h.envs,
		Deploys:       h.deploys,
		Audit:         h.audit,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	h.api = httptest.NewServer(handler)
	if h.env != nil {
		h.env["FORGE_API_URL"] = h.api.URL
	}
	h.t.Cleanup(h.api.Close)
}

// serveTemplates restarts the in-process API with templates from dir (version "dev").
func (h *harness) serveTemplates(dir string) {
	h.templates = server.DirSource{Dir: dir}
	h.startAPI(h.authn)
}

type result struct {
	out, err string
	code     int
}

func (r result) String() string {
	return fmt.Sprintf("exit %d\nstdout:\n%s\nstderr:\n%s", r.code, r.out, r.err)
}

func (h *harness) run(args ...string) result {
	h.t.Helper()
	var out, errOut bytes.Buffer
	d := Deps{
		IO:      IOStreams{In: strings.NewReader(h.stdin), Out: &out, ErrOut: &errOut, InTTY: h.tty, OutTTY: h.tty},
		Env:     func(k string) (string, bool) { v, ok := h.env[k]; return v, ok },
		Store:   h.store,
		Getwd:   func() (string, error) { return h.dir, nil },
		Now:     time.Now,
		Version: "0.1.0",
	}
	code := Run(context.Background(), d, args)
	return result{out.String(), errOut.String(), code}
}

func (h *harness) ok(args ...string) result {
	h.t.Helper()
	r := h.run(args...)
	if r.code != 0 {
		h.t.Fatalf("forge %s: %v", strings.Join(args, " "), r)
	}
	return r
}

func (h *harness) fails(code int, args ...string) result {
	h.t.Helper()
	r := h.run(args...)
	if r.code != code {
		h.t.Fatalf("forge %s: want exit %d, got %v", strings.Join(args, " "), code, r)
	}
	return r
}

func TestServiceLifecycle(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	h.ok("service", "register", "payments", "--team", "billing", "--owner", "ana@example.com", "--lifecycle", "production",
		"--repo", "https://github.com/acme/payments", "--label", "tier=1")
	h.ok("service", "register", "ledger", "--team", "billing", "--depends-on", "payments")
	h.fails(6, "service", "register", "payments", "--team", "billing") // already exists

	r := h.ok("service", "list")
	want := "NAME       TEAM      LIFECYCLE    OWNERS\nledger     billing   -            -\npayments   billing   production   ana@example.com\n"
	if r.out != want {
		t.Errorf("list table:\n%q\nwant\n%q", r.out, want)
	}

	r = h.ok("service", "list", "-o", "name")
	if r.out != "services/ledger\nservices/payments\n" {
		t.Errorf("list name: %q", r.out)
	}

	r = h.ok("service", "get", "payments", "-o", "json")
	var svc map[string]any
	if err := json.Unmarshal([]byte(r.out), &svc); err != nil {
		t.Fatalf("get json: %v\n%s", err, r.out)
	}
	if svc["name"] != "services/payments" || svc["lifecycle"] != "LIFECYCLE_PRODUCTION" || svc["repoUrl"] != "https://github.com/acme/payments" {
		t.Errorf("get json: %v", svc)
	}

	h.ok("service", "update", "payments", "--lifecycle", "deprecated", "--add-owner", "li@example.com")
	r = h.ok("service", "owners", "payments", "-o", "name")
	if r.out != "ana@example.com\nli@example.com\n" {
		t.Errorf("owners: %q", r.out)
	}
	r = h.ok("service", "update", "payments", "--remove-owner", "ana@example.com", "-o", "yaml")
	if !strings.Contains(r.out, "lifecycle: LIFECYCLE_DEPRECATED") || strings.Contains(r.out, "ana@") {
		t.Errorf("update yaml:\n%s", r.out)
	}
	h.fails(2, "service", "update", "payments") // nothing to update

	r = h.ok("service", "deps", "ledger", "-o", "name")
	if r.out != "services/payments\n" {
		t.Errorf("deps: %q", r.out)
	}

	// Deleting a dependency fails; non-interactive deletes need --yes.
	h.fails(6, "service", "delete", "payments", "--yes")
	r = h.fails(2, "service", "delete", "ledger")
	if !strings.Contains(r.err, "--yes") {
		t.Errorf("missing --yes hint: %v", r)
	}
	h.ok("service", "delete", "ledger", "--dry-run")
	h.ok("service", "get", "ledger") // dry run kept it
	h.ok("service", "delete", "ledger", "--yes")
	h.fails(3, "service", "get", "ledger")
}

func TestDryRunRegisterChangesNothing(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	r := h.ok("service", "register", "payments", "--team", "billing", "--dry-run")
	if !strings.Contains(r.out, "dry run") {
		t.Errorf("out: %q", r.out)
	}
	h.fails(3, "service", "get", "payments")
}

func TestInteractiveDeleteConfirmation(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.ok("service", "register", "payments", "--team", "billing")
	h.tty = true

	h.stdin = "nope\n"
	h.fails(2, "service", "delete", "payments")
	h.ok("service", "get", "payments")

	h.stdin = "payments\n"
	h.ok("service", "delete", "payments")
	h.fails(3, "service", "get", "payments")
}

func TestAuthErrors(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	delete(h.env, "FORGE_TOKEN")
	r := h.fails(4, "service", "list")
	if !strings.Contains(r.err, "forge auth login") {
		t.Errorf("missing login hint: %v", r)
	}

	h.env["FORGE_TOKEN"] = "bogus"
	h.fails(4, "service", "list")

	h.env["FORGE_TOKEN"] = readerToken
	h.ok("service", "list")
	h.fails(5, "service", "register", "x", "--team", "t")

	r = h.ok("auth", "status", "-o", "json")
	var st authStatus
	if err := json.Unmarshal([]byte(r.out), &st); err != nil {
		t.Fatal(err)
	}
	if st.Email != "bo@example.com" || len(st.Permissions) != 1 || st.Credentials != "FORGE_TOKEN" {
		t.Errorf("status: %+v", st)
	}
}

func TestUsageErrors(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	r := h.fails(2, "servce")
	if !strings.Contains(r.err, `did you mean "service"`) {
		t.Errorf("no suggestion: %v", r)
	}
	h.fails(2, "service", "list", "--bogus")
	h.fails(2, "service", "list", "-o", "xml")
	h.fails(2, "service", "get", "Not_Valid")
	h.fails(2, "service", "get") // no name and no forge.yaml
	h.fails(2, "service", "register", "x")
	h.fails(2, "service", "register", "x", "--team", "t", "--lifecycle", "alive")
	h.fails(2, "context", "use")

	delete(h.env, "FORGE_API_URL")
	r = h.fails(2, "service", "list")
	if !strings.Contains(r.err, "forge context set") {
		t.Errorf("missing context hint: %v", r)
	}
}

func TestContexts(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	delete(h.env, "FORGE_API_URL")

	h.fails(3, "context", "current")
	h.ok("context", "set", "local", "--api", h.api.URL)
	h.ok("context", "set", "prod", "--api", "https://forge.example.com", "--namespace", "billing")
	h.fails(2, "context", "set", "bad", "--api", "forge.example.com")
	h.fails(3, "context", "use", "nope")

	r := h.ok("context", "list")
	if !strings.Contains(r.out, "*         local") {
		t.Errorf("first context should be current:\n%s", r.out)
	}
	h.ok("service", "list") // reaches the API through the context

	r = h.ok("context", "list", "-o", "json")
	for _, key := range []string{`"currentContext": "local"`, `"name": "prod"`, `"api": "https://forge.example.com"`, `"namespace": "billing"`} {
		if !strings.Contains(r.out, key) {
			t.Errorf("context list -o json lacks %s:\n%s", key, r.out)
		}
	}

	h.ok("context", "use", "prod")
	r = h.ok("context", "current", "-o", "name")
	if r.out != "prod\n" {
		t.Errorf("current: %q", r.out)
	}
	r = h.ok("context", "current", "-c", "local", "-o", "json")
	if !strings.Contains(r.out, h.api.URL) {
		t.Errorf("--context override: %s", r.out)
	}

	data, err := os.ReadFile(h.env["FORGE_CONFIG"])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "token") {
		t.Errorf("config must never hold credentials:\n%s", data)
	}
}

func TestInitAndManifestDefaults(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fails(2, "init") // --team required without a terminal
	h.ok("init", "--name", "payments", "--team", "billing", "--owner", "ana@example.com")
	h.fails(6, "init", "--team", "billing")

	h.ok("service", "register") // name and fields from forge.yaml
	r := h.ok("service", "owners", "-o", "name")
	if r.out != "ana@example.com\n" {
		t.Errorf("owners from manifest: %q", r.out)
	}
	h.fails(2, "service", "delete") // delete never defaults from the manifest

	// A broken forge.yaml is reported, not mistaken for a missing one.
	bad := "apiVersion: forge.dev/v1\nservice:\n  name: payments\n  tem: billing\n"
	if err := os.WriteFile(filepath.Join(h.dir, "forge.yaml"), []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	r = h.fails(2, "service", "get")
	if !strings.Contains(r.err, "forge.yaml") || !strings.Contains(r.err, "tem") {
		t.Errorf("invalid manifest error should name the file and the field:\n%s", r.err)
	}
	r = h.fails(1, "doctor", "-o", "json")
	if !strings.Contains(r.out, `"name": "config",
      "status": "ok"`) || !strings.Contains(r.out, `"name": "manifest",
      "status": "fail"`) {
		t.Errorf("doctor should report config ok and the manifest as failed:\n%s", r.out)
	}
}

func TestVersionAndDoctor(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	r := h.ok("version", "--client", "-o", "json")
	if !strings.Contains(r.out, `"version": "0.1.0"`) || strings.Contains(r.out, "server") {
		t.Errorf("version --client: %s", r.out)
	}
	r = h.ok("version", "-o", "json")
	if !strings.Contains(r.out, `"server"`) {
		t.Errorf("version: %s", r.out)
	}
	r = h.ok("doctor")
	for _, want := range []string{"api ", "logged in as ana@example.com"} {
		if !strings.Contains(r.out, want) {
			t.Errorf("doctor missing %q:\n%s", want, r.out)
		}
	}
	h.env["FORGE_TOKEN"] = "bogus"
	r = h.fails(1, "doctor")
	if !strings.Contains(r.out, "fail") {
		t.Errorf("doctor should report auth failure:\n%s", r.out)
	}
}

func TestSkewWarning(t *testing.T) {
	t.Parallel()
	cases := []struct {
		cli, server, min string
		warn             bool
	}{
		{"1.4.0", "1.4.2", "1.0.0", false},
		{"1.3.0", "1.4.0", "1.0.0", false},
		{"1.2.0", "1.4.0", "1.0.0", true},
		{"0.9.0", "1.0.0", "1.0.0", true},
		{"dev", "1.4.0", "1.0.0", false},
	}
	for _, c := range cases {
		if got := skewWarning(c.cli, c.server, c.min) != ""; got != c.warn {
			t.Errorf("skewWarning(%s, %s, %s) warn=%v", c.cli, c.server, c.min, got)
		}
	}
}

// TestLoginDeviceFlow runs 'forge auth login' against a fake identity
// provider, then uses the stored credentials.
func TestLoginDeviceFlow(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	idToken := fakeIDToken("ana", time.Now().Add(time.Hour))

	polls := 0
	idp := httptest.NewServer(nil)
	idp.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			fmt.Fprintf(w, `{"issuer":%q,"authorization_endpoint":"%[1]s/auth","token_endpoint":"%[1]s/token","device_authorization_endpoint":"%[1]s/device/code"}`, idp.URL)
		case "/device/code":
			fmt.Fprintf(w, `{"device_code":"dc","user_code":"WXYZ-1234","verification_uri":"%s/device","expires_in":60,"interval":1}`, idp.URL)
		case "/token":
			if polls++; polls == 1 {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, `{"error":"authorization_pending"}`)
				return
			}
			fmt.Fprintf(w, `{"access_token":"at","token_type":"bearer","expires_in":3600,"refresh_token":"rt","id_token":%q}`, idToken)
		default:
			http.NotFound(w, r)
		}
	})
	t.Cleanup(idp.Close)

	h.issuer = idp.URL
	h.startAPI(server.StaticAuthenticator{idToken: {Subject: "ana", Email: "ana@example.com", Groups: []string{"platform"}}})
	delete(h.env, "FORGE_TOKEN")

	r := h.ok("auth", "login")
	if !strings.Contains(r.err, "WXYZ-1234") || !strings.Contains(r.err, "Logged in") {
		t.Errorf("login output: %v", r)
	}
	creds, err := h.store.Get(h.api.URL)
	if err != nil || creds.IDToken != idToken || creds.RefreshToken != "rt" {
		t.Fatalf("stored creds: %v %+v", err, creds)
	}
	h.ok("service", "register", "payments", "--team", "billing")
	r = h.ok("auth", "token")
	if strings.TrimSpace(r.out) != idToken {
		t.Errorf("token: %q", r.out)
	}
	h.ok("auth", "logout")
	h.fails(4, "service", "list")
}

func fakeIDToken(sub string, exp time.Time) string {
	payload, _ := json.Marshal(map[string]any{"sub": sub, "email": sub + "@example.com", "exp": exp.Unix(), "aud": "forge-cli"})
	return "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func TestSlug(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{"My_Service": "my-service", "payments": "payments", "1st-svc": "st-svc", "--x--": "x"} {
		if got := slug(in); got != want {
			t.Errorf("slug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestListFilters(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.ok("service", "register", "a", "--team", "billing")
	h.ok("service", "register", "b", "--team", "search")
	h.ok("service", "register", "c", "--team", "billing")

	if r := h.ok("service", "list", "--team", "billing", "-o", "name"); r.out != "services/a\nservices/c\n" {
		t.Errorf("--team: %q", r.out)
	}
	if r := h.ok("service", "list", "--limit", "2", "-o", "name"); r.out != "services/a\nservices/b\n" {
		t.Errorf("--limit: %q", r.out)
	}
	if r := h.ok("service", "list", "--team", "nobody"); r.out != "" || !strings.Contains(r.err, "No services") {
		t.Errorf("empty: %v", r)
	}
	if r := h.ok("service", "list", "--team", "nobody", "-o", "json"); strings.ReplaceAll(r.out, " ", "") != "{\n\"services\":[]\n}\n" {
		t.Errorf("empty json should be the empty response: %q", r.out)
	}
}
