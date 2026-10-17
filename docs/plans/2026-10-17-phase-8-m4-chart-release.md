# Phase 8, Milestone 4: Helm/OCI chart release

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `deploy/argocd/chart` becomes a versioned Helm/OCI artifact published to `ghcr.io` on every release tag, and showmino's own ArgoCD `ApplicationSet` consumes it the same way any other adopter would — one supported path, instead of a path inside this git repo pinned by commit SHA.

**Architecture:** Add a chart packaging/push step to the existing `vX.Y.Z`-tag release workflow (`helm package` + `helm push` to an OCI reference, the same trigger goreleaser already runs on). Switch `deploy/k3s/deploys.yaml.tmpl`'s `ApplicationSet` chart source from `repoURL`/`path`/a git-SHA `targetRevision` to the OCI reference with a chart-semver `targetRevision`, and change `render.sh`'s validation of `FORGE_DEPLOY_CHART_REVISION` to match (a chart version, not a 40-character SHA; its "does this revision have `dns.yaml`" guard now checks the published chart instead of local git history).

**Tech Stack:** Helm CLI (chart packaging, OCI push/pull), GitHub Actions, POSIX `sh`.

**Spec:** `docs/specs/2026-10-17-phase-8-portability-design.md` (Milestone 4)

## Global Constraints

