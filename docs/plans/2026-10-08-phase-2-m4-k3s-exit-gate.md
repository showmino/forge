# Phase 2, Milestone 4: k3s Rollout and the Phase 2 Exit Gate

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Run the Phase 2 forge-api on the k3s demo cluster as the `mino-forge` GitHub App, extend the CLI smoke tests to templates and `create`, and pass the Phase 2 exit gate there: a service scaffolded with `forge create` passes its own CI and appears in the catalog.

**Architecture:** `deploy/k3s/forge.yaml.tmpl` gains the App settings (ID, org, template repo) and mounts the App key from a Secret `forge-github-app`; `render.sh` emits them only when `FORGE_GITHUB_APP_ID` is set, so stock installs without an App keep working. The image is built from `main` and pushed to ghcr; the cluster pulls it with the existing `mino-ghcr` secret. `cli-smoke.sh` gets non-destructive template and create cases; the exit gate itself is a single real create against the cluster's API, checked and cleaned up by hand.

**Tech Stack:** k3s 1.37, MetalLB, kubectl, Docker/ghcr, Dex, the forge CLI, `gh`.

**Spec:** `docs/specs/2026-10-08-phase-2-templates-design.md` section 6 milestone 4 and section 4 "Testing" (end to end on k3s). Design doc `docs/index.md` (Phase 2 exit gate). Cluster facts: `.claude/skills/forge/verifying.md` section 3.

## Global Constraints

- Branch `phase-2-m4` from up-to-date `main`; `git branch --show-current` before every commit. Gate: `make build test lint` (Go 1.27, golangci-lint v2.14.0). I push, open the PR and merge after CI is green (owner rule).
- Touch only the `forge` namespace. Dex and forge-api stay on the LAN LoadBalancer IPs `10.150.1.220` / `10.150.1.221`.
- The App key never leaves the machine except into the Secret `forge-github-app` (key `private-key.pem`); never print it, never commit it, never put it in an env var or ConfigMap. Mounted read-only with mode 0440 and the pod's `fsGroup: 65532`, so the non-root API (uid 65532) can read it and nothing else in the container needs to.
- **Owner confirmation needed before:** creating the Secret, deploying (rolling out) the new image, and the real `forge create` of the exit gate. Everything before those steps is read-only or local.
- Keep `deploy/k3s` working without an App (`FORGE_GITHUB_APP_ID` unset → no Secret, no App flags; templates and create report "not configured").
- Commit after every task with the trailer `Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>`.

## Review Focus

1. **Rendering without an App** (`FORGE_GITHUB_APP_ID` unset) must produce exactly the milestone-1 manifests: no Secret volume, no App env. Pinned in Task 1.
2. **A missing Secret with the App configured** must stop the pod from starting (clear event), not start an API that half works. Pinned in Task 1 (Secret volume not optional) and observed in Task 3.
3. **The 128 MiB memory limit** vs milestone 2's 100 MiB tree cap: raise to 256 MiB so a large release cannot OOM-kill the API. Pinned in Task 1.
4. **The in-memory catalog** is emptied by every rollout: the smoke script must not assume earlier state (it registers `payments` itself). Pinned in Task 2.
5. **The exit gate's repo** must be deleted afterwards and its catalog entry removed, even if its CI fails. Pinned in Task 4.

---

### Task 0: Branch

- [ ] **Step 1**

```bash
git switch main && git pull --ff-only && git switch -c phase-2-m4 && git branch --show-current
```

Expected: `phase-2-m4`.

---

### Task 1: Manifests with the GitHub App

**Files:**
- Modify: `deploy/k3s/forge.yaml.tmpl` (forge-api Deployment), `deploy/k3s/render.sh`
- Test: `deploy/k3s/render_test.sh` (create)

**Interfaces:**
- Produces: `render.sh` reads `FORGE_GITHUB_APP_ID` (optional), `FORGE_GITHUB_ORG` (default `showmino`), `FORGE_TEMPLATES_REPO` (default `showmino/forge-templates`). Lines of the template that end in `#app` are kept (without the marker) only when `FORGE_GITHUB_APP_ID` is set.

- [ ] **Step 1: Write the failing test** — `deploy/k3s/render_test.sh`:

