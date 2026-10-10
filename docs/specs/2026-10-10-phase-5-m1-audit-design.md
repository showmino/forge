# Phase 5, milestone 1: the audit log

Oct 10, 2026 · Showmino · Draft for review

Phase 5 is "resources and audit". Its exit gate: every mutating action is traceable in the audit log by user and service. Milestone 1 builds the audit log. Milestone 2 brings resources (the OpenTofu runner) and policy, and they are audited from the first apply.

## Decisions

| Question | Decision |
| --- | --- |
| Order | The audit log first, then resources and policy. Each has its own spec and plan. |
| What is recorded | Every write call, including refused ones (unauthenticated, permission denied, invalid, failed precondition). The final result of long-running work. forge-api's own actions, as actor `system`. Reads are not recorded. |
| Who reads it | Every org member: `services.read`, the same as the catalog, environments and deploys. CI workloads can't read it. |
| Retention | Kept, append-only. No API call changes or deletes an event, and a database trigger rejects `UPDATE`, `DELETE` and `TRUNCATE`. A retention job can come later if volume needs one. |
| How events are captured | One Connect interceptor around every write procedure, plus a hook in the Operations runner for outcomes. Not explicit calls in each handler, and not database triggers. |

## 1. The event and its storage

An audit event is an API resource named `auditEvents/{id}`. `id` comes from a Postgres sequence, so a higher id is a later event.

| Field | Content |
| --- | --- |
| `time` | When the event was appended |
| `kind` | `request` (a write call) or `outcome` (how long-running work ended) |
| `action` | One name per write procedure (table in section 2), e.g. `deploy.promote` |
| `actor` | `kind` (`user`, `workload`, `system`, `anonymous`), `subject`, `email` for users, `repository` for CI workloads |
| `service` | `services/<id>` when the action concerns a service; otherwise empty |
| `target` | The resource acted on, e.g. `deployments/payments/production`, `environments/pr-142` |
| `result` | `code`: `ok` or the Connect code in snake case (`permission_denied`, `invalid_argument`, …), or for outcomes `ok`, `failed` or `cancelled`. `message`: the error text, at most 500 characters. |
| `operation` | `operations/<id>` when the call started long-running work; its `outcome` event names the same operation |
| `details` | A string map with a few fields per action, from an allowlist (section 2). Tokens, secrets and request bodies are never stored. |
| `client` | `version` (the `Forge-Client-Version` header) and `request_id` (`X-Request-Id`) |

**Storage:**
- **Table:** migration `internal/store/migrations/0002_audit.sql` adds `audit_events` to `forge-db`:
  - `id bigserial` primary key;
  - one column per field above, with `details` as `jsonb`;
  - indexes on `(service, id)`, `(actor_email, id)`, `(actor_repository, id)` and `(time)`.
- **Append-only trigger:** a trigger function raises an exception on `UPDATE` and `DELETE` (row triggers) and on `TRUNCATE` (statement trigger). It stops bugs and accidents; a database administrator can still drop it.
- **Code:** `internal/store` gains an `AuditStore` interface:
  - `Append(ctx, *forgev1.AuditEvent) (*forgev1.AuditEvent, error)`: assigns `name` and `time`;
  - `Get(ctx, name)`;
  - `List(ctx, AuditQuery)`.
  - `AuditQuery` has service, actor, action, failed-only, since, until, before-id (the page cursor) and a limit; results come newest first.
- **Implementations:** `Memory` and `Postgres` both implement it, and one shared test suite runs against both. forge-api uses the Postgres one when `--database-url` is set, the memory one otherwise.

## 2. Capture

**Actions.** Each write procedure has an action name and a function that reads `service`, `target` and `details` from the request message:

| Procedure | Action | Target | Details |
| --- | --- | --- | --- |
| RegisterService | `service.register` | `services/<id>` | `validate_only` |
| UpdateService | `service.update` | `services/<id>` | `update_mask`, `validate_only` |
| DeleteService | `service.delete` | `services/<id>` | `validate_only` |
| CreateService | `service.create` | `services/<id>` | `template`, `template_version`, `validate_only` |
| CreateEnvironment | `environment.create` | `environments/<id>` | `ref`, `ttl`, `validate_only` |
| ExtendEnvironment | `environment.extend` | `environments/<id>` | `ttl` |
| DestroyEnvironment | `environment.destroy` | `environments/<id>` | `validate_only` |
| StartDeploy | `deploy.start` | `deployments/<service>/<env>` | `env`, `ref`, `validate_only` |
| RollbackDeploy | `deploy.rollback` | `deployments/<service>/<env>` | `to`, `validate_only` |
| PromoteDeploy | `deploy.promote` | `deployments/<service>/production` | `from`, `to`, `validate_only` |
| CancelOperation | `operation.cancel` | `operations/<id>` | |

