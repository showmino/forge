# Two forge-api pods

Status: **dropped by the owner on 2026-10-30.** Not built; forge-api stays
one pod (`replicas: 1`, `Recreate`). Kept as the record of what a second
pod would take, should it come up again. The dashboard (`web.replicas`) and
forgegate.io (`run.replicas`) got their two pods the same day.

## Why

forge-api runs as one pod (`replicas: 1`, `strategy: Recreate` in
`deploy/platform/chart/templates/forge-api.yaml`). So every upgrade, node
drain or crash takes the API down: the dashboard, the CLI and every CI deploy
fail until a new pod is ready, and any operation running at the time (a
deploy waiting for Argo CD, a create, a resource apply) stops without an
outcome. Its watchers see the stream end.

The goal: two or more forge-api pods behind the same Service, rolling
upgrades with no downtime, and a pod that dies mid-operation leaves an
operation that says so, instead of one that vanishes.

## What stops a second pod today

| State in memory | Where | With two pods |
| --- | --- | --- |
| Operations, their events and watchers | `Operations.ops` (`operations.go`) | `GetOperation`, `WatchOperation` and `CancelOperation` on the other pod answer NotFound: the dashboard's live log breaks half the time |
| One mutation per deploy target | `Deploys.running` | Two deploys of one service and environment can run at once, one on each pod |
| One run per resource target, the runs being watched, cancels | `Resources.running`, `live`, `cancelling` | The same; and the runner's report reaches whichever pod the `forge-api-runner` Service picks, which may not be the one waiting for it |
| A service being deleted | `Catalog.deleting` | A deploy can start on the other pod while the service is taken down |
| Previews whose namespace is not made yet | `Environments.pending` | Two creates can pass the per-service limit |
| Creates in progress, their results | `creator.creating`, `results` | Two creates of one name; a retried create on the other pod starts again |
| `request_id` of a retried call | `Deploys.requests`, `Environments.requests`, `Resources.requests` | A retry that lands on the other pod starts a second operation |
| Version cutting per repo | `Deploys.cutLocks` | Two pods can cut the same version (the tag write fails for one, so a deploy fails) |

Already shared or harmless:

- The catalog, the audit log, sessions and resource plans are in PostgreSQL.
  Migrations take an advisory lock (`store/postgres.go`), so two pods
  starting together migrate once.
- The deploy repo is written with a non-forced ref update, so two writers
  cannot lose a commit: the slower one fails and is retried.