```sh
#!/usr/bin/env sh
# Checks render.sh output with and without a GitHub App. Run: sh deploy/k3s/render_test.sh
set -eu
dir=$(dirname "$0")
fail=0
check() { # check <description> <command...>
  desc=$1; shift
  if "$@"; then echo "ok   $desc"; else echo "FAIL $desc"; fail=1; fi
}

without=$(sh "$dir/render.sh" --lb 10.150.1.220 10.150.1.221)
with=$(FORGE_GITHUB_APP_ID=5239131 sh "$dir/render.sh" --lb 10.150.1.220 10.150.1.221)

check "no App: no App settings"       sh -c "! printf '%s' \"\$1\" | grep -q -e FORGE_GITHUB_APP -e forge-github-app" _ "$without"
check "no App: no #app markers"        sh -c "! printf '%s' \"\$1\" | grep -q '#app'" _ "$without"
check "App: id"                        sh -c "printf '%s' \"\$1\" | grep -q 'FORGE_GITHUB_APP_ID, value: \"5239131\"'" _ "$with"
check "App: key file"                  sh -c "printf '%s' \"\$1\" | grep -q 'FORGE_GITHUB_APP_KEY_FILE, value: /etc/forge/github-app/private-key.pem'" _ "$with"
check "App: org and template repo"     sh -c "printf '%s' \"\$1\" | grep -q 'FORGE_GITHUB_ORG, value: showmino' && printf '%s' \"\$1\" | grep -q 'FORGE_TEMPLATES_REPO, value: showmino/forge-templates'" _ "$with"
check "App: secret volume, read-only" sh -c "printf '%s' \"\$1\" | grep -q 'secretName: forge-github-app' && printf '%s' \"\$1\" | grep -q 'readOnly: true'" _ "$with"
check "App: no #app markers"           sh -c "! printf '%s' \"\$1\" | grep -q '#app'" _ "$with"
check "App: key readable by the API" sh -c "printf '%s' \"\$1\" | grep -q 'fsGroup: 65532' && printf '%s' \"\$1\" | grep -q 'defaultMode: 0440'" _ "$with"
check "memory limit 256Mi"             sh -c "printf '%s' \"\$1\" | grep -q 'limits: {memory: 256Mi}'" _ "$with"
check "valid YAML for kubectl"         sh -c "printf '%s' \"\$1\" | kubectl apply --dry-run=client -f - >/dev/null" _ "$with"
check "bad App id refused"             sh -c "! FORGE_GITHUB_APP_ID='5; rm' sh \"$dir/render.sh\" --lb 10.150.1.220 10.150.1.221 >/dev/null 2>&1"
exit $fail
```

- [ ] **Step 2: Run it to verify it fails**

Run: `sh deploy/k3s/render_test.sh`
Expected: the "App:" and "memory limit" checks FAIL; "no App" checks pass.

- [ ] **Step 3: Implement** — in `deploy/k3s/forge.yaml.tmpl`, in the forge-api container, replace the `env:` list and add a volume mount, and add a pod volume, so the container reads:

```yaml
          env:
            - {name: FORGE_OIDC_ISSUER, value: "http://${DEX_HOST}/dex"}
            - {name: FORGE_OIDC_CLIENT_ID, value: forge-cli}
            - {name: FORGE_LOG_FORMAT, value: json}
            - {name: FORGE_GITHUB_APP_ID, value: "${FORGE_GITHUB_APP_ID}"} #app
            - {name: FORGE_GITHUB_APP_KEY_FILE, value: /etc/forge/github-app/private-key.pem} #app
            - {name: FORGE_GITHUB_ORG, value: ${FORGE_GITHUB_ORG}} #app
            - {name: FORGE_TEMPLATES_REPO, value: ${FORGE_TEMPLATES_REPO}} #app
          volumeMounts: #app
            - {name: github-app, mountPath: /etc/forge/github-app, readOnly: true} #app
```

add `fsGroup: 65532` to the forge-api pod's `securityContext` (after `runAsGroup: 65532`, unconditionally: secret files are root-owned, and the group lets the non-root API read a 0440 key), change `limits: {memory: 128Mi}` of forge-api to `limits: {memory: 256Mi}` (template trees are cached in memory, up to 100 MiB each), and add under the pod `spec:` (same level as `containers:`):

```yaml
      volumes: #app
        - name: github-app #app
          secret: {secretName: forge-github-app, defaultMode: 0440} #app
```

Add to the header comment: `#   ${FORGE_GITHUB_APP_ID} etc.: see render.sh; lines ending in #app exist only with an App.`

In `deploy/k3s/render.sh`, before the final pipeline add:

```sh
app_id=${FORGE_GITHUB_APP_ID:-}
org=${FORGE_GITHUB_ORG:-showmino}
templates_repo=${FORGE_TEMPLATES_REPO:-showmino/forge-templates}
case $app_id in
  *[!0-9]*) echo "invalid FORGE_GITHUB_APP_ID: $app_id" >&2; exit 2 ;;
esac
case $org$templates_repo in
  *[!A-Za-z0-9./_-]*) echo "invalid FORGE_GITHUB_ORG or FORGE_TEMPLATES_REPO" >&2; exit 2 ;;
esac
if [ -n "$app_id" ]; then
  app='s/ *#app$//'   # keep App lines, drop the marker
else
  app='/#app$/d'      # no App: drop them
fi
```

and add to the `sed` at the end (before the image substitution) `-e "$app" -e "s|\${FORGE_GITHUB_APP_ID}|$app_id|g" -e "s|\${FORGE_GITHUB_ORG}|$org|g" -e "s|\${FORGE_TEMPLATES_REPO}|$templates_repo|g"`, so every `#app` line is handled before variables are filled. Extend the usage comment at the top with:

```sh
# With the Forge GitHub App (templates and 'forge create'): create the Secret
# first, then render with its App ID:
#   kubectl -n forge create secret generic forge-github-app --from-file=private-key.pem=<key.pem>
#   FORGE_GITHUB_APP_ID=5239131 FORGE_API_IMAGE=... deploy/k3s/render.sh --lb <dex-ip> <api-ip> | kubectl apply -f -
```

- [ ] **Step 4: Run the test**

Run: `sh deploy/k3s/render_test.sh && sh deploy/k3s/render.sh 192.168.1.50.nip.io | kubectl apply --dry-run=client -f - >/dev/null && echo ingress-ok`
Expected: every line `ok`, then `ingress-ok`.

- [ ] **Step 5: Commit**

```bash
git add deploy/k3s && git commit -m "deploy/k3s: run forge-api as the GitHub App when FORGE_GITHUB_APP_ID is set

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 2: Smoke tests for templates and create

**Files:**
- Modify: `.claude/skills/forge/scripts/cli-smoke.sh`

**Interfaces:**
- Consumes: the cluster API with the App (Task 3) or any API with templates configured.

- [ ] **Step 1: Add cases** — insert before `echo; echo "#### output and global flags"`:

```sh
echo; echo "#### templates and create (no side effects)"
t 0 "template list" -- f template list
t 0 "template list json" -- f template list -o json
t 0 "template get go-grpc" -- f template get go-grpc
t 3 "template get missing" -- f template get nope
t 3 "template get unknown version" -- f template get go-grpc@v0.0.1
t 0 "create dry run" -- f create smoke-dry --template go-grpc --team platform --dry-run
t 6 "create name taken on GitHub" -- f create forge-templates --template go-grpc --team platform --dry-run
t 6 "create name taken in catalog" -- f create payments --template go-grpc --team platform --dry-run
t 2 "create bad parameter" -- f create smoke-dry --template go-grpc --team platform --set port=http --dry-run
t 2 "create unknown parameter" -- f create smoke-dry --template go-grpc --team platform --set nope=1 --dry-run
t 2 "create without --team (no tty)" -- f create smoke-dry --template go-grpc
```

(`payments` is registered by the script's own setup line, so the catalog conflict does not depend on earlier runs.)

- [ ] **Step 2: Run against a local API with templates and the App** (no side effects: dry runs only)

```bash
make build
bin/forge-api --insecure-dev-token --addr :18082 --github-app-id 5239131 \
  --github-app-key-file mino-forge.2026-10-08.private-key.pem --github-org showmino > /tmp/forge-smoke.log 2>&1 &
sleep 1
bin/forge context set smoke-local --api http://localhost:18082
FORGE_TOKEN=dev bash .claude/skills/forge/scripts/cli-smoke.sh bin/forge smoke-local > /tmp/smoke.log; tail -1 /tmp/smoke.log; grep -A5 FAIL /tmp/smoke.log
kill %1; bin/forge context delete smoke-local 2>/dev/null || true
```

Expected: the template/create cases pass. (The auth cases that log out and back in through Dex fail against an `--insecure-dev-token` API; that is expected here, and Task 3 runs the full script against the cluster with Dex.) If `forge context delete` does not exist, remove the `smoke-local` entry from `~/.config/forge/config.yaml` by hand.

- [ ] **Step 3: Commit**

```bash
git add .claude/skills/forge/scripts/cli-smoke.sh && git commit -m "cli-smoke: template and create cases

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 3: Roll out to k3s (owner confirms first)

