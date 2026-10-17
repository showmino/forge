# Phase 8, Milestone 3: Cluster portability

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `render.sh` and the chart can expose Dex, forge-api, and a deployed service — including a public address — on a cluster that has none of MetalLB, Istio's Gateway API specifically, or a Cloudflare tunnel, proven on a throwaway minikube instance.

**Architecture:** Every MetalLB-specific, Istio-specific, or k3s-Traefik-specific literal becomes a parameter with a default that preserves today's showmino behavior exactly. Where the current design structurally requires knowing a LoadBalancer's IP before rendering (so it can be baked into a `nip.io` hostname), add a second, hostname-first path that skips the annotation entirely and lets the operator supply (or already control, e.g. via their own DNS) the hostname instead — the same shape `render.sh`'s existing bare-domain mode already uses for the k3s/Traefik case, generalized to a plain `LoadBalancer` Service.

**Tech Stack:** POSIX `sh` (`deploy/k3s/render.sh`), Go templates in YAML (`deploy/k3s/*.tmpl`), Kubernetes Gateway API.

**Spec:** `docs/specs/2026-10-17-phase-8-portability-design.md` (Milestone 3)

## Global Constraints

- ArgoCD stays the one GitOps engine (unchanged).
- TLS termination for a public, tunnel-free address is the platform team's own responsibility; this milestone issues no certificates and adds no cert-manager dependency.
- DNS-provider pluggability is out of scope; a tunnel-free public address is reached by whatever hostname/IP the operator already controls and points there themselves.
- No change to the production k3s cluster. Every new mode defaults to behaving exactly as today when its new inputs are left unset; the exit proof runs on a throwaway minikube instance, per the owner's go-ahead, never on k3s.

## Review Focus

- `render.sh` invoked exactly as it is today (`--lb <ip> <ip>`, a bare domain, with or without `FORGE_PREVIEW_IP`) must render byte-identical output to before this milestone — every new mode is additive, reachable only through a new flag or env var.
- The new MetalLB-free exposure mode, given a hostname that is not `*.nip.io`-shaped, must not silently fall back to computing a `nip.io` hostname from an IP it was never given.
- A cluster whose Gateway API implementation is not Istio (the new `gatewayClassName` parameter) must still get working `allowedRoutes` selectors and listeners — the generalization must change only the class name, not the routing rules a conformant Gateway API implementation needs.
- `FORGE_PREVIEW_IP` and the new MetalLB-free preview/deploy domain input must be refused together (exactly one address mechanism active at a time), not silently prioritized.
- CloudNativePG's `Cluster` must still work when `storageClass` is left unset (the cluster's own default StorageClass is used) as well as when it's set explicitly to a name that doesn't exist on the target cluster — the latter is Kubernetes' own error to surface, not something `render.sh` should mask.

---

### Task 1: a MetalLB-free exposure mode for Dex and forge-api

**Files:**
- Create: `deploy/k3s/cloud-lb.yaml.tmpl` (a copy of `deploy/k3s/loadbalancer.yaml.tmpl` with the `metallb.universe.tf/loadBalancerIPs` annotation removed from both Services, and the header comment rewritten to describe a plain cloud/cluster-assigned `LoadBalancer`)
- Modify: `deploy/k3s/render.sh:85-106` (the `case ${1:-}` that picks the exposure template), `:75-79` (`usage()`)
- Modify: `deploy/k3s/render_test.sh` (new checks), `deploy/k3s/testdata/*.golden` (a new golden)

**Interfaces:**
- Consumes: nothing new.
- Produces: a new `render.sh` invocation form, `render.sh --cloud-lb <dex-host> <api-host>`, alongside the existing `<domain>` and `--lb <dex-ip> <api-ip>` forms.

