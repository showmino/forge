# Phase 3: Preview environments and operations

Oct 8, 2026 · Showmino · Draft for review

Implements Phase 3 of the [Forge CLI design doc](../index.md): `forge env create / list / get / extend / destroy` with a TTL, one Kubernetes namespace per preview, and long-running work as operations with `forge op list / watch / cancel`.

**Exit gate (from the design doc):** a new service goes from `forge create` to a running preview environment in under 10 minutes.

## Decisions

| Question | Decision |
| --- | --- |
| What runs in a preview | Forge generates it. forge-api applies a standard Deployment, Service and HTTPRoute from the service's `forge.yaml` and the image its CI pushed. Templates carry no Kubernetes YAML. |
| Where environment state lives | In the cluster. Each preview namespace carries its state in labels and annotations, so environments and their expiry survive forge-api restarts. No database yet. |
| Where operations live | In memory. A restart loses `op watch` for in-flight work, not the environment. |
| How previews are reached | A LAN-only Gateway API `Gateway` (class `istio`) on MetalLB IP 10.150.1.222; each env gets `http://<env>.preview.10.150.1.222.nip.io`. Nothing public. |
| Which build runs | A Git ref (default `main`). The API resolves it to a commit and deploys `ghcr.io/<org>/<service>:<sha7>`, waiting for CI to push that image. `forge create` is unchanged. |
| Kubernetes client | `client-go` with server-side apply (`applyconfigurations`); the dynamic client for `HTTPRoute`. No CRD or operator. |
| Access control | Any logged-in user may create, extend or destroy any environment, as with the catalog. |

**Assumptions** (confirm or correct during review):

- Previews run on the k3s demo cluster that already runs Forge, and only one Forge installation manages that cluster.
- Every service image listens on HTTP. A gRPC-only service is reached through ConnectRPC's HTTP/1.1 support, which `go-grpc` already has.
- A preview is one Deployment with one replica. No databases, dependencies or secrets of the service itself.
- Ten environments at once is enough for the demo cluster.

## 1. Operations

### Model

An operation is long-running work started by an RPC that returns at once. Phase 3 has two kinds: creating and destroying an environment.

```protobuf
message Operation {
  string name = 1;                 // "operations/op_9f2c4a1b"
  string target = 2;               // "environments/pr-142"
  string kind = 3;                 // "create_environment" | "destroy_environment"
  string creator = 4;              // subject of the caller's ID token
  google.protobuf.Timestamp create_time = 5;
  google.protobuf.Timestamp end_time = 6;   // unset while running
  bool done = 7;
  OperationError error = 8;        // set when done and failed or cancelled
  repeated OperationEvent events = 9;
}

message OperationEvent {
  google.protobuf.Timestamp time = 1;
  string step = 2;                 // "resolve", "image", "namespace", ...
  string message = 3;              // "waiting for ghcr.io/showmino/x:1a2b3c4 (CI: in_progress)"
}

message OperationError {
  string code = 1;                 // "image_timeout", "not_ready", "cancelled", "internal"
  string message = 2;
}
```

`OperationService`:

| RPC | Shape | Notes |
| --- | --- | --- |
| `GetOperation` | unary | Full events. |
| `ListOperations` | unary | Newest first; filter by `target` and `done`. Events omitted. |
| `WatchOperation` | server streaming | Sends every past event, then live events, then the final `Operation` and closes. Several watchers at once are fine. |
| `CancelOperation` | unary | Idempotent. Cancelling a finished operation is a no-op that returns it. |

### Store

`internal/server/operations.go` holds operations in a map behind one mutex. Each operation has a `context.CancelFunc` and a list of subscriber channels. Work runs in a goroutine detached from the request (`context.WithoutCancel` plus its own cancel), so a dropped stream never stops it; only `CancelOperation` does. Finished operations are kept for 24 hours, then pruned; at most 1000 are kept, oldest finished first.

A slow watcher must not block the work: subscriber channels are buffered, and a watcher that falls behind is closed with `ResourceExhausted`. It can reattach and gets the full replay.

### CLI

- `forge op list [--target environments/x] [--running]`: id, kind, target, state (`running`, `succeeded`, `failed`, `cancelled`), age.
- `forge op watch <id>`: renders events as `✓ step message` lines the way `forge create` does, then the result. It exits 0 on success and 7 when the operation failed or was cancelled. `--timeout` bounds the watch only; when it expires the CLI prints `still running; resume with: forge op watch <id>` and exits 8.
- `forge op cancel <id>`: asks for confirmation unless `--yes`; with `--wait` it watches until done.
- The renderer is shared with `forge env create` and `forge env destroy`.

The id may be given with or without `operations/`.

## 2. Environments

### API

`EnvironmentService`:

| RPC | Returns | Notes |
| --- | --- | --- |
| `CreateEnvironment` | `Operation` | Takes `name`, `service`, `ref` (default `main`), `ttl` (default 24h), `request_id`, `validate_only`. |
| `GetEnvironment` | `Environment` | Read from the namespace, plus live Deployment status. |
| `ListEnvironments` | `Environment[]` | Filter by `service`. |
| `ExtendEnvironment` | `Environment` | Synchronous. Sets `expire_time = now + ttl`. |
| `DestroyEnvironment` | `Operation` | Takes `validate_only`. |

```protobuf
message Environment {
  string name = 1;             // "environments/pr-142"
  string service = 2;          // "services/payments"
  string ref = 3;              // as given: "main", a branch, tag or sha
  string commit = 4;           // full sha it resolved to
  string image = 5;            // "ghcr.io/showmino/payments:1a2b3c4"
  string url = 6;              // "http://pr-142.preview.10.150.1.222.nip.io"
  State state = 7;             // CREATING, READY, FAILED, DESTROYING
  string state_message = 8;    // why FAILED, e.g. "ImagePullBackOff"
  string namespace = 9;        // "preview-pr-142"
  string creator = 10;
  google.protobuf.Timestamp create_time = 11;
  google.protobuf.Timestamp expire_time = 12;
  string operation = 13;       // last operation on it, if forge-api still knows it
}
```

**Rules:**

- **Names:** an env name is a DNS label of at most 40 characters, so `preview-<name>` and the host name stay within limits. Names are globally unique.
- **TTL:** at least 1h, at most 7d (`168h`), for both create and extend.
- **Limit:** at most `FORGE_PREVIEW_MAX` environments (default 10) exist at once, counting all states.
- **Idempotency:** `request_id` works as it does for `CreateService`: a retry with the same id returns the same operation instead of starting a second create.

### The create pipeline

Each step is an operation event.

1. **validate**
   - The service is in the catalog and has a GitHub repo URL in the configured org.
   - The name is free: no namespace `preview-<name>` exists.
   - The limit is not reached.
   - `validate_only` stops here and returns without creating an operation.
2. **resolve**
   - The GitHub App resolves `ref` to a commit (`GET /repos/{o}/{r}/commits/{ref}`).
   - It reads `forge.yaml` at that commit to get `run.port` and `run.health`. They default to 8080 and `/healthz`, so services made from go-grpc v1.0.0 work.
   - The image is `ghcr.io/<owner>/<service>:<sha7>`, matching what go-grpc's CI pushes.
3. **image**
   - Every 10 s, ask the registry whether the image tag exists.
   - The registry call is `HEAD /v2/<owner>/<service>/manifests/<sha7>` on ghcr.io, using the credentials in the pull secret forge-api is configured with.
   - Each wait also checks the CI run for that commit (`GET /repos/{o}/{r}/actions/runs?head_sha=`). The event shows its status, and a run that finished unsuccessfully fails the operation at once with `ci_failed`.
   - When every run for the commit has finished and the image has stayed missing for a tenth of the timeout (1m by default, at least two looks), the workflow builds no image for that commit (older templates build only on `main`), the image was deleted, or it has another name: `no_image`, without waiting for the timeout.
   - Gives up after `FORGE_PREVIEW_IMAGE_TIMEOUT` (default 10m) with `image_timeout`.
4. **namespace:** create `preview-<name>`.
   - Labels: `app.kubernetes.io/managed-by=forge`, `forge.dev/environment=<name>`, `forge.dev/service=<service>`.
   - Annotations: `forge.dev/ref`, `forge.dev/commit`, `forge.dev/image`, `forge.dev/creator`, `forge.dev/expire-time` (RFC 3339), `forge.dev/state`, `forge.dev/state-message`, `forge.dev/operation`.
5. **apply.** Server-side apply with field manager `forge-api` of:
   - **Pull secret:** a copy of forge-api's pull secret (`forge-pull`), and the namespace's default ServiceAccount set to use it.
   - **Limits:**
     - `ResourceQuota`: 1 CPU and 1Gi requests, 2 CPU and 2Gi limits, 10 pods.
     - `LimitRange`: default requests 50m and 64Mi, default limits 500m and 512Mi.
   - **Deployment** `<service>`: one replica, `runAsNonRoot`, read-only root filesystem, no privilege escalation, all capabilities dropped. Readiness and liveness probes are HTTP GETs on `run.health` and `run.port`. The env var `PORT` is set to `run.port`.
   - **Service** `<service>`: port 80 to `run.port`.
   - **`HTTPRoute`** `<service>`: parent `forge-previews` in namespace `forge`, host `<name>.<FORGE_PREVIEW_DOMAIN>`, routing to the Service.
