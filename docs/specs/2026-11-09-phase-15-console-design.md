# Phase 15: the console

Status: approved by the owner on 2026-11-09, with the decisions at the end.

## Why

The owner has a new design for the dashboard, "Forge Console" (a clickable
mock, 2026-11-09). It keeps the dashboard's design language (the dark rail,
the ember accent, Archivo, IBM Plex, the CLI line on every page) and changes
the pages:

- a home page with the actions a person may take and four headline numbers;
- a Catalog with tabs, search and filters;
- a service page made of cards;
- Deployments with headline numbers and a detail panel;
- a step-by-step Create;
- Rules, Ownership, Adoption and personal settings.

The mock also shows data Forge does not record: service tiers, SLO targets,
golden-path checks, on-call rotations, rules with modes, adoption numbers,
CLI tokens and notifications. The owner chose to build those too
(2026-11-09). They also chose one forge-web pull request for the new look,
checked on staging before it is released.

## What the owner chose (2026-11-09)

- **Build the missing data too**, not hide it or show it as "not set".
- **The new look is one forge-web pull request**, deployed to staging and
  looked at there before a release and a `web.image.tag` rollout.

## Milestones

| | What | Needs the API |
| --- | --- | --- |
| A | The new look on every page, with the data Forge has today | No |
| B | Catalog facts: tier, SLO target, on-call, team members | Yes |
| C | Golden-path checks | Yes, after B |
| D | Rules with modes | Yes |
| E | Adoption | Yes |
| F | Personal settings: CLI tokens | Yes |
| G | Service actions: restart and scale | Yes |

A ships first and on its own. Each later milestone adds its API, its CLI
commands and its cards or pages to the dashboard together. A card whose data
does not exist yet is left out until its milestone, never shown empty.

Notifications (email, Slack, in Forge) and new resource kinds (Redis, Kafka,
buckets) are not in this phase: Forge sends no messages today and runs only
PostgreSQL, and each needs its own design.

## A. The new look (forge-web only)

The mock's layout and components, on the API Forge has:

- **Shell:** the rail's groups Build (Catalog, Templates, TechDocs), Run
  (Environments, Clusters, Deployments, Resources) and Govern (Rules,
  Ownership, Adoption once E is built, Audit log), collapsible as today; the
  ⌘K palette; the theme switch; the person and their role at the foot,
  linking to their settings.