- Caches are per pod and only cost a second fill: templates, docs, teams,
  clusters, the deployments list (forge#182), CI branch lookups, and the
  hourly membership check of sessions.
- The anonymous rate limit and the ten-minute "one session per login" memory
  become per pod. A login that hits both pods could make two sessions, and
  the limit doubles. Accepted.

## Design

### 1. Operations in PostgreSQL

Migration `0005_operations`:

- `operations`: name, kind, target, creator, create/end time, state, error,
  the final response, `owner` (the pod's name) and `lease_until`,
  `cancel_requested`, `lock_key` and `request_key`.
- `operation_events`: operation, seq, step, message, time.

`Operations` gets a store interface with two implementations: the memory one
of today (`--in-memory`, tests) and PostgreSQL (whenever forge-api has a
database). `Start` inserts the row; `emit` appends an event; the end writes the
outcome. `List` and `Get` read the table. Pruning (24 hours, the newest 1000)
becomes a delete that any pod runs every few minutes.

**Watch** reads the past events from the table, then follows new ones. Each
pod `LISTEN`s on one channel, `forge_operations`; a write `NOTIFY`s it with
the operation's name, and the pod reads that operation's new events and sends
them to its watchers. A poll every 2 seconds covers a lost notification. A
watch can be served by any pod.

**Cancel** on the owner pod cancels the work as now. On another pod it sets
`cancel_requested` and notifies; the owner sees it and cancels.

### 2. Locks and retries are constraints

- A running operation that needs exclusivity sets `lock_key`
  (`deploy:<svc>/<env>`, `resource:<svc>/<env>`, `service:<svc>` for a delete,
  `create:<name>`). A unique partial index on `lock_key` where the operation
  is running makes the second insert fail; forge-api answers
  FAILED_PRECONDITION with the running operation, as today. A delete takes
  `service:<svc>`, and a deploy, preview or resource run checks for it in the
  same transaction. This replaces `Deploys.running`, `Resources.running`,
  `Catalog.deleting` and `creator.creating`.
- `request_key` (procedure, caller, request_id) is unique for 24 hours: a
  retry on any pod finds the operation the first call started. This replaces
  the three `requests` maps and `creator.results`.
- Previews' per-service limit is counted in the transaction that inserts the
  create operation, under an advisory transaction lock per service.
- Version cutting takes a session-level advisory lock keyed by the repo
  (`pg_advisory_lock(hash)`) on a dedicated connection for the length of the
  cut.

### 3. A pod that goes away

- The owner renews `lease_until` (now + 30s) of its running operations every
  10 seconds.
- Any pod that finds a running operation whose lease ran out ends it as
  failed with code `interrupted`: "forge-api stopped while this ran; run it
  again". That frees its `lock_key`. Every operation's steps can already be
  run again (deploys, deletes and resource repairs were built that way), so
  there is no automatic resume in this design.
- On SIGTERM a pod stops starting operations, gives its running ones up to
  `api.drainSeconds` (default 120) to end, then ends the rest as
  `interrupted` itself, without waiting for the lease.
- The runner's reports (`/runner/v1/`) are already written to the resource
  store. The pod that receives one also notifies `forge_operations`, and the
  owner of the run's operation reads the run from the store instead of a
  channel in `Resources.live`.

### 4. The chart

- `api.replicas` (default 1, so no install changes by itself). With more
  than one: `strategy: RollingUpdate` (`maxUnavailable: 0`), a
  PodDisruptionBudget `minAvailable: 1`, and a preferred pod anti-affinity
  across nodes. `terminationGracePeriodSeconds` is `api.drainSeconds` + 15.
- Nothing to check about the database: since chart 0.32.0 Forge's database
  is always a PostgreSQL the install runs (`database.existingSecret`), so all
  pods share it.
- Rolling upgrades run an old and a new forge-api at the same time, so from
  this design on **a migration must work with the previous release**: add
  columns and tables; drop or rename only one release later. That goes into
  AGENTS.md's rules.

## Not in this design

- Dex: it is one pod too, so a sign-in fails while it restarts. Dex keeps its
  state in the same PostgreSQL and can run two pods; that is a chart value
  (`login.replicas`) and could come with milestone 3 if the owner wants it.
- Resuming an interrupted operation where it stopped.
- More than one Forge writing one deploy repo (unchanged: one install, one
  repo).

## Milestones

1. **Operations and locks in PostgreSQL**, still one pod: the store, the
   migration, `lock_key` and `request_key`, version cutting. Behaviour is the
   same as today; the tests run against both stores (`FORGE_TEST_DATABASE_URL`).
2. **Two pods:** LISTEN/NOTIFY, watch and cancel from any pod, leases,
   `interrupted`, the drain, the runner's reports. Tests start two `server.New`
   on one database and drive them through two clients.
3. **The chart and the rollout:** `api.replicas`, the rolling update, the
   PodDisruptionBudget, anti-affinity, the drain; then showmino's
   `api.replicas: 2` in a pull request in showmino/kubernetes.

Each milestone is a release; the first two change nothing a user sees.

## Exit gate

On showmino's install with `api.replicas: 2`:

1. Both pods Ready; the dashboard and `forge service list` work.
2. A deploy started from the CLI, watched in the dashboard to the end
   (whichever pod each lands on), and cancelled from the other pod once.
3. A pod deleted during a deploy: the deploy ends `interrupted` within a
   minute, and running it again succeeds.
4. A rollout of the next forge release with `forge op watch` open on a
   deploy and a loop of `forge service list`: no failed call.