6. **ready**
   - Wait until the Deployment is Available, for at most `FORGE_PREVIEW_READY_TIMEOUT` (default 5m).
   - While waiting, the events report pod problems as they are seen: image pull errors, crash loops, failing probes.
   - **On success:** set `state=READY` and finish with the URL.
   - **On timeout:** set `state=FAILED` and `state-message` to the last pod problem, and fail the operation with `not_ready`. The namespace stays so it can be inspected, and it expires normally.

**Failures before the namespace exists** (validate, resolve, image) leave nothing behind. A failure after it, other than `not_ready`, marks the env FAILED.

**Cancel:**
- Stops the pipeline.
- If the namespace was created, destroys it as part of the same operation.
- The operation ends with `cancelled`.

### Destroy and expiry

`DestroyEnvironment`:
1. Sets `state=DESTROYING`.
2. Deletes the namespace.
3. Waits until it is gone (default 3m), then the operation succeeds.

A create running for the same env is cancelled first.

**The reaper** is a goroutine in forge-api:
- **Schedule:** every minute, it lists namespaces with `app.kubernetes.io/managed-by=forge`.
- **Action:** it starts a destroy operation (creator `forge-reaper`) for each one whose `expire-time` has passed and that is not already DESTROYING.
- **Malformed state:** a missing or unparsable `expire-time` counts as expired one TTL maximum (7d) after the namespace's creation time.
- **Restarts:** the expiry is on the namespace, so a restart neither loses nor extends it.

**Guard:**
- forge-api reads and changes only namespaces that carry the `managed-by=forge` label and the `preview-` prefix.
- Any other namespace is reported as "not found" and never touched.

### Environment state

- **Source:** `GetEnvironment` and `ListEnvironments` read namespaces, so they work after a restart.
- **State:** taken from the `forge.dev/state` annotation. A READY env whose Deployment is no longer available is shown as READY with a `state_message` such as `0/1 pods ready`.
- **`operation` field:** set only while forge-api still has that operation in memory.

## 3. CLI

### `forge env create <name> [--service S] [--ref R] [--ttl 24h]`

- `--service` defaults to `forge.yaml`'s service when run inside a repo, otherwise it is required.
- **Waiting:** by default the CLI watches the operation as `forge op watch` does, then prints the environment with its URL. `--wait=false` prints `operations/<id>` (or the id alone with `-o name`) and exits 0.
- **Dry run:** `--dry-run` sends `validate_only`.
- **Retries:** the CLI generates `request_id` and reuses it on retries, as `forge create` does.

### `forge env list [--service S]`

Table columns: name, service, state, ref, commit (7), expires (relative), url. `-o json|yaml|name`.

### `forge env get <name>`

Every field, plus the last pod problem when not ready.

### `forge env extend <name> --ttl 48h`

Prints the new expiry.

### `forge env destroy <name>`

- Requires `--yes` or a TTY confirmation, and supports `--dry-run` and `--wait=false`.
- Exit codes: 0 when destroyed, 3 when no such environment.

An env name may be given with or without `environments/`.

## 4. Configuration

| forge-api flag / env | Default | Meaning |
| --- | --- | --- |
| `--previews` / `FORGE_PREVIEWS` | off | Enable `EnvironmentService` and the reaper. Off: env RPCs return `FailedPrecondition` "previews are not configured". |
| `--preview-domain` / `FORGE_PREVIEW_DOMAIN` | none (required when on) | e.g. `preview.10.150.1.222.nip.io` |
| `--preview-gateway` / `FORGE_PREVIEW_GATEWAY` | `forge/forge-previews` | Parent Gateway `namespace/name` |
| `--preview-pull-secret` / `FORGE_PREVIEW_PULL_SECRET` | `forge/mino-ghcr` | dockerconfigjson Secret copied into each env and used for registry checks |
| `--preview-max` / `FORGE_PREVIEW_MAX` | 10 | Environments at once |
| `FORGE_PREVIEW_IMAGE_TIMEOUT`, `FORGE_PREVIEW_READY_TIMEOUT` | 10m, 5m | Step timeouts |

forge-api uses the in-cluster config, or `KUBECONFIG` when run locally. Previews need the GitHub App, since resolve and the CI check go through it.

**`forge.yaml`** gains an optional `run` section, validated by `forge init` and `forge service register`:

```yaml
run:
  port: 8080        # 1-65535
  health: /healthz  # an absolute path
```

## 5. Deploy (k3s)

`render.sh` with `FORGE_PREVIEWS=1` (requires a GitHub App ID) adds:

- **A ServiceAccount `forge-api`** and a ClusterRole bound to it:
  - namespaces: get, list, watch, create, patch, delete
  - in any namespace: secrets, serviceaccounts, services, resourcequotas, limitranges, deployments and `httproutes` (get, list, watch, create, patch, delete); pods and events (get, list, watch)
  - The guard above keeps forge-api to its own namespaces. Kubernetes RBAC cannot express "only namespaces I created", so that rule lives in code and in tests.
