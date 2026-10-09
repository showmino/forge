---
name: forge
description: Use when working in the Forge repo (showmino/forge) - the internal developer platform with the forge CLI, forge-api, ConnectRPC protos, Dex login and k3s demo deploy - for any change, new command or RPC, phase planning, build/test/lint failures, or verifying login and the cluster deploy.
---

# Forge

Forge is an internal developer platform: the `forge` CLI, the `forge-api` server and
the Protobuf contract they share, in one Go module (`github.com/showmino/forge`).
The approved design doc is `docs/index.md` (command surface, auth, API, output
and exit codes, phases with exit gates, open questions): read the relevant
section before designing anything. README.md has the user-facing docs; this
skill holds what neither says.

## Decisions (settled; do not reopen without the owner)

- ConnectRPC, not grpc-gateway. One Go module.
- Dex for local and demo login; production IdP deferred. CLI uses the OIDC device flow and sends the **ID token** as bearer; the API verifies it with the CLI client ID (`forge-cli`) as audience.
- Argo CD only (Flux later). OpenTofu jobs run by the API (not Crossplane).
- Templates live in a Git repo, versioned by tag.
- Telemetry off. Distribution: GitHub Releases + Homebrew tap (`showmino/homebrew-tap`).
- Demos run on the owner's k3s cluster (see verifying.md for its quirks).

## Conventions

| Rule | Where it lives |
| --- | --- |
| `forge <noun> <verb>`, one file per noun | `internal/cli/<noun>.go`, registered in `NewRoot` (`root.go`) |
| No package globals; commands get everything via `Deps` (wrapped by `*Factory`) | `internal/cli/factory.go` |
| Commands return errors; only `main` picks exit codes | `internal/exitcode` (`exitcode.New`, `Usagef`); `cli.Run` classifies Connect codes |
| Every command supports `-o table\|json\|yaml\|name`; JSON is the resource itself | `output.Print(w, format, output.View{Headers, Rows, Names, Data})` |
| Golden tests for output | `internal/output/testdata/*.golden`; `make update-golden` after an intended change |
| CLI tests run the real command tree against an in-process API | `newHarness(t)` in `internal/cli/cli_test.go` (static tokens `t-writer`, `t-reader`) |
| Protos in `proto/forge/v1`, AIP-style names (`services/{id}`), `request_id`, `validate_only`, etags | regenerate with `make generate`; `gen/` is committed and CI checks it |
| Server permissions are strings checked per RPC | `require(ctx, PermX)` in `internal/server/authn.go` |

Adding an RPC: edit proto, then `make generate`, then the handler in `internal/server` (register it in `server.New`), then the client in `pkg/client`, the CLI noun, and harness tests. Run `make test lint` before every commit (TDD: test first).

## Quick reference

```sh
export PATH=$HOME/.local/go/bin:$HOME/go/bin:$PATH   # Go is not on the system PATH
make build test lint                                 # Go 1.27, golangci-lint v2.14.0, as CI
make generate | make update-golden | make api-dev     # api-dev: token "dev", no IdP
```

**REQUIRED for any login or deploy check:** follow `verifying.md` in this skill directory
(local Dex, the k3s cluster, the scripted device-login approval, known failure symptoms).

## Status and roadmap