- ArgoCD stays the one GitOps engine (unchanged).
- No change to the production k3s cluster without the owner's explicit OK. This milestone prepares and proves the switch; applying it to showmino's own instance still goes through the normal rollout procedure in `docs/agents/verifying.md`.
- Helm chart versions are plain SemVer with no leading `v` (`0.20.0`, not `v0.20.0`); the OCI tag that results is that version string, which will visibly differ from the release's Git tag and the `forge-api` image tag (`v0.20.0`) by that one character. This is a Helm/OCI convention, not an inconsistency to engineer around.
- **Owner decision needed before Task 2/3 can be applied to production:** for another organization's ArgoCD to pull `ghcr.io/showmino/forge-chart` at all, that GHCR package must either be public, or showmino's ArgoCD (and anyone else's) needs registered pull credentials for it. Making the package public is the only option consistent with this phase's own goal ("any organization can take and use it") and is this plan's recommendation (Task 1, Step 6) — but package visibility is a GitHub setting change, which `AGENTS.md` treats as the owner's call ("settings on GitHub ... Ask first, every time"), so confirm it with the owner before relying on it, rather than assuming the recommendation stands as approval.

## Review Focus

- A tag push must publish the chart with a version that matches the release tag (minus the `v`), not the chart's own unrelated internal `Chart.yaml` default — a stale or mismatched version would silently make `targetRevision: vX.Y.Z` (or a bare `X.Y.Z`) fail to resolve.
- `render.sh` given a `FORGE_DEPLOY_CHART_REVISION` that is *not* a valid chart semver (for example the old 40-character SHA shape, left over from a script someone didn't update) must refuse clearly, not pass it through to a broken ArgoCD source.
- The dns.yaml-presence guard (`render.sh:332-333` today) must still refuse enabling `FORGE_PUBLIC_DOMAINS` against a chart version that predates `templates/dns.yaml`, now checked against the published OCI artifact instead of local git history — and must fail with a clear, actionable message if the registry can't be reached at all (a new failure mode this check didn't have before, since it was previously pure local git state).
- The AppProject's `sourceRepos` allow-list must name the OCI registry ArgoCD will actually pull from, in whatever form that ArgoCD version's Application CRD expects for an OCI Helm source — verify this against the ArgoCD version running on the cluster (`kubectl -n argocd get deploy argocd-repo-server -o jsonpath='{.spec.template.spec.containers[0].image}'`) and its own documentation, rather than assuming the shape below is exactly right; it is a best-effort draft, not a confirmed API contract.
- Once the git-commit chart source is gone, `https://github.com/${FORGE_REPO}.git` (added to the AppProject's `sourceRepos` by Phase 8 Milestone 1) becomes unused there and should be removed rather than left as dead allow-list clutter.

---

### Task 1: publish the chart on release

**Files:**
- Modify: `.github/workflows/release.yml` (a new step after the `goreleaser-action` step)
- Modify: `deploy/argocd/chart/Chart.yaml` (nothing to change here structurally — the version is overridden at package time, not edited in git; confirm this with a comment if it helps a future reader)

**Interfaces:**
- Consumes: nothing new.
- Produces: a published OCI artifact at `oci://ghcr.io/showmino/forge-chart`, tagged with the release's SemVer (no leading `v`).

- [ ] **Step 1: Add a Helm chart push step to the release workflow**

  After the `goreleaser/goreleaser-action@v6` step in `.github/workflows/release.yml`, add:
  ```yaml
      - name: Push the deploy chart
        run: |
          version=${GITHUB_REF_NAME#v}
          helm registry login ghcr.io -u "${{ github.actor }}" -p "${{ secrets.GITHUB_TOKEN }}"
          helm package deploy/argocd/chart --version "$version" --app-version "$version" -d /tmp
          helm push "/tmp/forge-service-$version.tgz" oci://ghcr.io/showmino
  ```
  (`forge-service` is `Chart.yaml`'s current `name:`; adjust the archive filename in the `helm push` line if you rename the chart as part of this task — Helm's package output is always `<name>-<version>.tgz`.) Install `helm` on the runner first if it isn't already on the `ubuntu-latest` image's `PATH` (check with `helm version` in a throwaway step, or add `- uses: azure/setup-helm@v4` before this step).

  Add `packages: write` to the job's `permissions:` block if it isn't already sufficient for OCI artifact pushes under the existing `GITHUB_TOKEN` (the image push already uses `packages: write`; an OCI Helm push to the same registry under the same token should need no more).

- [ ] **Step 2: Decide and record the chart's published name**

  Keep `forge-service` (today's `Chart.yaml` name), or rename it to something clearer for an external consumer (for example `forge`, matching the CLI and the overall project name) — this is yours to decide; if you rename it, update the `helm package`/`helm push` commands above and `Chart.yaml`'s `name:` field together, and carry the new name through every reference in Task 2.

- [ ] **Step 3: Test the packaging step locally (no push)**

  Run: `helm package deploy/argocd/chart --version 0.0.0-test --app-version 0.0.0-test -d /tmp && tar tzf /tmp/*-0.0.0-test.tgz`
  Expected: the archive lists `Chart.yaml`, `values.yaml`, and every file under `templates/`.

- [ ] **Step 4: Commit the workflow change**

  ```bash
  git add .github/workflows/release.yml
  git commit -m "release: publish the deploy chart as a versioned OCI artifact"
  ```

  This step alone has no effect until the next real `vX.Y.Z` tag push — the exit proof for this task is in Task 3.

- [ ] **Step 5: Confirm with the owner whether the GHCR package should be public**

  Before relying on any other organization (or even showmino's own ArgoCD, if Task 2/3 proceed) being able to pull this chart, confirm with the owner whether `ghcr.io/showmino/forge-chart` (or whatever name Step 2 settled on) should be a public package — the recommendation in this plan's Global Constraints, but a GitHub package-visibility setting is the owner's call per `AGENTS.md`. If they decline, Task 2/3 need registered OCI pull credentials for ArgoCD instead (out of scope for this plan as written; flag it back to the design if that's the answer).

---

### Task 2: switch the ApplicationSet's chart source to the published OCI artifact

**Files:**
- Modify: `deploy/k3s/deploys.yaml.tmpl:3,15-18,73-79` (the comment, the AppProject's `sourceRepos`, the ApplicationSet's first `sources` entry)
- Modify: `deploy/k3s/render.sh:167,174-177,332-333` (`FORGE_DEPLOY_CHART_REVISION`'s shape validation and the `dns.yaml`-presence guard)
- Modify: `deploy/k3s/render_test.sh`, `deploy/k3s/testdata/*.golden`

**Interfaces:**
- Consumes: the published chart from Task 1 (its name and OCI path).
- Produces: `FORGE_DEPLOY_CHART_REVISION` now takes a chart SemVer (for example `0.20.0`), not a 40-character commit SHA; everything else that reads this env var (none, per the earlier recon — it is only ever consumed by `deploys.yaml.tmpl`'s `targetRevision`) is unaffected.

- [ ] **Step 1: Write the failing render_test.sh checks**

  ```sh
  check "chart revision accepts a semver" sh -c "FORGE_GITHUB_APP_ID=5239131 FORGE_DEPLOYS=1 FORGE_PREVIEW_IP=10.150.1.222 FORGE_DEPLOY_CHART_REVISION=0.20.0 sh \"\$1\" --lb 10.150.1.220 10.150.1.221 | grep -q 'targetRevision: 0.20.0'" _ "$dir/render.sh"
  check "chart revision refuses the old SHA shape" sh -c "! FORGE_GITHUB_APP_ID=5239131 FORGE_DEPLOYS=1 FORGE_PREVIEW_IP=10.150.1.222 FORGE_DEPLOY_CHART_REVISION=2b84f5e1676aa92090e43737790655e6436cb070 sh \"\$1\" --lb 10.150.1.220 10.150.1.221 >/dev/null 2>&1" _ "$dir/render.sh"
  ```
  Keep (don't delete) `render_test.sh`'s existing checks that reference the chart revision in other contexts — update any that hardcode the old 40-character SHA example to a SemVer example instead, so the whole file stays internally consistent.

- [ ] **Step 2: Run `sh deploy/k3s/render_test.sh` to verify the first new check fails and the second already incidentally passes (for the wrong reason)**

  Run: `sh deploy/k3s/render_test.sh`
  Expected: the SemVer check FAILs (today's validation rejects anything that isn't exactly 40 hex characters); the SHA-rejection check may already pass today by coincidence rather than by the new rule — don't rely on that, Step 4 makes it pass for the right reason.

- [ ] **Step 3: Change the chart-revision validation to a SemVer shape**

  At `render.sh:174-177`, replace the 40-character-hex check with a plain SemVer check (`MAJOR.MINOR.PATCH`, optionally with a pre-release/build suffix — reuse an existing simple pattern rather than inventing a full SemVer grammar; the goal is to catch obvious mistakes like a leftover commit SHA or a `v`-prefixed value, not to validate every corner of the SemVer spec). Update the error message to say "a chart version, such as 0.20.0" instead of "a full commit SHA".

- [ ] **Step 4: Point the `dns.yaml`-presence guard at the published chart**

  Replace `render.sh:332-333`'s `git -C "$dir" cat-file -e "$chart_rev:deploy/argocd/chart/templates/dns.yaml"` with a check against the OCI artifact, for example:
  ```sh
  helm pull "oci://ghcr.io/showmino/forge-chart" --version "$chart_rev" -d "$tmpdir" 2>/dev/null &&
    tar tzf "$tmpdir"/*.tgz | grep -q 'templates/dns.yaml'
  ```
  (adjust the registry path to whatever Task 1 Step 2 settled on). Use a `mktemp -d` temp directory, cleaned up after the check. Update the error message: it must now say plainly that this check needs network access to `ghcr.io` and what to do if that's the actual failure (as opposed to the chart genuinely lacking the template) — don't let a registry outage masquerade as "your chart revision is too old."

- [ ] **Step 5: Update `deploys.yaml.tmpl`**

  Line 3's comment: `${DEPLOY_CHART_REVISION} forge commit the chart is taken from (pinned)` → describe it as the published chart's version instead.
  Lines 16-18 (AppProject `sourceRepos`): remove `https://github.com/${FORGE_REPO}.git` (no longer used once the chart source below changes) and add the OCI registry reference in whatever form ArgoCD's Application CRD expects (see this plan's Review Focus — verify against the cluster's actual ArgoCD version rather than trusting this draft blindly); keep `https://github.com/${FORGE_DEPLOY_REPO}.git` (the values repo is unaffected).
  Lines 73-79 (the ApplicationSet's first `sources` entry): replace the git `repoURL`/`path`/`targetRevision` triple with the OCI chart reference and the chart's SemVer `targetRevision`, keeping the `helm.releaseName`/`helm.valueFiles` block as-is (those configure how the chart is *rendered*, independent of where it came from).

- [ ] **Step 6: Run `sh deploy/k3s/render_test.sh` to verify both new checks pass**

  Run: `sh deploy/k3s/render_test.sh`
  Expected: PASS, including every pre-existing check.

- [ ] **Step 7: Update goldens, run `make build test lint`**

  Run: `make update-golden && make build test lint`
  Expected: PASS; inspect `git diff deploy/k3s/testdata` — every golden that renders the ApplicationSet's chart source changes shape (git fields → OCI fields); this is the intended effect of this task, not a regression to chase down.

- [ ] **Step 8: Commit**

  ```bash
  git add deploy/k3s/deploys.yaml.tmpl deploy/k3s/render.sh deploy/k3s/render_test.sh deploy/k3s/testdata
  git commit -m "deploy/k3s: source the chart from its published OCI release instead of a git commit"
  ```

---

### Task 3: exit proof

**Files:**
- Modify: `deploy/k3s/live.env` (change `FORGE_DEPLOY_CHART_REVISION` from a commit SHA to the matching chart SemVer)
- Modify: `deploy/k3s/live/forge.yaml` (via `render-live.sh`)

**Interfaces:**
- Consumes: Task 1's published chart, Task 2's render changes.
- Produces: nothing new; this task is the exit-gate check.

- [ ] **Step 1: Push a real tag and confirm the chart publishes**

  After Task 1 and Task 2 are merged, push the next `vX.Y.Z` release tag as usual (`AGENTS.md`: tagging a merged change needs no extra OK). Confirm the workflow's new step succeeded, then, from any machine with network access (no cluster needed):
  Run: `helm show chart oci://ghcr.io/showmino/forge-chart --version <the version>`
  Expected: prints the chart's metadata at that version, with no authentication error (if Task 1 Step 5's visibility decision was "public") or after `helm registry login` (if not).

- [ ] **Step 2: Confirm the owner's visibility decision actually took effect**

  If the package was meant to be public (Task 1, Step 5), confirm `helm show chart` in Step 1 worked with no login from an unauthenticated shell. If it still requires authentication, the package is still private — stop here and go back to the owner rather than silently proceeding with a rollout that would only work for showmino itself.

- [ ] **Step 3: Prepare showmino's own switch-over (prepare only; applying needs the owner's OK)**

  Set `FORGE_DEPLOY_CHART_REVISION` in `deploy/k3s/live.env` to the new chart's SemVer (matching the tag just pushed, minus the `v`). Run `sh deploy/k3s/render-live.sh --check`, then `kubectl diff -f deploy/k3s/live/forge.yaml` (read-only; per `AGENTS.md`, this alone needs no OK) and inspect it: it should show the `ApplicationSet`'s chart source changing from the git fields to the OCI fields, and nothing else.

- [ ] **Step 4: Show the owner the diff; apply only with their OK**

  Follow the normal rollout procedure in `docs/agents/verifying.md` exactly: show the diff, wait for the OK, server dry run, apply, verify every deployed service's `Application` is still `Synced`/`Healthy` on the new source, merge the pull request. Record what was checked in `verifying.md`, per `AGENTS.md`'s "Keeping this true".