- **A `Gateway` `forge-previews`** (class `istio`) in `forge`:
  - Listener: HTTP on port 80 for `*.preview.10.150.1.222.nip.io`.
  - `allowedRoutes`: namespaces with `app.kubernetes.io/managed-by=forge`.
  - Address: the MetalLB IP 10.150.1.222 through the `metallb.universe.tf/loadBalancerIPs` annotation on the generated Service.
- **forge-api settings:** the `FORGE_PREVIEW_*` settings above, and `serviceAccountName: forge-api`.

## 6. The `go-grpc` template (v1.1.0)

- **Manifest:** `forge.yaml` in the skeleton gains `run: {port: <port param>, health: /healthz}`.
- **Port:** the server reads `PORT` when set, falling back to the template's `port` parameter.
- **Release:** tagged `v1.1.0`, after `forge template lint` passes with the new schema.

## 7. Errors, security and testing

### Errors

| Situation | Code | CLI exit |
| --- | --- | --- |
| Bad name, TTL or ref syntax, unknown service in `forge.yaml` | `InvalidArgument` | 2 |
| Unknown service, environment, operation, or ref | `NotFound` | 3 |
| Environment exists, limit reached, service has no repo, previews off | `AlreadyExists` / `FailedPrecondition` | 6 |
| Operation ended failed or cancelled | the watch ends with the operation's error | 7 |
| Watch timed out (`--timeout`) | `DeadlineExceeded` on the client | 8 |
| Kubernetes, GitHub or the registry unreachable | `Unavailable` | 1, with the dependency hint |

### Security

- **Exposure:** previews are reachable on the LAN only, through 10.150.1.222. Nothing goes through the public `main-gateway`.
- **Credentials:** the pull secret is copied, never logged. Registry and GitHub tokens are never part of event messages.
- **Pod lockdown:** pods run non-root with a read-only root filesystem, and the quota bounds what one preview can use.
- **Guard:** forge-api's cluster-wide RBAC is broad. The label and prefix guard is enforced in one function every Kubernetes call goes through, and tested.

### Testing

- **Operations store:**
  - Replay then live events.
  - Several watchers.
  - A slow watcher is dropped without blocking the work.
  - Cancel before and after done.
  - Pruning.
  - A dropped request does not stop work.
- **Environment pipeline:** runs against `k8s.io/client-go/kubernetes/fake`, a fake dynamic client and an `httptest` GitHub and registry. Covered:
  - Each step's success and failure.
  - Cancel at each step.
  - The guard refusing foreign namespaces.
  - `request_id` replay.
  - The limit.
- **Reaper:** an injected clock covers expiry, malformed annotations and an env already being destroyed.
- **CLI:** golden tests for `env` and `op` output and exit codes through the existing harness.
- **Smoke:** `cli-smoke.sh` gains env and op cases. Validation, dry runs and not-found need no images. One real create and destroy runs against an existing service image.
- **Exit gate on k3s:**

  ```bash
  time (forge create gate-<date> --template go-grpc --team platform && forge env create gate-<date> --service gate-<date>)
  ```

  It must finish READY in under 10 minutes, and `curl` on the URL must answer. Afterwards: destroy the env, delete the repo and the catalog entry; the ghcr package is left for the owner.

## 8. Rollout

Three milestones, each a PR:

1. **Operations.** `operation.proto`, the in-memory store, `OperationService`, `forge op list / watch / cancel`, and the shared stream renderer. Tested with a fake operation kind until milestone 2.
2. **Environments.** `environment.proto`, the `run` section of `forge.yaml`, the GitHub ref, file, CI and registry lookups, the Kubernetes pipeline with the guard, the reaper, and `forge env create / list / get / extend / destroy`.
3. **k3s and the exit gate.** RBAC, Gateway and settings in `deploy/k3s`; go-grpc v1.1.0; smoke cases; rollout; the 10-minute gate.

### Needs from the owner

| When | What |
| --- | --- |
| Milestone 2 | Grant the `mino-forge` App **Actions: read**, used for the CI status while waiting for the image |
| Milestone 3 | Approve the rollout, MetalLB IP 10.150.1.222 for the preview Gateway, and the exit gate run (creates and deletes a real repo) |

## Out of scope for Phase 3

- Moving `forge create` onto operations. It keeps its own stream.
- Persistent storage: operations, the catalog and idempotency records stay in memory.
- Per-user or per-team permissions on environments.
- Databases, dependencies, secrets or several workloads per preview.
- Automatic previews per pull request (from CI). That arrives with workload identity in Phase 4.
- TLS and public preview URLs.
