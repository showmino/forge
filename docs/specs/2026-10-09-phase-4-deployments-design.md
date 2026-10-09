# Phase 4: Deployments through Argo CD

Oct 9, 2026 · Showmino · Draft for review

Implements Phase 4 of the [Forge CLI design doc](../index.md): `forge deploy start / status / history / rollback / promote` through Argo CD, and deploys from CI with workload identity.

**Exit gate (from the design doc):** a team ships to staging from CI without kubectl or Argo access.

## Decisions

| Question | Decision |
| --- | --- |
| How a deploy reaches Argo CD | GitOps. forge-api, as the GitHub App, commits the new version to a Forge-owned repo `showmino/forge-deploys`. An ApplicationSet turns each service-and-environment directory into an Argo CD Application with automatic sync. |
| What gets rendered | A Forge Helm chart (`charts/service`): Deployment, Service, HTTPRoute, quota and limits, locked down like previews. The chart lives in the forge repo; each directory in `forge-deploys` holds only a `values.yaml`. |
| Environments | Two fixed ones, in promotion order: `staging` → `production`. Namespace `<service>-<env>`, LAN URL `http://<service>.<env>.<gateway-ip>.nip.io` through the existing `forge-previews` Gateway. |
| The cluster | The k3s cluster is the production cluster for everything on it. Forge's Applications live in their own AppProject, which may only deploy into Forge's service namespaces. |
| CI authentication | forge-api verifies GitHub Actions OIDC tokens directly (audience `forge`), with no token exchange and no Forge signing key. A CI token may only deploy its own service to staging, at the commit and default branch it was minted for. |
| Who may do what | Humans with `services.write` may deploy, roll back and promote any service in any environment. CI tokens may only `deploy start` to staging, as above. |

**Assumptions** (confirm or correct during review):

- **Services:** every deployed service is a Forge catalog service with a GitHub repo in the org, whose CI pushes `ghcr.io/<owner>/<service>:<sha7>`, as go-grpc does.
- **Argo CD:** v3.5.3, with the org-wide GitHub App repo credential `argocd-repo-kubernetes` for `https://github.com/showmino`, which can read `forge` and `forge-deploys` provided its App is installed on them.
- **Images:** they pull with the existing pull secret (`mino-ghcr`), copied into each service namespace by the chart's pre-sync hook, which is defined in section 2.
- **No database:** deploy history is the git history of `forge-deploys`, so a forge-api restart loses nothing but in-flight operation records.

## 1. The deploy repo and Argo CD

### `showmino/forge-deploys`

Private. Only forge-api, as the App, writes to it, on `main`.

```
services/<service>/staging/values.yaml
services/<service>/production/values.yaml
```

`values.yaml`:

```yaml
image: ghcr.io/showmino/payments:1a2b3c4
port: 8080            # forge.yaml run.port at that commit (default 8080)
health: /healthz      # forge.yaml run.health (default /healthz)
replicas: 1
host: payments.staging.10.150.1.222.nip.io
forge:                # the record of this deploy; the chart ignores it
  service: services/payments
  env: staging
  ref: main
  commit: 1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d
  actor: ana@example.com          # or github:showmino/payments
  operation: operations/op_9f2c4a1b0c3d
  kind: deploy                    # deploy | rollback | promote
  time: 2026-10-09T12:00:00Z
```

Each change is one commit with the message `<kind> <service> to <env>: <image> (<actor>)` and the trailer `Forge-Operation: <operation>`.

### The chart (`deploy/argocd/chart` in the forge repo)

It renders, into the Application's namespace, the same shape previews use (`internal/server/env_cluster.go`):

- **Deployment** `<service>`, with a single replica by default:
  - pod: `runAsNonRoot`, seccomp RuntimeDefault, no ServiceAccount token, `imagePullSecrets: [forge-pull]`;
  - container: read-only root filesystem, no privilege escalation, all capabilities dropped;
  - probes on `health:port`, and `PORT` set.
- **Service** `<service>`: port 80 → `port`.
- **HTTPRoute** `<service>`: parent `forge/forge-previews` (section name `<env>`), host `host`.
- **ResourceQuota and LimitRange** `forge`: the same values as previews.

