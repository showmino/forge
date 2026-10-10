# Phase 5, milestone 2: resources and policy

Oct 10, 2026 · Showmino · Approved; amended as built on Oct 10, 2026

Phase 5 is "resources and audit". Milestone 1 built the audit log. Milestone 2 lets a service ask for a resource in `forge.yaml` and get it through `forge resource plan` and `forge resource apply`. Forge API runs OpenTofu for it, checks every plan against policy, and records every plan and apply in the audit log.

The first resource is a Postgres database. The runner, the plan store and the policy engine are built so that later resource types reuse them unchanged.

## Decisions

| Question | Decision |
| --- | --- |
| First resource | A Postgres database, made as a CloudNativePG cluster in the service's namespace. One per service in this version. |
| Environments | Staging and production each get their own database. Previews get none. |
| Where OpenTofu runs | One Kubernetes Job per plan and per apply, in a `forge-runner` namespace, with its own service account and image. Not inside forge-api, and not Crossplane or Argo CD. |
| Policy | Rego rules, evaluated in forge-api with the Open Policy Agent library, on the OpenTofu plan. The rules live in this repo and are built into the binary. |
| Plan and apply | `plan` saves the plan and its policy result. `apply` runs exactly that plan. A plan is used once, expires after 24 hours, and is refused if `forge.yaml` or the real state changed since. |
| Who | Writers plan and apply. A service's CI may plan for its own service and cannot apply. Readers list and get. |
| Deleting a database | Refused by policy unless the plan was made with `--allow-destroy` by a writer. The volumes stay on disk (`longhorn-retain`). |
| State | OpenTofu state, plans and resource records are stored in Forge's Postgres through forge-api. The runner holds no database password. |

## 1. What a developer sees

### The manifest

`forge.yaml` gains a `resources` list.

```yaml
resources:
  - name: main
    type: postgres
    storage: 5Gi            # default 1Gi
    environments:           # optional, per environment
      production:
        storage: 20Gi
        instances: 2        # default: 1 in staging, 2 in production
```

| Field | Rule |
| --- | --- |
| `name` | A DNS label of at most 30 characters, unique in the list |
| `type` | `postgres`. Any other value is invalid. |
| `storage` | Whole gibibytes, written `<n>Gi`, at least `1Gi`. Default `1Gi`. |
| `instances` | 1 to 3. Default 1 in staging and 2 in production. |
| `environments` | Keys `staging` and `production` only. Each may set `storage` and `instances`, which replace the top-level values for that environment. |

This version accepts at most one `postgres` entry per service. The Postgres version, the storage class and the pod sizes are set by the platform in the module (section 2), not by the developer.

forge-api validates the list and works out the values for one environment. The CLI only reads the file and sends the list.

### The commands

| Command | What it does |
| --- | --- |
| `forge resource plan --env <env>` | Reads `forge.yaml`, starts an Operation, and prints what would change, the policy result and the plan name. `--allow-destroy` lets the plan delete a database. `-f` names another manifest file. |
| `forge resource apply <plan>` | Shows the plan, asks to confirm, then runs exactly that plan as an Operation. `--env <env>` in place of a name picks the caller's own newest plan for that environment that is ready to apply. `--yes` skips the question. `--dry-run` runs every check and stops before the Job. |
| `forge resource list` | The resources of a service, per environment, with their state and live health. `--env` narrows it. |
| `forge resource get <name>` | One resource: its settings, state, live health, and the plan that last changed it. |

All four support `-o table|json|yaml|name`. `plan` and `apply` support `--wait` and `--timeout` like other long-running commands, and wait by default.

Names follow the existing style:

- `resources/<service>/<env>/<name>`, for example `resources/payments/staging/main`
- `resourcePlans/<id>`, where the id is 12 random hex characters

A plan prints like this:

```
Plan resourcePlans/7f3a91c2b04e for payments (staging)
  + postgres main   storage 5Gi, instances 1
Policy: passed
Apply it with: forge resource apply resourcePlans/7f3a91c2b04e
```

A plan with nothing to change prints "No changes". It cannot be applied, with one exception: when Forge's own records are out of line with what exists (an apply failed or was cut short), applying the empty plan puts the records right, and the plan's operation says so.

