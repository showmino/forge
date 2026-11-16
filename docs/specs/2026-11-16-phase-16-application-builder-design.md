# Phase 16: the application builder

Status: approved by the owner on 2026-11-16, with the recommended answers to
the four questions at the end.

It is the last of the five ways into Forge the owner chose on 2026-10-29
(`2026-10-29-deploy-sources-design.md`, where it was called phase 13; that
number went to team reviews, so it is phase 16 here).

## Why

A real product is rarely one service. It is a web app, an API, a worker and a
database, owned by one team and released together. Forge knows each of those
as a separate service today:

- nothing says they belong together (`service.dependencies` is a list of
  names that the dashboard shows and nothing acts on);
- a part finds another part only if a person writes its address into
  `run.env` by hand, once per environment;
- a database belongs to exactly one service, so a worker cannot use the
  database of its API;
- a release to production is one promote per service, in an order the person
  has to remember, and a rollback is the same in reverse;
- a worker cannot run at all: the service chart probes every service over
  HTTP and gives it a Service and a route.

## Goals

- An **application** groups services ("parts") under one name and one team.
- A part is an ordinary service of any kind Forge already has: from a
  template, an image, a connected repo or a chart. It keeps its own page,
  logs, versions and history.
- Parts find each other, and the application's database, through environment
  variables Forge sets. Nobody writes an address by hand.
- A part may be a **worker**: no port, no address.
- The application is promoted to production, and rolled back, in one step.
- The dashboard has a builder page: see the parts and what connects them,
  add a part from any source, and see the `forge.yaml` each part will get
  before anything is written.

## Non-goals

- **A new way to run code.** A part deploys exactly as a service does today.
- **Deploying staging together.** Staging stays per part: each part's CI
  deploys its own `main`, as now. "Together" means production.
- **Infrastructure outputs.** Handing the outputs of a `nextjs-infra` repo's
  OpenTofu or Bicep code to the app was left to this phase by
  `2026-11-08-app-with-infrastructure-design.md`. It is not here: those runs
  happen in GitHub Actions and Forge never sees their outputs. It needs its
  own design.
- New resource kinds (Redis, queues). PostgreSQL only, as today.
- Several parts in one repo (a monorepo). One part, one service, at most one
  repo.
- Parts on different teams.

What would make this a failure even if it works: an application that is a
second kind of thing with its own deploy path, so that a part behaves
differently from a plain service.

## Approaches