- **Phase 1 (done):** catalog (in memory), contexts, auth, init, doctor, version, CI, GoReleaser. Dex login verified locally and on k3s (2026-10-08).
- **Phase 2 (done):** templates and `forge create`, spec `docs/specs/2026-10-08-phase-2-templates-design.md`. Milestone 1: `internal/template`, `forge template lint`, `go-grpc` in `showmino/forge-templates` (`docs/plans/2026-10-08-phase-2-m1-template-lint.md`). Milestone 2: GitHub App auth, `TemplateService`, `forge template list/get` (`...-m2-template-service.md`). Milestone 3: `CreateService`, `forge create` (`...-m3-create.md`). Milestone 4: forge-api on k3s as the App, key from 1Password (`...-m4-k3s-exit-gate.md`). Exit gate passed on k3s on 2026-10-08: `forge create forge-e2e-20261008` made the repo, its CI (`test`, `image`) passed, and it appeared in the catalog.
- **Phase 2 follow-up backlog (minor, deferred in review):**
  - Templates: lint findings name `skeleton/` instead of the file and line; a rendered file `a` and directory `a/` are not caught as a collision; file names of only spaces are accepted; a missing path says "is not a directory"; a `--render-dir` conflict exits before printing findings and can leave partial output; list defaults like `[a, b]` are accepted as strings; default-value findings don't say "default"; a required text parameter whose pattern rejects the lint sample `sample` can never pass lint.
  - `go-grpc`: assumes a lowercase GitHub owner; its `forge.yaml` values aren't quoted; it still targets Go 1.24.
  - TemplateService: downloaded tags stay in memory until restart (100 MiB cap each); raw GitHub error text (repo paths, App ID) can reach callers in "unavailable" messages; a reinstalled App or revoked token needs a restart or up to an hour; `--templates-dir` silently wins over the App flags; `GetTemplate` checks the name before the login; every `ListTemplates` re-parses each `template.yaml` and re-logs broken ones.
  - Create: text files with NUL bytes go inline instead of as blobs; an empty answer to a required prompt is accepted; the "repo could not be deleted" hint doesn't name the repo; a repo GitHub created after a timed-out create isn't explained; shutdown doesn't wait for running creates; the record of past creates grows until restart and isn't tied to the caller.
