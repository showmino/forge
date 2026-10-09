# Phase 4 Milestone 3: k3s and the Exit Gate Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A go-grpc service created with `forge create` ships to staging from its own CI on the k3s cluster, with no human acting after `forge create`, and a human promotes it to production.

**Architecture:** forge-api already commits deploys to `showmino/forge-deploys` and verifies GitHub Actions tokens (milestones 1 and 2). This milestone adds the cluster side: an Argo CD AppProject and ApplicationSet that turn `forge-deploys` directories into Applications, Gateway listeners for staging and production, and a Role that lets forge-api read Applications. All of it is rendered by `deploy/k3s/render.sh` when `FORGE_DEPLOYS=1`. Two code additions come out of what the cluster showed:
- forge-api refuses to deploy into a `<service>-<env>` namespace that exists and isn't Forge's. `docs-dev-staging` already exists on the cluster.
- forge-api serves the `forge` CLI binary to CI jobs at `GET /cli/forge-linux-<arch>`. Generated repos have no token for the private forge repo, so `go install` from CI is impossible.

The go-grpc template gains a `deploy` job on the LAN runner set `kube-runners`. Then the rollout and the gate.

**Tech Stack:** Go 1.27.2, POSIX sh (`render.sh`), Argo CD v3.5.3 (ApplicationSet git directory generator, multi-source Applications), Gateway API v1 on Istio, GitHub Actions on ARC (`gha-runner-scale-set`), `gh` and `argocd` CLIs.

**Spec:** `docs/specs/2026-10-09-phase-4-deployments-design.md`, sections 1 ("Argo CD objects"), 5 and 6 ("Exit gate on k3s"), 7 (milestone 3).

## Global Constraints

- Namespaces are `<service>-<env>`, labelled `app.kubernetes.io/managed-by: forge`, `forge.dev/service: <service>`, `forge.dev/deploy-env: <env>`. Environments are `staging` and `production`.
- AppProject `forge-services` has:
  - sourceRepos `https://github.com/showmino/forge.git` and `https://github.com/showmino/forge-deploys.git`;
  - destinations: this cluster, namespaces `*-staging` and `*-production`;
  - `clusterResourceWhitelist: [{group: "", kind: Namespace}]`;
  - namespace resources: only the chart's kinds (Deployment, Service, HTTPRoute, ResourceQuota, LimitRange).
- ApplicationSet `forge-services` has:
  - a git directory generator over `forge-deploys` `services/*/*` with `requeueAfterSeconds: 30`, producing Application `<service>-<env>`;
  - sources: the chart from the forge repo at a pinned revision (path `deploy/argocd/chart`), plus `$values` from `forge-deploys` `services/<service>/<env>/values.yaml`;
  - automated sync with prune and self-heal, `CreateNamespace=true`, and `managedNamespaceMetadata` with the three labels.
- Gateway `forge/forge-previews` gains listeners `staging` (`*.staging.<ip>.nip.io`) and `production` (`*.production.<ip>.nip.io`), each allowing routes only from namespaces labelled `app.kubernetes.io/managed-by: forge`.
- forge-api gets a Role in `argocd`: `applications` get, list and watch. No other new permission.
- `render.sh` with `FORGE_DEPLOYS=1` requires `FORGE_PREVIEW_IP`. Without it, output is byte-for-byte unchanged.
- The go-grpc `deploy` job:
  - runs after `image` on pushes to `main`, on `runs-on: kube-runners`, with `permissions: id-token: write`;
  - gets the API URL from the org variable `FORGE_API_URL`;
  - runs `forge deploy start --env staging`.

  The `test` and `image` jobs stay on `ubuntu-latest`.
- One production cluster: nothing Forge renders may touch a namespace Forge does not own, and no new long-lived secrets.
- `make test lint` before every commit; `sh deploy/k3s/render_test.sh` for render changes (it needs `kubectl`).
- Commit messages end with `Co-Authored-By: <the model that wrote the commit> <noreply@anthropic.com>`.

## Review Focus

1. **A service whose namespace already exists and isn't Forge's.** `docs-dev-staging` exists today. `forge deploy start docs-dev --env staging` must refuse before committing anything (exit 6), not let Argo take over the namespace. Pinned in Task 1.
2. **Deleting a deploy directory must remove its workload.** Without Argo's resources finalizer on the generated Applications, deleting a `forge-deploys` directory deletes the Application but leaves the Deployment running. Pinned in Task 3 (the ApplicationSet golden contains the finalizer) and checked live in the gate's cleanup.
3. **The CLI download serves exactly the two binaries.** Path traversal, other names, other methods and a missing directory all get 404 or 405, never a file outside the directory. Pinned in Task 2.
4. **An unpinned chart.** `render.sh` refuses `FORGE_DEPLOYS=1` without a 40-hex `FORGE_DEPLOY_CHART_REVISION`, so a forge `main` push can never silently change every service's manifests. Pinned in Task 3.
5. **A CI job that can't reach the API, or has no runner.** The `deploy` job fails in a way that names the URL it tried (`curl -f` with the URL echoed) rather than hanging, and needs `vars.FORGE_API_URL` to be set (an empty value fails with a clear message). Pinned in Task 4.

---

