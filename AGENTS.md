# Working on Forge

This file is for every coding agent that works in this repository (GitHub
Copilot, Claude Code, Codex, Cursor and others), and for people. It is the one
place the working rules live: tool-specific files (`CLAUDE.md`,
`.github/copilot-instructions.md`, `.claude/skills/forge/SKILL.md`) only point
here. Read it before you change anything.

## What Forge is

An internal developer platform that replaces Backstage for the `showmino`
organization: the `forge` CLI, the `forge-api` server and the Protobuf
contract they share, in one Go module (`github.com/showmino/forge`). It runs
on the owner's k3s cluster.

| Repo | What |
| --- | --- |
| `showmino/forge` (this one) | CLI, API, protos, the deploy chart, the cluster manifests (`deploy/k3s`) |
| `showmino/forge-web` | The web dashboard: Next.js, a thin client of the same API. It is itself a Forge service |
| `showmino/forge-templates` | The templates `forge create` scaffolds from (`go-grpc`, `nextjs`), versioned by repo-wide tags |
| `showmino/forge-deploys` | The GitOps repo. Only forge-api writes to it; never edit it by hand |
| `showmino/kubernetes` | The owner's cluster repo (Cloudflare tunnel, DNS records, Argo CD). Open a pull request there; the owner merges |
| `showmino/techdocs` | The organization's documentation, itself a Forge service. The user documentation of Forge lives in its `docs/forge/` |

## Read first

| File | Read it for |
| --- | --- |
| `docs/index.md` | The approved design: commands, auth, API, output and exit codes, the phases and their exit gates. Read the relevant section before designing anything |
| `docs/agents/status.md` | How far the plan has got: what is built, what is live, what is left, known follow-ups |
| `docs/agents/verifying.md` | The runbook: what runs on the cluster right now, the render command that is live, how to check each feature |
| `docs/specs/` | One design per phase or feature, as built |
| `README.md` | The user-facing documentation |

`docs/plans/` holds the step-by-step plans of earlier phases. They are history:
they name files that have since moved (for example `.claude/skills/forge/…`,
now `docs/agents/…`).

## Rules

These were set by the owner. They are not suggestions.

### The cluster is production

- The k3s cluster in the kube context is **production**. There is one cluster;
  "staging" and "production" are environments inside it.
- **Read-only commands are fine** (`kubectl get`, `diff`, `logs`,
  `--dry-run=server`). **Anything that changes the cluster needs the owner's
  explicit OK each time:** applying manifests, restarting a workload,
  promoting a service to production, deleting anything.
- A rollout follows one procedure, written out in `docs/agents/verifying.md`:
  what is live is `deploy/k3s/live/forge.yaml` (rendered from
  `deploy/k3s/live.env` by `render-live.sh`) and there is no drift (Argo CD
  shows it Synced; `kubectl diff` on it shows nothing but Argo CD's
  `tracking-id` annotations); change `live.env`, render, read the diff; show it
  to the owner; after the OK, server dry run, then apply; verify; merge the
  pull request so `main` says what is live. Argo CD shows Forge as the
  application `forge`, with manual sync: never press Sync without the OK.
- Never write to the production database directly. Migrations run when
  forge-api starts.
- A reviewer or subagent you start gets no cluster access: tell it to run no
  `kubectl`.

### What needs the owner's OK

| Free to do | Ask first, every time |
| --- | --- |
| Create branches, push them, open pull requests in `forge`, `forge-web` and `forge-templates` | Any change to the cluster (see above) |
| Merge a pull request to `main` there, once every check has passed (see below) | Creating a real service or repository (`forge create`) |
| Tag a release after its change is merged | Deleting anything: a service, a branch (also a merged one), a repository, an image, a namespace |
| Open a pull request in `showmino/kubernetes` | Merging in `showmino/kubernetes`; settings on GitHub (app permissions, OAuth apps, org variables) |

An approval covers what was asked and nothing more. "Yes" to a rollout is not
a yes to the next one.

### Merging

- Merge only when **every check of the commit is `success`**. Read the
  result; do not chain a merge behind a wait and assume it passed. A merge
  with a red test happened once here. A flaky test is fixed, not merged past.
- In this repo no job is skipped on a pull request: all five (`test`, `lint`,
  `proto`, `goreleaser-check`, `runner-image`) must pass. The exception is a
  pull request that only changes `docs/**` or the Markdown files at the root
  (`README.md`, `AGENTS.md`, ...): CI does not run for it at all (the owner's
  rule: checks on docs alone only cost time), and it merges once it contains
  the current `main`. Docs elsewhere (test data, templates) run CI as usual.
  A rollout pull request that only changes `deploy/k3s/live.env` and
  `deploy/k3s/live/forge.yaml` gets no CI either (the owner's rule,
  2026-10-16). CI's check that `live/forge.yaml` is the render of `live.env` then does not
  run: run `sh deploy/k3s/render-live.sh --check` yourself before you push.
  In `forge-web` the
  jobs `image` and `deploy` only run on `main` and show as skipped on a pull
  request; that is expected, and `test` must pass. Any other skip is a stop.
