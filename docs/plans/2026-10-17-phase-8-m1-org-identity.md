# Phase 8, Milestone 1: Org identity is configuration, not code

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Remove every compiled-in `showmino` default from `forge-api`/the CLI/the chart so another GitHub organization can run its own Forge by configuration alone, with showmino's own production render behaving exactly as it does today.

**Architecture:** `cmd/forge-api`'s flags and `render.sh`'s shell variables both currently fall back to literal `showmino`-shaped values when their env var is unset. Remove those literals (making the values required, or deriving a sensible default from the org the operator already configured), fix the one chart template that hardcodes showmino's own repo URLs directly, and add the two lines `live.env` needs so showmino's render is unaffected.

**Tech Stack:** Go (`cmd/forge-api`, `internal/server`), POSIX `sh` (`deploy/k3s/render.sh`), Go templates in YAML (`deploy/k3s/*.tmpl`).

**Spec:** `docs/specs/2026-10-17-phase-8-portability-design.md` (Milestone 1)

## Global Constraints

- GitHub stays the identity/CI/SCM provider; ArgoCD stays the one GitOps engine (unchanged by this milestone).
- No change to the production k3s cluster without the owner's explicit OK. This milestone's own exit proof is a dry render/diff, not an apply.
- Additive only: `deploy/k3s/live.env` as it exists today, rendered by the code this milestone produces, must `kubectl diff` to nothing beyond the usual `tracking-id` annotation noise, after the two lines this plan adds to it.
- Exit bar is non-regression: showmino's own render stays correct. No second GitHub org/App is stood up in this milestone.

## Review Focus

- An operator sets `--github-app-id`/`FORGE_GITHUB_APP_ID` but leaves `--github-org` unset: `forge-api` must refuse to start with a clear message, not silently act as `showmino`'s instance. (Already enforced by `cmd/forge-api/templates.go:23`; the review here is that removing the literal default doesn't weaken or bypass that check.)
- An operator turns on `--deploys`/`FORGE_DEPLOYS=1` but leaves the GitOps repo unset: `forge-api` must refuse to start with a clear message naming the missing flag, not fall back to `showmino/forge-deploys`.
- An operator turns on `--previews`/`FORGE_PREVIEWS=1` but leaves the pull secret unset: `forge-api` must refuse to start with a clear message, not fall back to `forge/mino-ghcr`.
- `render.sh` invoked with no `FORGE_GITHUB_ORG` at all must exit 2 with a clear message, not silently render `showmino` into the manifest.
- Pure local development (`bin/forge-api --oidc-issuer ...` with no `--github-app-id`, as in `docs/agents/verifying.md` section 2) must keep starting successfully with no org configured at all — removing the default must not newly require `--github-org` where no GitHub-App-backed feature is in use.

---

### Task 1: `cmd/forge-api` and `internal/server` — no compiled-in showmino defaults

**Files:**
- Modify: `cmd/forge-api/main.go:66,69,74,78,94` (the five flag defaults)
- Modify: `internal/server/server.go:86-88` (the `GitHubOrg` fallback)
- Modify: every `internal/server/*_test.go` that constructs `server.Options{}` via `server.New(...)` without setting `GitHubOrg` and asserts something that resolves through the old default (found with the command in Step 5)
- Test: the existing `cmd/forge-api/templates_test.go` (`TestGitHubApp`), `cmd/forge-api/deploys_test.go` (`TestDeploys`), `cmd/forge-api/previews_test.go` (`TestPreviews`) already exercise the empty-org/empty-repo/empty-pull-secret error paths against the helper functions directly (`githubApp`, `deploys`, `previews`); this task does not change their logic, only the literal the caller passes when a flag is unset.

**Interfaces:**
- Consumes: nothing new.
- Produces: no new exported names. `cmd/forge-api/main.go`'s flag defaults for `--templates-repo`, `--deploy-repo`, `--cli-repo`, `--preview-pull-secret`, `--github-org` become `""` (previously `"showmino/forge-templates"`, `"showmino/forge-deploys"`, `"showmino/forge"`, `"forge/mino-ghcr"`, `"showmino"` respectively).