### Task 1: Refuse deploys into a namespace Forge doesn't own

**Files:**
- Modify: `internal/server/env_cluster.go` (add `ServiceNamespaceFree`)
- Modify: `internal/server/deploy_work.go` (`StartDeploy`), `internal/server/deploys.go` (`RollbackDeploy`, `PromoteDeploy`)
- Test: `internal/server/deploy_cluster_test.go` (or a new `internal/server/deploy_namespace_test.go`)

**Interfaces:**
- Consumes: `(*Cluster).checkServiceNamespace(ctx, service, env) error`. It returns nil when the namespace has Forge's three labels, `errUnlabelled` (which wraps `errNoEnv`) when it exists without them, `errNoEnv` when it's absent, or a Kubernetes error.
- Produces: `func (c *Cluster) ServiceNamespaceFree(ctx context.Context, service, env string) error`. It returns nil when `<service>-<env>` is absent or Forge's, `FailedPrecondition` naming the namespace when it exists without Forge's labels, and `Unavailable` on other errors.

- [ ] **Step 1: Write the failing test**

Add to `internal/server/deploy_cluster_test.go` (or create `deploy_namespace_test.go` in package `server` with the imports it needs):

```go
func TestDeployRefusesForeignNamespace(t *testing.T) {
	t.Parallel()
	f := newDeployFixture(t, false, nil)
	ctx := context.Background()
	// A namespace someone else made, with the name Forge would use.
	if _, err := f.cluster.Kube.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "payments-staging", Labels: map[string]string{"team": "docs"}},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, validateOnly := range []bool{true, false} {
		_, err := f.client.StartDeploy(ctx, as("writer", &forgev1.StartDeployRequest{Service: "services/payments", Env: "staging", ValidateOnly: validateOnly}))
		if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "payments-staging") {
			t.Errorf("validate_only=%v: %v (want FailedPrecondition naming the namespace)", validateOnly, err)
		}
	}
	if hist, err := f.repo.History(ctx, "services/payments/staging/values.yaml", 1); err != nil || len(hist) != 0 {
		t.Errorf("deploy repo history %v, %v: want nothing committed", hist, err)
	}
	// Only the foreign namespace is refused: production is free.
	if _, err := f.start(t, "production"); err != nil {
		t.Fatalf("production: %v", err)
	}
}

func TestPromoteRefusesForeignNamespace(t *testing.T) {
	t.Parallel()
	f := newDeployFixture(t, false, nil)
	ctx := context.Background()
	resp, err := f.start(t, "staging")
	if err != nil {
		t.Fatal(err)
	}
	waitCIOp(t, f.ops, resp.GetOperation().GetName())
	if _, err := f.cluster.Kube.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "payments-production"},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	_, err = f.client.PromoteDeploy(ctx, as("writer", &forgev1.PromoteDeployRequest{Service: "services/payments"}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "payments-production") {
		t.Errorf("promote into a foreign namespace: %v", err)
	}
}
```

If `envtest.Repo.History` returns an error for a path never written, accept `github.ErrNotFound` there as "nothing committed" too.

- [ ] **Step 2: Run them to see them fail**

Run: `export PATH=$HOME/.local/go/bin:$HOME/go/bin:$PATH && go test ./internal/server -run 'TestDeployRefusesForeignNamespace|TestPromoteRefusesForeignNamespace'`
Expected: FAIL. StartDeploy succeeds (or its op fails later with "exists without Forge's labels"), and promote is accepted.

- [ ] **Step 3: Add `ServiceNamespaceFree`**

In `internal/server/env_cluster.go`, after `checkServiceNamespace`:

```go
// ServiceNamespaceFree reports whether Forge may deploy into <service>-<env>:
// it must not exist yet, or be one Argo CD made for Forge. On the one
// production cluster, a namespace with that name that isn't Forge's is
// someone else's, and Argo CD would otherwise take it over.
func (c *Cluster) ServiceNamespaceFree(ctx context.Context, service, env string) error {
	err := c.checkServiceNamespace(ctx, service, env)
	switch {
	case err == nil, errors.Is(err, errNoEnv) && !errors.Is(err, errUnlabelled):
		return nil
	case errors.Is(err, errUnlabelled):
		return connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("namespace %s-%s exists and is not Forge's (it lacks Forge's labels); pick another service name or remove that namespace", service, env))
	default:
		return connect.NewError(connect.CodeUnavailable, fmt.Errorf("check namespace %s-%s: %w", service, env, err))
	}
}
```

Add the `connect` import if `env_cluster.go` lacks it.

- [ ] **Step 4: Call it before anything is committed**

- In `internal/server/deploy_work.go` `StartDeploy`, right after the F2 check that follows `resolveSource` (the `src.commit != w.SHA` guard) and before `preview := ...`:

  ```go
  	if err := d.o.Cluster.ServiceNamespaceFree(ctx, src.serviceID, m.GetEnv()); err != nil {
  		return nil, err
  	}
  ```

- In `internal/server/deploys.go` `RollbackDeploy`, before `preview := d.deploymentFrom(...)`:

  ```go
  	if err := d.o.Cluster.ServiceNamespaceFree(ctx, svc, env); err != nil {
  		return nil, err
  	}
  ```