- [ ] **Step 1: Write the failing render_test.sh checks**

  Add to `deploy/k3s/render_test.sh`:
  ```sh
  cloudlb=$(sh "$dir/render.sh" --cloud-lb dex.example.test api.example.test)
  check "cloud-lb: no metallb annotation" sh -c "! printf '%s' \"\$1\" | grep -q 'metallb.universe.tf'" _ "$cloudlb"
  check "cloud-lb: plain LoadBalancer Services" sh -c "printf '%s' \"\$1\" | grep -q 'type: LoadBalancer' && printf '%s' \"\$1\" | grep -qF 'host: dex.example.test'" _ "$cloudlb"
  check "cloud-lb: rejects an IP where a hostname is expected" sh -c "! sh \"\$1\" --cloud-lb 10.0.0.1 api.example.test >/dev/null 2>&1" _ "$dir/render.sh"
  ```
  (The third check is a judgment call, not a hard requirement: if you decide an IP is a legitimate hostname-position value too — e.g. someone wants to reach Dex by bare IP — relax it; just don't let a value containing a path or scheme through silently.)

- [ ] **Step 2: Run `sh deploy/k3s/render_test.sh` to verify the new checks fail**

  Run: `sh deploy/k3s/render_test.sh`
  Expected: FAIL (the `--cloud-lb` case doesn't exist yet; `usage()` doesn't mention it).

- [ ] **Step 3: Create `cloud-lb.yaml.tmpl`**

  Copy `deploy/k3s/loadbalancer.yaml.tmpl`, remove the two `annotations: {metallb.universe.tf/loadBalancerIPs: ...}` blocks (and the now-empty `metadata.annotations` keys), and rewrite the header comment to explain that the cluster's own LoadBalancer implementation (a cloud provider, or `minikube tunnel`) assigns the address, and that `${DEX_HOST}`/`${API_HOST}` are whatever the operator will point at it afterward, not derived from an IP.

- [ ] **Step 4: Add the `--cloud-lb` case to `render.sh`**

  In the `case ${1:-}` block (`render.sh:85-106`), add a case alongside `--lb`:
  ```sh
  --cloud-lb)
    [ $# -eq 3 ] || usage
    dex_host=$2 api_host=$3
    for h in "$dex_host" "$api_host"; do
      case $h in *[!A-Za-z0-9.-]*) echo "invalid hostname: $h" >&2; exit 2 ;; esac
    done
    exposure=$dir/cloud-lb.yaml.tmpl
    ;;
  ```
  Update `usage()` (`render.sh:75-79`) to list the new form, and the top-of-file comment block to document it alongside the existing `--lb` documentation.

- [ ] **Step 5: Run `sh deploy/k3s/render_test.sh` to verify the new checks pass**

  Run: `sh deploy/k3s/render_test.sh`
  Expected: every check prints `ok`.

- [ ] **Step 6: Update the render goldens**

  Run: `make update-golden`, then inspect `git diff deploy/k3s/testdata` to confirm only new goldens appear (nothing existing changes).

- [ ] **Step 7: Run `make build test lint`**

  Run: `make build test lint`
  Expected: PASS.

- [ ] **Step 8: Commit**

  ```bash
  git add deploy/k3s/cloud-lb.yaml.tmpl deploy/k3s/render.sh deploy/k3s/render_test.sh deploy/k3s/testdata
  git commit -m "deploy/k3s: add a MetalLB-free LoadBalancer exposure mode for Dex and forge-api"
  ```

---

### Task 2: parameterize the Ingress controller class

**Files:**
- Modify: `deploy/k3s/ingress.yaml.tmpl:20,49` (`ingressClassName: traefik`)
- Modify: `deploy/k3s/render.sh` (a new `ingress_class` variable and its substitution)
- Modify: `deploy/k3s/render_test.sh`, `deploy/k3s/testdata/*.golden`

**Interfaces:**
- Consumes: nothing new.
- Produces: `FORGE_INGRESS_CLASS` env var (render.sh input), default `traefik`; new placeholder `${INGRESS_CLASS}`.

- [ ] **Step 1: Write the failing render_test.sh check**

  ```sh
  check "ingress class defaults to traefik" sh -c "sh \"\$1\" example.test | grep -q 'ingressClassName: traefik'" _ "$dir/render.sh"
  check "ingress class is overridable" sh -c "FORGE_INGRESS_CLASS=nginx sh \"\$1\" example.test | grep -q 'ingressClassName: nginx'" _ "$dir/render.sh"
  ```

- [ ] **Step 2: Run `sh deploy/k3s/render_test.sh` to verify the second check fails**

  Run: `sh deploy/k3s/render_test.sh`
  Expected: the first check already passes (today's literal default); the second FAILs (no such env var exists yet).

- [ ] **Step 3: Parameterize it**

  In `ingress.yaml.tmpl:20,49`, replace `ingressClassName: traefik` with `ingressClassName: ${INGRESS_CLASS}`. In `render.sh`, add `ingress_class=${FORGE_INGRESS_CLASS:-traefik}` near the other exposure-related variables, with the same safe-character validation the other class-name-shaped variables use, and add it to the `sed -e` chain.

- [ ] **Step 4: Run `sh deploy/k3s/render_test.sh` to verify both checks pass, update goldens, run `make build test lint`**

  Run: `sh deploy/k3s/render_test.sh && make update-golden && make build test lint`
  Expected: PASS; `git diff deploy/k3s/testdata` shows no change to existing goldens (the default is unchanged).

- [ ] **Step 5: Commit**

  ```bash
  git add deploy/k3s/ingress.yaml.tmpl deploy/k3s/render.sh deploy/k3s/render_test.sh deploy/k3s/testdata
  git commit -m "deploy/k3s: parameterize the Ingress class instead of assuming Traefik"
  ```

---

### Task 3: make the database's storage class optional

**Files:**
- Modify: `deploy/k3s/database.yaml.tmpl:15` (`storageClass: longhorn-retain`)
- Modify: `deploy/k3s/render.sh` (a new, optional `db_storage_class` variable)
- Modify: `deploy/k3s/render_test.sh`, `deploy/k3s/testdata/*.golden`
- Modify: `deploy/k3s/live.env` (add the explicit setting so showmino's render is unchanged)

**Interfaces:**
- Consumes: nothing new.
- Produces: `FORGE_DB_STORAGE_CLASS` env var (render.sh input), no default (omits the field, letting Kubernetes use the cluster's own default StorageClass); new placeholder block in `database.yaml.tmpl`.

- [ ] **Step 1: Write the failing render_test.sh checks**

  ```sh
  check "no storage class by default" sh -c "! FORGE_GITHUB_APP_ID=5239131 FORGE_RESOURCES=1 FORGE_RUNNER_IMAGE=x FORGE_DEPLOYS=1 FORGE_PREVIEW_IP=10.150.1.222 FORGE_DEPLOY_CHART_REVISION=abc sh \"\$1\" --lb 10.150.1.220 10.150.1.221 | grep -q storageClass" _ "$dir/render.sh"
  check "storage class is settable" sh -c "FORGE_DB_STORAGE_CLASS=longhorn-retain FORGE_GITHUB_APP_ID=5239131 FORGE_RESOURCES=1 FORGE_RUNNER_IMAGE=x FORGE_DEPLOYS=1 FORGE_PREVIEW_IP=10.150.1.222 FORGE_DEPLOY_CHART_REVISION=abc sh \"\$1\" --lb 10.150.1.220 10.150.1.221 | grep -q 'storageClass: longhorn-retain'" _ "$dir/render.sh"
  ```
  (Adjust the exact flags needed to reach the database/resources block to whatever `render_test.sh`'s existing resources-on tests already use — check `database.yaml.tmpl`'s own render.sh gating condition first; it may be unconditional rather than tied to `FORGE_RESOURCES`.)

- [ ] **Step 2: Run `sh deploy/k3s/render_test.sh` to verify the checks fail**

  Run: `sh deploy/k3s/render_test.sh`
  Expected: the first check FAILs (today's literal `longhorn-retain` is always present); the second FAILs (no such env var).

- [ ] **Step 3: Parameterize it**

  In `render.sh`, add `db_storage_class=${FORGE_DB_STORAGE_CLASS:-}` with the usual safe-character validation when non-empty. In `database.yaml.tmpl`, replace the literal `storageClass: longhorn-retain # the volumes outlive an accidental delete` line with a conditionally-present block driven by a new sed marker (matching the existing `#app`/`#deploys`/`#public` marker convention): wrap the line with a trailing marker, e.g. `#dbstorage`, and have `render.sh` strip lines marked `#dbstorage` when `db_storage_class` is empty (the same mechanism already used to strip `#app`/`#deploys`/etc. lines), substituting `${DB_STORAGE_CLASS}` into it when present.

- [ ] **Step 4: Run `sh deploy/k3s/render_test.sh` to verify both checks pass, update goldens, run `make build test lint`**

  Run: `sh deploy/k3s/render_test.sh && make update-golden && make build test lint`
  Expected: PASS. Inspect `git diff deploy/k3s/testdata`: existing goldens that render the database block will now show no `storageClass` line, since none of them currently set `FORGE_DB_STORAGE_CLASS` — this is an *intended* diff (document it in the commit message), not a regression, because no golden today represents showmino's real behavior with the new variable unset on purpose.

- [ ] **Step 5: Add the explicit setting to `live.env` so showmino's own render is unchanged**

  Add `FORGE_DB_STORAGE_CLASS=longhorn-retain` to `deploy/k3s/live.env`. Run `sh deploy/k3s/render-live.sh --check` and `git diff deploy/k3s/live/forge.yaml`: the diff must be empty (this one line keeps showmino's database exactly as it runs today).

- [ ] **Step 6: Commit**

  ```bash
  git add deploy/k3s/database.yaml.tmpl deploy/k3s/render.sh deploy/k3s/render_test.sh deploy/k3s/testdata deploy/k3s/live.env deploy/k3s/live/forge.yaml
  git commit -m "deploy/k3s: make CloudNativePG's storage class a setting, not a literal"
  ```

  Note: the `live.env`/`live/forge.yaml` change here still needs the owner's OK to actually apply, per `AGENTS.md` — this task prepares and proves the (empty) diff, it does not apply anything.

---

### Task 4: parameterize the previews/deploys Gateway's class and address mechanism

**Files:**
- Modify: `deploy/k3s/previews.yaml.tmpl:81,84,89,99,110` (`gatewayClassName: istio`, the MetalLB annotation, the three `nip.io` hostnames)
- Modify: `deploy/k3s/forge.yaml.tmpl:148` (`FORGE_PREVIEW_DOMAIN`'s value)
- Modify: `deploy/k3s/render.sh` (new `gateway_class` and `gateway_domain` variables, validation that `FORGE_PREVIEW_IP` and the new domain input are mutually exclusive)
- Modify: `deploy/k3s/render_test.sh`, `deploy/k3s/testdata/*.golden`

**Interfaces:**
- Consumes: nothing new.
- Produces: `FORGE_GATEWAY_CLASS` env var, default `istio` (parameterizes `previews.yaml.tmpl`'s `gatewayClassName`). `FORGE_GATEWAY_DOMAIN` env var, no default, mutually exclusive with `FORGE_PREVIEW_IP` — when set, the previews/staging/production Gateway listeners are reached under `*.preview.<domain>` / `*.staging.<domain>` / `*.production.<domain>` with no MetalLB annotation, and forge-api's own `FORGE_PREVIEW_DOMAIN` becomes `preview.<domain>` the same way it's computed from the IP today.

- [ ] **Step 1: Write the failing render_test.sh checks**

  ```sh
  gwclass=$(FORGE_GITHUB_APP_ID=5239131 FORGE_GATEWAY_CLASS=envoy-gateway FORGE_PREVIEW_IP=10.150.1.222 sh "$dir/render.sh" --lb 10.150.1.220 10.150.1.221)
  check "gateway class is overridable" sh -c "printf '%s' \"\$1\" | grep -q 'gatewayClassName: envoy-gateway'" _ "$gwclass"
  gwdomain=$(FORGE_GITHUB_APP_ID=5239131 FORGE_GATEWAY_DOMAIN=minikube.test sh "$dir/render.sh" --cloud-lb dex.example.test api.example.test)
  check "gateway domain: no metallb annotation" sh -c "! printf '%s' \"\$1\" | grep -q 'metallb.universe.tf'" _ "$gwdomain"
  check "gateway domain: preview hostname" sh -c "printf '%s' \"\$1\" | grep -qF 'hostname: \"*.preview.minikube.test\"'" _ "$gwdomain"
  check "gateway domain: forge-api's own FORGE_PREVIEW_DOMAIN" sh -c "printf '%s' \"\$1\" | grep -qF 'FORGE_PREVIEW_DOMAIN, value: preview.minikube.test'" _ "$gwdomain"
  check "FORGE_PREVIEW_IP and FORGE_GATEWAY_DOMAIN together are refused" sh -c "! FORGE_GITHUB_APP_ID=5239131 FORGE_PREVIEW_IP=10.150.1.222 FORGE_GATEWAY_DOMAIN=minikube.test sh \"\$1\" --lb 10.150.1.220 10.150.1.221 >/dev/null 2>&1" _ "$dir/render.sh"
  ```

- [ ] **Step 2: Run `sh deploy/k3s/render_test.sh` to verify the new checks fail**

  Run: `sh deploy/k3s/render_test.sh`
  Expected: FAIL (none of this exists yet).

- [ ] **Step 3: Add `gateway_class` and make `gatewayClassName` a parameter**

  In `render.sh`, add `gateway_class=${FORGE_GATEWAY_CLASS:-istio}` (safe-character validation, same style as `org`). In `previews.yaml.tmpl:81`, replace `gatewayClassName: istio` with `gatewayClassName: ${GATEWAY_CLASS}`. Add it to the `sed -e` chain.

- [ ] **Step 4: Add `gateway_domain`, and compute one `preview_base` value from whichever address input is given**

  In `render.sh`, add `gateway_domain=${FORGE_GATEWAY_DOMAIN:-}` with a domain-shaped validation (reuse the character class already used for the bare-domain `ingress.yaml.tmpl` case). Add the mutual-exclusion check: `if [ -n "$preview_ip" ] && [ -n "$gateway_domain" ]; then echo "set only one of FORGE_PREVIEW_IP and FORGE_GATEWAY_DOMAIN" >&2; exit 2; fi`. Compute one `preview_base` shell variable: `preview.$preview_ip.nip.io` when `$preview_ip` is set (today's behavior, unchanged), or `$gateway_domain` directly when that's set instead — wait, note the asymmetry: today's base (before the `preview.`/`staging.`/`production.` prefix) is `$preview_ip.nip.io`, not `preview.$preview_ip.nip.io` — compute `preview_base=$preview_ip.nip.io` (IP mode) or `preview_base=$gateway_domain` (domain mode), and prefix `preview.`/`staging.`/`production.` onto `$preview_base` at each of the three hostname lines and at the `FORGE_PREVIEW_DOMAIN` line, instead of today's inline `preview.${PREVIEW_IP}.nip.io`.

  Replace `previews.yaml.tmpl:89,99,110`'s `"*.preview.${PREVIEW_IP}.nip.io"` / `"*.staging.${PREVIEW_IP}.nip.io"` / `"*.production.${PREVIEW_IP}.nip.io"` with `"*.preview.${PREVIEW_BASE}"` / `"*.staging.${PREVIEW_BASE}"` / `"*.production.${PREVIEW_BASE}"`. Replace `forge.yaml.tmpl:148`'s `value: preview.${PREVIEW_IP}.nip.io` with `value: preview.${PREVIEW_BASE}`. Add `${PREVIEW_BASE}` to the `sed -e` chain, set from `$preview_base`.

  Make the Gateway's `infrastructure.annotations` block (`previews.yaml.tmpl:82-84`) conditional: present (with `metallb.universe.tf/loadBalancerIPs: ${PREVIEW_IP}`) only in IP mode, omitted entirely in domain mode — reuse the same line-stripping-by-marker mechanism Task 3 introduced for `#dbstorage` (a new marker, e.g. `#previewip`, stripped when `$gateway_domain` is set instead of `$preview_ip`).

- [ ] **Step 5: Run `sh deploy/k3s/render_test.sh` to verify every check passes**

  Run: `sh deploy/k3s/render_test.sh`
  Expected: PASS, including every pre-existing check (today's IP-mode output must be byte-identical to before).

- [ ] **Step 6: Update goldens, run `make build test lint`**

  Run: `make update-golden && make build test lint`
  Expected: PASS; `git diff deploy/k3s/testdata` shows no change to any existing golden.

- [ ] **Step 7: Commit**

  ```bash
  git add deploy/k3s/previews.yaml.tmpl deploy/k3s/forge.yaml.tmpl deploy/k3s/render.sh deploy/k3s/render_test.sh deploy/k3s/testdata
  git commit -m "deploy/k3s: parameterize the previews/deploys Gateway's class and let a domain replace the MetalLB IP"
  ```

---

### Task 5: minikube exit proof — the golden path with none of MetalLB, Istio, or the tunnel

**Files:** none (verification only).

**Interfaces:** none.

- [ ] **Step 1: Start minikube and install a Gateway API implementation**

  Run: `minikube start`. Install a lightweight Gateway API controller — Envoy Gateway is a reasonable default choice for a throwaway test cluster (a single `helm install`/manifest apply, no dependency on Istio's wider control plane); if that proves awkward in practice, substitute whatever Gateway API implementation is simplest to stand up in minikube and set `FORGE_GATEWAY_CLASS` to its class name in the next step. This choice is yours to make; it does not change any code from Tasks 1-4.

- [ ] **Step 2: Render Forge for minikube**

  Pick a domain you control for this test (for example a hostname under a domain you already have, or entries you'll add to `/etc/hosts` pointing at whatever address `minikube tunnel` assigns). Render with the new modes:
  ```sh
  FORGE_GITHUB_APP_ID=<a real App ID> FORGE_GITHUB_ORG=<org> FORGE_TEMPLATES_REPO=<org>/forge-templates \
    FORGE_GATEWAY_CLASS=<the class you installed> FORGE_GATEWAY_DOMAIN=<your test domain> \
    FORGE_DEPLOYS=1 FORGE_DEPLOY_CHART_REVISION=$(git rev-parse HEAD) \
    sh deploy/k3s/render.sh --cloud-lb dex.<your test domain> forge.<your test domain> > /tmp/minikube-forge.yaml
  ```
  Read the file before applying it (never pipe a render straight to `kubectl apply` unseen, per `AGENTS.md`'s own "mistakes that have been made here"). Apply it to the minikube context specifically — confirm `kubectl config current-context` says `minikube` first.

- [ ] **Step 3: Run `minikube tunnel`, point your test hostnames at the assigned addresses**

  Run `minikube tunnel` in the background; `kubectl -n forge get svc` shows the `EXTERNAL-IP`s MetalLB would otherwise have assigned. Point `dex.<your test domain>` and `forge.<your test domain>` (and the Gateway's address, for `*.preview./staging./production.<your test domain>`) at those IPs — `/etc/hosts` entries are enough for a local proof; real DNS is not required (per the design's non-goals: TLS/DNS for real public reachability is explicitly the platform team's concern, not something this proof needs to demonstrate end to end).

- [ ] **Step 4: Run the golden path**

  `forge auth login` against the minikube instance (approve the device code), then `forge create <throwaway-name> --template go-grpc --team <a real team>`, watch its CI, then `forge deploy status <throwaway-name>` and reach it at `http://<throwaway-name>.staging.<your test domain>` (or `https://` if your Gateway terminates TLS itself — not required). This exercises creation, staging deploy, and the new tunnel-free routing all at once, matching how earlier phases' exit gates worked (`docs/agents/verifying.md`'s Phase 3/4 gates).

  If a real GitHub App/repo isn't available for this throwaway test, substitute a `helm install` of a prebuilt test image directly against the chart's `staging`/`production` values shape (as Task 4 of the M2 plan did for the secrets proof) to demonstrate the routing mechanism in isolation, and say so plainly when recording the result — that proves the infrastructure, not the full CLI-to-CI-to-deploy path.

- [ ] **Step 5: Clean up and record the result**

  `minikube delete`. Record in the Phase 8 status update (`AGENTS.md`'s "Keeping this true") what was actually proven on minikube, on this date, which Gateway API implementation was used, and which parts (if any) were simulated via `helm install` rather than the full CLI path — not on the production k3s cluster.
