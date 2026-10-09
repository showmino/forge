# Groundwork before Phase 5: GitHub login and durable storage

Oct 9, 2026 · Showmino · Draft for review

Two gaps stand between Phase 4 and Phase 5; this work closes both.

1. **Security gap.** On k3s, Dex logs people in with a demo static user (`admin@example.com` / `password`). `FORGE_WRITER_GROUPS` is empty, so every login gets `services.write`. That one permission covers registering services, `forge create`, `forge env create` and deploys, promotion to production included.
2. **Durability gap.** The service catalog lives in forge-api's memory. A restart or rollout wipes it, and CI deploys then fail: a GitHub Actions token maps to a service through the catalog's `repo_url`. Dex keeps its logins in memory too. Phase 5's audit log needs a database anyway.

## Decisions

| Question | Decision |
| --- | --- |
| Login source | GitHub, through Dex's GitHub connector. Only members of the `showmino` org can log in. |
| Who may write | Members of the team `platform-engineering`: `FORGE_WRITER_GROUPS=showmino:platform-engineering`. Other members get `services.read` only. |
| What is durable | Postgres holds the service catalog and Dex's data. Operations stay in memory; deploys stay in git, previews in Kubernetes labels. |
| How | A `ServiceStore` interface with an in-memory and a Postgres implementation. The Postgres one uses pgx and hand-written SQL, and migrations are embedded in forge-api. |
| Database | A CloudNativePG cluster `forge-db` in namespace `forge`, with a database for forge-api and one for Dex. |
| Backups | None for now, as on the cluster's other Postgres clusters. They go on the backlog. |

## 1. GitHub login and permissions

**Dex on k3s** (`deploy/k3s/forge.yaml.tmpl`):
- **Removed:** `enablePasswordDB` and `staticPasswords`. No password login remains.
- **Added:** a GitHub connector:
  ```yaml
  connectors:
    - type: github
      id: github
      name: GitHub
      config:
        clientID: $GITHUB_CLIENT_ID
        clientSecret: $GITHUB_CLIENT_SECRET
        redirectURI: http://${DEX_HOST}/dex/callback
        orgs:
          - name: ${FORGE_DEX_GITHUB_ORG}
        teamNameField: slug
  ```
  - With `orgs`, Dex refuses anyone who is not a member of that org.
  - Groups arrive as `<org>:<team-slug>`, for example `showmino:platform-engineering`.
  - The CLI already asks for the `groups` scope, so no CLI change is needed.
- **Credentials:** the Deployment reads `GITHUB_CLIENT_ID` and `GITHUB_CLIENT_SECRET` from the Secret `dex-github` (keys `client_id`, `client_secret`) through `env.valueFrom.secretKeyRef`. The ConfigMap holds no secret value.
- **1Password:** when `FORGE_DEX_ONEPASSWORD_ITEM` is set, render.sh adds a `OnePasswordItem` named `dex-github`, as it does for `forge-github-app`. Without it, the owner creates the Secret by hand.

**forge-api on k3s:** `FORGE_WRITER_GROUPS` is rendered into the Deployment.

**render.sh:**
- `FORGE_DEX_GITHUB_ORG` defaults to `FORGE_GITHUB_ORG` (`showmino`).
- `FORGE_WRITER_GROUPS` defaults to `<org>:platform-engineering`. render.sh refuses an empty value, so the k3s install can never grant every login write access.
- Both values are validated with the same character checks as the other settings.

**Unchanged:**
- local development: `deploy/dex/config.yaml` keeps its demo user and memory storage, and `make api-dev` keeps the static `dev` token;
- the `Authorizer` code (`internal/server/authn.go`): it already grants `services.write` only to `WriterGroups` when that list is non-empty;
- CI callers: they authenticate with GitHub Actions tokens (`ByIssuer`) and get only `deploy:<service>:staging`, so the writer groups don't affect them.

**What people see:**
- Everyone logs in again (`forge auth login`). The browser step now goes to GitHub, and the first login asks to authorize the OAuth App.
- A person's identity becomes their GitHub email.
- Members outside `platform-engineering` can list and read services, environments, deploys and operations. A write call fails with `PermissionDenied … lacks permission services.write`.

**Tests:**
- render tests:
  - the k3s render has no `staticPasswords`, no `enablePasswordDB` and no `hash:`;
  - it has the GitHub connector with the org and the callback URL;
  - Dex reads the Secret `dex-github` through `secretKeyRef`;
  - forge-api has `FORGE_WRITER_GROUPS, value: showmino:platform-engineering`;
  - an empty `FORGE_WRITER_GROUPS` or a bad org is refused;
  - the 1Password item appears only with `FORGE_DEX_ONEPASSWORD_ITEM`;
  - the goldens are updated.
