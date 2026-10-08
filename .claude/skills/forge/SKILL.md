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
GOTOOLCHAIN=go1.24.13 make build test lint           # CI parity (golangci-lint v2.5.0)
make generate | make update-golden | make api-dev     # api-dev: token "dev", no IdP
```

**REQUIRED for any login or deploy check:** follow `verifying.md` in this skill directory
(local Dex, the k3s cluster, the scripted device-login approval, known failure symptoms).

## Status and roadmap

- **Phase 1 (done):** catalog (in memory), contexts, auth, init, doctor, version, CI, GoReleaser. Dex login verified locally and on k3s (2026-10-08).
- **Phase 2 (in progress):** milestone 1 (`internal/template`, `forge template lint`, `go-grpc` in `showmino/forge-templates`) per `docs/plans/2026-10-08-phase-2-m1-template-lint.md`; then `forge template list/get` and `forge create`; first golden path a Go gRPC service. Exit gate: a scaffolded service passes its own CI and appears in the catalog. Blocking open question in the design doc: the template format (doc recommends a Forge format, `template.yaml` with typed parameters plus a `skeleton/` rendered with Go `text/template`, and a Backstage importer only if the `~/code/template-*` repos are still used). Brainstorm and get a written spec approved before code.
- **Design doc items Phase 1 did not build:** OpenTelemetry tracing (`traceparent`, `FORGE_TRACE`), rich error details (`BadRequest`, `ErrorInfo`), `auth login --web` (PKCE), daily version-skew check, testscript tests (the CLI harness in `cli_test.go` plays that role), a CLI container image. No release is tagged yet, so the Phase 1 gate's "Homebrew install" is unverified.
- **Later:** environments, deploys (Argo CD), resources (OpenTofu jobs).
- **Known follow-ups:** credentials file fallback is not encrypted (newer gosec flags it as G117); macOS binary not notarized; no LICENSE; k3s demo serves plain HTTP with Dex's demo password.

## Common mistakes

- Running `make lint` with Go 1.25+ or the latest golangci-lint, then "fixing" the typecheck or G117 errors CI never reports.
- Following the README's `k3s ctr images import` path on the demo cluster: the dev machine is not a node.
- Exposing Dex through Istio `*.showmino.com`: it is public, and Dex still has the demo password.
- Adding a package-level variable or calling `os.Exit` outside `main`.