**The pull secret `forge-pull`** is not rendered by the chart; it would put credentials in git. Instead, forge-api writes it into `<service>-<env>` on the first deploy, reusing `Cluster.PullSecretData` from Phase 3:
- **Namespaces** are created by Argo (`CreateNamespace=true`), so forge-api waits for the namespace before writing the secret.
- **forge-api's ClusterRole** gains `secrets: create` (it already has it). The guard in code limits the write to namespaces labelled `app.kubernetes.io/managed-by: forge` with a `forge.dev/deploy-env` label.
- **Ordering:** the Deployment's first pods may fail to pull until the secret exists. That is normal and the sync step waits through it.

### Argo CD objects (applied by `deploy/k3s/render.sh` when deploys are on)

- **AppProject `forge-services`:**
  - sourceRepos: `https://github.com/showmino/forge.git` and `https://github.com/showmino/forge-deploys.git`;
  - destinations: this cluster, namespaces `*-staging` and `*-production`;
  - `clusterResourceWhitelist: [{group: "", kind: Namespace}]`, for `CreateNamespace`;
  - namespace resources: only the kinds the chart renders;
  - no other cluster-scoped kinds.
- **ApplicationSet `forge-services`:** a git directory generator over `forge-deploys` `services/*/*`, which produces Application `<service>-<env>`. The generator sets `requeueAfterSeconds: 30` (Argo's default is 180s, which would make a first deploy wait about 3 minutes for its namespace; there are no webhooks because the cluster is LAN-only):
  - project `forge-services`;
  - sources: the chart (forge repo, path `deploy/argocd/chart`, a pinned revision), plus `$values` from `forge-deploys` `services/<service>/<env>/values.yaml`;
  - destination namespace `<service>-<env>`;
  - automated sync with prune and self-heal, `CreateNamespace=true`;
  - `managedNamespaceMetadata` labels `app.kubernetes.io/managed-by: forge`, `forge.dev/service: <service>`, `forge.dev/deploy-env: <env>`.
- **The Gateway `forge-previews`** gains two listeners, `staging` (`*.staging.<ip>.nip.io`) and `production` (`*.production.<ip>.nip.io`), each allowing routes from namespaces labelled `app.kubernetes.io/managed-by: forge`.

Preview namespaces carry the same label and keep using the `http` listener.

## 2. API

`DeployService` (`proto/forge/v1/deploy.proto`):

| RPC | Returns | Notes |
| --- | --- | --- |
| `StartDeploy` | `Operation` | `service`, `env`, `ref` (default `main`), `request_id`, `validate_only`. |
| `GetDeployment` | `Deployment` | The current state of one service in one environment. |
| `ListDeployments` | `Deployment[]` | Filter by service or environment. |
| `ListDeployHistory` | `DeployRecord[]` | Newest first, from the git log of that `values.yaml`, capped at 50. |
| `RollbackDeploy` | `Operation` | `service`, `env`, `to` (a `forge-deploys` commit, default the previous record), `validate_only`. |
| `PromoteDeploy` | `Operation` | `service`, `from` (default `staging`), `to` (default `production`), `validate_only`. |

`Deployment` has these fields:
- name `deployments/<service>/<env>`, `service`, `env`;
- `image`, `ref`, `commit`, `url`;
- `sync` (`SYNCED`, `OUT_OF_SYNC`, `UNKNOWN`) and `health` (`HEALTHY`, `PROGRESSING`, `DEGRADED`, `MISSING`, `UNKNOWN`), both from the Application;
- `message`: Argo's condition or health message;
- `deploy_commit`: the `forge-deploys` commit;
- `deployed_by` and `deploy_time`;
- `operation`: in memory, as for environments.

`DeployRecord`: `deploy_commit`, `kind`, `image`, `ref`, `commit`, `actor`, `time`, `operation`.

### The pipeline

Each step is an operation event.

1. **validate**
   - The service is in the catalog with a repo in the org, and the environment is `staging` or `production`.
   - The caller is allowed (section 3).
   - No deploy, rollback or promote is running for this target.
2. **resolve** (start only): the ref becomes a commit through the App, and `forge.yaml` gives `run.port` and `run.health`.
3. **image** (start only): wait for the image, failing at once when the commit's CI failed. This is the same code as `env create`, factored into a shared helper.
4. **commit:**
   - Read `values.yaml` at the head of `forge-deploys` `main`, if any.
   - Build the new one, then commit it with the git data API (blob, tree, commit, update the ref).
   - When the ref update is rejected because `main` moved, rebuild on the new head and retry, at most 3 times (`git_conflict`).
   - **Rollback** commits the chosen earlier `values.yaml` content, with a new `forge:` record.
   - **Promote** copies `image`, `port` and `health` from the source environment, sets `host` for the target, and writes a new record.
5. **namespace:** wait (5m, `FORGE_DEPLOY_NAMESPACE_TIMEOUT`) for `<service>-<env>` to exist, then write `forge-pull` into it on the first deploy.
6. **sync:**
   - Watch the Application `<service>-<env>` in `argocd` (`applications.argoproj.io`, read only).
   - Done when its synced revision is the new `forge-deploys` commit, `sync=Synced` and `health=Healthy`.
   - Events report sync and health changes and Argo's messages.
   - Timeout: 10m (`FORGE_DEPLOY_SYNC_TIMEOUT`), then `not_healthy` with the last message.
   - No automatic rollback. The commit stays, Argo keeps reconciling, and the CLI hint names `forge deploy rollback`.

**Cancel:**
- Before the commit, cancel stops the operation and nothing changes.
- After the commit, it only stops watching. The operation ends `cancelled` with the note that the commit stands.

**Concurrency:**
- **Per target:** one mutating operation per `<service>/<env>`, held in memory, like the environments `pending` map. A second gets `FailedPrecondition` naming the running operation.
- **request_id:** the same request_id returns the same operation.

## 3. Authentication and permissions

### GitHub Actions OIDC

forge-api accepts a bearer token from two issuers:
- **The configured OIDC issuer (Dex):** as today.
- **`https://token.actions.githubusercontent.com`:**
  - verified with its JWKS (go-oidc);
  - audience `forge` (`FORGE_GITHUB_OIDC_AUDIENCE`);
  - enabled when deploys are on.

The issuer in the token picks the verifier. A token whose `iss` matches neither is `Unauthenticated`.

**Turning a GitHub token into a Principal:**

| Claim | Requirement |
| --- | --- |
| `repository_owner` | the configured org (`showmino`), compared case-insensitively |
| `repository` | `<org>/<repo>`; the service is the catalog entry whose `repo_url` is `https://github.com/<org>/<repo>` (none → `PermissionDenied`) |
| `ref` | the repo's default branch, `refs/heads/main`, read through the App and cached for 10 minutes |
| `sha` | the deploy's commit: a CI deploy always deploys exactly this commit, and its `ref` field is ignored |

The Principal has subject `github:<org>/<repo>` and a single permission, `deploy:<service>:staging`. A CI principal may also call `GetOperation` and `WatchOperation` on operations it started, and `GetDeployment` for its own service's staging deployment, so `forge deploy start` can wait and print. Every other RPC returns `PermissionDenied`, including all other reads. A failed check names the claim that didn't match. Token values are never logged.

### Permissions per RPC

| RPC | Human (Dex) | CI (GitHub) |
| --- | --- | --- |
| `GetDeployment` | `services.read` | its own service's staging deployment only |
| `ListDeployments`, `ListDeployHistory` | `services.read` | no |
| `GetOperation`, `WatchOperation` | `services.read` | only operations it started |
| `StartDeploy` | `services.write` | `deploy:<service>:staging` for its own service; the commit is the token's `sha` |
| `RollbackDeploy`, `PromoteDeploy` | `services.write` | no |

Production is reached only by humans: `promote`, `rollback` or `deploy start --env production`.

## 4. CLI

```
forge deploy start [<service>] --env staging|production [--ref main] [--wait=true] [--dry-run]
forge deploy status [<service>] [--env E]       # table across both envs by default
forge deploy history [<service>] --env E [--limit 20]
forge deploy rollback [<service>] --env E [--to <deploy-commit>] [--yes] [--dry-run]
forge deploy promote [<service>] [--from staging] [--to production] [--yes] [--dry-run]
```

- **Service:** defaults to `forge.yaml`'s service, as `forge env` does.
- **Waiting:** like `env create`, the CLI watches the operation and then prints the deployment. `--wait=false` prints the operation name.
- **Confirmation:** `rollback` and `promote` to production need `--yes` or a terminal confirmation.
- **Exit codes:** as the design doc says. 7 means the operation failed: `not_healthy`, `ci_failed`, `image_timeout`, `git_conflict`.

**In GitHub Actions:**
- **Token pick-up:** when `ACTIONS_ID_TOKEN_REQUEST_URL` and `ACTIONS_ID_TOKEN_REQUEST_TOKEN` are set and `FORGE_TOKEN` isn't, the CLI requests an OIDC token with `audience=forge` and uses it as the bearer.
- **Ref:** with `GITHUB_SHA` set and no `--ref`, `deploy start` deploys `GITHUB_SHA`.
- **The workflow** needs `permissions: id-token: write` and the API URL (`FORGE_API_URL`).

## 5. Configuration and deploy (k3s)

**forge-api flags** (off by default; they need the GitHub App, previews and Kubernetes access):

| Flag / env | Default | Meaning |
| --- | --- | --- |
| `--deploys` / `FORGE_DEPLOYS` | off | Serve `DeployService` and accept GitHub OIDC tokens. |
| `--deploy-repo` / `FORGE_DEPLOY_REPO` | `showmino/forge-deploys` | The GitOps repo. |
| `--deploy-domain-suffix` | from `FORGE_PREVIEW_DOMAIN`'s IP: `<env>.<ip>.nip.io` | Hosts are `<service>.<env>.<ip>.nip.io`. |
| `FORGE_GITHUB_OIDC_AUDIENCE` | `forge` | Audience for CI tokens. |
| `FORGE_DEPLOY_SYNC_TIMEOUT` | `10m` | The sync step's timeout. |
| `FORGE_DEPLOY_NAMESPACE_TIMEOUT` | `5m` | The namespace step's timeout. |

**`render.sh` with `FORGE_DEPLOYS=1`** (requires `FORGE_PREVIEW_IP`) adds:
- the AppProject and ApplicationSet;
- the two Gateway listeners;
- a Role in `argocd` (applications get, list and watch) bound to `forge-api`;
- `FORGE_DEPLOYS=1` on forge-api.

The pull-secret write needs no new permission: Phase 3's ClusterRole already allows `secrets: create`.

**go-grpc v1.2.0** adds a `deploy` job to the skeleton's CI. It runs after `image` on pushes to `main`, `runs-on: action-runner-set` (a LAN runner, since the API is LAN-only), with `permissions: id-token: write`:
- **Get the CLI:** `go install github.com/showmino/forge/cmd/forge@main`, using the read token the template repo already uses (`FORGE_READ_TOKEN`).
  - This works only while forge is private and the org secret exists. The CLI's first GitHub release replaces it.
  - The org variable `FORGE_API_URL` points at the API.
- **Deploy:** `forge deploy start --env staging`.

**As built (milestone 3):**

- The runner scale set is `kube-runners` (Argo app `action-runner-set`), so `runs-on: kube-runners`.
- The CI job downloads the CLI from forge-api (`$FORGE_API_URL/cli/forge-linux-<arch>`) instead of `go install`, so it needs no token for the private forge repo.
- The chart revision is a pinned commit (`FORGE_DEPLOY_CHART_REVISION`).
- Generated Applications carry Argo's resources finalizer.
- forge-api refuses a `<service>-<env>` namespace that exists and isn't Forge's.

## 6. Errors, security and testing

### Errors

| Situation | Code | CLI exit |
| --- | --- | --- |
| Bad environment, ref or service name | `InvalidArgument` | 2 |
| Unknown service, deployment, or rollback target | `NotFound` | 3 |
| Bad, expired or untrusted token | `Unauthenticated` | 4 |
| CI token used outside its scope | `PermissionDenied` | 5 |
| A deploy running for the target, promote from an empty environment, nothing to roll back to, deploys off | `FailedPrecondition` | 6 |
| Operation ended failed or cancelled | — | 7 |
| GitHub, Kubernetes or Argo unreachable | `Unavailable` | 1 |

### Security

- **One production cluster.** The AppProject confines Forge's Applications to `*-staging` / `*-production` namespaces and to the chart's kinds. forge-api itself only reads Applications and writes one secret per service namespace.
- **CI tokens** are short-lived (GitHub's 5 to 10 minutes), audience-bound and scoped to one service in staging, only from the default branch, at the token's own commit.
- **Audit:** every change is a commit in `forge-deploys` whose message names the actor and the operation.
- **No new long-lived secrets.**

### Testing

- **Git writes:** an `httptest` fake of the GitHub git data API (refs, blobs, trees, commits; ref update with fast-forward checks). It covers first deploy, update, conflict-and-retry, rollback content and promote copy.
- **Argo:** a fake dynamic client holding Applications whose `status.sync` and `status.health` the tests set. It covers success, `Degraded`, never synced, a wrong revision, and timeout.
- **GitHub OIDC:** a local issuer (`httptest` JWKS plus an RSA key) signing test tokens. It covers audience, issuer, owner, repository → service, default branch, sha, expiry, and each RPC's permission.
- **Pipeline:**
  - per-target concurrency, request_id, cancel before and after the commit, validate_only;
  - the shared image wait with `env create`, run against both callers.
- **Chart:** golden output of the chart rendered with the Helm Go SDK (`helm.sh/helm/v3`, in tests only) for staging and production values.
- **CLI:** golden `deploy status` and `history`, exit codes, and pick-up of the Actions token from an `httptest` token endpoint.
- **Exit gate on k3s:**
  1. `forge create gate-<date>` from go-grpc v1.2.0.
  2. Its first push to `main` runs CI: test → image → deploy. No human acts after `forge create`.
  3. Check: `forge deploy status gate-<date>` shows staging Synced and Healthy at the pushed commit, and the URL answers `/healthz` and Greet.
  4. A human then runs `forge deploy promote --yes`, and production answers.
  5. Clean up: delete both Applications (by deleting their `forge-deploys` directories through the App), the repo and the catalog entry.

## 7. Rollout

Three milestones, each a PR:

1. **Deploy engine:**
   - `deploy.proto`;
   - git data API writes as the App (`internal/github`);
   - the Argo Application reader (`internal/server`);
   - the chart and its golden tests;
   - `DeployService` with human authentication;
   - the shared image-wait helper;
   - `forge deploy start / status / history / rollback / promote`.
2. **Workload identity:** the GitHub OIDC verifier and Principal mapping, per-RPC permission checks, and the CLI's Actions token pick-up.
3. **k3s and the exit gate:**
   - `forge-deploys` created;
   - render additions (AppProject, ApplicationSet, Gateway listeners, Role on Applications);
   - go-grpc v1.2.0 with the `deploy` job and the org variable `FORGE_API_URL`;
   - rollout, then the gate.

### Needs from the owner

| When | What |
| --- | --- |
| Milestone 3 | Create `showmino/forge-deploys` (private) with an initial commit on `main` (e.g. `auto_init` with a README): the git data API cannot commit to an empty repo. Or let me create it with the App. |
| Milestone 3 | Confirm the App behind `argocd-repo-kubernetes` can read `forge` and `forge-deploys`. Argo must fetch the chart from the private forge repo. |
| Milestone 3 | Org variable `FORGE_API_URL` = `http://forge.10.150.1.221.nip.io`. |
| Milestone 3 | **A runner on the LAN.** GitHub-hosted runners cannot reach the LAN-only API, so the skeleton's `deploy` job uses `runs-on: action-runner-set`. That is the org's actions-runner-controller scale set (Argo app `action-runner-set`, chart `gha-runner-scale-set` 0.14.2, `githubConfigUrl: https://github.com/showmino`). On 2026-10-09 no `AutoscalingRunnerSet` existed in the cluster and the app was OutOfSync, so the owner fixes or confirms it before the gate. The `test` and `image` jobs stay on `ubuntu-latest`. |
| Milestone 3 | Approve the rollout and the gate (creates a real repo). |

## Out of scope for Phase 4

- Public production URLs (`*.showmino.com`), TLS.
- Automatic rollback, approval gates, deploy windows, canaries.
- More environments than staging and production, or more clusters.
- CI systems other than GitHub Actions; long-lived API tokens.
- Deploying workloads that are not Forge-created services; databases and other resources (Phase 5).
- Persisting deploy operations (history is in git).