1. **A field in each part's `forge.yaml`** (chosen). `service.application:
   shop` makes the service a part of `shop`. The application exists while a
   service names it. Git stays the one source of truth, as for tier and SLO;
   the builder page writes ordinary `forge.yaml` files, which is what the
   2026-10-29 description asks it to show; a part can leave by deleting a
   line. Cost: the application has no file of its own, so facts about the
   whole (its name, its team) are derived from its parts and must agree.
2. **An application file in the deploy repo** (`applications/shop.yaml`
   listing its parts). One place to read the whole. But two places can
   disagree (the file says `worker` is a part, `worker` was moved or
   deleted), and every change to a part's membership is a second commit in a
   second repo.
3. **One `forge.yaml` with several parts.** Reads well for a monorepo, but
   it is a new manifest shape, a new deploy path per part and a new chart
   layout: the failure described above.

## Milestones

| | What | Needs the API |
| --- | --- | --- |
| A | Applications: the field, the reads, `forge application list\|get` | Yes |
| B | Links: `<PART>_URL` and a shared database | Yes, and the service chart |
| C | Worker parts | Yes, and the service chart |
| D | Promote and roll back as one | Yes |
| E | The builder page | forge-web, after A to D |

Each milestone ships its API, its CLI and its user documentation in
forgegate together. The dashboard gets a read-only Applications page with A
and the builder with E.

## A. Applications

- `forge.yaml`: `service.application`, optional, a lowercase DNS label.
  `Service.application` in the catalog. The forge-api that knows the field
  must be live before any `forge.yaml` sets it (an older API refuses unknown
  fields and that service's deploys fail).
- Every part of an application has the same `service.team`. A deploy or a
  register that would break this is refused, with the other parts' team in
  the message.
- An application's name may equal the name of one of its parts (`shop` with
  parts `shop`, `shop-api`, `shop-worker`) and of no other service.
- New `ApplicationService`, reads for every reader: `ListApplications`,
  `GetApplication` (`applications/{id}`: team, parts with their kind, their
  staging and production versions and health, the links between them, the
  resources). Computed from the catalog; nothing is stored in this
  milestone.
- CLI: `forge application list`, `forge application get <name>`, and
  `forge service update --application <name>` (empty to leave).
  `forge create --application <name>` puts a new service into one.
- Dashboard: an Applications tab in the Catalog and an application page
  (parts as cards, each linking to its service page); a service page names
  its application.

## B. Links

- `forge.yaml`: `run.links`, a list of part names of the same application.
  For each, the pod gets `<PART>_URL` (the name in upper case, `-` as `_`):
  the linked part's address inside the cluster, in the same environment. A
  link to a worker, to a service outside the application, or to itself is
  refused. `<PART>_URL` names join the reserved names of `run.env`.
- A link works for every kind of part; for a chart part the address is the
  chart's `expose` service and port.
- **The database.** A resource is still declared by one part, in its
  `forge.yaml`. Another part of the same application uses it with
  `run.links: [<part>/<resource>]` and gets `DATABASE_URL`, the same
  credentials the owning part gets. One database per pod, as today: a part
  that has its own resource cannot link another's.
- A preview of one part links to the other parts, and the database, of
  staging.
- Links order nothing at deploy time: a part must tolerate its neighbour
  being briefly absent, as any service must.
- `GetApplication` reports the links, and a link whose target is not
  running in that environment.

**As built (2026-11-16):** `run.links` and `<PART>_URL` are built, for
deploys and promotes. Two things differ from the above. A part that runs a
chart cannot have links of its own, because Forge gives a chart no
variables; it can be linked to. Previews get no links, as they get no
`run.env` either.

**The shared database, as built (2026-11-16).** The owning part opts in
with `share: true` on the resource; the link is refused until it is
deployed that way in the same environment. The owner chose how the
credentials cross namespaces (2026-11-16): forge-api copies the URL into
the Secret `forge-linked-database` in the linking part's namespace at each
of its deploys, with access scoped by the service chart and no cluster-wide
read of Secrets. As built after the independent review: the chart lets
forge-api replace and delete that one Secret in every service's namespace
(so that a copy can be removed after its link is gone) and read the one
Secret of a database only where its service shares it; the copy is first
made with the `secrets: create` forge-api already had. A copy is removed
once a deploy without the link stands, and from every linking part when the
owner stops sharing; a service cannot change application while it links or
shares. A changed password reaches the linking part at its next deploy.

## C. Workers

- `forge.yaml`: `run.worker: true`. The service chart then renders no
  Service, no route and no DNS record, and no HTTP probes; the pod is ready
  when its process runs. `run.port`, `run.health`, `run.domain`, `run.host`
  and a public address are refused together with it.
- Everything else is unchanged: deploys, versions, restart, scale, logs,
  previews (a preview of a worker has no address).

**As built (2026-11-16):** as above, with one difference: a worker has no
preview at all (`forge env create` refuses it), because a preview is an
address to look at.

## D. Promote and roll back as one

- `PromoteApplication` (`forge application promote <name>`): promotes every
  part whose staging version differs from production, as one operation.
  Parts go in link order (a part after the parts it links to); parts with no
  order between them go together. It is refused when a part is unhealthy in
  staging.
- Each part's promotion is the existing one, with the existing rules:
  promotion policy, approvals, versions and the audit event per part. An
  approval is asked for the application once and names every part and
  version it covers.
- If a part fails, the operation stops and returns the parts it already
  promoted to their previous versions. The outcome names what was promoted,
  what failed and what was put back.
- A successful promote records a **release**: a number and each part's
  version (`shop release 7: shop v1.4.0, shop-api v2.1.3, shop-worker
  v0.9.0`). New table `application_releases` (migration 0009).
- `RollbackApplication` (`forge application rollback <name> [--to <n>]`):
  returns every part to the versions of the previous release, or of
  release `n`.
- `forge application releases <name>` lists them.
- Both writes are admin and owning-team actions like a service's promote
  and rollback (`application.promote`, `application.rollback` in the role
  table), are in `auditActions`, and a personal token needs the deploy
  scope. A CI workload cannot call them.
- Promoting or rolling back a single part stays possible. The application
  page then shows that production no longer matches a release.

**As built (2026-11-16).** Differences from the above:

- Parts are promoted one at a time, in link order and then by name, not
  several at once.
- There is no approval step: Forge's approvals are for the production runs
  of infrastructure repos, and promoting a service has none. Each part's
  promotion policy applies as it does to `forge deploy promote`.
- No new actions in the role table: the calls need `deploy.promote` and
  `deploy.rollback` on the application's team. The audit log has them as
  `application.promote` and `application.rollback`.
- A part's promotion inside the application's operation writes no audit
  event of its own: the one operation has one outcome, and its events name
  each part.
- When a part fails, the failed part is put back too, not only the parts
  before it. A part that was new in production stays.
- A rollback that fails part way stops; running it again finishes it.
- Without a number, a rollback goes to the newest release that is not what
  production runs, which is the one before the newest unless production
  moved since.
- The operation locks production of every part, and checks under the locks
  that nothing changed since it was planned.

After the independent review: a put back and a rollback restore the values
at the exact deploy repo commit (a restart's or a scale's too, and one
older than the 50 deploys a single rollback searches); a chart part whose
values changed counts as changed; the audit event names the parts and
keeps each part's policy outcome. Not done: `request_id` on the two calls
(the locks refuse a second call), and a way to cancel a put back.

## E. The builder page (forge-web)

- **New application:** name and team, then parts.
- **Add a part** from any source the Create page has: a template, an image,
  an existing repo, a chart; or an existing service of the team. Mark it a
  worker, give it a database, link it to other parts.
- **The picture:** parts as boxes, links as lines between them, the database
  as a box of its own on the part that declares it.
- **Before anything is written:** the `forge.yaml` of every part, and the
  CLI commands that would do the same, as the Create page shows its command.
- **Create** runs the existing calls, one part at a time, and shows each
  part's progress. A part that fails can be retried without redoing the
  others. The builder has no API of its own.
- An existing application opens in the same page to add or link parts; a
  change to a part with a repo is a pull request there, as service options
  are today.

## Exit check

In the dashboard a writer builds an application of a web part, a worker and
a PostgreSQL database; the web part and the worker both use the database and
the worker reaches the web part through its `_URL`. They deploy to staging.
One promote takes the application to production, and one rollback returns it
to the release before.

## Left to the builder

- How the chart value for links is named, and whether `<PART>_URL` is
  written into the deploy repo's values per environment or computed by the
  chart from the part's name and namespace.
- How the owning part's database Secret reaches the linking part's
  namespace.
- The exact wording of errors and the layout of the application page.

## What the owner chose (2026-11-16)

1. **The name** is `forge application` and "Applications". `forge app` and
   "Apps" stay reserved for the app catalog (phase 14, not built).
2. **Staging** stays per part: each part deploys to staging on its own and
   only production moves together. One staging deploy for the whole
   application would have to await a build of every part's repo.
3. **A failed promote** puts the already promoted parts back, instead of
   leaving production mixed for a person to decide.
4. **Infrastructure outputs** stay out of this phase (see Non-goals).