**Files:** none in the repo.

- [ ] **Step 1: Build and push the image from this branch's code**

```bash
IMG=ghcr.io/showmino/forge-api:$(git rev-parse --short HEAD)
docker build --build-arg COMMIT=$(git rev-parse HEAD) -t $IMG . && docker push $IMG
```

- [ ] **Step 2: The Secret comes from 1Password** (owner decision): the owner creates the item `forge-github-app` in the vault `Kube` with a field `github_app_private_key` holding the PEM. Rendering with `FORGE_ONEPASSWORD_ITEM=vaults/Kube/items/forge-github-app` adds a `OnePasswordItem`, and the cluster's 1Password operator creates the Secret `forge-github-app`. After applying (Step 3), check without printing the key:

```bash
kubectl -n forge get secret forge-github-app -o jsonpath='{.data}' | python3 -I -c 'import json,sys; print(sorted(json.load(sys.stdin)))'
```

Expected: a list containing `github_app_private_key`.

- [ ] **Step 3: Deploy**

```bash
FORGE_GITHUB_APP_ID=5239131 FORGE_ONEPASSWORD_ITEM=vaults/Kube/items/forge-github-app FORGE_API_IMAGE=$IMG \
  sh deploy/k3s/render.sh --lb 10.150.1.220 10.150.1.221 | kubectl apply -f -
kubectl -n forge rollout status deploy/forge-api --timeout=180s
kubectl -n forge logs deploy/forge-api | tail -3
```

Expected: rollout succeeds; logs show `forge-api listening`, no "templates are not configured" line.

- [ ] **Step 4: Check templates through the cluster and run the smoke script**

```bash
bin/forge context use k3s
python3 -I .claude/skills/forge/scripts/approve-device-login.py bin/forge --context k3s
bin/forge template list
bash .claude/skills/forge/scripts/cli-smoke.sh bin/forge k3s > /tmp/smoke-k3s.log; tail -1 /tmp/smoke-k3s.log; grep -A5 FAIL /tmp/smoke-k3s.log
```

Expected: `go-grpc  v1.0.0 …`; the smoke script reports `0 failed`.

---

### Task 4: The Phase 2 exit gate on k3s (owner confirms first)

- [ ] **Step 1: Create**

```bash
NAME=forge-e2e-$(date +%Y%m%d)
bin/forge --context k3s create $NAME --template go-grpc --team platform
bin/forge --context k3s service get $NAME -o json | grep -E '"name"|"repoUrl"'
```

Expected: five `✓` lines, `services/<name> created`, and the catalog entry with `repoUrl`.

- [ ] **Step 2: Its CI passes**

```bash
sleep 10; gh run watch -R showmino/$NAME --exit-status "$(gh run list -R showmino/$NAME -L1 --json databaseId -q '.[0].databaseId')"
```

Expected: `test` and `image` green. This is the exit gate.

- [ ] **Step 3: Clean up, whatever Step 2 showed**

Delete the catalog entry (`bin/forge --context k3s service delete $NAME --yes`) and the repo with an App installation token (JWT from the key with `openssl`, installation `169296924`, `DELETE /repos/showmino/$NAME` → 204, then revoke the token → 204), as in milestone 3. `gh repo view showmino/$NAME` must report it gone. Tell the owner the ghcr package `forge-e2e-<date>` remains.

---

### Task 5: Docs and wrap-up

**Files:**
- Modify: `.claude/skills/forge/verifying.md` (section 3), `.claude/skills/forge/SKILL.md` (status), `README.md` (k3s section), `docs/index.md` (Phase 2 row: done)

- [ ] **Step 1:** In `verifying.md` section 3, add the App Secret command, the `FORGE_GITHUB_APP_ID=5239131` render line, and the note that every rollout empties the in-memory catalog. In `SKILL.md`, mark Phase 2 done (milestones 1–4, exit gate passed on k3s on the date of Task 4) and list the deferred minors from milestones 1–3 as the follow-up backlog. In `README.md`'s k3s section, add the two App lines. In `docs/index.md`, mark Phase 2 done in the delivery table the way Phase 1 is marked.

- [ ] **Step 2: Gate, PR, merge**

```bash
make build test lint
git add -A .claude README.md docs && git commit -m "docs: Phase 2 done; k3s with the GitHub App

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
git push -u origin phase-2-m4
gh pr create --base main --title "Phase 2 milestone 4: k3s rollout and the exit gate" --body-file <prepared body>
```

Then wait for CI and merge.