- In `PromoteDeploy`, before `preview := d.deploymentFrom(...)`:

  ```go
  	if err := d.o.Cluster.ServiceNamespaceFree(ctx, svc, to); err != nil {
  		return nil, err
  	}
  ```

- [ ] **Step 5: Run the tests**

Run: `go test -race ./internal/server -run 'Foreign|TestStartDeploy|TestCI|TestDeploy|TestPromote|TestRollback' -v 2>&1 | tail -30`
Expected: PASS.

- [ ] **Step 6: `make test lint`, commit**

```bash
git add internal/server/env_cluster.go internal/server/deploy_work.go internal/server/deploys.go internal/server/deploy_cluster_test.go
git commit -m "deploys: refuse a service namespace that exists and is not Forge's"
```

(Add the Co-Authored-By line naming your model.)

---

### Task 2: forge-api serves the CLI to CI jobs

**Files:**
- Modify: `internal/server/server.go` (`Options.CLIDir`, route)
- Create: `internal/server/cli_download.go`
- Test: `internal/server/cli_download_test.go`
- Modify: `cmd/forge-api/main.go` (flag `--cli-dir` / `FORGE_CLI_DIR`)
- Modify: `Dockerfile`, `build/forge-api.Dockerfile`, `.goreleaser.yaml`
- Modify: `README.md`

**Interfaces:**
- Produces:
  - `Options.CLIDir string`: a directory holding `forge-linux-amd64` and/or `forge-linux-arm64`; empty means off.
  - `GET /cli/{name}`: no auth. It serves exactly those two names as `application/octet-stream`; everything else is 404.

- [ ] **Step 1: Write the failing test**

Create `internal/server/cli_download_test.go`:

```go
package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestCLIDownload(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "forge-linux-amd64"), []byte("amd64 build"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "secret.txt"), []byte("not served"), 0o644); err != nil {
		t.Fatal(err)
	}
	h, err := New(Options{Authenticator: StaticAuthenticator{}, CLIDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/cli/forge-linux-amd64")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "amd64 build" || resp.Header.Get("Content-Type") != "application/octet-stream" {
		t.Errorf("amd64: %d %q %q", resp.StatusCode, body, resp.Header.Get("Content-Type"))
	}

	for _, path := range []string{
		"/cli/forge-linux-arm64", // allowed name, not built
		"/cli/secret.txt",
		"/cli/..%2fsecret.txt",
		"/cli/../cli/secret.txt",
		"/cli/forge-darwin-amd64",
		"/cli/",
	} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusOK || string(body) == "not served" {
			t.Errorf("%s: %d %q", path, resp.StatusCode, body)
		}
	}
	resp, err = http.Post(srv.URL+"/cli/forge-linux-amd64", "text/plain", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST: %d", resp.StatusCode)
	}
}

func TestCLIDownloadOff(t *testing.T) {
	t.Parallel()
	h, err := New(Options{Authenticator: StaticAuthenticator{}})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL + "/cli/forge-linux-amd64")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("no CLI dir: %d", resp.StatusCode)
	}
}
```

- [ ] **Step 2: Run them to see them fail**

Run: `go test ./internal/server -run TestCLIDownload`
Expected: FAIL to compile (`unknown field CLIDir`).

- [ ] **Step 3: Implement**

Create `internal/server/cli_download.go`:

```go
package server

import (
	"net/http"
	"path/filepath"
	"regexp"
)

// cliNameRE names the CLI builds forge-api serves, so CI jobs on the LAN can
// fetch a CLI that matches the API without a token for the private forge repo.
var cliNameRE = regexp.MustCompile(`^forge-linux-(amd64|arm64)$`)

// cliDownload serves dir/<name> for the names cliNameRE allows; anything
// else, or no dir, is 404. The binaries are not secret, so there is no auth.
func cliDownload(dir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if dir == "" || !cliNameRE.MatchString(name) {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		http.ServeFile(w, r, filepath.Join(dir, name))
	}
}
```

In `internal/server/server.go`, add to `Options` (after `Deploys`):

```go
	// CLIDir holds forge CLI builds (forge-linux-amd64, forge-linux-arm64)
	// served at GET /cli/<name>; empty serves none.
	CLIDir string
```

and next to the `/healthz` route:

```go
	mux.HandleFunc("GET /cli/{name}", cliDownload(o.CLIDir))
```

In `cmd/forge-api/main.go`, add a flag next to the others:

```go
		cliDir       = flag.String("cli-dir", env("FORGE_CLI_DIR", ""), "directory of forge CLI builds served at /cli/forge-linux-<arch> for CI jobs")
```

and pass `CLIDir: *cliDir` in `server.Options`.

- [ ] **Step 4: Put the CLI in the images**

`Dockerfile` (repo root, used for k3s images): after the forge-api `go build`, in the same build stage:

```dockerfile
RUN for arch in amd64 arm64; do \
      CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath \
        -ldflags "-s -w -X github.com/showmino/forge/internal/version.Version=${VERSION} -X github.com/showmino/forge/internal/version.Commit=${COMMIT}" \
        -o /out/cli/forge-linux-$arch ./cmd/forge; \
    done
```

and in the final stage:

```dockerfile
COPY --from=build /out/cli /cli
ENV FORGE_CLI_DIR=/cli
```

Update the Dockerfile's first comment to: `# Image for forge-api. It also carries the forge CLI for linux, which forge-api serves to CI jobs at /cli/.`

`build/forge-api.Dockerfile` (GoReleaser): GoReleaser puts each platform's binaries under `${TARGETPLATFORM}/`. Add `ARG TARGETARCH` and:

```dockerfile
COPY ${TARGETPLATFORM}/forge /cli/forge-linux-${TARGETARCH}
ENV FORGE_CLI_DIR=/cli
```

In `.goreleaser.yaml`, `dockers_v2[0].ids: [forge-api, forge]` so the `forge` binary is in the image context. A release image then serves its own architecture's CLI, which is what a runner on the same cluster needs. Run `go run github.com/goreleaser/goreleaser/v2@latest check` if the repo's CI uses it (look at `.github/workflows/ci.yml`'s goreleaser-check job for the exact command) and report the result.

- [ ] **Step 5: Run tests; build the image locally**

Run: `go test -race ./internal/server -run TestCLIDownload -v` → PASS.
Run: `docker build -t forge-api:cli-test . && docker run --rm --entrypoint=/forge-api forge-api:cli-test --help 2>&1 | grep -c cli-dir`. It should print 1. distroless has no shell, so check the files with `docker create` plus `docker cp`:
`id=$(docker create forge-api:cli-test) && docker cp $id:/cli /tmp/claude-cli-check && docker rm $id && ls -l /tmp/claude-cli-check && rm -rf /tmp/claude-cli-check`. Both binaries should be listed.

- [ ] **Step 6: README**

In `README.md`'s GitHub Actions paragraph (added in milestone 2), add: "forge-api serves the matching CLI for Linux at `$FORGE_API_URL/cli/forge-linux-amd64` (and `-arm64`), so a job needs no token for the forge repo."

- [ ] **Step 7: `make test lint`, commit**

```bash
git add internal/server/cli_download.go internal/server/cli_download_test.go internal/server/server.go cmd/forge-api/main.go Dockerfile build/forge-api.Dockerfile .goreleaser.yaml README.md
git commit -m "forge-api: serve the forge CLI for linux at /cli/ for CI jobs"
```

---

### Task 3: render.sh deploys the Argo CD objects; production chart golden; docs

**Files:**
- Create: `deploy/k3s/deploys.yaml.tmpl`
- Modify: `deploy/k3s/render.sh`, `deploy/k3s/previews.yaml.tmpl` (Gateway listeners with `#deploys`), `deploy/k3s/forge.yaml.tmpl` (`FORGE_DEPLOYS` env with `#deploys`)
- Modify: `deploy/k3s/render_test.sh`; create `deploy/k3s/testdata/deploys.golden`
- Create: `deploy/argocd/chart/testdata/production.yaml`, `deploy/argocd/chart/testdata/production.golden`; modify `Makefile` (`chart-test`)
- Modify: `README.md` (k3s section), `docs/specs/2026-10-09-phase-4-deployments-design.md` (as-built notes), `.claude/skills/forge/verifying.md` (deploys and teardown)

**Interfaces:**
- Produces:
  - `FORGE_DEPLOYS=1 FORGE_DEPLOY_CHART_REVISION=<40-hex> FORGE_PREVIEW_IP=... render.sh --lb ...` adds:
    - AppProject `forge-services` and ApplicationSet `forge-services` in `argocd`;
    - Role/RoleBinding `forge-api-applications` in `argocd`;
    - the Gateway listeners `staging` and `production`;
    - env `FORGE_DEPLOYS=1` on forge-api.

- [ ] **Step 1: Write the failing render tests**

Append to `deploy/k3s/render_test.sh`, before `exit $fail`:

```sh
rev=0123456789abcdef0123456789abcdef01234567
dep=$(FORGE_DEPLOYS=1 FORGE_DEPLOY_CHART_REVISION=$rev FORGE_GITHUB_APP_ID=5239131 FORGE_PREVIEW_IP=10.150.1.222 sh "$dir/render.sh" --lb 10.150.1.220 10.150.1.221)
# deploys_section: everything deploys.yaml.tmpl adds (from its first line to the end).
deploys_section() { printf '%s\n' "$dep" | awk '/^# Deploys:/ {on=1} on'; }
gateway_listeners() {
  printf '%s\n' "$dep" | awk '$0 == "kind: Gateway" {on=1} on' | grep -qF 'name: staging' &&
    printf '%s\n' "$dep" | grep -qF 'hostname: "*.staging.10.150.1.222.nip.io"' &&
    printf '%s\n' "$dep" | grep -qF 'hostname: "*.production.10.150.1.222.nip.io"'
}
deploys_exact() { deploys_section | cmp -s - "$dir/testdata/deploys.golden"; }
prev_unchanged() { [ "$(FORGE_GITHUB_APP_ID=5239131 FORGE_PREVIEW_IP=10.150.1.222 sh "$dir/render.sh" --lb 10.150.1.220 10.150.1.221)" = "$prev" ]; }

check "deploys: forge-api setting"          sh -c "printf '%s' \"\$1\" | grep -qF 'FORGE_DEPLOYS, value: \"1\"'" _ "$dep"
check "deploys: no FORGE_DEPLOYS without it" sh -c "! printf '%s' \"\$1\" | grep -qF FORGE_DEPLOYS" _ "$prev"
check "deploys: gateway listeners"          gateway_listeners
check "deploys: no listeners without it"    sh -c "! printf '%s' \"\$1\" | grep -qF '.staging.'" _ "$prev"
check "deploys: Argo objects and RBAC exactly as reviewed" deploys_exact
check "deploys: chart pinned to the revision" sh -c "printf '%s' \"\$1\" | grep -qF 'targetRevision: $rev'" _ "$dep"
check "deploys: no #deploys markers"        sh -c "! printf '%s' \"\$1\" | grep -qF '#deploys'" _ "$dep"
check "deploys: valid YAML for kubectl"     sh -c "printf '%s' \"\$1\" | kubectl apply --dry-run=client -f - >/dev/null" _ "$dep"
check "previews output unchanged by deploys support" prev_unchanged
check "deploys need previews"               sh -c "! FORGE_DEPLOYS=1 FORGE_DEPLOY_CHART_REVISION=$rev FORGE_GITHUB_APP_ID=1 sh \"$dir/render.sh\" --lb 10.150.1.220 10.150.1.221 >/dev/null 2>&1"
check "deploys need a pinned chart revision" sh -c "! FORGE_DEPLOYS=1 FORGE_GITHUB_APP_ID=1 FORGE_PREVIEW_IP=10.150.1.222 sh \"$dir/render.sh\" --lb 10.150.1.220 10.150.1.221 >/dev/null 2>&1"
check "deploys refuse a branch as revision" sh -c "! FORGE_DEPLOYS=1 FORGE_DEPLOY_CHART_REVISION=main FORGE_GITHUB_APP_ID=1 FORGE_PREVIEW_IP=10.150.1.222 sh \"$dir/render.sh\" --lb 10.150.1.220 10.150.1.221 >/dev/null 2>&1"
check "bad FORGE_DEPLOYS refused"           sh -c "! FORGE_DEPLOYS=yes FORGE_GITHUB_APP_ID=1 FORGE_PREVIEW_IP=10.150.1.222 sh \"$dir/render.sh\" --lb 10.150.1.220 10.150.1.221 >/dev/null 2>&1"
```

`kubectl apply --dry-run=client` needs the cluster's discovery for the Argo CD CRDs. The dev machine's kubeconfig reaches the k3s cluster, which has them.

- [ ] **Step 2: Run to see them fail**

Run: `sh deploy/k3s/render_test.sh`
Expected: the new `deploys:` checks FAIL; every existing check still `ok`.

- [ ] **Step 3: Write `deploy/k3s/deploys.yaml.tmpl`**

```yaml
# Deploys: Forge's Argo CD objects and forge-api's read access to them.
# render.sh adds this when FORGE_DEPLOYS=1.
#   ${DEPLOY_CHART_REVISION}  forge commit the chart is taken from (pinned)
# Applications are named <service>-<env> and deploy into the namespace of that
# name; forge-api refuses a namespace that exists and isn't Forge's.
apiVersion: argoproj.io/v1alpha1
kind: AppProject
metadata:
  name: forge-services
  namespace: argocd
spec:
  description: Forge services, deployed by forge-api through showmino/forge-deploys
  sourceRepos:
    - https://github.com/showmino/forge.git
    - https://github.com/showmino/forge-deploys.git
  destinations:
    - {server: https://kubernetes.default.svc, namespace: "*-staging"}
    - {server: https://kubernetes.default.svc, namespace: "*-production"}
  clusterResourceWhitelist:
    - {group: "", kind: Namespace}
  namespaceResourceWhitelist:
    - {group: apps, kind: Deployment}
    - {group: "", kind: Service}
    - {group: gateway.networking.k8s.io, kind: HTTPRoute}
    - {group: "", kind: ResourceQuota}
    - {group: "", kind: LimitRange}
---
apiVersion: argoproj.io/v1alpha1
kind: ApplicationSet
metadata:
  name: forge-services
  namespace: argocd
spec:
  goTemplate: true
  goTemplateOptions: ["missingkey=error"]
  generators:
    - git:
        repoURL: https://github.com/showmino/forge-deploys.git
        revision: main
        directories:
          - path: services/*/*
        # LAN-only, so no webhooks; Argo's default is 180s.
        requeueAfterSeconds: 30
  template:
    metadata:
      name: '{{ index .path.segments 1 }}-{{ .path.basename }}'
      labels:
        app.kubernetes.io/managed-by: forge
      # Deleting a services/<service>/<env> directory removes its workload too.
      finalizers:
        - resources-finalizer.argocd.argoproj.io
    spec:
      project: forge-services
      sources:
        - repoURL: https://github.com/showmino/forge.git
          targetRevision: ${DEPLOY_CHART_REVISION}
          path: deploy/argocd/chart
          helm:
            releaseName: '{{ index .path.segments 1 }}'
            valueFiles:
              - '$values/{{ .path.path }}/values.yaml'
        - repoURL: https://github.com/showmino/forge-deploys.git
          targetRevision: main
          ref: values
      destination:
        server: https://kubernetes.default.svc
        namespace: '{{ index .path.segments 1 }}-{{ .path.basename }}'
      syncPolicy:
        automated:
          prune: true
          selfHeal: true
        syncOptions:
          - CreateNamespace=true
        managedNamespaceMetadata:
          labels:
            app.kubernetes.io/managed-by: forge
            forge.dev/service: '{{ index .path.segments 1 }}'
            forge.dev/deploy-env: '{{ .path.basename }}'
---
# forge-api reads Applications to report sync and health.
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: forge-api-applications
  namespace: argocd
rules:
  - apiGroups: [argoproj.io]
    resources: [applications]
    verbs: [get, list, watch]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: forge-api-applications
  namespace: argocd
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: forge-api-applications
subjects:
  - {kind: ServiceAccount, name: forge-api, namespace: forge}
```

- [ ] **Step 4: Markers and render.sh**

In `deploy/k3s/previews.yaml.tmpl`, append these listeners to the Gateway's `listeners:` list, each line ending with ` #deploys`:

```yaml
    - name: staging #deploys
      protocol: HTTP #deploys
      port: 80 #deploys
      hostname: "*.staging.${PREVIEW_IP}.nip.io" #deploys
      allowedRoutes: #deploys
        namespaces: #deploys
          from: Selector #deploys
          selector: #deploys
            matchLabels: #deploys
              app.kubernetes.io/managed-by: forge #deploys
    - name: production #deploys
      protocol: HTTP #deploys
      port: 80 #deploys
      hostname: "*.production.${PREVIEW_IP}.nip.io" #deploys
      allowedRoutes: #deploys
        namespaces: #deploys
          from: Selector #deploys
          selector: #deploys
            matchLabels: #deploys
              app.kubernetes.io/managed-by: forge #deploys
```

In `deploy/k3s/forge.yaml.tmpl`, after the `FORGE_PREVIEW_PULL_SECRET` env line, add:

```yaml
            - {name: FORGE_DEPLOYS, value: "1"} #deploys
```

Use the same indentation as the line above it.

In `deploy/k3s/render.sh`:
- Header comment, after the previews paragraph:

  ```sh
  # Deploys (forge deploy) need previews, plus the forge commit Argo CD takes the
  # chart from; lines with a trailing "deploys" marker exist only then:
  #   FORGE_DEPLOYS=1 FORGE_DEPLOY_CHART_REVISION=$(git rev-parse HEAD) FORGE_PREVIEW_IP=... | kubectl apply -f -
  ```

- After the previews validation block:

  ```sh
  deploys=${FORGE_DEPLOYS:-}
  chart_rev=${FORGE_DEPLOY_CHART_REVISION:-}
  case $deploys in
    ''|1) ;;
    *) echo "invalid FORGE_DEPLOYS: $deploys (use 1)" >&2; exit 2 ;;
  esac
  if [ -n "$deploys" ]; then
    [ -n "$preview_ip" ] || { echo "FORGE_DEPLOYS needs FORGE_PREVIEW_IP" >&2; exit 2; }
    case $chart_rev in
      *[!0-9a-f]*|'') echo "FORGE_DEPLOYS needs FORGE_DEPLOY_CHART_REVISION, a 40-character commit SHA" >&2; exit 2 ;;
    esac
    [ ${#chart_rev} -eq 40 ] || { echo "FORGE_DEPLOY_CHART_REVISION must be a full commit SHA" >&2; exit 2; }
    deploys_marks='s/ *#deploys$//'
  else
    deploys_marks='/#deploys$/d'
  fi
  ```

- In the `{ ... }` block, after the previews section:

  ```sh
    if [ -n "$deploys" ]; then
      echo "---"
      cat "$dir/deploys.yaml.tmpl"
    fi
  ```

- In the `sed` call, add `-e "$deploys_marks"` next to `-e "$previews"`, and `-e "s|\${DEPLOY_CHART_REVISION}|$chart_rev|g"`.

- [ ] **Step 5: Golden, then run**

Generate the golden from the deploys section and read it once to check it is exactly the template with the revision filled in:

```sh
FORGE_DEPLOYS=1 FORGE_DEPLOY_CHART_REVISION=0123456789abcdef0123456789abcdef01234567 FORGE_GITHUB_APP_ID=5239131 FORGE_PREVIEW_IP=10.150.1.222 \
  sh deploy/k3s/render.sh --lb 10.150.1.220 10.150.1.221 | awk '/^# Deploys:/ {on=1} on' > deploy/k3s/testdata/deploys.golden
```

Run: `sh deploy/k3s/render_test.sh`
Expected: every line `ok`.

Then run a server-side dry run against the real cluster, which validates the Argo CRDs' schemas without changing anything:
`FORGE_DEPLOYS=1 FORGE_DEPLOY_CHART_REVISION=$(git rev-parse HEAD) FORGE_GITHUB_APP_ID=5239131 FORGE_PREVIEW_IP=10.150.1.222 sh deploy/k3s/render.sh --lb 10.150.1.220 10.150.1.221 | kubectl apply --dry-run=server -f - 2>&1 | tail -20`.
Every object must say `(server dry run)` with no error. Report the output. Do NOT run `kubectl apply` without `--dry-run`.

- [ ] **Step 6: Production chart golden**

Create `deploy/argocd/chart/testdata/production.yaml`: the same as `staging.yaml` with `env: production`, `host: payments.production.10.150.1.222.nip.io`, `replicas: 2`, `forge.env: production`, `forge.kind: promote`. In `Makefile` `chart-test`, add a second render:

```make
	$(HELM) template payments deploy/argocd/chart --namespace payments-production \
	  -f deploy/argocd/chart/testdata/production.yaml | diff -u deploy/argocd/chart/testdata/production.golden -
```

Generate `production.golden` with that `helm template` command (without the diff). Check it once: the HTTPRoute `sectionName: production`, `replicas: 2`, the production host. Run `make chart-test` → no diff.

- [ ] **Step 7: Docs**

- `README.md`, k3s section, after the previews paragraph:

  ````markdown
  Deploys (`forge deploy`) need previews, the GitOps repo `showmino/forge-deploys`
  (private, with an initial commit), and the forge commit Argo CD takes the chart
  from:

  ```sh
  FORGE_DEPLOYS=1 FORGE_DEPLOY_CHART_REVISION=$(git rev-parse HEAD) FORGE_PREVIEW_IP=<gateway-ip> ... \
    deploy/k3s/render.sh --lb <dex-ip> <api-ip> | kubectl apply -f -
  ```

  This adds the Argo CD AppProject and ApplicationSet `forge-services`, a Role
  that lets forge-api read Applications, and the Gateway listeners for
  `http://<service>.staging.<gateway-ip>.nip.io` and `.production.`. Argo CD's
  repo credentials must be able to read both repos.
  ````

- `docs/specs/2026-10-09-phase-4-deployments-design.md`, a short "As built (milestone 3)" note at the end of section 5:
  - The runner scale set is `kube-runners` (Argo app `action-runner-set`), so `runs-on: kube-runners`.
  - The CI job downloads the CLI from forge-api (`$FORGE_API_URL/cli/forge-linux-<arch>`) instead of `go install`, so it needs no token for the private forge repo.
  - The chart revision is a pinned commit (`FORGE_DEPLOY_CHART_REVISION`).
  - Generated Applications carry Argo's resources finalizer.
  - forge-api refuses a `<service>-<env>` namespace that exists and isn't Forge's.
- `.claude/skills/forge/verifying.md`: add a "Deploys (Phase 4)" section with:
  - the render command above;
  - checking the ApplicationSet with `kubectl -n argocd get applicationset forge-services -o jsonpath='{.status.conditions}'`;
  - teardown: delete the `services/<svc>` directory in `forge-deploys`, wait for the Applications to disappear, then delete the `<svc>-staging`/`-production` namespaces;
  - deleting the Argo objects: `kubectl -n argocd delete applicationset,appproject forge-services` and `kubectl -n argocd delete role,rolebinding forge-api-applications`.

- [ ] **Step 8: Commit**

`make test lint` (Go is untouched, but keep the habit), `make chart-test`, `sh deploy/k3s/render_test.sh`, then:

```bash
git add deploy/k3s Makefile deploy/argocd/chart/testdata README.md docs/specs/2026-10-09-phase-4-deployments-design.md .claude/skills/forge/verifying.md
git commit -m "k3s: Argo CD AppProject, ApplicationSet and Gateway listeners for deploys; production chart golden"
```

---

### Task 4: go-grpc v1.2.0, the `deploy` job (repo `showmino/forge-templates`)

**Files (in `~/code/forge-templates`, a separate repo; work on a branch `go-grpc-v1.2.0`):**
- Modify: `go-grpc/skeleton/.github/workflows/ci.yml`
- Modify: `go-grpc/skeleton/README.md` if it describes CI (add one line about the deploy job)

- [ ] **Step 1: Add the job**

Append to `go-grpc/skeleton/.github/workflows/ci.yml` (the file is a Go template with `{% %}` delimiters; GitHub's `${{ }}` passes through):

```yaml

  # Deploys main to staging through Forge. Runs on the LAN runners, which can
  # reach the Forge API, and logs in with this job's GitHub OIDC token: the
  # token only lets it deploy this service to staging, at this commit.
  deploy:
    if: github.event_name == 'push' && github.ref == 'refs/heads/main'
    needs: image
    runs-on: kube-runners
    permissions:
      contents: read
      id-token: write
    env:
      FORGE_API_URL: ${{ vars.FORGE_API_URL }}
    steps:
      - uses: actions/checkout@v4
      - name: Get the forge CLI from the Forge API
        run: |
          if [ -z "$FORGE_API_URL" ]; then
            echo "::error::set the organization variable FORGE_API_URL to the Forge API (for example http://forge.10.150.1.221.nip.io)"
            exit 1
          fi
          case $(uname -m) in
            x86_64) arch=amd64 ;;
            aarch64|arm64) arch=arm64 ;;
            *) echo "::error::unsupported runner architecture $(uname -m)"; exit 1 ;;
          esac
          url="$FORGE_API_URL/cli/forge-linux-$arch"
          echo "downloading $url"
          curl -fsS --retry 3 --max-time 120 -o "$RUNNER_TEMP/forge" "$url"
          chmod +x "$RUNNER_TEMP/forge"
          "$RUNNER_TEMP/forge" version
      - name: Deploy to staging
        run: '"$RUNNER_TEMP/forge" deploy start --env staging'
```

- [ ] **Step 2: Lint and render it**

From the forge repo: `cd ~/code/forge && go run ./cmd/forge template lint ~/code/forge-templates/go-grpc` must report no findings. Also render it with `--render-dir` if lint supports it (`go run ./cmd/forge template lint --help`), and check the rendered `ci.yml`. It must parse as YAML (`python3 -I -c 'import sys,yaml; yaml.safe_load(open(sys.argv[1]))' <file>`, or by reading it), and `${{ vars.FORGE_API_URL }}` must survive rendering unchanged.

- [ ] **Step 3: Commit and open the PR**

```bash
cd ~/code/forge-templates
git checkout -b go-grpc-v1.2.0
git add go-grpc/skeleton/.github/workflows/ci.yml go-grpc/skeleton/README.md
git commit -m "go-grpc: deploy main to staging through Forge from CI (v1.2.0)"
```

The controller pushes, opens the PR, merges it, and tags `v1.2.0` (rollout step R6).

---

## Rollout and the exit gate (controller, after Tasks 1-4 are merged)

Each step is run by the controller and checked before the next. The owner authorized all of milestone 3 ("Do all the milestone 3, you can use gh cli and argocd cli as well").

- **R1, image.** On `main` after the forge PR merges:
  ```sh
  docker build --build-arg VERSION=$(git describe --tags --always) --build-arg COMMIT=$(git rev-parse HEAD) \
    -t ghcr.io/showmino/forge-api:$(git rev-parse --short HEAD) .
  docker push ...
  ```
- **R2, `forge-deploys`.** Run `gh repo create showmino/forge-deploys --private --add-readme --description "Forge deploys: one values.yaml per service and environment, written by forge-api"`. Then check that the App `mino-forge` can see it: its installation covers all repos; verify with an App token, or by forge-api's first commit in the gate.
- **R3, render and apply.**
  ```sh
  FORGE_DEPLOYS=1 FORGE_DEPLOY_CHART_REVISION=<main sha> FORGE_PREVIEW_IP=10.150.1.222 \
    FORGE_ONEPASSWORD_ITEM=vaults/Kube/items/forge-github-app FORGE_GITHUB_APP_ID=5239131 \
    FORGE_API_IMAGE=ghcr.io/showmino/forge-api:<sha7> deploy/k3s/render.sh --lb 10.150.1.220 10.150.1.221
  ```
  Review it with `kubectl diff -f -`, then `kubectl apply -f -`. Wait for `kubectl -n forge rollout status deploy/forge-api`. Check:
  - the ApplicationSet has no error condition: Argo can read `forge-deploys` with `argocd-repo-kubernetes`;
  - `/cli/forge-linux-amd64` downloads from `http://forge.10.150.1.221.nip.io`;
  - forge-api's log says deploys are on and GitHub OIDC is accepted.
- **R4, org variable `FORGE_API_URL`.** Run `gh variable set FORGE_API_URL --org showmino --visibility all --body http://forge.10.150.1.221.nip.io`. This needs the `admin:org` scope, which the current `gh` login lacks. The owner runs `gh auth refresh -h github.com -s admin:org` (or sets the variable in the org settings). With that scope, also check that the runner group `kube` serves all repos: `gh api orgs/showmino/actions/runner-groups`.
- **R5, a runner-reachability check.** From a pod in `arc-runners`, the API must answer. Use the existing runner pod: `kubectl -n arc-runners exec <runner pod> -c runner -- curl -fsS http://forge.10.150.1.221.nip.io/healthz`.
- **R6, template release.** Push `go-grpc-v1.2.0`, open the PR, merge, then `git tag v1.2.0 && git push origin v1.2.0` in `forge-templates`.
- **R7, the gate.** Log in as a human (`forge auth login`; scripted approval per verifying.md), then:
  1. `forge create gate-<MMDDhhmm> --template go-grpc --team platform`. After this, no human acts until step 3.
  2. `gh run watch` the repo's CI: `test` → `image` → `deploy`, all green.
  3. Check:
     - `forge deploy status gate-<x>` shows staging Synced and Healthy at the pushed commit;
     - `http://gate-<x>.staging.10.150.1.222.nip.io/healthz` returns 200;
     - Greet answers (`python3 -I` urllib, as in the Phase 3 gate).
  4. `forge deploy promote gate-<x> --yes`; production answers `/healthz` and Greet at `.production.`.
  5. Check the Argo Applications report both revisions (multi-source `status.sync.revisions`), to settle the milestone 1 question.
- **R8, cleanup.**
  - Delete `services/gate-<x>` in `forge-deploys` (`gh api -X DELETE repos/showmino/forge-deploys/contents/<path>` for each `values.yaml`). Wait until both Applications and their Deployments are gone (the finalizer), then delete the two namespaces.
  - Run `forge service delete gate-<x>` and `gh repo delete showmino/gate-<x> --yes`.
  - The ghcr package needs the `delete:packages` scope. Leave it for the owner, with the other leftover packages.