The plan fills in the exact field names from the protos. The rule: only identifiers and settings, never free text from the caller. Text that comes from the caller (identifiers, client headers, error messages) is cleaned to valid UTF-8, stripped of NUL bytes, stripped of control and bidi characters (each replaced by U+FFFD) and cut to 256 characters (500 for the result message) before it is stored.

Service: for the catalog calls, from the target. For environments and deploys, from the request's `service` field when it has one, otherwise from the environment or deployment name. For `operation.cancel`, from the operation's stored service.

**The interceptor** (`internal/server/audit.go`) runs outermost, before authentication:
1. **Reads pass through.** For a procedure not in the table, it calls `next` and records nothing.
2. **Setup.** For a write, it puts an empty event in the request context and calls `next`. The auth interceptor fills in the actor when it authenticates: a user (subject, email), a workload (subject, repository), or nothing. If nothing is filled in, the actor is `anonymous` with no subject.
3. **Handlers add the operation.** A handler that starts long-running work names its operation in the event. `Operations.Start` does this itself from the context, so no handler changes.
4. **Append.** After `next` returns, the interceptor sets `result` from the returned error (`ok` when there is none), adds `service`, `target` and `details` from the request message, and appends the event. If the handler panics, the event is recorded as `internal` (message `panic: <value>`) and the panic is then raised again.

Streaming: `CreateService` is server-streaming. The interceptor wraps the stream to read its request message, and records the code the stream ends with. A create that fails after the repo was made ends the stream without an error, so the creator notes the failure on the event: it is recorded as `failed`, with the step in the message (`push: ... (rolled back: true)`).

**Outcomes.**
- **At start:** `Operations.Start` copies the actor, action and service of the event in its context onto the operation.
- **At finish:** when the operation finishes (`finish`), the runner appends an `outcome` event with the same action, actor, service and target. `operation` is the operation's name, and `result` is `ok`, `failed` with the error message, or `cancelled`.
- **No audit context:** an operation started without one, such as by the system, gets actor `system`.

**System actions.** The preview reaper appends `environment.expire` with actor `system` only for a destroy it starts itself. Its destroy operation's outcome follows, as for any operation.

**If the append fails** (the database is down), forge-api logs the full event at ERROR level as JSON, with the message "audit event not stored", and returns the call's own result unchanged. The catalog shares the database, so in that state nearly every write already fails with `Unavailable`, and the log still records the attempt.

**Nothing missed.** A test walks every method of every `forge.v1` service in the generated descriptors. It fails if a method is in neither the audit table nor an explicit list of reads, or if it is in both. The list of reads names every read method, not a pattern: today GetService, ListServices, ListTemplates, GetTemplate, GetEnvironment, ListEnvironments, GetDeployment, ListDeployments, ListDeployHistory, GetOperation, ListOperations, WatchOperation, GetServerInfo and GetStatus, plus ListAuditEvents and GetAuditEvent.

## 3. API and CLI

**`proto/forge/v1/audit.proto`**, service `AuditService`:
- `ListAuditEvents(ListAuditEventsRequest) returns (ListAuditEventsResponse)`.
  - Filters: `service`, `actor` (an email or a repository `<org>/<repo>`), `action`, `failed_only`, `since`, `until` (timestamps).
  - Paging: `page_size` (default 50, max 500) and `page_token`; the response has `next_page_token`.
  - Order: newest first. The token holds the last id, so new events never shift a page.
- `GetAuditEvent(GetAuditEventRequest{name}) returns (AuditEvent)`.
- Both need `services.read`. Neither is a workload procedure, and neither is audited.
- `AuditEvent` carries the fields from section 1, with `result.code` as a string.