- [ ] **Step 1: Change the five flag defaults in `cmd/forge-api/main.go`**

  Lines 66, 69, 74, 78, 94: change the third argument of each `env(...)` call from its literal `showmino`-shaped string to `""`. Leave every other part of each `flag.String(...)` call (name, env var name, help text) unchanged except removing the now-false claim in the help text that the flag "defaults to showmino" where it says so (line 69's `--github-org` help text, and the top-of-file comment at lines 15-16 that documents the old default — update that comment to say these are required instead of defaulted).

- [ ] **Step 2: Remove the `GitHubOrg` fallback in `internal/server/server.go`**

  Delete the three lines at `internal/server/server.go:86-88`:
  ```go
  if o.GitHubOrg == "" {
      o.GitHubOrg = "showmino"
  }
  ```
  An empty `Options.GitHubOrg` now stays empty (valid when no GitHub-App-backed feature is configured, per the Review Focus above).

- [ ] **Step 3: Run `make build test lint`**

  Run: `make build test lint`
  Expected: the build and lint pass; some `internal/server` tests FAIL because they relied on the removed default (they construct `server.Options{}` without `GitHubOrg` and assert something — a repo URL, a team string — that only matched because of the old `"showmino"` fallback).

- [ ] **Step 4: Find every affected test**

  Run: `grep -rL 'GitHubOrg:' internal/server/*_test.go | xargs grep -l 'server.New('`
  Expected: the list of files to fix in the next step (the same list that `go test` step 3 just failed on, plus any that happened to already pass by coincidence of an empty org not mattering to that particular test).

- [ ] **Step 5: Add `GitHubOrg: "showmino"` to each affected test's `server.Options{}` literal**

  For each file the previous step listed, add `GitHubOrg: "showmino"` to the `server.Options{}` (or equivalent options struct literal) it builds before calling `server.New`. This is a literal test fixture choice (matching what the rest of that test file already assumes, e.g. `create_test.go`'s fake repo host checking `org != "showmino"`), not a behavior change.

- [ ] **Step 6: Run `make build test lint` again**

  Run: `make build test lint`
  Expected: PASS, no regressions.

- [ ] **Step 7: Commit**

  ```bash
  git add cmd/forge-api/main.go internal/server/server.go internal/server/*_test.go
  git commit -m "forge-api: remove compiled-in showmino defaults for org and repo flags"
  ```

---

### Task 2: `render.sh` and the chart templates — parameterize the remaining org/repo literals

**Files:**
- Modify: `deploy/k3s/render.sh:15-16,24-25,113-114,151` (the shell-level showmino defaults and their documenting comments)
- Modify: `deploy/k3s/forge.yaml.tmpl:146` area (add `FORGE_CLI_REPO`/`${FORGE_REPO}` under the `#app` marker, add `FORGE_DEPLOY_REPO`/`${FORGE_DEPLOY_REPO}` under the `#deploys` marker)
- Modify: `deploy/k3s/deploys.yaml.tmpl:15,17,18,55,73,80` (replace the six literal `showmino/forge`/`showmino/forge-deploys` references with `${FORGE_REPO}`/`${FORGE_DEPLOY_REPO}`)
- Modify: `deploy/k3s/render_test.sh` (the `sed` substitution list and any assertions pinned to the old literal defaults, e.g. the `"App: org and template repo"` check)
- Modify: `deploy/k3s/testdata/*.golden` (via `make update-golden`)
- Modify: `deploy/argocd/chart/testdata/*.golden` only if `make chart-test` finds a diff (it shouldn't: this task doesn't touch the chart itself, only `deploy/k3s`)

**Interfaces:**
- Consumes: nothing new.
- Produces: two new render.sh shell variables, `forge_repo` and `deploy_repo`, and two new template placeholders, `${FORGE_REPO}` and `${FORGE_DEPLOY_REPO}`, substituted the same way `${FORGE_GITHUB_ORG}` and `${FORGE_TEMPLATES_REPO}` already are (the `sed -e` chain around `render.sh:372-373`).

- [ ] **Step 1: Write the failing render_test.sh assertions first**

  Add to `deploy/k3s/render_test.sh`, alongside the existing `"App: org and template repo"` check:
  ```sh
  check "no FORGE_GITHUB_ORG: render.sh refuses" sh -c "! FORGE_GITHUB_APP_ID=5239131 sh \"\$1\" --lb 10.150.1.220 10.150.1.221 >/dev/null 2>&1" _ "$dir/render.sh"
  check "App: deploy repo defaults from org" sh -c "printf '%s' \"\$1\" | grep -q 'FORGE_DEPLOY_REPO, value: showmino/forge-deploys'" _ "$(FORGE_GITHUB_APP_ID=5239131 FORGE_DEPLOYS=1 FORGE_PREVIEW_IP=10.150.1.222 sh "$dir/render.sh" --lb 10.150.1.220 10.150.1.221)"
  check "App: cli repo defaults from org" sh -c "printf '%s' \"\$1\" | grep -q 'FORGE_CLI_REPO, value: showmino/forge'" _ "$with"
  check "deploys.yaml has no literal showmino" sh -c "! FORGE_GITHUB_APP_ID=5239131 FORGE_DEPLOYS=1 FORGE_PREVIEW_IP=10.150.1.222 FORGE_DEPLOY_CHART_REVISION=abc sh \"\$1\" --lb 10.150.1.220 10.150.1.221 2>/dev/null | grep -q showmino" _ "$dir/render.sh"
  ```
  (The last check asserts no *hardcoded* `showmino` remains in the output when a different org is given; keep an explicit `FORGE_GITHUB_ORG=someorg` variant of the same render alongside it to prove the substitution actually took effect, e.g. `| grep -q someorg/forge-deploys`.)

- [ ] **Step 2: Run `sh deploy/k3s/render_test.sh` to verify the new checks fail**

  Run: `sh deploy/k3s/render_test.sh`
  Expected: the four new checks print `FAIL` (org isn't required yet; `FORGE_DEPLOY_REPO`/`FORGE_CLI_REPO` don't exist yet; `deploys.yaml.tmpl` still has literal showmino text).

- [ ] **Step 3: Make `FORGE_GITHUB_ORG` required in `render.sh`**

  At `render.sh:113`, replace
  ```sh
  org=${FORGE_GITHUB_ORG:-showmino}
  ```
  with a required-variable form that exits 2 with a clear message when unset (matching the existing style of `FORGE_WRITER_GROUPS`'s required check at lines 132-144), e.g. `org=${FORGE_GITHUB_ORG:?FORGE_GITHUB_ORG is required}`. Update the usage comment at lines 15-16 to say `FORGE_GITHUB_ORG` is required, not defaulted.

- [ ] **Step 4: Derive `templates_repo`, `forge_repo`, `deploy_repo` from `$org` instead of a literal**

  At `render.sh:114`, change
  ```sh
  templates_repo=${FORGE_TEMPLATES_REPO:-showmino/forge-templates}
  ```
  to
  ```sh
  templates_repo=${FORGE_TEMPLATES_REPO:-$org/forge-templates}
  ```
  Add two new lines near it:
  ```sh
  forge_repo=${FORGE_REPO:-$org/forge}
  deploy_repo=${FORGE_DEPLOY_REPO:-$org/forge-deploys}
  ```
  Reuse the existing character-class validation at line 119 (currently checking `$org`/`$templates_repo`) to also cover `$forge_repo` and `$deploy_repo`.

- [ ] **Step 5: Require `FORGE_PREVIEW_PULL_SECRET` with no literal default**

  At `render.sh:151`, replace
  ```sh
  pull_secret=${FORGE_PREVIEW_PULL_SECRET:-mino-ghcr}
  ```
  with `pull_secret=${FORGE_PREVIEW_PULL_SECRET:?FORGE_PREVIEW_PULL_SECRET is required when previews are configured}` (only reached when previews are being rendered at all, matching the existing structure of that code block).

- [ ] **Step 6: Add `${FORGE_REPO}` and `${FORGE_DEPLOY_REPO}` to the `sed` substitution chain**

  In the long `sed -e` chain around `render.sh:372-373`, add `-e "s|\${FORGE_REPO}|$forge_repo|g" -e "s|\${FORGE_DEPLOY_REPO}|$deploy_repo|g"` alongside the existing `FORGE_GITHUB_ORG`/`FORGE_TEMPLATES_REPO` substitutions.

- [ ] **Step 7: Add the two new env vars to `forge.yaml.tmpl`**

  After `forge.yaml.tmpl:146` (`FORGE_TEMPLATES_REPO`, marked `#app`), add:
  ```yaml
              - {name: FORGE_CLI_REPO, value: ${FORGE_REPO}} #app
  ```
  After `forge.yaml.tmpl:150` (`FORGE_DEPLOYS`, marked `#deploys`), add:
  ```yaml
              - {name: FORGE_DEPLOY_REPO, value: ${FORGE_DEPLOY_REPO}} #deploys
  ```

- [ ] **Step 8: Replace the six literal showmino references in `deploys.yaml.tmpl`**

  - Line 15 (`description: Forge services, deployed by forge-api through showmino/forge-deploys`) → `through ${FORGE_DEPLOY_REPO}`.
  - Line 17 (`- https://github.com/showmino/forge.git`) → `- https://github.com/${FORGE_REPO}.git`.
  - Line 18 (`- https://github.com/showmino/forge-deploys.git`) → `- https://github.com/${FORGE_DEPLOY_REPO}.git`.
  - Line 55 (`repoURL: https://github.com/showmino/forge-deploys.git`) → `repoURL: https://github.com/${FORGE_DEPLOY_REPO}.git`.
  - Line 73 (`repoURL: https://github.com/showmino/forge.git`) → `repoURL: https://github.com/${FORGE_REPO}.git`.
  - Line 80 (`repoURL: https://github.com/showmino/forge-deploys.git`) → `repoURL: https://github.com/${FORGE_DEPLOY_REPO}.git`.

- [ ] **Step 9: Run `sh deploy/k3s/render_test.sh` to verify the new checks pass**

  Run: `sh deploy/k3s/render_test.sh`
  Expected: every check prints `ok`, including the four added in Step 1.

- [ ] **Step 10: Update the render goldens**

  Run: `make update-golden`
  Expected: `deploy/k3s/testdata/*.golden` updates to reflect the new `FORGE_CLI_REPO`/`FORGE_DEPLOY_REPO` lines; diff them by eye to confirm nothing else moved.

- [ ] **Step 11: Run the full test/lint suite**

  Run: `make build test lint`
  Expected: PASS.

- [ ] **Step 12: Commit**

  ```bash
  git add deploy/k3s/render.sh deploy/k3s/forge.yaml.tmpl deploy/k3s/deploys.yaml.tmpl deploy/k3s/render_test.sh deploy/k3s/testdata
  git commit -m "deploy/k3s: derive repo URLs from the configured org instead of hardcoding showmino"
  ```

---

### Task 3: `live.env` — the non-regression proof

**Files:**
- Modify: `deploy/k3s/live.env` (add two lines)
- Modify: `deploy/k3s/live/forge.yaml` (via `render-live.sh`, not by hand)

**Interfaces:**
- Consumes: Task 1 and Task 2's required flags/variables.
- Produces: nothing new; this task is the exit-gate check.

- [ ] **Step 1: Add the two now-required lines to `live.env`**

  Add, anywhere in the file (matching its existing flat `KEY=value` style):
  ```
  FORGE_GITHUB_ORG=showmino
  FORGE_PREVIEW_PULL_SECRET=mino-ghcr
  ```
  (`FORGE_TEMPLATES_REPO`, `FORGE_REPO`/`--cli-repo`, and `FORGE_DEPLOY_REPO` need no new line: their Task 2 defaults, `$org/forge-templates`, `$org/forge`, `$org/forge-deploys`, already resolve to showmino's exact existing values once `FORGE_GITHUB_ORG=showmino` is set.)

- [ ] **Step 2: Render and check**

  Run: `sh deploy/k3s/render-live.sh --check`
  Expected: passes (the committed `live/forge.yaml` already matches, or the command regenerates it — run `sh deploy/k3s/render-live.sh` without `--check` if it reports a diff, then re-run `--check`).

- [ ] **Step 3: Diff against the previous `live/forge.yaml`**

  Run: `git diff deploy/k3s/live/forge.yaml`
  Expected: the diff shows only the two new env var lines (`FORGE_GITHUB_ORG`, `FORGE_PREVIEW_PULL_SECRET`) added to the forge-api container spec, and the `FORGE_CLI_REPO`/`FORGE_DEPLOY_REPO` lines Task 2 added — nothing else changes. This is the milestone's exit bar: showmino's render is otherwise byte-identical.

- [ ] **Step 4: Commit**

  ```bash
  git add deploy/k3s/live.env deploy/k3s/live/forge.yaml
  git commit -m "deploy/k3s: make live.env's org and repo config explicit instead of relying on removed defaults"
  ```

  Note: this commit changes files that are rolled out to the production cluster. Per `AGENTS.md`, actually applying it still needs the owner's OK through the normal rollout procedure in `docs/agents/verifying.md` — this task only prepares and proves the diff; it does not apply anything.