For a service that has previews, the plan output adds one line: previews get no database and no `DATABASE_URL`.

### Exit codes

| Case | Exit code |
| --- | --- |
| The plan was made and passed policy | 0 |
| Policy denied the plan, at plan or at apply | 5 |
| The caller lacks permission | 5 |
| The plan is stale, expired, used, or has no changes while Forge's records already match | 6 (conflict), as for other `failed_precondition` answers |
| The run failed (an OpenTofu error) | 7 (operation failed) |
| The run or the wait timed out | 8 (timeout) |

## 2. How a run works

### The parts

| Part | Where | Job |
| --- | --- | --- |
| `ResourceService` | forge-api, the public API | Plan, apply, list and get. It checks permissions, stores plans, checks policy and feeds the audit log. |
| Runner launcher | forge-api | Starts one Job per run in `forge-runner` and watches it. Behind an interface, so tests use a fake. |
| `forge-runner` | The Job's image | A small Go program in this repo (`cmd/forge-runner`). The image also holds OpenTofu, the Kubernetes provider and the Postgres module, so a run downloads nothing. |
| Runner API | forge-api, a second port | What the runner talks to: fetch its work, send progress, hand back results, and read and write OpenTofu state. |
| Postgres module | In the runner image | OpenTofu code that turns `storage` and `instances` into one CloudNativePG cluster in `<service>-<env>`. |

### The public API

`proto/forge/v1/resource.proto` defines `ResourceService`:

| Call | Result | Notes |
| --- | --- | --- |
| `PlanResources` | An Operation | Takes the service, the environment, the declared resource list, `allow_destroy` and a `request_id`. A repeated `request_id` returns the first call's Operation. |
| `GetResourcePlan` | A plan | State, summary of changes, policy result and messages, creator, times. Never the raw OpenTofu plan. |
| `ListResourcePlans` | Plans, newest first | Filters: service, environment, state, and "created by the caller". |
| `ApplyResourcePlan` | An Operation | Takes the plan name and, when the CLI has a manifest at hand, the declared resource list for the stale check. `validate_only` (the CLI's `--dry-run`) stops before the Job. |
| `ListResources` | Resources | Filters: service and environment. |
| `GetResource` | A resource | |

The Operation kinds are `resource.plan` and `resource.apply`. Their target is `resources/<service>/<env>`.

### A plan, step by step

1. forge-api checks permission and validates the list. It takes the lock for `resources/<service>/<env>`; a second run for the same service and environment is refused with `failed_precondition` and names the running Operation.
2. If the namespace `<service>-<env>` exists without Forge's labels, the call is refused, as deploys are today. A plan creates nothing on the cluster.
3. forge-api saves a plan record in state `planning`, with the resolved settings, their digest, and the hash of a new one-time token.
4. The launcher creates a Secret holding the token and a Job that mounts it. The Operation starts and streams events.
5. The runner fetches its work with the token: the mode, the settings as OpenTofu variables, and the state address.
6. It runs `tofu init`, `tofu plan -out`, and `tofu show -json`, sending each step as an event.
7. It hands back the plan file and the plan JSON.
8. forge-api builds the summary of changes, checks the JSON against policy (section 3), and saves the plan as `planned` or `denied`.

### An apply, step by step

1. forge-api checks permission and the plan (below), re-checks policy, takes the lock, and moves the plan to `applying` with a new token.
2. If the namespace `<service>-<env>` does not exist, forge-api creates it with the three labels Argo CD's ApplicationSet gives it (`app.kubernetes.io/managed-by: forge`, `forge.dev/service`, `forge.dev/deploy-env`). A production database can then exist before the first promote.
3. The runner fetches its work, which now includes the saved plan file, and runs `tofu init` and `tofu apply` on that file. The module waits until CloudNativePG reports the cluster healthy.
4. The runner hands back the outputs (the cluster name and the name of its connection Secret).
5. forge-api marks the plan `applied` and makes its resource records match what the plan declared: one `ready` record per declared resource, and no record for anything else in that service and environment. A record keeps its Secret name when a run reports none. The Operation's last event is: "Deploy to give the app `DATABASE_URL`."

So after a plan that deletes the database, its record is gone.

### When apply refuses a plan

| Check | Refusal |
| --- | --- |
| The plan is not in state `planned` | "plan was denied by policy", "plan was already applied", "plan failed" or "plan expired" |
| The plan is older than 24 hours | "plan expired; run `forge resource plan` again" |
| The plan has no changes and Forge's records already match what it declares | "nothing to apply" |
| The service is no longer in the catalog | "is no longer in the catalog" |
| Another run for the same service and environment is still going, also one started before a forge-api restart | "is still running for resources/<service>/<env>" |
| The CLI sent the manifest's resources and their digest differs from the plan's | "forge.yaml changed since this plan was made" |
| The stored state's serial differs from the one the plan was made on | "the resources changed since this plan was made" |
| Policy, run again on the saved plan, now denies it | the policy messages |

All are `failed_precondition`, except the policy refusal, which is `permission_denied`. OpenTofu's own check of a saved plan against the state stays as a second guard.

The checks run twice: once when the call arrives, and again once forge-api holds the lock for the target, so a plan that went stale in between is not started. A dry run (`validate_only`) that policy denies answers `permission_denied` and leaves the plan as it was.

### Plan and resource states

| Plan state | Meaning |
| --- | --- |
| `planning` | The plan Job is running |
| `planned` | Made, passed policy, ready to apply |
| `denied` | Made, refused by policy. Kept with its messages. |
| `failed` | The plan run failed or timed out |
| `applying` | The apply Job is running |
| `applied` | Applied |
| `apply_failed` | The apply run failed or timed out |
| `expired` | Not applied within 24 hours |

| Resource state | Meaning |
| --- | --- |
| `applying` | A first apply is running |
| `ready` | The last apply worked |
| `failed` | The last apply failed. Plan and apply again to repair it. |
| `deleting` | A destroy is running |

A resource also carries its live health, read from the CloudNativePG cluster when the resource is listed or fetched: the operator's phase text and the count of ready instances. If the cluster cannot be read, health is `unknown` and the call still succeeds.

### The runner API

- It is defined in its own proto package, `forge.runner.v1`, and is not part of the CLI's client.
- forge-api serves it on a second listener (`--runner-addr`, default `:8081`), behind a ClusterIP Service. The LoadBalancer exposes only the public port.
- A NetworkPolicy on forge-api's pods allows the runner port from the `forge-runner` namespace only, and the public port as today.
- Every call carries the run's token. forge-api stores only the token's SHA-256. A token works for one run, and only while that run's plan is `planning` or `applying` and its deadline has not passed.
- Calls: fetch the work, send an event, complete the run (with the plan file and JSON, or the outputs, or an error).
- OpenTofu state uses the `http` backend against the same listener, with the token as the password. It supports lock and unlock. A plan run may read the state and may not write it.

### The Job

| Setting | Value |
| --- | --- |
| Namespace | `forge-runner` |
| Image | `ghcr.io/showmino/forge-runner:<tag>`, set by `FORGE_RUNNER_IMAGE` |
| Retries | None (`backoffLimit: 0`) |
| Deadline | 15 minutes (`activeDeadlineSeconds: 900`) |
| Cleanup | Removed one hour after it ends; its token Secret is owned by the Job and goes with it |
| Security | Non-root, read-only root file system, all capabilities dropped, a writable `/tmp` |
| Size | Requests 100m CPU and 256Mi; limit 512Mi |

When a Job fails, the Operation fails with the last lines of the runner's output. Text from the runner goes through the same cleaning as other caller text before it is stored or printed.

### The Postgres module

One `kubernetes_manifest` resource of kind `postgresql.cnpg.io/v1` `Cluster`:

| Setting | Value |
| --- | --- |
| Name and namespace | The resource name, in `<service>-<env>` |
| Image | The platform's pinned Postgres 18 image, the same one `forge-db` uses |
| Storage | The declared size, class `longhorn-retain` |
| Instances | The declared count |
| Pod size | Requests 50m CPU and 256Mi; limit 512Mi |
| Labels | `app.kubernetes.io/managed-by: forge`, `forge.dev/service`, `forge.dev/resource` |
| Wait | Until the cluster reports healthy, at most 10 minutes |

Its outputs are the cluster name and the name of the Secret CloudNativePG writes (`<name>-app`). The module never reads the Secret, so no password enters the state or the plan.

The OpenTofu and provider versions are pinned in the runner's Dockerfile, and the image build runs `tofu validate` on the module.

### Rights on the cluster

| Who | New rights |
| --- | --- |
| The runner's service account | Manage `postgresql.cnpg.io` clusters. A ValidatingAdmissionPolicy limits its writes to namespaces labelled `app.kubernetes.io/managed-by: forge`. |
| forge-api | Create, get and delete Jobs and Secrets in `forge-runner`. Read `postgresql.cnpg.io` clusters, for live health. |

### Storage

Migration `0003_resources.sql` adds three tables, and `internal/store` gains a `ResourceStore` with memory and Postgres implementations, tested by the shared suites.

| Table | Holds |
| --- | --- |
| `resources` | One row per service, environment and name: type, settings, state, outputs, the last plan, times |
| `resource_plans` | One row per plan: service, environment, settings and digest, state, creator, summary, policy result and fingerprint, `allow_destroy`, the state serial, the token hash and deadline, the plan file and plan JSON, times |
| `resource_states` | One row per service and environment: the OpenTofu state, its serial, and the lock |

The plan file and plan JSON are cleared when a plan is applied or expires. The summary and policy result stay.

### Failure and restart

- A sweep, at start and then every minute, marks a `planning` or `applying` plan whose deadline passed as `failed` or `apply_failed`, and marks an unused `planned` plan older than 24 hours as `expired`. It reads every page of plans, and it asks the launcher to stop the runner of a run it ends.
- After a restart forge-api does not know which runs are still going, so it also asks the store: while a plan for a service and environment is `planning` or `applying` and its deadline has not passed, a new plan or apply for that target is refused.
- Tokens, plans and state are in Postgres. If forge-api restarts during a run, the runner can still write state and complete the run, so Forge never loses track of a database it created.
- The Operation itself is lost on a restart, as with deploys. The audit outcome is not: every run that ends gets exactly one outcome event, whether it ends through a live Operation, through a completion after a restart, or through the sweep.
- A failed apply can leave a half-made database. The state records what exists, the resource shows `failed`, and a new plan and apply repair it. If that new plan has nothing to change, applying it still puts Forge's records right.

## 3. Policy

### The engine and the rules

forge-api evaluates Rego in process with `github.com/open-policy-agent/opa/v1/rego`. The rules live in `policy/` in this repo, in package `forge.resources`, and are embedded in the binary. Changing a rule is a pull request and a forge-api release.

A rule set that does not compile stops forge-api from starting. An error while evaluating refuses the plan. Forge never applies a plan it could not check.

The rules are written to fail closed. In Rego a rule that reads a missing field, or a field of the wrong type, does not fire, so every rule reads optional fields through one helper with a fallback, and a change the rules cannot read is refused by a rule of its own: a change with no readable actions, a database with no readable size or instance count, an environment the limits do not know, or a list of changes that is not a list. A plan that is not a JSON object is an evaluation error.

### What a rule sees

```json
{
  "plan": { "…": "the output of tofu show -json" },
  "forge": {
    "service": "payments",
    "environment": "production",
    "resources": [{"name": "main", "type": "postgres", "storage_gi": 20, "instances": 2}],
    "actor": {"kind": "user", "subject": "…"},
    "allow_destroy": false
  }
}
```

A rule adds a message to `deny` to refuse the plan or to `warn` to comment on it.

### The starting rules

| Rule | Result |
| --- | --- |
| The plan deletes or replaces a database and `allow_destroy` is false | deny |
| Storage above 20Gi in staging or 50Gi in production | deny |
| Storage smaller than it is now | deny |
| Production with fewer than 2 instances | deny |
| More than 3 instances | deny |
| A storage class other than `longhorn-retain` | deny |
| The plan touches anything but a CloudNativePG cluster in `<service>-<env>`; on a replace both the old and the new object are checked | deny |
| An environment other than staging or production | deny |
| Fewer than 1 instance | deny |
| Staging with more than 1 instance | warn |

The last deny rule guards the platform: if the module or the runner ever tried to change something else, policy stops it.

### When policy runs and what is kept

- On every plan, before it is saved as `planned` or `denied`.
- Again at apply, on the saved plan JSON, so rules that changed since the plan decide.
- The plan keeps the result, the messages and a fingerprint of the rule set (the first 12 hex characters of the SHA-256 of the rule files).
- `allow_destroy` is fixed when the plan is made. A CI caller's request with `allow_destroy` is refused with `permission_denied` before any run starts.

A denied plan ends its Operation with the code `policy_denied`, and the CLI prints each message and exits 5:

```
Plan resourcePlans/7f3a91c2b04e for payments (production): denied by policy
  ✗ storage 200Gi is above the 50Gi limit for production
```

## 4. The app, changes and removal

### How the app gets the database

- The deploy chart gains one optional value, `database.secret`. When set, the container gets `DATABASE_URL` from that Secret's `uri` key.
- forge-api sets the value each time it writes deploy values (deploy, promote, rollback), from the resource records of that service and environment: the Secret name of a `postgres` resource that has been applied and is not `deleting`. It is always worked out fresh, never copied from an older deploy or from another environment.
- A running app does not change when a database is applied. The next deploy gives it `DATABASE_URL`.
- If a deploy names a Secret that is not there yet, Kubernetes holds the pod and starts it once the Secret appears.
- Forge never reads the Secret.

### Room in the namespace

The chart's ResourceQuota is 1 CPU and 1Gi of requests, 2 CPUs and 2Gi of limits, and 10 pods. Two database instances plus two app replicas reach the memory limit, so a rolling update would be blocked. The quota is raised to 2 CPUs and 2Gi of requests, 4 CPUs and 4Gi of limits, and 15 pods. This and `database.secret` go in one chart revision, rolled out before the first apply.

### Changing a database

More storage or more instances is a normal plan and apply; CloudNativePG grows volumes in place. Less storage is denied by policy.

### Removing a database

1. Remove it from `forge.yaml` and run `forge resource plan --env <env> --allow-destroy`.
2. Run `forge resource apply`. It asks the caller to type the resource name; `--yes` skips this.
3. Forge deletes the cluster. The volumes stay on disk, so a platform engineer can recover the data by hand.
4. The next deploy drops `DATABASE_URL`.

### Deleting a service

Deleting a service from the catalog is refused with `failed_precondition` while it has a resource record in any environment. The message names the resources and the command that removes them.

## 5. Permissions, audit and tests

### Permissions

| Call | Reader | Writer | The service's CI |
| --- | --- | --- | --- |
| `ListResources`, `GetResource`, `ListResourcePlans` | yes | yes | no |
| `GetResourcePlan` | yes | yes | its own service's plans |
| `PlanResources` | no | yes | its own service, either environment, never `allow_destroy` |
| `ApplyResourcePlan` | no | yes | no |

`PlanResources` and `GetResourcePlan` join `workloadProcedures`, and their handlers narrow a CI caller to its own service. A writer may apply a plan that CI made; apply shows it first.

### Audit

Two actions join the audit log: `resource.plan` and `resource.apply`. Each has a request event and an outcome event, like deploys.

| Detail key | Content |
| --- | --- |
| `environment` | `staging` or `production` |
| `plan` | The plan name |
| `policy` | `passed` or `denied`, on outcome events and on a refused apply |
| `policy_rules` | The rule set's fingerprint |
| `allow_destroy` | `true` when set |
| `changes` | Counts, for example `create=1 update=0 delete=0`, on outcome events |

The target is `resources/<service>/<env>`. A plan denied by policy has the outcome `failed` with the first message, so `forge audit list --failed` finds it. The four read calls join `auditReads`. The runner's calls are not events of their own; they belong to the run the plan or apply records.

### Configuration

| Setting | Meaning |
| --- | --- |
| `FORGE_RESOURCES=1` (render.sh) | Renders the runner namespace, service account, admission policy, NetworkPolicy and runner Service, and turns resources on in forge-api. It requires `FORGE_DEPLOYS=1` and `FORGE_RUNNER_IMAGE`. |
| `--runner-image` / `FORGE_RUNNER_IMAGE` | The runner image |
| `--runner-addr` / `FORGE_RUNNER_ADDR` | The runner API's listen address, default `:8081` |
| `--runner-namespace` / `FORGE_RUNNER_NAMESPACE` | Default `forge-runner` |

With resources off, the `ResourceService` calls answer `failed_precondition` with a message that says so, as previews and deploys do when they are off.

### Tests

| Area | How |
| --- | --- |
| forge-api | Unit tests with a fake launcher: plan, apply, each refusal in section 2, locking, expiry, the sweep, a completion after a restart, CI narrowing |
| Storage | The shared suites, on memory and on Postgres |
| Policy | For every rule, a passing and a failing case on plan JSON captured from real OpenTofu runs |
| Runner | The program against a fake `tofu`; the image build runs `tofu validate` |
| Runner API | Token checks: wrong token, used token, expired run, a plan run writing state |
| Deploy values | `database.secret` is set, dropped and never copied across environments |
| CLI | Golden files for plan, apply, list and get |
| Manifests | The render goldens and the chart goldens |
| Audit | The existing test that every procedure is in `auditActions` or `auditReads` |

Tests cannot prove a real apply on a real cluster. The exit gate does.

## 6. Delivery and the exit gate

One spec, three plans:

1. **API and policy.** The manifest, `ResourceService`, storage, the policy engine and rules, the CLI and audit. It works end to end with a fake launcher.
2. **Runner.** The runner program and image, the Job launcher, the runner API and state storage, the Postgres module, the chart change and the k3s manifests. It starts by proving the module on a throwaway local cluster.
3. **Rollout and exit gate on k3s.** The owner approves before anything touches the cluster. The rollout follows the existing routine: render, server dry run, `kubectl diff`, apply, with a rollback file.

### Exit gate

With a throwaway service on k3s:

1. A plan with 200Gi of storage is denied and exits 5.
2. A valid plan is applied to staging, and the database becomes healthy.
3. After a deploy, the app's pod has `DATABASE_URL`.
4. The service's CI can plan and is refused on apply.
5. A plan that removes the database is denied without `--allow-destroy` and works with it.
6. `forge audit list --service` shows every step with the right actor and policy result.
7. Everything the gate made is removed, including the retained volumes.

## Risks

| Risk | Answer |
| --- | --- |
| `kubernetes_manifest` shows changes on every plan because the operator fills in defaults, or cannot plan into a namespace that does not exist yet | Plan 2 proves the module on a local cluster first, including a plan into a missing namespace. If it cannot be made quiet, the module uses another provider resource; nothing outside the module changes. |
| A compromised runner could send a false plan JSON | The runner image is built from this repo and pinned by tag. The admission policy limits what its service account can change whatever it claims. |
| The raised quota lets an app use more memory than before | Accepted. The LimitRange defaults per container stay as they are. |
| The first apply of a new production namespace races Argo CD's first sync | forge-api creates the namespace with Argo CD's labels, which is the state Argo CD expects to find. The exit gate applies to staging; Plan 3 also applies to the throwaway service's production namespace before its first promote, and checks that the promote then syncs. |

## Known limits

- A database is applied before the code that uses it is deployed. A new service that needs its database at start fails its first deploy until the database is applied and the service is deployed again.
- Previews get no database.
- Service databases have no backups in this version.
- Recovering a deleted database from its retained volumes is manual.
- The rules check `spec.storage` only. The module does not use CloudNativePG's separate WAL storage or tablespaces; if it ever does, the rules must cover them.
- Text from a runner (change names, actions, error messages) is cleaned and cut to 256 or 500 characters before it is stored or shown.

## Out of scope

- Other resource types, and more than one database per service
- Previews with databases
- Backups and restores of service databases
- Rules loaded from outside the binary
- Applies by CI
- Postgres major-version upgrades