- **Home:** the hero with the actions the person's role allows (create,
  deploy, templates, docs); the four numbers (services, deploys today and
  their success rate, what needs this person, production's health). Then
  "Services that need a look" or "Your services", "Needs you" or "Get
  started", recent activity, platform status and quick links. The role is
  the signed-in person's, never picked on the page.
- **Catalog:** tabs Services, Templates, Resources and Clusters (Clusters
  for admins, as today); search, filters for owner and status; one table
  with the worst environment of each service.
- **Service page:** cards for the overview (repo, environments, template,
  last deploy, owners, Argo CD), related resources and dependencies (the
  catalog's `dependencies`), staging and production, recent deploys and
  previews. Tabs: Overview, Deployments, Dependencies, Docs, Resources,
  Settings. Settings keeps what it has, including environments and the
  danger zone.
- **Deployments:** the numbers (today, success rate, median duration,
  failed); search and filters for service, environment and status; the
  table, and the selected deploy's details with its operation, Argo CD
  application and the actions the role allows.
- **Create:** the steps Template, Name and owner, Template inputs, How it
  runs and Review, with the command and the `forge.yaml` it makes, for every
  source the form has today (template, image, chart, repo, infrastructure).
- **Environments, Templates, Resources, Ownership, Audit log, TechDocs,
  Clusters, Operations:** the mock's look on today's data. Approvals move
  under Rules, as "Waiting for approval".
- **Not built:** the mock's "Design language" page, the role picker on
  the home page and its sample data.

## B. Catalog facts

`forge.yaml`'s `service` gets three optional fields, and the catalog gets
them as `Service` fields. The owning team changes them with `forge service
update`, as other service settings:

```yaml
service:
  tier: 1          # 1 critical, 2 high, 3 medium; empty is "not set"
  slo: "99.9%"     # the availability target, as text
```

On-call belongs to a team, not a service. `Team` gets `oncall` (the name of
the rotation, set by an admin with `forge team update <team> --oncall
<name>`) and `member_count`, read from GitHub through the App, which already
has Members: read. The dashboard shows them on the Catalog, the service page
and Ownership.

## C. Golden-path checks

Forge works out five checks for each service. `ListServiceChecks` (readers;
one service or all) returns them, with how many of those that apply pass:

| Check | Passes when |
| --- | --- |
| CI pipeline | CI deployed the service to staging in the last 30 days (the audit log), or its repo has a workflow in `.github/workflows` |
| SLO defined | `service.slo` is set (B) |
| Tracing | `service.tracing: true` in `forge.yaml`, or `forge service update --tracing` |
| Runbook | the repo has `docs/runbook.md` |
| On-call | the owning team has an on-call rotation (B) |

CI and Runbook do not apply to a service without a repo (an image or a
chart): they count neither way. The catalog, the audit log and the teams'
settings are read on every call, so a change shows at once; what the repo
holds is read from GitHub and kept for five minutes.

The service page shows them as the golden-path card, and Catalog can filter
on "off the golden path". `forge service checks [name] [--missing]` shows
them in the CLI.

Built (2026-11-10), two changes to the plan above: the checks are their own
call, not fields of `GetService` and `ListServices` (a stored service would
otherwise carry facts that go stale, and listing the catalog would wait on
GitHub). And the templates do not set `service.tracing` yet: none of them
sends traces today, and the check must not say otherwise. A template that
adds OpenTelemetry sets it in its forge.yaml.

## D. Rules with modes

The owner chose (2026-11-10) to make only the policy rules switchable. The
rules that guard Forge itself stay enforced and are listed as such.

| Rule | Kind | Default |
| --- | --- | --- |
| `images.pinned-version`: an image names a version, not `latest` or none | Images | enforce |
| `images.allowed-registries`: images come from the allowed registries | Images | enforce |
| `charts.allowed-repos`: charts come from the allowed repositories | Charts | enforce |
| `promotion.healthy-staging`: staging is Synced and Healthy before a promote | Promotion | enforce |
| `resources.storage-limit`: storage within the environment's limit | Resources | enforce |
| `resources.production-ha`: production databases run at least 2 instances | Resources | enforce |
| `resources.instance-limit`: at most the environment's instances | Resources | enforce |
| `resources.staging-single`: staging needs one instance | Resources | warn |

Always enforced, shown without a switch: a plan touches only the declared
database in the service's own namespace, matches forge.yaml, is readable,
and deletes only with `--allow-destroy`, and storage never shrinks; an image
is not another service's CI image and a registry or chart host is not an
internal address; a promotion ships the deploy that was checked; a target
belongs to the team and has the environment and room.

Modes:

- **enforce**: as today, the action is refused;
- **warn**: the action goes ahead; the warning is in the answer where the
  call has one (a resource plan) and in the audit event (`policy_warnings`);
- **off**: the rule is not evaluated.

In the rules, each switchable rule's checks are tagged with its id
(`violation`), and `policy/modes.rego` turns a violation into a deny or a
warning by the rule's mode. Modes are stored in the table `rule_modes`; a rule
nobody changed has its default, so nothing changes on upgrade. When the modes
cannot be read, every switchable rule is enforced, the strictest choice. Each
audit event that checked rules records `policy_modes`: the modes in force that
differ from the defaults, next to `policy_rules`, the rules' fingerprint.

`PolicyService.ListRules` (readers) and `UpdateRule` (admins, action
`rule.update`, audited); `forge policy list` and `forge policy set <rule>
--mode warn`. The dashboard's Rules page lists them by kind, with a mode
switch for admins, and links to the approvals.

## E. Adoption

`GetAdoption` (readers) is computed from the audit log and the catalog:

- active users (distinct people with any action in the last 30 days);
- deploys in the last 30 days and the 30 before;
- the self-service ratio (the share of writes done by people outside the
  admin groups);
- resources applied;
- a weekly series of deploys, self-served writes and active users for 12
  weeks.

"Where adoption is stuck" lists teams with no service, failing services,
services off the golden path (C), teams without on-call (B) and resources
not applied.

Built (2026-11-11): each person's audit event records their role as the
detail `role` (`admin` or `member`), so the self-service ratio counts only
changes made from 0.54.0 on. `GetAdoption` reads the audit log of the last
60 days or 12 weeks, whichever is longer (at most 200,000 events), and keeps
the result five minutes. Resources applied and what is stuck come from the
catalog, the checks and the resources, which the dashboard already reads.
`forge audit adoption [--weekly]`.

## F. CLI tokens

Personal tokens for CI and scripts, next to today's sessions: `fgp_…`, made
by a person with a name, scopes (read; deploy; settings) and an expiry (7,
30 or 90 days, or never). A token acts as its maker, never with more than
their role, and is stored only as a hash. `forge auth token create|list|
revoke` and the settings page. Expired and revoked tokens are refused, and
every use updates "last used".

## G. Service actions

`forge service restart <svc> --env <env>` restarts its pods (a rolling
restart through the deploy repo's annotation, so Argo CD does it).
`forge service scale <svc> --env <env> --replicas <n>` writes the replica
count for that environment. Both use the team's role, are operations, and
are audited. The service page gets an Actions tab with both.

## Decided by the owner (2026-11-09)

1. **Tracing check:** a `forge.yaml` field (`service.tracing`) that the
   templates set, not a scan of the repo's dependencies.
2. **Rules:** modes for today's rules only. The mock's new rules (weekend
   freeze, 30 minutes healthy in staging before a promote, preview names)
   come later, one by one.
3. **Self-service ratio:** writes by people outside the admin groups.
4. **Order after A:** B and C, then D, E, F, G.

## Exit check

The new look runs in production. A service has a tier, an SLO and its
golden-path checks. An admin sets a rule to warn and sees an action go
through with the warning in the audit log. Adoption shows the last 12
weeks. A person makes a CLI token that deploys from a script and then
revokes it. A team member scales a service in staging and restarts it.