- **The checks run once, on the pull request, and not again after the merge**
  (the owner's rule: a second run of the same checks only costs time). That
  makes two things matter. `main` changes only through pull requests. And
  before you merge, the branch must contain the current `main`: if `main` has
  moved since the checks ran, merge `main` into the branch and wait for the
  checks again, otherwise the combination was never tested. Check it with
  `gh api repos/showmino/<repo>/compare/main...<branch> --jq .behind_by`
  (must be 0).
- Never force-push `main`. Never rewrite published history.
- A change that is risky (auth, what becomes reachable from the internet,
  anything on the path to production) gets **one independent review** before
  it merges: a second model or person reading the diff cold. Fix what it
  finds; say what you chose not to fix and why.

### Every feature is documented in techdocs

- Each new feature, and each change to what a feature does (a command, a
  flag, a forge.yaml field, a dashboard page, an error people will meet), also
  gets its user documentation in `showmino/techdocs`, under `docs/forge/`
  (the owner's rule, 2026-10-16). It is part of the same work: open the
  techdocs pull request alongside the forge, forge-web or forge-templates one,
  not later.
- Write it for the people who use Forge: what to do and the commands to type,
  checked against the code. Update the page that covers the feature, and
  `16-command-reference.md` when a command or flag changes (it is the output
  of `forge <command> --help`). Run `forge docs lint` on the checkout.
- A techdocs pull request follows the same merging rules as here. Merging it
  deploys techdocs to staging; promoting it to production needs the owner's OK
  like any other promotion.

### Secrets

- Never print, log, paste or commit: the GitHub App private key
  (`mino-forge.*.private-key.pem` in the repo root, ignored by git), the
  database URL or password, the OAuth client secret, ID tokens, session
  tokens (`fgs_…`), run tokens. Name the key, never the value.
- The credentials file (`~/.config/forge/credentials.json`) holds a live
  session. Read field names only.
- Kubernetes Secrets: list their key names, not their data.

### How the owner likes to work

- **Fast, and little ceremony.** Build the change; do not write long plan
  documents or split small work across many agents. A new phase gets a short
  design in `docs/specs/` and the owner's go-ahead before code.
- **Ask when the decision is theirs** (names, exposure, what a feature should
  do), with a recommendation first. Do not ask about what you can read in the
  code or decide by convention.
- **No long-running tests by default.** Ask before starting one (for example
  the end-to-end test on a throwaway cluster).
- **Say what you did not check.** If a step needs the owner's browser or a
  setting on GitHub, say so and stop there. Report failures with their output.
- Forge replaces Backstage. Do not design around Backstage.

## How to work here

```sh
export PATH=$HOME/.local/go/bin:$HOME/go/bin:$PATH   # on the owner's machine Go is not on the system PATH
make build test lint                                 # Go 1.27, golangci-lint v2.14.0, as CI
make generate                                        # after a proto change; gen/ is committed and CI checks it
make update-golden                                   # after an intended change of CLI output
make chart-test                                      # the deploy chart against its goldens
sh deploy/k3s/render_test.sh                         # the cluster manifests; NOT in CI, run it when deploy/k3s changes
make db                                              # a local PostgreSQL for the store tests
FORGE_TEST_DATABASE_URL='postgres://postgres:forge@localhost:55432/postgres?sslmode=disable' make test
```

Run `make test lint` before every commit. Write the test first.

### Decisions that are settled (do not reopen without the owner)

- ConnectRPC, not grpc-gateway. One Go module.
- Login is Dex with GitHub behind it; only members of `showmino` get in, and
  writing needs the team `showmino:platform-engineering`. A login becomes a
  Forge session (`fgs_…`), which is what clients send from then on.
- Argo CD only (Flux later). OpenTofu jobs run by the API (not Crossplane),
  except DNS records, which use the owner's `CloudflareRecordsApplication`.
- Templates live in a Git repo, versioned by tag.
- The dashboard is a Next.js app from Forge's own `nextjs` template, deployed
  by Forge; it has no endpoints of its own.
- Telemetry off. Releases are GitHub Releases, cut by pushing a `vX.Y.Z` tag.

### Conventions

| Rule | Where it lives |
| --- | --- |
| `forge <noun> <verb>`, one file per noun | `internal/cli/<noun>.go`, registered in `NewRoot` (`root.go`) |
| No package globals; commands get everything via `Deps` (wrapped by `*Factory`) | `internal/cli/factory.go` |
| Commands return errors; only `main` picks exit codes | `internal/exitcode` (`exitcode.New`, `Usagef`); `cli.Run` classifies Connect codes |
| Every command supports `-o table\|json\|yaml\|name`; JSON is the resource itself | `output.Print(w, format, output.View{Headers, Rows, Names, Data})` |
| Golden tests for output | `internal/output/testdata/*.golden`, `internal/cli/testdata` |
| CLI tests run the real command tree against an in-process API | `newHarness(t)` in `internal/cli/cli_test.go` (static tokens `t-writer`, `t-reader`) |
| Protos in `proto/forge/v1`, AIP-style names (`services/{id}`), `request_id`, `validate_only`, etags | `make generate` |
| Who may do what: a write RPC calls `authorize(ctx, Act..., service)` (or `authorizeTeam`) with an action of the role table; reads use `require(ctx, PermServicesRead)` | `internal/server/rbac.go` (roles reader, team, admin; `actionRoles`) |
| Every write RPC is in `auditActions`, every read in `auditReads` | `internal/server/audit.go`; `TestAuditCoversEveryProcedure` fails otherwise |
| A resource run ends through `Resources.endRun`, which changes the plan once and appends the one outcome event | Operations started with `withoutOutcome(ctx)`; never append a resource outcome anywhere else |
| Policy rules are `policy/*.rego`, package `forge.resources`, built into the binary | every rule has a passing and a failing case in `policy/policy_test.go` |
| Comments and docs are plain English, written for someone who was not there | the code around you |

Adding an RPC: edit the proto, `make generate`, the handler in
`internal/server` (register it in `server.New`), the client in `pkg/client`,
the CLI noun, harness tests; classify it in `auditActions` or `auditReads`.
The dashboard has a copy of the protos: after the change is merged here, run
`sh scripts/sync-proto.sh ../forge` in a checkout of `showmino/forge-web`
(the script is in that repo, not this one) and open a pull request there.

### Mistakes that have been made here

- Merging with a failed check, by chaining the merge behind a wait.
- Deleting a merged remote branch without asking.
- Changing a comment in a manifest template and leaving the render goldens
  behind (`render_test.sh` is not in CI).
- Running lint with an older golangci-lint than v2.14.0 (it cannot read Go
  1.27 export data), or "fixing" the two `G117` nolints in
  `internal/auth/store.go`: storing the credential is intended.
- Following the README's `k3s ctr images import` path on the cluster: the dev
  machine is not a node. Images come from `ghcr.io`.
- Giving a CI workload a new RPC by adding a permission: workloads pass the
  interceptor only for the procedures in `workloadProcedures` (`authn.go`),
  and each of those handlers must call its `ci_scope.go` check.
- Adding a package-level variable or calling `os.Exit` outside `main`.
- Changing the login's address without changing the GitHub OAuth app's
  callback URL to match: every new sign-in fails until both agree.
- Giving a service's `forge.yaml` a field (`run.domain`, `run.host`) before
  the forge-api that knows it is live: an older API rejects unknown fields
  and every deploy of that service fails.
- Extracting the render command out of `verifying.md` with grep and running
  it (2026-10-13). The match was an old setup block ending in
  `| kubectl apply -f -`: it applied a render without deploys, the Gateway
  lost its public listeners and every Forge service answered 404 for about
  ten minutes, until the approved render was applied again. Write the render
  command out yourself, send it to a file, and never run text taken from the
  docs.

## What an agent can and cannot reach

Knowing this file is not the same as having access.

- **On the owner's machine** (an editor's agent mode, a terminal agent): you
  have what the owner has: `kubectl` against production, the `forge` CLI with
  a live session, `gh`, the App key file on disk. Every rule above applies in
  full.
- **In the cloud** (for example GitHub's coding agent, which works on a
  runner): you can read the repo, build, test and open a pull request. You
  cannot reach the cluster, the LAN address of the API, or the App key, and
  you must not try to work around that. Leave rollouts and anything that needs
  the cluster to someone on the owner's machine, and say so in the pull
  request. `.github/workflows/copilot-setup-steps.yml` installs the toolchain
  for that environment.

## Keeping this true

- A change that alters a fact in `AGENTS.md`, `docs/agents/status.md` or
  `docs/agents/verifying.md` updates that file **in the same pull request**.
  After a rollout, `verifying.md` gets the new image, the new render command
  and what was checked.
- A change to what users see also updates `docs/forge/` in
  `showmino/techdocs` (see "Every feature is documented in techdocs" above).
  `README.md` here stays the reference next to the code.
- Do not copy these rules into a tool's own file or memory and edit them
  there. If a tool needs a file of its own, make it a pointer to this one.
- If you find something here that is no longer true, fix it, or tell the
  owner. Do not work around it silently.
- When the owner sets a new rule, it goes into "Rules" above, with the reason
  if they gave one.