- **Phase 3 (done):** preview environments and operations, spec docs/specs/2026-10-08-phase-3-environments-design.md. Milestone 1: operations (OperationService, in-memory store, forge op list/watch/cancel; docs/plans/2026-10-08-phase-3-m1-operations.md). Milestone 2: preview environments (EnvironmentService, internal/server/env_*.go, internal/registry, forge env create/list/get/extend/destroy; forge-api --previews; ...-m2-environments.md). Milestone 3: k3s with FORGE_PREVIEW_IP (forge-api ServiceAccount + least-privilege ClusterRole, Gateway forge/forge-previews on 10.150.1.222) and go-grpc v1.1.0 (run:, $PORT) (docs/plans/2026-10-09-phase-3-m3-k3s-exit-gate.md). Exit gate passed on k3s on 2026-10-09: forge create + forge env create READY in 4m37s, /healthz 200 and Greet answered at the URL.
- **Phase 3 follow-up backlog (minor, deferred in review):**
  - Operations: final watch message repeats all events; expired operations stay readable until the next prune; a real error under a cancel is reported as "cancelled"; reattach does not retry a failed reopen; failed operations print no body with `-o json`; a few untested edge paths (attempt cap, Watch without a token); op list golden alignment.
  - Environments: request_id map unpruned and not scoped to the environment; `e.mu` held across a Kubernetes list (bounded by the 30s client timeout); Status ignores Deployment ReplicaFailure (quota admission races) and failing readiness probes; READY is set before the HTTPRoute is Accepted; List shows no live status; Destroy's response shows the pre-destroy state; one spurious destroy is possible when a create ends while a destroy starts.
  - Pods and inputs: no `runAsUser` (images with a named USER fail); caller labels could override ownership labels; `EnvGone` unguarded; image refs with digests accepted; `FORGE_PREVIEWS` accepts only 1/true and a bad `FORGE_PREVIEW_MAX` is reported as -1; `clip` may split UTF-8.
  - Tests: some timing-based tests (TestCreateNotReady's 100ms) and a near-tautological malformed-expiry test; the slash-in-ref test cannot see escaping.
  - go-grpc still targets Go 1.24 and golangci-lint v2.5.0 in the services it generates.
- **Phase 4 (done):** spec docs/specs/2026-10-09-phase-4-deployments-design.md. Milestone 1 (done, PR #14): deploy engine (DeployService, internal/server/deploy*.go, github.RepoFiles, deploy/argocd/chart with make chart-test, forge deploy) per docs/plans/2026-10-09-phase-4-m1-deploy-engine.md. Milestone 2: CI workload identity (internal/server/ci_auth.go verifies GitHub Actions tokens, ByIssuer routes by iss, ci_scope.go confines workloads; internal/auth/actions.go and the CLI's Actions token pick-up) per docs/plans/2026-10-09-phase-4-m2-ci-identity.md. Milestone 3 (done, PR #16; go-grpc v1.2.0): namespace and Application guards, `/cli/` download, render.sh's Argo CD objects, per docs/plans/2026-10-09-phase-4-m3-k3s-exit-gate.md. Live on k3s since 2026-10-09 (forge-api c31fba9, chart pinned to c31fba9; org variable FORGE_API_URL; runner set `kube-runners`). Exit gate passed on k3s on 2026-10-09: `forge create gate-10092019`, its CI ran test → image → deploy (34s) with no human action, staging Synced/Healthy at the pushed commit by `github:showmino/gate-10092019`, `/healthz` and Greet answered; `forge deploy promote --yes` and production answered too; cleanup via forge-deploys removed both Applications and workloads. Pods get a writable `/tmp` (emptyDir, 256Mi) since the Next.js template (docs/specs/2026-10-09-nextjs-template-design.md, PR #17, live on k3s at forge-api/chart 18b1232).
- **Templates (showmino/forge-templates, repo-wide tags):** `go-grpc` and, since v1.3.0, `nextjs` (Next.js 16.4 standalone, TypeScript, Tailwind 4 via the Turbopack loader, Vitest; distroless Node 24 as uid 65532; `.next/cache` → `/tmp`, `experimental.isrFlushToDisk: false` so a read-only root works; health `/api/healthz`, port 3000). Base images come from `mirror.gcr.io` (Docker Hub rate-limits CI runners; go-grpc v1.2.1). The templates repo's CI builds every rendered template (npm or make). Live check passed on k3s on 2026-10-09: `forge create web-10092207 --template nextjs`, CI test → image → deploy in 3.5 min, staging page and `/api/healthz` OK, a preview READY in ~10s, a second push redeployed cleanly.

  Phase 4 follow-up backlog:
  - Any workflow or event on the default branch (pull_request_target, workflow_dispatch, schedule) gets a deploy principal; consider pinning `event_name` / `job_workflow_ref`.
  - A bad-signature token carrying GitHub's iss makes forge-api refetch GitHub's JWKS; rate-limit the refetches.
  - A JWKS fetch failure is reported as an invalid token (exit 4) rather than Unavailable.
  - A DefaultBranch 404 maps to Unavailable.
  - Raw GitHub text appears in Unavailable messages.
  - `forge auth token` prints the Actions token unmasked.
  - The Actions token request ignores the command's context (30s cap).
  - The streaming client interceptor drops token errors.
  - A ValidatingAdmissionPolicy binding forge-api's secret create and namespace patch/delete to Forge-labelled namespaces (today a compromised forge-api could write Argo `repository` secrets in `argocd`).
  - Dex still has the demo password and `FORGE_WRITER_GROUPS` is empty, so any LAN user who logs in can deploy and promote to production.
  - Database backups: `forge-db` (CloudNativePG) has none yet.
  - The GoReleaser image's `COPY ${TARGETPLATFORM}/forge` is unbuilt until the first release snapshot.
- **Design doc items Phase 1 did not build:** OpenTelemetry tracing (`traceparent`, `FORGE_TRACE`), rich error details (`BadRequest`, `ErrorInfo`), `auth login --web` (PKCE), daily version-skew check, testscript tests (the CLI harness in `cli_test.go` plays that role), a CLI container image. No release is tagged yet, so the Phase 1 gate's "Homebrew install" is unverified.
- **Groundwork (done):** k3s login through GitHub (writer group `showmino:platform-engineering`), and CloudNativePG `forge-db` holds the catalog and Dex's logins (spec docs/specs/2026-10-09-groundwork-auth-storage-design.md).
- **Later:** environments, resources (OpenTofu jobs).
- **Known follow-ups:** credentials file fallback is not encrypted (newer gosec flags it as G117); macOS binary not notarized; no LICENSE; k3s demo serves plain HTTP with Dex's demo password.

## Common mistakes

- Running lint with an older golangci-lint than CI's v2.14.0 (it cannot read Go 1.27 export data), or "fixing" the two `G117` nolints in `internal/auth/store.go`: storing the refresh token is intended; encrypting the file fallback is the real follow-up.
- Following the README's `k3s ctr images import` path on the demo cluster: the dev machine is not a node.
- Exposing Dex through Istio `*.showmino.com`: it is public, and Dex still has the demo password.
- Adding a package-level variable or calling `os.Exit` outside `main`.
- Giving a CI workload a new RPC by adding a permission: workloads pass the interceptor only for the procedures in `workloadProcedures` (`authn.go`), and each of those handlers must call its `ci_scope.go` check.