**CLI** (`internal/cli/audit.go`, noun `audit`):
- **`forge audit list`:**
  - Flags: `--service`, `--actor`, `--action`, `--failed`, `--since` (a duration such as `24h` or `7d`, or an RFC 3339 time), `--until`, `--limit` (default 50; follows pages up to the limit).
  - Table columns: `ID`, `TIME`, `ACTOR`, `ACTION`, `SERVICE`, `TARGET`, `RESULT`.
  - An `outcome` row's action starts with `↳ `.
  - The actor shown is the email, the repository, `system` or `anonymous`.
  - There is no default service from the repo's `forge.yaml`: audit queries are explicit.
- **`forge audit get <id | auditEvents/<id>>`:** every field, including details, the operation and the client.
- All formats as elsewhere: `-o table|json|yaml|name`.
- The client goes in `pkg/client`, as for the other services.

**Tests:**
- server tests: each filter, paging while new events arrive, permissions (reader yes, workload no);
- interceptor tests: refused calls (no token, wrong permission), a validate-only call, a streaming create, an operation's outcome, the reaper's `system` event, and an append failure (logged, result unchanged);
- the completeness test from section 2;
- CLI harness tests, with golden files for the table and the detail view.

## 4. Rollout and the exit gate

**Rollout:**
- **What changes:** a new forge-api image only.
  - **Manifests:** no change.
  - **Migration:** `0002_audit.sql` runs at startup, under the existing migration lock.
  - **Chart:** stays pinned at `18b12326d3df2c1a11b9f664f378b579eb472aaf`.
- **Order, as usual:**
  1. Render with only `FORGE_API_IMAGE` changed.
  2. Run a server dry run.
  3. Read the `kubectl diff`; it should show only the image.
  4. Apply, with a rollback file kept to hand.
- **Rolling update:** for the few seconds both pods serve, writes that reach the old pod are not audited. Accepted.
- **Rollback:** the previous image `9d310f0` ignores `audit_events`, and the migration stays.

**Exit gate on k3s:**
1. `forge create audit-gate-<date>`. Its CI tests, builds and deploys to staging as the workload `showmino/audit-gate-<date>`.
2. As the owner:
   - `forge env create` and `forge env destroy`;
   - `forge deploy promote` and `forge deploy rollback` in production;
   - `forge service update audit-gate-<date> --display-name …`.
3. A write with no token, sent as a plain HTTP request to `RegisterService`. It must be recorded as actor `anonymous`, result `unauthenticated`.
4. `forge audit list --service audit-gate-<date>` must show every step:
   - each with its actor: the owner's email, the CI repository, `anonymous`;
   - each with its result;
   - an `outcome` row for the deploys and the environment operations.

   `forge audit get` on the promote shows its details and operation. `CreateService` streams its result instead of starting an operation, so its request row carries its final result.
5. Clean up as in earlier gates. The cleanup's own events are expected in the log.

**Docs:**
- README: an audit section.
- `docs/index.md`: Phase 5 milestone 1 built; exit gate pending.
- `.claude/skills/forge/SKILL.md`: the status.

## Risks

- **Volume:** one row per write call. At this org's scale that's hundreds a day, a few MB a year. A retention job comes when it matters.
- **Integrity:** the trigger makes the table append-only for the API and for mistakes, not against a database administrator. Shipping events to a separate sink would close that gap, and is out of scope.
- **Database down:** events are then only in forge-api's logs, as described in section 2.
- **Overlap at rollout:** a few seconds of unaudited writes on the old pod.

## Known limits

- **Undecodable calls:** a write call whose body Connect cannot decode (malformed JSON, a wrong content type) is rejected before the interceptors, so it is not recorded. Nothing can change on that path.
- **Missing service:** some refused calls carry no service, because the lookup comes after the checks: a permission-denied destroy or extend of an environment, a cancel of an operation, and a refused `CreateService`. So `--service` can miss them.
- **Restarts:** an operation interrupted by a forge-api restart gets no outcome row.
- **Anonymous flood:** stored events from anonymous callers are limited to 60 a minute; the rest are only logged (a warning with the running count).
- **Refused CI token:** a CI token that is valid but refused (a feature branch, say) is recorded as `anonymous`, with the repository in the message.

## Out of scope

- Resources, the OpenTofu runner and policy (milestone 2).
- Exporting events to an external sink.
- Auditing reads.
- Retention jobs.
- Showing events on service pages (the Phase 6 dashboard will).
