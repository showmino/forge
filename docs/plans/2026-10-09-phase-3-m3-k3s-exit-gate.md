# Phase 3 Milestone 3: k3s and the Exit Gate Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Preview environments run on the k3s demo cluster, and the Phase 3 exit gate passes there. The gate: a new service goes from `forge create` to a running preview environment, answering at its URL, in under 10 minutes.

**Architecture:**
- **Manifests:** `deploy/k3s/render.sh` gains `FORGE_PREVIEW_IP`. When it is set, the output adds `previews.yaml.tmpl`, which holds four things:
  - forge-api's ServiceAccount, with the pull secret;
  - a ClusterRole for exactly the calls `internal/server/env_cluster.go` makes;
  - a Role that reads only the pull secret;
  - an Istio Gateway API `Gateway` on a MetalLB IP.
  It also switches on forge-api's `FORGE_PREVIEW*` settings and its ServiceAccount, through lines marked `#previews` in `forge.yaml.tmpl`.
- **Template:** `go-grpc` v1.1.0 in `showmino/forge-templates` adds `run:` to the skeleton's `forge.yaml` and makes the server listen on `$PORT`.
- **Run on the cluster:** the code is rolled out to k3s, then the gate is run and timed.

**Tech Stack:** POSIX sh (`render.sh`), Kubernetes RBAC, Gateway API v1 (Istio 1.30 `istio` GatewayClass, Gateway API bundle v1.6.1), MetalLB 0.14.5, Go 1.27 (forge), Go 1.24 (go-grpc skeleton, unchanged).

