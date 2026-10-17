# Phase 8, Milestone 2: Secrets become a backend-neutral contract

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A service's `run.secrets` entry in `forge.yaml` no longer hard-requires 1Password's `vaults/<vault>/items/<item>` addressing. 1Password keeps working exactly as today (zero migration for existing services); a generic `<group>/<item>` form becomes valid too, for an instance that runs a different secret-populating mechanism.

**Architecture:** The addressing shape (`internal/manifest/options.go`'s `itemPath` regex and the `Vault()`/`ItemName()` parsing it feeds) widens to accept either shape; the governed-group concept (`--secret-vaults`/`SecretDeniedItems`) stays as today's mechanism, generalized to read whichever shape was given. The chart's Kubernetes-level env wiring (`_helpers.tpl`'s `forge.env`) already only needs a Secret name and key, so it is untouched; only the thing that *creates* that Secret (`templates/onepassword.yaml`) becomes one of possibly several, selected by a new chart value `secretBackend` (default `onepassword`, preserving today's behavior).

**Tech Stack:** Go (`internal/manifest`, `internal/server`, `cmd/forge-api`), Helm (`deploy/argocd/chart`).

**Spec:** `docs/specs/2026-10-17-phase-8-portability-design.md` (Milestone 2)

## Global Constraints

- The backend is chosen once per running Forge instance (a chart value), never per secret or per service.
- No second, production-grade secret-backend integration ships in this milestone. The seam is proven with a stand-in (a hand-made Kubernetes Secret), not a maintained adapter for a real second backend.
- Every existing forge.yaml with `item: vaults/<vault>/items/<item>` must keep validating and deploying exactly as it does today — no migration, no behavior change for the 1Password path.
- No change to the production k3s cluster. This milestone's own proof runs against a throwaway minikube instance, per the owner's go-ahead.

## Review Focus

- An existing forge.yaml with the legacy `item: vaults/Kube/items/forge-db-app` form must still pass `validateOptions` and still deploy through `onepassword.yaml` unchanged (regression coverage, not just new-shape coverage).
- A new, generic `item: kube/forge-test-secret` form must pass `validateOptions`, and `secretsAllowed` must still enforce the allow/deny-list against it (group `kube` must be in `--secret-vaults`, and the item name itself must not be denied) — the generalization must not accidentally bypass the existing governance.
- A malformed `item` value that matches neither shape (for example `justaname`, or a 1Password-looking path missing the literal `vaults`/`items` words, like `Kube/forge-db-app`) must still be refused with a clear error, not silently accepted as "generic."
- When `secretBackend` is anything other than `onepassword`, the chart must not render `onepassword.yaml` at all (no orphaned `OnePasswordItem` nobody's operator will ever satisfy).
- The Kubernetes Secret name a `run.secrets` entry resolves to (`<name>-secret`, after the rename in Task 2) must still be impossible to collide with a Secret Forge or CloudNativePG creates for another purpose — the same guarantee `options.go:161-166`'s comment already documents for the old `-1password` suffix.

---

### Task 1: `internal/manifest` — widen the addressing shape

**Files:**
- Modify: `internal/manifest/options.go:62-86,100-112,161-180` (the `Secret` struct, its regexes, `Vault()`/`ItemName()`, `ValidVault`, and `validateOptions`'s per-secret checks)
- Test: `internal/manifest/options_test.go` (add cases; file may not exist yet under that exact name — add to whichever `_test.go` in that package already covers `validateOptions`/`Secret`)

**Interfaces:**
- Consumes: nothing new.
- Produces: `func (s Secret) Group() string` (replaces `Vault()`), `func ValidGroup(s string) bool` (replaces `ValidVault`). `ItemName() string` keeps its name and signature. Both `internal/server/deploys.go` and `cmd/forge-api/deploys.go` (Task 2) consume these two renamed functions.

- [ ] **Step 1: Write the failing tests**

  ```go
  func TestSecretGroupAndItemName(t *testing.T) {
      cases := []struct {
          item, wantGroup, wantItem string
      }{
          {"vaults/Kube/items/forge-db-app", "Kube", "forge-db-app"}, // legacy 1Password shape, unchanged
          {"kube/forge-test-secret", "kube", "forge-test-secret"},     // new generic shape
      }
      for _, c := range cases {
          s := Secret{Item: c.item}
          if got := s.Group(); got != c.wantGroup {
              t.Errorf("Group() of %q = %q, want %q", c.item, got, c.wantGroup)
          }
          if got := s.ItemName(); got != c.wantItem {
              t.Errorf("ItemName() of %q = %q, want %q", c.item, got, c.wantItem)
          }
      }
  }

  func TestValidateOptionsSecretShapes(t *testing.T) {
      cases := []struct {
          name, item string
          ok         bool
      }{
          {"db", "vaults/Kube/items/forge-db-app", true},  // legacy, still valid
          {"db", "kube/forge-test-secret", true},          // generic, newly valid
          {"db", "justaname", false},                       // neither shape
          {"db", "Kube/forge-db-app", false},                // looks 1Password-ish but missing vaults/items words, and is a valid *generic* 2-segment form too — see note below
      }
      for _, c := range cases {
          r := &Run{Secrets: []Secret{{Name: c.name, Item: c.item, Env: map[string]string{"X": "field"}}}}
          err := r.validateOptions()
          if (err == nil) != c.ok {
              t.Errorf("validateOptions() item %q: err = %v, want ok=%v", c.item, err, c.ok)
          }
      }
  }
  ```

  Note on the fourth case: `Kube/forge-db-app` has exactly two slash-separated segments, so it *is* a valid generic `<group>/<item>` reference (group `Kube`, item `forge-db-app`) even though it looks like a truncated 1Password path. Change that case's `ok` to `true` once you've confirmed this is the intended, documented behavior (a two-segment string is always a generic reference; only a string with the literal `vaults/.../items/...` shape is treated as the legacy form) — this is not ambiguous in the implementation, only in a human's first reading of the test table, so fix the test table rather than the code if this trips you up.

- [ ] **Step 2: Run the tests to verify they fail**

  Run: `go test ./internal/manifest/... -run 'TestSecretGroupAndItemName|TestValidateOptionsSecretShapes' -v`
  Expected: FAIL (`Group` undefined, or the generic shape is refused).

- [ ] **Step 3: Widen `itemPath`, rename `Vault()`→`Group()` and `ValidVault`→`ValidGroup`**

  At `options.go:102`, replace:
  ```go
  itemPath = regexp.MustCompile(`^vaults/[A-Za-z0-9._-]+/items/[A-Za-z0-9._-]+$`)
  ```
  with a pattern that accepts either the legacy four-segment 1Password shape or a generic two-segment `<group>/<item>` shape (same character class for each segment).

  Replace `Vault()` (`options.go:71-77`) with `Group() string`: split `s.Item` on `/`; when there are exactly 4 segments and they look like `vaults/<x>/items/<y>` (literal `vaults` and `items` words), return the second segment; when there are exactly 2 segments, return the first; otherwise `""`. Update `ItemName()` (`options.go:80-86`) the same way (4-segment: fourth segment; 2-segment: second segment).

  Rename `ValidVault` (`options.go:111-112`) to `ValidGroup`, same body. Update the doc comments on `Secret` (`options.go:62-63`) and on `Group`/`ValidGroup` to describe both shapes, e.g.: "Group is the governed group of the item: the 1Password vault for a `vaults/<vault>/items/<item>` reference, or the first segment of a plain `<group>/<item>` reference for another secret backend."

  Update the per-secret error message at `options.go:167-169` to describe both accepted shapes.

- [ ] **Step 4: Run the tests to verify they pass**

  Run: `go test ./internal/manifest/... -v`
  Expected: PASS, including every pre-existing test in the package (the widened regex must not reject anything the old one accepted).

- [ ] **Step 5: Commit**

  ```bash
  git add internal/manifest/options.go internal/manifest/*_test.go
  git commit -m "manifest: accept a backend-neutral secret reference alongside 1Password's"
  ```

---

### Task 2: `internal/server` and `cmd/forge-api` — update the callers

**Files:**
- Modify: `internal/server/deploys.go:80-85,246-270` (`SecretVaults` doc comment, `secretsAllowed`'s calls to the renamed methods, its error text)
- Modify: `cmd/forge-api/deploys.go:83-84,93-107` (`--secret-vaults`/`--secret-denied-items` help text, the call to `manifest.ValidVault`)
- Modify: `internal/server/*_test.go` and `cmd/forge-api/deploys_test.go` wherever they reference `Vault()`/`ValidVault` or assert on the old "1Password" wording in an error message

**Interfaces:**
- Consumes: `manifest.Secret.Group()`, `manifest.ValidGroup` from Task 1.
- Produces: no new exported names.

- [ ] **Step 1: Update `secretsAllowed` to call the renamed methods**

  At `internal/server/deploys.go:250,256,258`, replace `s.Vault()` with `s.Group()` (twice) and keep `s.ItemName()` as-is. Generalize the error text at line 251 (`"this Forge API takes no 1Password secrets"`) and line 262 (`"name the item %q by its title, not its 1Password ID"`) to not assume 1Password specifically, e.g. "this Forge API takes no secrets" and "name the item by its title, not an opaque ID" — keep the `onePasswordID` regex check itself (it's still a useful guard against 1Password IDs specifically when that is the configured backend; it does no harm when it isn't, since a different backend's names won't happen to match a 26-character lowercase-alphanumeric string).

  Update the `SecretVaults`/`SecretDeniedItems` doc comments at `deploys.go:80-85` to describe them as "the secret groups a service's run.secrets may read" rather than specifically 1Password vaults.

- [ ] **Step 2: Update `cmd/forge-api/deploys.go`**

  At line 102, replace `manifest.ValidVault(v)` with `manifest.ValidGroup(v)`; at line 103, change the error text from `"must be a 1Password name"` to `"must be a secret group name"`. Update the `--secret-vaults`/`--secret-denied-items` flag help text at `main.go:83-84` the same way.

- [ ] **Step 3: Run `make build test lint`**

  Run: `make build test lint`
  Expected: compile errors or test failures at every remaining `.Vault()`/`ValidVault` reference and every test asserting the old "1Password" wording.

- [ ] **Step 4: Fix the remaining references found in Step 3**

  Update each failing test to use `.Group()`/`ValidGroup` and the new error wording.

- [ ] **Step 5: Run `make build test lint` again**

  Run: `make build test lint`
  Expected: PASS.

- [ ] **Step 6: Commit**

  ```bash
  git add internal/server/deploys.go cmd/forge-api/deploys.go internal/server/*_test.go cmd/forge-api/deploys_test.go
  git commit -m "server: generalize secret-group validation wording off 1Password"
  ```

---

### Task 3: the chart — a selectable secret backend, a backend-neutral Secret name

**Files:**
- Modify: `deploy/argocd/chart/templates/_helpers.tpl:15` (the `-1password` suffix)
- Modify: `deploy/argocd/chart/templates/onepassword.yaml` (the `-1password` suffix; gate the whole template on `secretBackend`)
- Modify: `deploy/argocd/chart/values.yaml` (add `secretBackend: onepassword` near the existing `secrets: []` default, with a comment)
- Modify: `internal/manifest/options.go:161-166` (the comment explaining the Secret-name suffix choice)
- Modify: `deploy/k3s/deploys.yaml.tmpl:32` (the AppProject's `namespaceResourceWhitelist` entry for `onepassword.com/OnePasswordItem`)
- Create: `deploy/argocd/chart/testdata/secret-backend-none.yaml` and its `.golden`
- Modify: `deploy/argocd/chart/testdata/options.golden` (the `-1password` → `-secret` rename changes its existing expected output)

**Interfaces:**
- Consumes: nothing new.
- Produces: chart value `secretBackend` (string, default `"onepassword"`); the Kubernetes Secret a `run.secrets` entry resolves to is now named `<name>-secret` (was `<name>-1password`).

- [ ] **Step 1: Rename the suffix**

  `_helpers.tpl:15`: `{{- $secret := printf "%s-1password" .name }}` → `{{- $secret := printf "%s-secret" .name }}`.
  `onepassword.yaml`: `name: {{ printf "%s-1password" .name }}` → `name: {{ printf "%s-secret" .name }}`; update its header comment (lines 1-4) to say `-secret` instead of `-1password`.
  `options.go:161-163`: update the comment's example suffix from `-1password` to `-secret`.

  Leave `deploy/k3s/deploys.yaml.tmpl:32`'s `{group: onepassword.com, kind: OnePasswordItem}` AppProject entry as-is, but add a one-line comment above it (next to the existing `# run.secrets, run.jobs and run.autoscaling (forge.yaml).` comment at line 31) noting that a fork using a different `secretBackend` adds its own backend's kind to this same list by hand — the list itself needs no new plumbing to support that, since it's a static file in the operator's own fork, not something `render.sh` generates conditionally. This is the whole of what "the AppProject's resource whitelist can allow-list a different backend's CRD" requires.

- [ ] **Step 2: Gate `onepassword.yaml` on `secretBackend`**

  Add `{{- if eq (.Values.secretBackend | default "onepassword") "onepassword" }}` immediately after the header comment, and `{{- end }}` at the end of the file (after the existing `{{- end }}` that closes the `range`). Add to `values.yaml`, near the existing `secrets: []` entry:
  ```yaml
  # Which operator turns a run.secrets entry into a Kubernetes Secret.
  # "onepassword": the 1Password Connect operator (the default). Anything
  # else: nothing renders here; something else must create a Secret named
  # <name>-secret with the fields run.secrets[].env maps.
  secretBackend: onepassword
  ```

- [ ] **Step 3: Update `options.golden` for the renamed suffix**

  Run: `go run helm.sh/helm/v3/cmd/helm@v3.21.4 template payments deploy/argocd/chart --namespace payments-production -f deploy/argocd/chart/testdata/options.yaml > deploy/argocd/chart/testdata/options.golden`
  Then inspect the diff (`git diff deploy/argocd/chart/testdata/options.golden`) to confirm the *only* change is `stripe-1password` → `stripe-secret` throughout.

- [ ] **Step 4: Add the `secretBackend: none` case**

  Create `deploy/argocd/chart/testdata/secret-backend-none.yaml` as a copy of `options.yaml` with one line changed: add `secretBackend: none` at the top level. Generate its golden:
  Run: `go run helm.sh/helm/v3/cmd/helm@v3.21.4 template payments deploy/argocd/chart --namespace payments-production -f deploy/argocd/chart/testdata/secret-backend-none.yaml > deploy/argocd/chart/testdata/secret-backend-none.golden`
  Then inspect it to confirm there is no `OnePasswordItem` anywhere in the output, while the Deployment's `secretKeyRef: {name: stripe-secret, ...}` lines are still there (the env wiring doesn't care who creates the Secret).

- [ ] **Step 5: Add the new case to the Makefile's `chart-test` target**

  In `Makefile`, after the existing `options.yaml`/`options.golden` line in `chart-test`, add:
  ```make
  	$(HELM) template payments deploy/argocd/chart --namespace payments-production \
  	  -f deploy/argocd/chart/testdata/secret-backend-none.yaml | diff -u deploy/argocd/chart/testdata/secret-backend-none.golden -
  ```

- [ ] **Step 6: Run `make chart-test`**

  Run: `make chart-test`
  Expected: PASS, every case including the new one.

- [ ] **Step 7: Run `make build test lint`**

  Run: `make build test lint`
  Expected: PASS.

- [ ] **Step 8: Commit**

  ```bash
  git add deploy/argocd/chart Makefile internal/manifest/options.go
  git commit -m "chart: make the secret-populating operator selectable, rename the Secret suffix off -1password"
  ```

---

### Task 4: minikube exit proof

**Files:** none (verification only; no repo changes beyond notes in the commit message if something is discovered broken).

**Interfaces:** none.

- [ ] **Step 1: Start minikube**

  Run: `minikube start` (on the owner's server, per the owner's go-ahead to use it for this phase's testing). No MetalLB, Istio, or Cloudflare addon — a bare minikube cluster is the point.

- [ ] **Step 2: Render and install a throwaway service's chart release directly, with the generic secret shape and no backend**

  Create a values file (scratch, not committed) equivalent to `deploy/argocd/chart/testdata/secret-backend-none.yaml` but with `secrets: [{name: test, item: kube/forge-test-secret, env: {TEST_VALUE: value}}]` and a real, pullable `image`. Install it directly with Helm against the minikube context (no forge-api, no GitHub, no ArgoCD involved — this proves the chart/Kubernetes mechanics in isolation):
  Run: `helm install secret-proof deploy/argocd/chart --namespace secret-proof --create-namespace -f <that values file>`

- [ ] **Step 3: Hand-create the Secret the chart expects, and confirm the pod reads it**

  Run: `kubectl -n secret-proof create secret generic test-secret --from-literal=value=hello`
  Then: `kubectl -n secret-proof rollout status deploy/test` (or whatever the Deployment's name resolves to) and `kubectl -n secret-proof exec deploy/test -- printenv TEST_VALUE`
  Expected: the pod starts (it was likely `CreateContainerConfigError` until the Secret existed) and `TEST_VALUE` prints `hello` — proving the env wiring works with a Secret that came from something other than the 1Password operator, addressed by the new generic `item` shape.

- [ ] **Step 4: Clean up and note the result**

  Run: `helm uninstall secret-proof -n secret-proof && kubectl delete ns secret-proof`
  Record in the Phase 8 status update (see `AGENTS.md`'s "Keeping this true") that this milestone's exit proof ran on minikube on the owner's server, on this date, with this result — not on the production k3s cluster.