- a server test: with `WriterGroups: [showmino:platform-engineering]`, a principal in `showmino:test-team` can call `ListServices` but gets `PermissionDenied` from `RegisterService`, `CreateService`, `CreateEnvironment` and `StartDeploy`; a principal in `showmino:platform-engineering` passes the permission check.

## 2. The database and the catalog store

**Cluster** (new file `deploy/k3s/database.yaml.tmpl`, always rendered):
- a CloudNativePG `Cluster` named `forge-db` in namespace `forge`:
  - 2 instances, image `ghcr.io/cloudnative-pg/postgresql:18.4-system-trixie` (as `showmino-db-production` uses);
  - 5Gi on `longhorn-retain`, so the volumes outlive an accidental delete of the cluster;
  - bootstrap `initdb` with database `forge`, owner `forge`; the operator (CNPG 1.30) generates the Secret `forge-db-app`;
  - resources: requests 50m CPU and 256Mi memory, limit 512Mi.
- a CloudNativePG `Database` named `forge-db-dex`: database `dex`, owner `forge`, on cluster `forge-db`.

**Dex storage:**
```yaml
storage:
  type: postgres
  config:
    host: forge-db-rw.forge.svc
    port: 5432
    database: dex
    user: forge
    password: $DEX_DB_PASSWORD
    ssl:
      mode: require
```
- `DEX_DB_PASSWORD` comes from `forge-db-app`'s `password` key.
- Logins, refresh tokens and Dex's signing keys survive a Dex restart.

**forge-api settings:**
- `--database-url` / `FORGE_DATABASE_URL`; on k3s, from `forge-db-app`'s `uri` key.
- **Set:** at startup forge-api connects, retrying for up to about a minute as it does for OIDC discovery. It applies pending migrations under a Postgres advisory lock, so two pods in a rollout can't migrate at once, then serves.
- **Unset:** it uses the in-memory store and logs a warning that the catalog is lost on restart. This is for `make api`, `make api-dev` and tests.
- The Deployment keeps `replicas: 1`, because operations are still in memory.

**Schema** (`internal/store/migrations/0001_services.sql`, embedded):

| Table | Columns |
| --- | --- |
| `forge_migrations` | `version` (primary key), `applied_at` |
| `services` | `name` text primary key (`services/<id>`); `revision` bigint (the etag number); `repo` text, the lowercased `<owner>/<repo>` from `repo_url` (nullable; indexed); `owner_team` text (indexed); `data` jsonb (the `Service` message, protojson); `create_time`, `update_time` |
| `service_requests` | `request_id` text primary key; `service` jsonb (the result returned); `created_at`. Rows older than 24 hours are deleted on each register. |

- Dex has its own database, so no table names can collide; `forge_migrations` is prefixed anyway, because the Phase 5 audit tables will join this schema.
- `repo` is derived when a service is written, with the parsing `repoOf` does today; services whose `repo_url` is not a GitHub URL of the org have it NULL.

**Code:**
- A new package `internal/store`:
  - `Memory`: today's maps under a mutex;
  - `Postgres`: pgx v5 with a `pgxpool.Pool`, plus `Migrate`;
  - shared types: `ErrNotFound`, and the interface below.
- **The interface** keeps the catalog's rules in one place, the handlers, with the store handling only storage and atomicity:
  - **reads:** get by name, list (owner-team filter, after-name cursor, limit), names of all services, and services for a repo;
  - **writes:** run inside a store transaction that offers get, put (with revision), delete, the names of services that depend on a name, and remember or recall a request ID.
  - `Postgres` serializes write transactions with a transaction-scoped advisory lock. Catalog writes are rare, and this keeps the existence and dependency checks atomic without row-locking subtleties.
- `server.Catalog` keeps every RPC handler and its validation, and stores through the interface. `NewCatalog(store, now)` takes the store; a nil store means `store.NewMemory()`.
- The helpers `has`, `get`, `ids` and `servicesForRepo` gain a `context.Context` and an error. Their callers (`create.go`, `source.go`, `deploys.go`, `ci_auth.go`) return `Unavailable` when the database fails.