**Spec:** `docs/specs/2026-10-08-phase-3-environments-design.md`: section 5 (Deploy), section 6 (go-grpc v1.1.0), section 7 (exit gate), section 8 (milestone 3, needs from the owner). Milestone 2 is merged (PR #12). Read `internal/server/env_cluster.go` for the API calls the ClusterRole must allow.

## Global Constraints

- **Cluster facts** (checked read-only on 2026-10-09):
  - MetalLB pool 10.150.1.200-250. Forge uses .220 (Dex), .221 (API) and **.222 (previews Gateway)**; .222 is free.
  - The annotation is `metallb.universe.tf/loadBalancerIPs`.
  - The pull secret `forge/mino-ghcr` keys its login as `ghcr.io`.
  - Istio has no `discoverySelectors` and there is no PeerAuthentication.
- **Gateway:**
  - name `forge-previews`, namespace `forge`, class `istio`;
  - one HTTP listener on port 80 with hostname `*.preview.<ip>.nip.io`;
  - `allowedRoutes` from namespaces labelled `app.kubernetes.io/managed-by: forge`.
- **forge-api settings, with previews on:**
  - `FORGE_PREVIEWS=1`, `FORGE_PREVIEW_DOMAIN=preview.<ip>.nip.io`, `FORGE_PREVIEW_PULL_SECRET=forge/<pull secret>`;
  - the gateway stays at its default `forge/forge-previews`;
  - `serviceAccountName: forge-api`.
- **ClusterRole: least privilege for the calls env_cluster.go makes.** It allows:
  - namespaces: get, list, create, patch, delete;
  - secrets, services, resourcequotas, limitranges: create;
  - pods: list;
  - deployments: get, create;
  - httproutes: create.
  Reading the pull secret is a namespaced Role on `forge` with `resourceNames: [<pull secret>]`. There is no cluster-wide get on secrets and no wildcard verbs.
- **render.sh:** previews need a GitHub App ID (`FORGE_GITHUB_APP_ID`). `FORGE_PREVIEW_IP` must be digits and dots, and `FORGE_PREVIEW_PULL_SECRET` (default `mino-ghcr`) must be a DNS-1123 name. Without `FORGE_PREVIEW_IP` the output is byte-identical to today's.
- **go-grpc v1.1.0:**
  - The skeleton's `forge.yaml` gains `run: {port: <port param>, health: /healthz}`.
  - The server listens on `:$PORT` when PORT is set, otherwise on `:<port param>`.
  - The image tag stays `ghcr.io/<owner>/<service>:<sha7>`.
- **Exit gate:** `forge create` followed by `forge env create`, timed by `time`, must finish READY in **under 10 minutes**. Then `curl` must get 200 from `/healthz`, and a Greet answer, at the URL.
- **Owner confirms first:** the forge-templates PR merge and its `v1.1.0` tag (Task 1), the k3s rollout (Task 3), and the exit gate, which creates a real repo (Task 4).
  - The standing push/PR/merge rule covers `showmino/forge` only.
  - Deleting the gate's repo needs the owner, because the permission check blocked it in Phase 2.
- **Never print or commit** the App key (`*.pem`) or the pull secret's content.
- **Gate:** before each forge commit that touches code or deploy files, run `make build test lint` and `sh deploy/k3s/render_test.sh`, with `PATH=$HOME/.local/go/bin:$HOME/go/bin:$PATH` (Go 1.27.2).

## Review Focus

1. **Rendering without `FORGE_PREVIEW_IP` must be byte-identical to `main`.** Any change to existing installs is a regression. Covered by Task 2's `render_test.sh` check "no previews: output unchanged".
2. **forge-api with previews on must still pull its own image.** Switching to a new ServiceAccount drops the default SA's `imagePullSecrets` patch. The new SA must carry the pull secret. Covered by Task 2's check "previews: SA has the pull secret" and verified in Task 3's rollout.
3. **The ClusterRole must not be broader than env_cluster.go needs.** That means no wildcard verbs or resources and no cluster-wide secret read. Covered by Task 2's checks "no wildcard", "no secret get in the ClusterRole" and "Role is limited to the pull secret".
4. **The Gateway must actually get 10.150.1.222 and accept routes from preview namespaces only.** Covered by Task 2's render checks and by Task 3 Step 3 on the cluster: `Programmed=True` and address .222.
5. **A service made from go-grpc must answer on `$PORT` at `/healthz` over plain HTTP/1.1.** The probes and the Gateway depend on it. Covered by Task 1's `TestListenAddr` in the skeleton and by the gate's `curl` in Task 4.

---

### Task 0: Branch

- [ ] **Step 1:** `cd /home/mino/code/forge && git branch --show-current`. Expected: `phase-3-m3` (created from `main` at `5ef06a8`). If not, run `git checkout main && git pull && git checkout -b phase-3-m3`.

---

### Task 1: go-grpc v1.1.0 (`run:` and `$PORT`)

**Repo:** `~/code/forge-templates` (`showmino/forge-templates`; `main` at `4c11278`, tag `v1.0.0`).

**Files (all under `go-grpc/skeleton/`):**
- Modify: `forge.yaml`, `cmd/{% .Service.Name %}/main.go`, `README.md`
- Create: `cmd/{% .Service.Name %}/main_test.go`

The skeleton uses `{% %}` delimiters. `{% .Params.port %}` is the template's `port` parameter (int, default 8080), and `{% .Service.Name %}` is the service id.

- [ ] **Step 1: Branch.** `cd ~/code/forge-templates && git checkout main && git pull && git checkout -b go-grpc-v1.1.0`

- [ ] **Step 2: Write the failing test** `go-grpc/skeleton/cmd/{% .Service.Name %}/main_test.go`:

```go
package main

import "testing"

func TestListenAddr(t *testing.T) {
	env := map[string]string{}
	getenv := func(k string) string { return env[k] }
	if got := listenAddr(getenv); got != ":{% .Params.port %}" {
		t.Errorf("without PORT: %q", got)
	}
	env["PORT"] = "9999"
	if got := listenAddr(getenv); got != ":9999" {
		t.Errorf("with PORT=9999: %q", got)
	}
}
```

- [ ] **Step 3: Run lint and the render build to see it fail.** From `~/code/forge-templates`:

```bash
export PATH=$HOME/.local/go/bin:$HOME/go/bin:$PATH
R=$(mktemp -d)
/home/mino/code/forge/bin/forge template lint . --render-dir "$R"
(cd "$R/go-grpc" && make tools test)
```

Expected: the lint passes, but `make test` fails with `undefined: listenAddr`. Build `bin/forge` from forge `main` first if it's missing: `cd /home/mino/code/forge && make build`.

- [ ] **Step 4: Implement.** In `main.go`, replace

```go
	addr := flag.String("addr", ":{% .Params.port %}", "listen address")
```

with

```go
	addr := flag.String("addr", listenAddr(os.Getenv), "listen address (default :$PORT when PORT is set)")
```

and add after `main`:

```go
// listenAddr is ":$PORT" when PORT is set, as in Forge preview environments,
// and the template's port otherwise.
func listenAddr(getenv func(string) string) string {
	if p := getenv("PORT"); p != "" {
		return ":" + p
	}
	return ":{% .Params.port %}"
}
```

  Append to the skeleton's `forge.yaml`, after the `repo:` line, at the top level and not indented under `service`:

```yaml
run:
  port: {% .Params.port %}
  health: /healthz
```

  In the skeleton's `README.md`, after the CI sentence, add:

```
`forge env create <name>` runs that image in a preview environment; the
server listens on `$PORT` there (`run:` in `forge.yaml`).
```

- [ ] **Step 5: See it pass.** Rerun Step 3's commands, and also run `make lint` in the render. Expected: lint is clean, `make tools test lint` passes, and `grep -A2 '^run:' "$R/go-grpc/forge.yaml"` shows `port: 8080` and `health: /healthz`.

- [ ] **Step 6: Commit and push the branch, then open a PR** (on that repo's own branch):

```bash
git add go-grpc
git commit -m "go-grpc: run settings in forge.yaml and listen on \$PORT (v1.1.0)"
git push -u origin go-grpc-v1.1.0
gh pr create -R showmino/forge-templates --base main --title "go-grpc v1.1.0: run: and \$PORT" --body "Adds run: {port, health} to the skeleton's forge.yaml and makes the server listen on \$PORT when set, for Forge preview environments (Phase 3)."
gh pr checks -R showmino/forge-templates --watch
```

Expected: the repo's `lint` workflow passes. It builds forge from `main`, which already knows `run:`, lints and renders every template, then runs `make tools test lint` on each.

- [ ] **Step 7 (owner confirms): Merge and tag.**

```bash
gh pr merge -R showmino/forge-templates --merge
git checkout main && git pull
git tag -a v1.1.0 -m "go-grpc v1.1.0: run: and \$PORT" && git push origin v1.1.0
```

Expected: `git tag` lists `v1.0.0 v1.1.0`. forge-api picks up the new tag within its 1-minute tag cache.

---

### Task 2: k3s manifests for previews

**Files:**
- Create: `deploy/k3s/previews.yaml.tmpl`
- Modify: `deploy/k3s/forge.yaml.tmpl`, `deploy/k3s/render.sh`, `deploy/k3s/render_test.sh`
- Create: `deploy/k3s/testdata/lb.golden`, `deploy/k3s/testdata/lb-app.golden`, `deploy/k3s/testdata/section.sh`

**Interfaces:**
- Produces render variables: `FORGE_PREVIEW_IP` (turns previews on) and `FORGE_PREVIEW_PULL_SECRET` (default `mino-ghcr`). Lines marked `#previews` in `forge.yaml.tmpl` exist only with previews on.

- [ ] **Step 1: Save today's output as goldens**, before editing any template. They pin Review Focus 1, which says rendering without previews must not change:

```bash
mkdir -p deploy/k3s/testdata
sh deploy/k3s/render.sh --lb 10.150.1.220 10.150.1.221 > deploy/k3s/testdata/lb.golden
FORGE_GITHUB_APP_ID=5239131 sh deploy/k3s/render.sh --lb 10.150.1.220 10.150.1.221 > deploy/k3s/testdata/lb-app.golden
```

- [ ] **Step 2: Write the failing checks.** Create `deploy/k3s/testdata/section.sh`. It prints one manifest kind's section; the checks run in `sh -c`, which cannot see the test script's functions, so they source this file:

```sh
# section <kind> <manifests>: the lines from "kind: <kind>" to the next "---".
section() {
  printf '%s\n' "$2" | awk -v k="kind: $1" '$0 == k {on=1} on && /^---/ {on=0} on'
}
```

  In `render_test.sh`, after the line `onepw=$(...)`, add:

```sh
prev=$(FORGE_GITHUB_APP_ID=5239131 FORGE_PREVIEW_IP=10.150.1.222 sh "$dir/render.sh" --lb 10.150.1.220 10.150.1.221)
sec=". \"$dir/testdata/section.sh\";"
```

  Before `exit $fail`, add (`grep -F` matches fixed strings, so `*` needs no escaping):

```sh
check "no previews: output unchanged"       sh -c "sh \"$dir/render.sh\" --lb 10.150.1.220 10.150.1.221 | cmp -s - \"$dir/testdata/lb.golden\""
check "no previews, App: output unchanged"  sh -c "FORGE_GITHUB_APP_ID=5239131 sh \"$dir/render.sh\" --lb 10.150.1.220 10.150.1.221 | cmp -s - \"$dir/testdata/lb-app.golden\""
check "previews: forge-api settings"        sh -c "printf '%s' \"\$1\" | grep -qF 'FORGE_PREVIEWS, value: \"1\"' && printf '%s' \"\$1\" | grep -qF 'FORGE_PREVIEW_DOMAIN, value: preview.10.150.1.222.nip.io' && printf '%s' \"\$1\" | grep -qF 'FORGE_PREVIEW_PULL_SECRET, value: forge/mino-ghcr'" _ "$prev"
check "previews: forge-api service account" sh -c "printf '%s' \"\$1\" | grep -qF 'serviceAccountName: forge-api'" _ "$prev"
check "previews: SA has the pull secret"    sh -c "$sec section ServiceAccount \"\$1\" | grep -qF -- '- name: mino-ghcr'" _ "$prev"
check "previews: gateway on the IP"         sh -c "$sec section Gateway \"\$1\" | grep -qF 'metallb.universe.tf/loadBalancerIPs: 10.150.1.222' && section Gateway \"\$1\" | grep -qF 'hostname: \"*.preview.10.150.1.222.nip.io\"' && section Gateway \"\$1\" | grep -qF 'gatewayClassName: istio'" _ "$prev"
check "previews: routes only from forge namespaces" sh -c "$sec section Gateway \"\$1\" | grep -qF 'from: Selector' && section Gateway \"\$1\" | grep -qF 'app.kubernetes.io/managed-by: forge'" _ "$prev"
check "previews: no wildcard in RBAC"       sh -c "$sec ! { section ClusterRole \"\$1\"; section Role \"\$1\"; } | grep -qF '*'" _ "$prev"
check "previews: no secret read in the ClusterRole" sh -c "$sec section ClusterRole \"\$1\" | grep -A1 -F 'secrets' | grep -qxF '    verbs: [create]'" _ "$prev"
check "previews: Role limited to the pull secret" sh -c "$sec section Role \"\$1\" | grep -qF 'resourceNames: [mino-ghcr]'" _ "$prev"
check "previews: no #previews markers"      sh -c "! printf '%s' \"\$1\" | grep -qF '#previews'" _ "$prev"
check "previews: valid YAML for kubectl"    sh -c "printf '%s' \"\$1\" | kubectl apply --dry-run=client -f - >/dev/null" _ "$prev"
check "previews need an App"                sh -c "! FORGE_PREVIEW_IP=10.150.1.222 sh \"$dir/render.sh\" --lb 10.150.1.220 10.150.1.221 >/dev/null 2>&1"
check "bad preview IP refused"              sh -c "! FORGE_GITHUB_APP_ID=1 FORGE_PREVIEW_IP='1.2.3.4;x' sh \"$dir/render.sh\" --lb 10.150.1.220 10.150.1.221 >/dev/null 2>&1"
check "bad pull secret refused"             sh -c "! FORGE_GITHUB_APP_ID=1 FORGE_PREVIEW_IP=10.150.1.222 FORGE_PREVIEW_PULL_SECRET='A_b' sh \"$dir/render.sh\" --lb 10.150.1.220 10.150.1.221 >/dev/null 2>&1"
```

  If a quoting detail in these one-liners trips `sh`, keep each check's meaning and simplify its quoting, for example with a small shell function per check in `testdata/`. Say so in the report.

- [ ] **Step 3:** `sh deploy/k3s/render_test.sh`. Expected: the new `previews:` checks FAIL, and the two `output unchanged` checks pass because nothing has changed yet.

- [ ] **Step 4: Write `deploy/k3s/previews.yaml.tmpl`**

```yaml
# Preview environments: forge-api's ServiceAccount and cluster permissions, and
# the LAN Gateway previews are reached through. render.sh adds this when
# FORGE_PREVIEW_IP is set.
#   ${PREVIEW_IP}   MetalLB IP of the Gateway, e.g. 10.150.1.222
#   ${PULL_SECRET}  dockerconfigjson Secret in namespace forge, e.g. mino-ghcr
apiVersion: v1
kind: ServiceAccount
metadata:
  name: forge-api
  namespace: forge
imagePullSecrets:
  - name: ${PULL_SECRET}
---
# Exactly the calls internal/server/env_cluster.go makes. Kubernetes RBAC
# cannot limit them to Forge's own namespaces; forge-api enforces that itself
# (owned() in env_cluster.go).
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: forge-api-previews
rules:
  - apiGroups: [""]
    resources: [namespaces]
    verbs: [get, list, create, patch, delete]
  - apiGroups: [""]
    resources: [secrets, services, resourcequotas, limitranges]
    verbs: [create]
  - apiGroups: [""]
    resources: [pods]
    verbs: [list]
  - apiGroups: [apps]
    resources: [deployments]
    verbs: [get, create]
  - apiGroups: [gateway.networking.k8s.io]
    resources: [httproutes]
    verbs: [create]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: forge-api-previews
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: forge-api-previews
subjects:
  - {kind: ServiceAccount, name: forge-api, namespace: forge}
---
# forge-api reads the pull secret to copy it into previews and to check images.
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: forge-api-pull-secret
  namespace: forge
rules:
  - apiGroups: [""]
    resources: [secrets]
    resourceNames: [${PULL_SECRET}]
    verbs: [get]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: forge-api-pull-secret
  namespace: forge
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: forge-api-pull-secret
subjects:
  - {kind: ServiceAccount, name: forge-api, namespace: forge}
---
# LAN only: Istio deploys this Gateway as forge-previews-istio with a
# LoadBalancer Service, which MetalLB gives ${PREVIEW_IP}.
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: forge-previews
  namespace: forge
spec:
  gatewayClassName: istio
  infrastructure:
    annotations:
      metallb.universe.tf/loadBalancerIPs: ${PREVIEW_IP}
  listeners:
    - name: http
      protocol: HTTP
      port: 80
      hostname: "*.preview.${PREVIEW_IP}.nip.io"
      allowedRoutes:
        namespaces:
          from: Selector
          selector:
            matchLabels:
              app.kubernetes.io/managed-by: forge
```

- [ ] **Step 5: Mark the forge-api lines.** In `deploy/k3s/forge.yaml.tmpl`:
  - Add to the header comment: `Lines with a trailing "previews" marker comment exist only when render.sh gets FORGE_PREVIEW_IP.`
  - In the forge-api pod spec, add a line directly above `securityContext:` (pod level, same indentation):

```yaml
      serviceAccountName: forge-api #previews
```

  - In the forge-api `env:` list, after the `FORGE_TEMPLATES_REPO` line:

```yaml
            - {name: FORGE_PREVIEWS, value: "1"} #previews
            - {name: FORGE_PREVIEW_DOMAIN, value: preview.${PREVIEW_IP}.nip.io} #previews
            - {name: FORGE_PREVIEW_PULL_SECRET, value: forge/${PULL_SECRET}} #previews
```

- [ ] **Step 6: `render.sh`.**
  - Add to the header comment, after the 1Password lines:

```sh
# Preview environments (forge env) need the App too, plus the IP of their
# Gateway and the pull secret in namespace forge (default mino-ghcr):
#   FORGE_PREVIEW_IP=10.150.1.222 FORGE_GITHUB_APP_ID=5239131 ... | kubectl apply -f -
```

  - After the 1Password validation:

```sh
preview_ip=${FORGE_PREVIEW_IP:-}
pull_secret=${FORGE_PREVIEW_PULL_SECRET:-mino-ghcr}
case $preview_ip in
  *[!0-9.]*) echo "invalid FORGE_PREVIEW_IP: $preview_ip" >&2; exit 2 ;;
esac
case $pull_secret in
  ''|*[!a-z0-9.-]*) echo "invalid FORGE_PREVIEW_PULL_SECRET: $pull_secret" >&2; exit 2 ;;
esac
if [ -n "$preview_ip" ] && [ -z "$app_id" ]; then
  echo "FORGE_PREVIEW_IP needs FORGE_GITHUB_APP_ID" >&2; exit 2
fi
if [ -n "$preview_ip" ]; then
  previews='s/ *#previews$//'
else
  previews='/#previews$/d'
fi
```

  - In the `{ ... }` block, after the 1Password `fi`:

```sh
  if [ -n "$preview_ip" ]; then
    echo "---"
    cat "$dir/previews.yaml.tmpl"
  fi
```

  - Add `-e "$previews"` after `-e "$app"`, and `-e "s|\${PREVIEW_IP}|$preview_ip|g" -e "s|\${PULL_SECRET}|$pull_secret|g"` to the `sed` call.

- [ ] **Step 7:** `sh deploy/k3s/render_test.sh`. Expected: every check is `ok`. Then validate against the real API server without changing anything:

```bash
FORGE_GITHUB_APP_ID=5239131 FORGE_ONEPASSWORD_ITEM=vaults/Kube/items/forge-github-app FORGE_PREVIEW_IP=10.150.1.222 \
  FORGE_API_IMAGE=ghcr.io/showmino/forge-api:dryrun sh deploy/k3s/render.sh --lb 10.150.1.220 10.150.1.221 \
  | kubectl apply --dry-run=server -f - | tail -8
```

  Expected: `serviceaccount/forge-api created (server dry run)`, the ClusterRole, ClusterRoleBinding, Role and RoleBinding created, and `gateway.gateway.networking.k8s.io/forge-previews created (server dry run)`.

- [ ] **Step 8: Commit.**

```bash
make build test lint && sh deploy/k3s/render_test.sh
git add deploy/k3s
git commit -m "deploy/k3s: preview environments behind FORGE_PREVIEW_IP"
```

---

### Task 3: Roll out to k3s (owner confirms first)

**Files:** none in the repo.

- [ ] **Step 1: Build and push the image** from this branch:

```bash
IMG=ghcr.io/showmino/forge-api:$(git rev-parse --short HEAD)
docker build -q --build-arg COMMIT=$(git rev-parse HEAD) -t $IMG . && docker push $IMG
```

- [ ] **Step 2: Apply.**

```bash
FORGE_GITHUB_APP_ID=5239131 FORGE_ONEPASSWORD_ITEM=vaults/Kube/items/forge-github-app FORGE_PREVIEW_IP=10.150.1.222 FORGE_API_IMAGE=$IMG \
  sh deploy/k3s/render.sh --lb 10.150.1.220 10.150.1.221 | kubectl apply -f -
kubectl -n forge rollout status deploy/forge-api --timeout=180s
kubectl -n forge logs deploy/forge-api | head -5 | cut -c1-200
```

Expected: the rollout succeeds, and the forge-api pod pulls its image through the new ServiceAccount's pull secret. The logs show `forge-api listening` and do not show "preview environments are off". If the pod is `ErrImagePull`, the SA lacks the pull secret: see Review Focus 2.

- [ ] **Step 3: The Gateway.**

```bash
kubectl -n forge wait gateway/forge-previews --for=condition=Programmed --timeout=120s
kubectl -n forge get gateway forge-previews -o jsonpath='{.status.addresses[0].value}'; echo
kubectl -n forge get svc -l gateway.networking.k8s.io/gateway-name=forge-previews
curl -s -o /dev/null -w '%{http_code}\n' http://nothing.preview.10.150.1.222.nip.io/
```

Expected: `Programmed`, address `10.150.1.222`, and a `forge-previews-istio` Service with EXTERNAL-IP .222. `curl` answers 404, meaning the Gateway works and no route matches. If the address is not .222, check the generated Service's annotation; Istio copies `infrastructure.annotations` onto it.

- [ ] **Step 4: Permissions as forge-api sees them.**

```bash
for v in "create namespaces" "delete namespaces" "create deployments -n x" "create httproutes.gateway.networking.k8s.io -n x" "get secrets -n kube-system" "get secrets/mino-ghcr -n forge" "delete deployments -n x"; do
  printf '%-55s %s\n' "$v" "$(kubectl auth can-i $v --as=system:serviceaccount:forge:forge-api)"
done
```

Expected: yes, yes, yes, yes, **no**, yes, **no**.

- [ ] **Step 5: Smoke.**

```bash
bin/forge context use k3s
python3 -I .claude/skills/forge/scripts/approve-device-login.py bin/forge --context k3s
bin/forge env list
bash .claude/skills/forge/scripts/cli-smoke.sh bin/forge k3s > /tmp/claude-1000/-home-mino-code-forge/smoke-k3s.log; tail -1 /tmp/claude-1000/-home-mino-code-forge/smoke-k3s.log
```

Expected: `No environments.`, and `== N passed, 0 failed`. Use the scratchpad directory for the log if `/tmp/claude-1000/...` is not writable.

---

### Task 4: The Phase 3 exit gate (owner confirms first)

- [ ] **Step 1: Run and time it.**

```bash
NAME=gate-$(date +%m%d%H%M)
time (bin/forge --context k3s create $NAME --template go-grpc --team platform \
  && bin/forge --context k3s env create $NAME --service $NAME)
```

Expected:
- `forge create` shows five ✓ lines and `fetched template go-grpc@v1.1.0`.
- `forge env create` streams `resolve`, `image` (waiting, CI status), `namespace`, `apply`, `ready http://$NAME.preview.10.150.1.222.nip.io`, then prints the environment with `state ready`.
- **`real` under 10m.** That is the exit gate.

- [ ] **Step 2: It answers.**

```bash
URL=http://$NAME.preview.10.150.1.222.nip.io
curl -s -o /dev/null -w '%{http_code}\n' $URL/healthz
PKG=$(printf '%s' "$NAME" | tr -d -)
curl -s $URL/$PKG.v1.GreeterService/Greet -H 'Content-Type: application/json' -d '{"name":"gate"}'; echo
bin/forge --context k3s env get $NAME
kubectl -n preview-$NAME get deploy,svc,httproute,resourcequota,limitrange
```

Expected: `200`, and a JSON greeting containing `gate`. `env get` shows ready, the URL and the operation. The namespace has exactly the expected objects.

- [ ] **Step 3: Clean up, whatever Steps 1-2 showed.**

```bash
bin/forge --context k3s env destroy $NAME --yes
kubectl get ns preview-$NAME 2>&1 | tail -1          # NotFound
bin/forge --context k3s service delete $NAME --yes
```

The repo `showmino/$NAME` and its ghcr package stay for the owner to delete (`gh repo delete showmino/$NAME --yes`), unless the owner allows the App-token delete.

---

### Task 5: Docs, PR and merge

**Files:**
- Modify: `.claude/skills/forge/verifying.md` (section 3), `.claude/skills/forge/SKILL.md` (status), `README.md` (k3s section), `docs/index.md` (Phase 3 done), `docs/plans/2026-10-09-phase-3-m3-k3s-exit-gate.md` (tick the boxes)

- [ ] **Step 1: Docs.**
  - **`verifying.md` section 3:**
    - Add `FORGE_PREVIEW_IP=10.150.1.222` to the render command.
    - Add the Gateway checks from Task 3 Step 3 and the `can-i` table from Task 3 Step 4.
    - Add two symptom rows:
      - forge-api `ErrImagePull` after enabling previews: the ServiceAccount is missing the pull secret.
      - An env stuck in `image` with "registry: ...": the pull secret login is wrong or lacks package read.
  - **README k3s section:** add the `FORGE_PREVIEW_IP` line and one sentence: previews need the App and a free LoadBalancer IP for the Gateway. This fixes the dangling "(see the k3s section)" pointer.
  - **`SKILL.md`:** mark Phase 3 done: milestones 1-3, with the exit gate passed on k3s on the date of Task 4 and its time. Add the Phase 3 deferred minors as a follow-up backlog.
  - **`docs/index.md`:**
    - Mark the Phase 3 row `(done)`.
    - Change "Phases 1 and 2 are done; Phase 3 is next." to "Phases 1 to 3 are done; Phase 4 is next."

- [ ] **Step 2: Gate, PR, merge** (the standing rule covers `showmino/forge`):

```bash
make build test lint && sh deploy/k3s/render_test.sh
git add -A .claude README.md docs deploy
git commit -m "docs: Phase 3 done; previews on k3s"
git push -u origin phase-3-m3
gh pr create --base main --title "Phase 3 milestone 3: previews on k3s and the exit gate" --body-file <prepared body>
```

Wait for CI, then merge.