**Tests:**
- **One shared suite in `internal/store`** runs against both stores:
  - create, get, list with paging and the owner filter;
  - update with revisions, and delete;
  - dependents, request-ID recall, and repo lookup that ignores case;
  - concurrent writes;
  - for Postgres only: migrations are idempotent on a second run, and two concurrent `Migrate` calls are safe.
- The Postgres run needs `FORGE_TEST_DATABASE_URL`; without it those tests skip.
- In CI, the `test` job gets a `postgres:18` service container from `mirror.gcr.io/library/postgres:18` (Docker Hub's rate limits) and sets `FORGE_TEST_DATABASE_URL`.
- The existing catalog, CLI and server tests keep running on the memory store, unchanged apart from the new constructor.
- A forge-api test: with `--database-url`, a service registered before a restart of the server, re-created over the same database, is still there.

## 3. Rollout on k3s

**Owner, beforehand:**
1. Create a GitHub OAuth App owned by the `showmino` org:
   - name "Forge";
   - homepage `http://forge.10.150.1.221.nip.io`;
   - callback `http://dex.10.150.1.220.nip.io/dex/callback`.
2. Store its client ID and a client secret in 1Password as `vaults/Kube/items/forge-dex-github`, with fields `client_id` and `client_secret`.
3. If the org restricts third-party OAuth Apps, approve this one.

**Controller, in this order:**
1. **Code:** the work merges to `main`. A forge-api image `ghcr.io/showmino/forge-api:<sha7>` is built from that commit and pushed, as in Phase 4.
2. **Save the catalog:** `forge service list -o yaml` into a file, to re-register later. With the in-memory catalog, it holds only what was registered since the last restart.
3. **Rollback file:** the current live objects:
   - Dex: the ConfigMap `dex-config` and the Deployment `dex`;
   - forge-api: the Deployment `forge-api`;
   - and the exact render command in use today.
4. **Render with the new settings:** add `FORGE_DEX_ONEPASSWORD_ITEM=vaults/Kube/items/forge-dex-github` and the new `FORGE_API_IMAGE` to today's command. `FORGE_DEPLOY_CHART_REVISION` stays as it is; the chart doesn't change. Then the usual order:
   - a server dry run;
   - `kubectl diff`, read before anything is applied;
   - apply.
5. **Startup:** the database starts with Dex and forge-api already pointing at it.
   - The rolling updates keep the old pods serving until the new ones are ready.
   - Dex retries its storage connection; forge-api retries its database.
   - `kubectl -n forge wait cluster/forge-db --for=condition=Ready`, then the two rollouts finish.
6. **Re-register** the saved services.

**Checks after rollout:**
- `forge auth login` through GitHub succeeds. `forge auth status` shows the GitHub email and the group `showmino:platform-engineering`.
- `forge service register` works, a `kubectl -n forge rollout restart deploy/forge-api` follows, and the service is still listed afterwards.
- `kubectl -n forge rollout restart deploy/dex`, then a CLI call refreshes its token without a new login.
- A CI deploy still works: push to a test service's `main` and watch its `deploy` job, or re-run the last one.
- `kubectl -n forge get cluster forge-db` shows 2 ready instances; the Argo CD Applications are unchanged.

**Docs:** `docs/index.md` says Phases 1 to 4 are done, and the k3s section of the README describes GitHub login, the writer group and the database.

**Rollback:** apply the saved objects. Dex returns to memory storage and the static user; forge-api returns to the old image and the in-memory catalog. The database stays in place, unused, and is removed only with the owner's OK.

## Risks

- **Dex config expansion:** Dex expands `$VARS` in connector and storage config when `DEX_EXPAND_ENV` is not `false` (the default). A local Docker check of the k3s Dex config against a local Postgres proves it before rollout.
- **OAuth App access:** if the org restricts OAuth Apps and it isn't approved, Dex sees no org membership and refuses everyone. Owner step 3 covers this. The rollback is quick.
- **Losing write access:** writes need `showmino:platform-engineering`, and the owner is its only member today. A teammate who needs to deploy must be added to that team.
- **No backups:** losing both Longhorn replicas loses the catalog. It can be rebuilt with `forge service register` from each repo's `forge.yaml`, and Dex's data is only sessions. Backups to object storage are on the backlog.
- **Database down:** catalog calls fail with `Unavailable`; previews, deploys and CI deploys need the catalog and stop too. Two instances with automatic failover reduce this.

## Out of scope

- The audit log itself (Phase 5), backups, TLS verification of the database (`verify-full` with the CNPG CA), Dex with more than one replica, roles beyond read and write, and persisting operations.
