# Phase 5 Milestone 2, Plan 1: resources API and policy — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a writer plan and apply the Postgres database a service declares in `forge.yaml` through the Forge API and `forge resource`, with every plan checked by Rego policy and audited, working end to end against a fake runner.

**Architecture:**
- **API:** a new `ResourceService` (plan, apply, list, get) in forge-api. A plan and an apply are Operations.
- **Runs:** forge-api never runs OpenTofu. It starts a run through a `run.Launcher` and the runner calls back through `run.API` with a one-time token. This plan builds both sides in Go and a fake runner (`envtest.Runner`); Plan 2 adds the real Job launcher, the HTTP transport and the runner program.
- **Storage:** plans, resource records and OpenTofu state go in a `ResourceStore` (memory or PostgreSQL, migration `0003`).
- **Policy:** Rego rules in `policy/`, embedded in the binary, evaluated in process on the plan JSON.
- **One outcome per run:** a run ends through one conditional store transition; whoever makes it appends the audit outcome.

**Tech Stack:** Go 1.27, ConnectRPC, buf, pgx v5, Cobra, protojson, Open Policy Agent `github.com/open-policy-agent/opa v1.21.1`.

**Spec:** `docs/specs/2026-10-10-phase-5-m2-resources-policy-design.md`

**Checked:** the code and tests of Tasks 1 to 9 were applied to a throwaway copy of the repo on 2026-10-10. There, `go test -race ./...` (with PostgreSQL) and `make lint` pass, and the golden files come out as Task 9 shows them. If a step fails as written, look for a slip in copying before changing the design.

**Not in this plan (Plan 2):** the Kubernetes Job launcher, the runner HTTP API and second listener, the `forge-runner` program and image, the OpenTofu module, `database.secret` in deploy values and the chart, the k3s manifests, `forge-api` flags for resources. Plan 3 is the rollout and exit gate. In this plan `cmd/forge-api` does not turn resources on.

## As built

Reviews during execution changed the code in these places. Where the task text below differs, the code and this list are right.

- **Task 3:** `PutResourceState` in PostgreSQL is one `INSERT ... ON CONFLICT DO UPDATE ... WHERE` statement, so a write cannot get past a lock taken just before it.
- **Task 4:** the Rego rules were rewritten to fail closed (a total `field` helper; refusals for unreadable actions, an unknown environment, a non-list of changes, fewer than 1 instance; both ends of a replace checked). `Evaluate` returns an error for a plan that is not a JSON object. The namespace refusal quotes the namespace.
- **Task 6:** runner text is cleaned before it is stored (`cleanChanges`, and policy messages in `evaluate`); launcher `Stop` and `Status` errors are logged; `PlanResources` also refuses while the store shows a run still going for the target (`storedRun`).
- **Task 7:** `ApplyResourcePlan` repeats its checks under the lock, refuses a plan whose service left the catalog, uses `storedRun`, and a dry run never denies a plan. After an apply, records are made to match the plan's declared resources (`applyResources`), so a plan with no changes can be applied when records are out of line (`recordsInSync`). A record keeps its Secret name when a run reports none.
- **Task 8:** the sweep pages through plans (`ResourceOptions.SweepPage`) and stops the runner of a run it ends.
- **Final review:** plan lists carry no plan files (and migration 0003 has a partial index on open plans); a reader's refused apply is audited under its service; the runner API refuses an unlock with no lock id.

### Left for plan 2 by the reviews

The whole-branch review sorted the smaller findings of the task reviews. These are the ones to take up in plan 2 (the runner); `.claude/skills/forge/SKILL.md` lists the larger obligations.

- **Policy:** `actions: ["forget"]` on a database, and any change with no `after` and no `delete`, pass without `allow_destroy`. Close both with one rule that whitelists actions, when the captured fixtures arrive. No rule ties the plan's clusters to the declared resources (name, size, count). The rules read `input.forge.environment` and `service` directly, so they are silent if the key is absent (the Go engine always sends it).
- **Store:** `LockResourceState` in PostgreSQL can return a nil holder together with `ErrStateLocked`; the HTTP 423 answer must cope.
- **Runs:** `Stop` is not called when `Start` fails after creating the runner, and a cancel during `Start` ends as `launch_failed`. There is no test that races `Complete` against a cancel or the deadline. `storedRun` reads at most 10 open plans, and it is checked outside the start transaction, so two forge-api processes in a rolling update could both start a run. The watch deadline is a fresh timer, not the token's deadline.
- **Apply and records:** nothing is logged when a run omits the Secret of a resource that has one. `withHealth` logs a warning per resource and reads serially with no timeout. No test covers a policy evaluation error at apply or a namespace failure. A repair plan (no changes, records out of line) says so only in an operation event.
- **CLI:** `apply --env --yes` and `--dry-run` do not name the plan before the call; with a plan name and another service's manifest the `forge.yaml` check is skipped silently; `planText` prints server text without the output sanitiser; apply and get have no golden files.
- **Other:** the catalog's delete guard is off when resources are off; failure text keeps the head of the runner's output, not the tail, and loses newlines; `requests` (request ids of plans) is never pruned.

## Global Constraints

- **Exact names:**
  - resource names `resources/<service>/<env>/<name>`; plan names `resourcePlans/<id>`, id = 12 lowercase hex characters;
  - operation kinds and audit actions `resource.plan` and `resource.apply`; operation and audit target `resources/<service>/<env>`;
  - plan states `planning`, `planned`, `denied`, `failed`, `applying`, `applied`, `apply_failed`, `expired`; resource states `applying`, `ready`, `failed`, `deleting`;
  - change actions `create`, `update`, `delete`, `replace`;
  - operation error codes `policy_denied`, `run_failed`, `run_timeout`, `runner_failed`, `launch_failed`, plus the existing `cancelled` and `internal`;
  - audit detail keys `environment`, `plan`, `policy` (`passed` or `denied`), `policy_rules`, `allow_destroy`, `changes` (`create=1 update=0 delete=0`), `validate_only`.
- **Environments:** `staging` and `production` only.
- **Manifest rules:** `name` is a lowercase DNS label of at most 30 characters, unique; `type` is `postgres`; at most one `postgres` entry; `storage` is `<n>Gi` with n ≥ 1, default `1Gi`; `instances` is 1 to 3, default 1 in staging and 2 in production; `environments` keys are `staging` and `production`.
- **Permissions:**
  - `ListResources`, `GetResource`, `ListResourcePlans`: `services.read`; not for CI.
  - `GetResourcePlan`: `services.read`, or a CI workload for its own service's plans.
  - `PlanResources`: `services.write`, or a CI workload for its own service, never with `allow_destroy`.
  - `ApplyResourcePlan`: `services.write`; never CI.
  - Only `PlanResources` and `GetResourcePlan` join `workloadProcedures`.
- **Policy:**
  - Rules are in package `forge.resources`, in `policy/*.rego`, embedded.
  - A rule set that does not compile is an error from `policy.New`. An evaluation error never lets a plan through.
  - Limits: storage at most 20Gi in staging and 50Gi in production; production at least 2 instances; at most 3 instances; storage class `longhorn-retain`; no shrinking; only CloudNativePG clusters in `<service>-<env>`; deletes need `allow_destroy`.
  - The fingerprint is the first 12 hex characters of the SHA-256 of the rule files.
- **Plans:** used once; expire 24 hours after creation; a run has 15 minutes; one run at a time per service and environment.
- **Secrets:**
  - A run token is 32 random bytes, hex. forge-api stores only its SHA-256 and never logs the token.
  - The API never returns the plan file or the plan JSON.
  - Never log or print the database URL.
- **Caller text:** text from the runner (events, errors) goes through `auditText` before it is stored or emitted.
- **When resources are off:** a nil `*Resources` answers every call with `failed_precondition` and the message `resources are not configured on this Forge API`.
- **Tests and tooling:**
  - `go test ./...` passes with no database. Postgres tests run when `FORGE_TEST_DATABASE_URL` is set (local: `make db`, then `export FORGE_TEST_DATABASE_URL='postgres://postgres:forge@localhost:55432/postgres?sslmode=disable'`).
  - Go is at `~/.local/go/bin`, tools at `~/go/bin`: `export PATH=$HOME/.local/go/bin:$HOME/go/bin:$PATH`.
  - Lint with `make lint` (golangci-lint v2.14.0 and `buf lint`), then `make test`.
  - After proto changes run `make generate` and commit `gen/`.
  - Golden files: `make update-golden`, then read the diff.
- **The cluster is production:** this plan needs no cluster. Do not run `kubectl` except read-only commands.
- **Commits:** end every commit message with `Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>`. Work on branch `phase-5-resources`.

## Review Focus

1. **A run reported twice, late, or with another run's token.** `Complete` after the run ended, or with a wrong or expired token, is refused and adds no second outcome event (Task 6: `TestRunTokens`, `TestCompleteOnce`).
2. **Plan JSON that is empty, not JSON, or huge.** The plan becomes `failed`, never `planned` (Task 6: `TestPlanUnreadableResult`).
3. **Odd `forge.yaml` values.** `storage: 5`, `5GB`, `0Gi`, a 20-digit size, negative instances, a `prod` environment key, an empty list entry and a duplicate name are each refused with a message that names the field (Task 2: `TestResolveRejects`).
4. **Apply sent twice.** Two applies of one plan give one run; the second is refused and the plan is used once (Task 7: `TestApplyOnce`).
5. **A run cancelled midway.** `forge op cancel` stops the runner, marks the plan failed, frees the target for a new plan, and writes one `cancelled` outcome (Task 6: `TestPlanCancel`).

## File structure

| File | Responsibility |
| --- | --- |
| `proto/forge/v1/resource.proto` | `ResourceService` and its messages |
| `pkg/client/client.go` | `Client.Resources` |
| `internal/resource/resource.go` | Validate a declared list; work out settings per environment; digest |
| `internal/resource/changes.go` | Read the changes out of an OpenTofu plan JSON |
| `internal/manifest/manifest.go` | `resources` in `forge.yaml` |
| `internal/store/resource.go` | `ResourceStore`, `ResourceTx`, `PlanRecord`, `PlanQuery`, `ResourceState` |
| `internal/store/resource_memory.go`, `resource_postgres.go` | The two implementations |
| `internal/store/migrations/0003_resources.sql` | Tables `resources`, `resource_plans`, `resource_states` |
| `policy/policy.go`, `policy/resources.rego` | The policy engine and the starting rules |
| `internal/run/run.go` | Types shared by forge-api, the fake runner and (Plan 2) the real runner: `Launcher`, `API`, `Spec`, `Work`, `Result` |
| `internal/envtest/runner.go` | `envtest.Runner`, a fake launcher that plays the runner and OpenTofu |
| `internal/server/resources.go` | `Resources`: options, access, `PlanResources`, `GetResourcePlan`, `ListResourcePlans` |
| `internal/server/resource_run.go` | Starting and watching a run, ending it once, the `run.API` methods, the outcome event |
| `internal/server/resource_apply.go` | `ApplyResourcePlan`, `ListResources`, `GetResource` |
| `internal/server/resource_sweep.go` | Deadlines and expiry |
| `internal/server/operations.go`, `audit.go`, `authn.go`, `catalog.go`, `server.go` | Small changes: no outcome for resource operations, audit helpers and actions, CI procedures, the delete guard, wiring |
| `internal/cli/resource.go` | `forge resource plan`, `apply`, `list`, `get` |

---

### Task 1: ResourceService proto, client, and audit classification

**Files:**
- Create: `proto/forge/v1/resource.proto`
- Create: `internal/server/resource_names.go`
- Modify: `pkg/client/client.go` (the `Client` struct and `New`)
- Modify: `internal/server/audit.go` (`auditActions`, `auditReads`)
- Modify: `internal/server/audit_test.go` (one new test)
- Generated: `gen/forge/v1/resource.pb.go`, `gen/forge/v1/forgev1connect/resource.connect.go`

**Interfaces:**
- Consumes: nothing from other tasks.
- Produces:
  - Go types `forgev1.ResourceDeclaration`, `ResourceOverride`, `ResourceSettings`, `Resource`, `ResourceHealth`, `ResourceChange`, `PolicyResult`, `ResourcePlan` and the request/response messages below.
  - Enums `forgev1.Resource_STATE_APPLYING|READY|FAILED|DELETING` and `forgev1.ResourcePlan_STATE_PLANNING|PLANNED|DENIED|FAILED|APPLYING|APPLIED|APPLY_FAILED|EXPIRED`.
  - `forgev1connect.ResourceServiceHandler`, `ResourceServiceClient`, and the six `ResourceService…Procedure` constants.
  - `client.Client.Resources forgev1connect.ResourceServiceClient`.
  - Constants in package `server`: `resourceNamePrefix = "resources/"`, `planNamePrefix = "resourcePlans/"`, `opResourcePlan = "resource.plan"`, `opResourceApply = "resource.apply"`.

- [ ] **Step 1: Write the proto**

Create `proto/forge/v1/resource.proto`:

```proto
syntax = "proto3";

package forge.v1;

import "forge/v1/operation.proto";
import "google/protobuf/timestamp.proto";

// ResourceService plans and applies the resources a service declares in
// forge.yaml. A plan is made by an OpenTofu run, checked against policy and
// saved; an apply runs exactly that plan. Both return an Operation.
// Names: "resources/{service}/{env}/{name}" and "resourcePlans/{id}".
service ResourceService {
  // PlanResources plans the declared resources for one environment.
  rpc PlanResources(PlanResourcesRequest) returns (PlanResourcesResponse) {}

  // GetResourcePlan returns one plan: its changes and policy result.
  rpc GetResourcePlan(GetResourcePlanRequest) returns (GetResourcePlanResponse) {
    option idempotency_level = NO_SIDE_EFFECTS;
  }

  // ListResourcePlans returns plans, newest first.
  rpc ListResourcePlans(ListResourcePlansRequest) returns (ListResourcePlansResponse) {
    option idempotency_level = NO_SIDE_EFFECTS;
  }

  // ApplyResourcePlan applies a saved plan.
  rpc ApplyResourcePlan(ApplyResourcePlanRequest) returns (ApplyResourcePlanResponse) {}

  // ListResources returns resources, by name.
  rpc ListResources(ListResourcesRequest) returns (ListResourcesResponse) {
    option idempotency_level = NO_SIDE_EFFECTS;
  }

  // GetResource returns one resource.
  rpc GetResource(GetResourceRequest) returns (GetResourceResponse) {
    option idempotency_level = NO_SIDE_EFFECTS;
  }
}

// ResourceDeclaration is one entry of forge.yaml's resources list.
message ResourceDeclaration {
  string name = 1;
  // "postgres".
  string type = 2;
  // "<n>Gi"; empty means the default.
  string storage = 3;
  // 0 means the default.
  int32 instances = 4;
  // Overrides per environment, keyed "staging" or "production".
  map<string, ResourceOverride> environments = 5;
}

// ResourceOverride replaces a declaration's values in one environment.
message ResourceOverride {
  string storage = 1;
  int32 instances = 2;
}

// ResourceSettings are a declaration worked out for one environment.
message ResourceSettings {
  string name = 1;
  string type = 2;
  int32 storage_gi = 3;
  int32 instances = 4;
}

// Resource is one resource of a service in one environment.
message Resource {
  // State is how the last apply left the resource.
  enum State {
    STATE_UNSPECIFIED = 0;
    STATE_APPLYING = 1;
    STATE_READY = 2;
    STATE_FAILED = 3;
    STATE_DELETING = 4;
  }

  // "resources/{service}/{env}/{name}".
  string name = 1;
  // "services/{service}".
  string service = 2;
  // "staging" or "production".
  string env = 3;
  string type = 4;
  ResourceSettings settings = 5;
  State state = 6;
  // Read live when the resource is listed or fetched.
  ResourceHealth health = 7;
  // The Secret with the connection string, in the service's namespace.
  string secret = 8;
  // The plan that last changed it.
  string last_plan = 9;
  google.protobuf.Timestamp create_time = 10;
  google.protobuf.Timestamp update_time = 11;
}

// ResourceHealth is what the cluster reports for a resource now.
message ResourceHealth {
  // The operator's phase text, or "unknown" when it cannot be read.
  string phase = 1;
  int32 ready_instances = 2;
  int32 instances = 3;
}

// ResourceChange is one thing a plan would do.
message ResourceChange {
  // "create", "update", "delete" or "replace".
  string action = 1;
  // The resource's short name, or the plan address when it is not a known resource.
  string resource = 2;
  string type = 3;
  // Unset on create.
  ResourceSettings before = 4;
  // Unset on delete.
  ResourceSettings after = 5;
}

// PolicyResult is what the rules said about a plan.
message PolicyResult {
  bool denied = 1;
  repeated string deny = 2;
  repeated string warn = 3;
  // The fingerprint of the rule set that decided.
  string rules = 4;
}

// ResourcePlan is a saved plan for one service and environment.
message ResourcePlan {
  // State is where the plan is in its life.
  enum State {
    STATE_UNSPECIFIED = 0;
    STATE_PLANNING = 1;
    STATE_PLANNED = 2;
    STATE_DENIED = 3;
    STATE_FAILED = 4;
    STATE_APPLYING = 5;
    STATE_APPLIED = 6;
    STATE_APPLY_FAILED = 7;
    STATE_EXPIRED = 8;
  }

  // "resourcePlans/{id}".
  string name = 1;
  // "services/{service}".
  string service = 2;
  string env = 3;
  State state = 4;
  // The declared resources, worked out for env.
  repeated ResourceSettings resources = 5;
  repeated ResourceChange changes = 6;
  // Unset until the plan has been checked.
  PolicyResult policy = 7;
  bool allow_destroy = 8;
  // Who made it: an email, or a CI subject.
  string creator = 9;
  // The newest operation on it.
  string operation = 10;
  google.protobuf.Timestamp create_time = 11;
  google.protobuf.Timestamp expire_time = 12;
  google.protobuf.Timestamp apply_time = 13;
  // Why it failed, when it did.
  string message = 14;
  // Who applied it.
  string applier = 15;
}

message PlanResourcesRequest {
  // "services/{service}".
  string service = 1;
  // "staging" or "production".
  string env = 2;
  // forge.yaml's resources list. Empty plans the removal of everything.
  repeated ResourceDeclaration resources = 3;
  // Let the plan delete resources. Writers only.
  bool allow_destroy = 4;
  // Retries with the same request_id return the same operation.
  string request_id = 5;
}

message PlanResourcesResponse {
  Operation operation = 1;
  ResourcePlan plan = 2;
}

message GetResourcePlanRequest {
  // "resourcePlans/{id}".
  string name = 1;
}

message GetResourcePlanResponse {
  ResourcePlan plan = 1;
}

message ListResourcePlansRequest {
  // Only this service, "services/{service}".
  string service = 1;
  // Only this environment.
  string env = 2;
  // Only these states; empty means any.
  repeated ResourcePlan.State states = 3;
  // Only plans the caller made.
  bool mine = 4;
  // At most this many (default 50, at most 500).
  int32 page_size = 5;
  string page_token = 6;
}

message ListResourcePlansResponse {
  repeated ResourcePlan plans = 1;
  string next_page_token = 2;
}

message ApplyResourcePlanRequest {
  // "resourcePlans/{id}".
  string name = 1;
  // forge.yaml's resources list now, compared with the plan's when
  // check_resources is set.
  repeated ResourceDeclaration resources = 2;
  bool check_resources = 3;
  // Run every check and stop before the run.
  bool validate_only = 4;
}

message ApplyResourcePlanResponse {
  // Unset when validate_only.
  Operation operation = 1;
  ResourcePlan plan = 2;
}

message ListResourcesRequest {
  // Only this service, "services/{service}".
  string service = 1;
  // Only this environment.
  string env = 2;
}

message ListResourcesResponse {
  repeated Resource resources = 1;
}

message GetResourceRequest {
  // "resources/{service}/{env}/{name}".
  string name = 1;
}

message GetResourceResponse {
  Resource resource = 1;
}
```

- [ ] **Step 2: Generate and build**

Run: `export PATH=$HOME/.local/go/bin:$HOME/go/bin:$PATH && make generate && go build ./... && buf lint`
Expected: no output from `go build` and `buf lint`; `gen/forge/v1/resource.pb.go` and `gen/forge/v1/forgev1connect/resource.connect.go` exist.

- [ ] **Step 3: Run the audit completeness test and see it fail**

Run: `go test ./internal/server -run TestAuditCoversEveryProcedure`
Expected: FAIL with six lines like `/forge.v1.ResourceService/PlanResources is neither in auditActions nor in auditReads: classify it`.

- [ ] **Step 4: Add the name constants**

Create `internal/server/resource_names.go`:

```go
package server

// Names and kinds of the resource API.
const (
	resourceNamePrefix = "resources/"
	planNamePrefix     = "resourcePlans/"
	opResourcePlan     = "resource.plan"
	opResourceApply    = "resource.apply"
)
```

- [ ] **Step 5: Write the failing test for the two audit actions**

Append to `internal/server/audit_test.go`:

```go
func TestAuditDescribesResourceCalls(t *testing.T) {
	t.Parallel()
	plan := auditActions[forgev1connect.ResourceServicePlanResourcesProcedure]
	svc, target, det := plan.describe(&forgev1.PlanResourcesRequest{Service: "payments", Env: "staging", AllowDestroy: true})
	if plan.name != "resource.plan" || svc != "services/payments" || target != "resources/payments/staging" {
		t.Errorf("plan: %q %q %q", plan.name, svc, target)
	}
	if det["environment"] != "staging" || det["allow_destroy"] != "true" {
		t.Errorf("plan details: %v", det)
	}
	if _, _, det := plan.describe(&forgev1.PlanResourcesRequest{Service: "services/payments", Env: "production"}); det["allow_destroy"] != "" {
		t.Errorf("allow_destroy must be absent when false: %v", det)
	}

	apply := auditActions[forgev1connect.ResourceServiceApplyResourcePlanProcedure]
	svc, target, det = apply.describe(&forgev1.ApplyResourcePlanRequest{Name: "resourcePlans/0123456789ab", ValidateOnly: true})
	if apply.name != "resource.apply" || svc != "" || target != "resourcePlans/0123456789ab" {
		t.Errorf("apply: %q %q %q", apply.name, svc, target)
	}
	if det["plan"] != "resourcePlans/0123456789ab" || det["validate_only"] != "true" {
		t.Errorf("apply details: %v", det)
	}
}
```

Run: `go test ./internal/server -run TestAuditDescribesResourceCalls`
Expected: FAIL (a nil `describe` panics).

- [ ] **Step 6: Classify the six procedures**

In `internal/server/audit.go`, add to `auditActions`, after the `OperationServiceCancelOperationProcedure` entry:

```go
	forgev1connect.ResourceServicePlanResourcesProcedure: {opResourcePlan, func(m any) (string, string, map[string]string) {
		r := m.(*forgev1.PlanResourcesRequest)
		s := serviceName(r.GetService())
		return s, resourceNamePrefix + strings.TrimPrefix(s, servicePrefix) + "/" + r.GetEnv(),
			details("environment", r.GetEnv(), "allow_destroy", flag(r.GetAllowDestroy()))
	}},
	// The request names only the plan; the handler adds the service, the
	// target and the environment once it has read the plan.
	forgev1connect.ResourceServiceApplyResourcePlanProcedure: {opResourceApply, func(m any) (string, string, map[string]string) {
		r := m.(*forgev1.ApplyResourcePlanRequest)
		return "", r.GetName(), details("plan", r.GetName(), "validate_only", flag(r.GetValidateOnly()))
	}},
```

and to `auditReads`, after the two `AuditService` entries:

```go
	forgev1connect.ResourceServiceGetResourcePlanProcedure,
	forgev1connect.ResourceServiceListResourcePlansProcedure,
	forgev1connect.ResourceServiceListResourcesProcedure,
	forgev1connect.ResourceServiceGetResourceProcedure,
```

- [ ] **Step 7: Add the client**

In `pkg/client/client.go`, add to the `Client` struct after `Audit`:

```go
	Resources    forgev1connect.ResourceServiceClient
```

and in `New`, after the `Audit:` line:

```go
		Resources:    forgev1connect.NewResourceServiceClient(hc, baseURL, opts...),
```

- [ ] **Step 8: Run the tests**

Run: `go test ./internal/server -run 'TestAudit' && go build ./... && make lint`
Expected: PASS, and lint is clean.

- [ ] **Step 9: Commit**

```bash
git add proto gen pkg/client internal/server/resource_names.go internal/server/audit.go internal/server/audit_test.go
git commit -m "proto: ResourceService, its client, and its audit actions

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 2: Declared resources: validation, settings per environment, plan changes, the manifest

**Files:**
- Create: `internal/resource/resource.go`, `internal/resource/changes.go`
- Create: `internal/resource/resource_test.go`, `internal/resource/changes_test.go`
- Modify: `internal/manifest/manifest.go`
- Modify: `internal/manifest/manifest_test.go`

**Interfaces:**
- Consumes: `forgev1.ResourceDeclaration`, `ResourceOverride`, `ResourceSettings`, `ResourceChange` (Task 1).
- Produces, in package `resource` (`github.com/showmino/forge/internal/resource`):
  - `const TypePostgres = "postgres"`
  - `func Validate(decls []*forgev1.ResourceDeclaration) error`
  - `func Resolve(decls []*forgev1.ResourceDeclaration, env string) ([]*forgev1.ResourceSettings, error)`: settings sorted by name
  - `func Digest(settings []*forgev1.ResourceSettings) string`
  - `func Changes(planJSON []byte) ([]*forgev1.ResourceChange, error)`: sorted by resource, then action
  - `func Count(changes []*forgev1.ResourceChange) string`: `create=1 update=0 delete=0`, with ` replace=N` added when N > 0
  - `func Destructive(changes []*forgev1.ResourceChange) bool`
- Produces, in package `manifest`: `Manifest.Resources []Resource`, types `Resource` and `ResourceOverride`, `func (m *Manifest) Declarations() []*forgev1.ResourceDeclaration`.

- [ ] **Step 1: Write the failing tests for Resolve, Validate and Digest**

Create `internal/resource/resource_test.go`:

```go
package resource

import (
	"strings"
	"testing"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
)

func decl(name, storage string, instances int32) *forgev1.ResourceDeclaration {
	return &forgev1.ResourceDeclaration{Name: name, Type: TypePostgres, Storage: storage, Instances: instances}
}

func TestResolveDefaultsAndOverrides(t *testing.T) {
	t.Parallel()
	d := decl("main", "5Gi", 0)
	d.Environments = map[string]*forgev1.ResourceOverride{"production": {Storage: "20Gi", Instances: 3}}

	stg, err := Resolve([]*forgev1.ResourceDeclaration{d}, "staging")
	if err != nil {
		t.Fatal(err)
	}
	if len(stg) != 1 || stg[0].GetName() != "main" || stg[0].GetType() != "postgres" || stg[0].GetStorageGi() != 5 || stg[0].GetInstances() != 1 {
		t.Errorf("staging: %v", stg)
	}
	prod, err := Resolve([]*forgev1.ResourceDeclaration{d}, "production")
	if err != nil {
		t.Fatal(err)
	}
	if prod[0].GetStorageGi() != 20 || prod[0].GetInstances() != 3 {
		t.Errorf("production: %v", prod)
	}

	// No storage and no instances: 1Gi, and 1 instance in staging, 2 in production.
	bare := []*forgev1.ResourceDeclaration{decl("main", "", 0)}
	if s, _ := Resolve(bare, "staging"); s[0].GetStorageGi() != 1 || s[0].GetInstances() != 1 {
		t.Errorf("bare staging: %v", s)
	}
	if s, _ := Resolve(bare, "production"); s[0].GetStorageGi() != 1 || s[0].GetInstances() != 2 {
		t.Errorf("bare production: %v", s)
	}

	// An override with only one value keeps the other from the top level.
	half := decl("main", "5Gi", 2)
	half.Environments = map[string]*forgev1.ResourceOverride{"production": {Instances: 3}, "staging": nil}
	if s, _ := Resolve([]*forgev1.ResourceDeclaration{half}, "production"); s[0].GetStorageGi() != 5 || s[0].GetInstances() != 3 {
		t.Errorf("half override: %v", s)
	}
	if s, err := Resolve([]*forgev1.ResourceDeclaration{half}, "staging"); err != nil || s[0].GetInstances() != 2 {
		t.Errorf("nil override: %v %v", s, err)
	}

	// An empty list is valid: it plans the removal of everything.
	if s, err := Resolve(nil, "staging"); err != nil || len(s) != 0 {
		t.Errorf("empty: %v %v", s, err)
	}
}

func TestResolveRejects(t *testing.T) {
	t.Parallel()
	over := func(env, storage string, instances int32) *forgev1.ResourceDeclaration {
		d := decl("main", "5Gi", 1)
		d.Environments = map[string]*forgev1.ResourceOverride{env: {Storage: storage, Instances: instances}}
		return d
	}
	for name, tc := range map[string]struct {
		decls []*forgev1.ResourceDeclaration
		want  string
	}{
		"storage without a unit":  {[]*forgev1.ResourceDeclaration{decl("main", "5", 1)}, `main: storage "5" must be whole gibibytes such as 5Gi`},
		"storage in GB":           {[]*forgev1.ResourceDeclaration{decl("main", "5GB", 1)}, `main: storage "5GB"`},
		"zero storage":            {[]*forgev1.ResourceDeclaration{decl("main", "0Gi", 1)}, `main: storage "0Gi"`},
		"a 20-digit size":         {[]*forgev1.ResourceDeclaration{decl("main", "99999999999999999999Gi", 1)}, `main: storage "99999999999999999999Gi"`},
		"fractional storage":      {[]*forgev1.ResourceDeclaration{decl("main", "1.5Gi", 1)}, `main: storage "1.5Gi"`},
		"negative instances":      {[]*forgev1.ResourceDeclaration{decl("main", "5Gi", -1)}, "main: instances -1 must be 1 to 3"},
		"four instances":          {[]*forgev1.ResourceDeclaration{decl("main", "5Gi", 4)}, "main: instances 4 must be 1 to 3"},
		"an unknown environment":  {[]*forgev1.ResourceDeclaration{over("prod", "5Gi", 1)}, "main: environments.prod must be staging or production"},
		"a bad override":          {[]*forgev1.ResourceDeclaration{over("production", "big", 2)}, `main: environments.production: storage "big"`},
		"a bad override instance": {[]*forgev1.ResourceDeclaration{over("staging", "5Gi", 9)}, "main: environments.staging: instances 9 must be 1 to 3"},
		"an empty entry":          {[]*forgev1.ResourceDeclaration{nil}, "entry 1 is empty"},
		"a duplicate name":        {[]*forgev1.ResourceDeclaration{decl("main", "", 0), decl("main", "", 0)}, `name "main" is used twice`},
		"two databases":           {[]*forgev1.ResourceDeclaration{decl("a", "", 0), decl("b", "", 0)}, "only one postgres resource per service is supported"},
		"an uppercase name":       {[]*forgev1.ResourceDeclaration{decl("Main", "", 0)}, `name "Main" must be a lowercase DNS label of at most 30 characters`},
		"a long name":             {[]*forgev1.ResourceDeclaration{decl(strings.Repeat("a", 31), "", 0)}, "must be a lowercase DNS label of at most 30 characters"},
		"no name":                 {[]*forgev1.ResourceDeclaration{decl("", "", 0)}, `name "" must be`},
		"another type":            {[]*forgev1.ResourceDeclaration{{Name: "cache", Type: "redis"}}, `cache: type "redis" must be postgres`},
		"no type":                 {[]*forgev1.ResourceDeclaration{{Name: "main"}}, `main: type "" must be postgres`},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			// A bad value is refused whichever environment is asked for.
			for _, env := range []string{"staging", "production"} {
				_, err := Resolve(tc.decls, env)
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Errorf("%s: %v, want it to contain %q", env, err, tc.want)
				}
			}
			if err := Validate(tc.decls); err == nil {
				t.Error("Validate accepted it")
			}
		})
	}
	if _, err := Resolve(nil, "dev"); err == nil || !strings.Contains(err.Error(), `environment "dev" must be staging or production`) {
		t.Errorf("unknown environment: %v", err)
	}
}

func TestDigest(t *testing.T) {
	t.Parallel()
	a := []*forgev1.ResourceSettings{{Name: "main", Type: "postgres", StorageGi: 5, Instances: 1}}
	b := []*forgev1.ResourceSettings{{Name: "main", Type: "postgres", StorageGi: 5, Instances: 1}}
	if Digest(a) != Digest(b) || len(Digest(a)) != 64 {
		t.Errorf("equal settings: %s %s", Digest(a), Digest(b))
	}
	b[0].StorageGi = 6
	if Digest(a) == Digest(b) {
		t.Error("a changed size must change the digest")
	}
	if Digest(nil) == Digest(a) {
		t.Error("the empty list must differ")
	}
}
```

- [ ] **Step 2: Run them and see them fail**

Run: `go test ./internal/resource`
Expected: FAIL to build (`undefined: Resolve`).

- [ ] **Step 3: Implement resource.go**

Create `internal/resource/resource.go`:

```go
// Package resource holds the rules for the resources a service declares in
// forge.yaml: what is valid, what a declaration means in one environment,
// and what an OpenTofu plan would change.
package resource

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
)

// TypePostgres is the one resource type of this version.
const TypePostgres = "postgres"

// Defaults and limits of a declaration. Policy sets the storage limits.
const (
	defaultStorageGi = 1
	maxInstances     = 3
)

var envs = []string{"staging", "production"}

var (
	nameRE    = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,28}[a-z0-9])?$`)
	storageRE = regexp.MustCompile(`^[1-9][0-9]{0,5}Gi$`)
)

func defaultInstances(env string) int32 {
	if env == "production" {
		return 2
	}
	return 1
}

// storageGi reads "<n>Gi"; "" is the default.
func storageGi(s string) (int32, error) {
	if s == "" {
		return defaultStorageGi, nil
	}
	if !storageRE.MatchString(s) {
		return 0, fmt.Errorf("storage %q must be whole gibibytes such as 5Gi", s)
	}
	n, err := strconv.ParseInt(strings.TrimSuffix(s, "Gi"), 10, 32)
	if err != nil {
		return 0, fmt.Errorf("storage %q must be whole gibibytes such as 5Gi", s)
	}
	return int32(n), nil
}

// instanceCount checks n; 0 is "use the default".
func instanceCount(n int32) error {
	if n < 0 || n > maxInstances {
		return fmt.Errorf("instances %d must be 1 to %d", n, maxInstances)
	}
	return nil
}

// Validate checks decls for every environment.
func Validate(decls []*forgev1.ResourceDeclaration) error {
	for _, env := range envs {
		if _, err := Resolve(decls, env); err != nil {
			return err
		}
	}
	return nil
}

// Resolve checks decls and returns their settings in env, sorted by name.
// Every value is checked, also those env does not use.
func Resolve(decls []*forgev1.ResourceDeclaration, env string) ([]*forgev1.ResourceSettings, error) {
	if !slices.Contains(envs, env) {
		return nil, fmt.Errorf("environment %q must be staging or production", env)
	}
	seen := map[string]bool{}
	postgres := 0
	out := make([]*forgev1.ResourceSettings, 0, len(decls))
	for i, d := range decls {
		if d == nil {
			return nil, fmt.Errorf("entry %d is empty", i+1)
		}
		name := d.GetName()
		if !nameRE.MatchString(name) {
			return nil, fmt.Errorf("name %q must be a lowercase DNS label of at most 30 characters", name)
		}
		if seen[name] {
			return nil, fmt.Errorf("name %q is used twice", name)
		}
		seen[name] = true
		if d.GetType() != TypePostgres {
			return nil, fmt.Errorf("%s: type %q must be postgres", name, d.GetType())
		}
		if postgres++; postgres > 1 {
			return nil, errors.New("only one postgres resource per service is supported")
		}
		gi, err := storageGi(d.GetStorage())
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if err := instanceCount(d.GetInstances()); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		instances := d.GetInstances()
		for _, k := range slices.Sorted(maps.Keys(d.GetEnvironments())) {
			if !slices.Contains(envs, k) {
				return nil, fmt.Errorf("%s: environments.%s must be staging or production", name, k)
			}
			o := d.GetEnvironments()[k]
			if o == nil {
				continue
			}
			ogi, err := storageGi(o.GetStorage())
			if err != nil {
				return nil, fmt.Errorf("%s: environments.%s: %w", name, k, err)
			}
			if err := instanceCount(o.GetInstances()); err != nil {
				return nil, fmt.Errorf("%s: environments.%s: %w", name, k, err)
			}
			if k != env {
				continue
			}
			if o.GetStorage() != "" {
				gi = ogi
			}
			if o.GetInstances() != 0 {
				instances = o.GetInstances()
			}
		}
		if instances == 0 {
			instances = defaultInstances(env)
		}
		out = append(out, &forgev1.ResourceSettings{Name: name, Type: TypePostgres, StorageGi: gi, Instances: instances})
	}
	slices.SortFunc(out, func(a, b *forgev1.ResourceSettings) int { return strings.Compare(a.GetName(), b.GetName()) })
	return out, nil
}

// Digest identifies a list of settings: equal lists have equal digests.
func Digest(settings []*forgev1.ResourceSettings) string {
	h := sha256.New()
	for _, s := range settings {
		fmt.Fprintf(h, "%s\x00%s\x00%d\x00%d\n", s.GetName(), s.GetType(), s.GetStorageGi(), s.GetInstances())
	}
	return hex.EncodeToString(h.Sum(nil))
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/resource`
Expected: PASS.

- [ ] **Step 5: Write the failing tests for Changes, Count and Destructive**

Create `internal/resource/changes_test.go`:

```go
package resource

import (
	"encoding/json"
	"fmt"
	"testing"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
)

// cluster is the object a kubernetes_manifest change holds for a database.
func cluster(name string, gi, instances int) map[string]any {
	return map[string]any{"manifest": map[string]any{
		"apiVersion": "postgresql.cnpg.io/v1", "kind": "Cluster",
		"metadata": map[string]any{"name": name, "namespace": "payments-staging"},
		"spec":     map[string]any{"instances": instances, "storage": map[string]any{"size": fmt.Sprintf("%dGi", gi), "storageClass": "longhorn-retain"}},
	}}
}

func change(address string, actions []string, before, after any) map[string]any {
	return map[string]any{"address": address, "type": "kubernetes_manifest",
		"change": map[string]any{"actions": actions, "before": before, "after": after}}
}

func planJSON(t *testing.T, changes ...map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"format_version": "1.2", "resource_changes": changes})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestChanges(t *testing.T) {
	t.Parallel()
	got, err := Changes(planJSON(t,
		change("kubernetes_manifest.database[\"main\"]", []string{"update"}, cluster("main", 5, 1), cluster("main", 10, 2)),
		change("kubernetes_manifest.database[\"old\"]", []string{"delete"}, cluster("old", 5, 1), nil),
		change("kubernetes_manifest.database[\"new\"]", []string{"create"}, nil, cluster("new", 1, 1)),
		change("kubernetes_manifest.database[\"swap\"]", []string{"delete", "create"}, cluster("swap", 5, 1), cluster("swap", 5, 1)),
		change("kubernetes_manifest.database[\"same\"]", []string{"no-op"}, cluster("same", 5, 1), cluster("same", 5, 1)),
	))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"update main 5/1 -> 10/2", "create new -> 1/1", "delete old 5/1 ->", "replace swap 5/1 -> 5/1"}
	if len(got) != len(want) {
		t.Fatalf("%d changes, want %d: %v", len(got), len(want), got)
	}
	for i, c := range got {
		line := c.GetAction() + " " + c.GetResource()
		if b := c.GetBefore(); b != nil {
			line += fmt.Sprintf(" %d/%d", b.GetStorageGi(), b.GetInstances())
		}
		line += " ->"
		if a := c.GetAfter(); a != nil {
			line += fmt.Sprintf(" %d/%d", a.GetStorageGi(), a.GetInstances())
		}
		if line != want[i] || c.GetType() != "postgres" {
			t.Errorf("change %d: %q (type %q), want %q", i, line, c.GetType(), want[i])
		}
	}
	if Count(got) != "create=1 update=1 delete=1 replace=1" || !Destructive(got) {
		t.Errorf("Count %q, Destructive %v", Count(got), Destructive(got))
	}
}

func TestChangesOddPlans(t *testing.T) {
	t.Parallel()
	for name, body := range map[string]string{"empty object": `{}`, "no changes": `{"resource_changes":[]}`} {
		got, err := Changes([]byte(body))
		if err != nil || len(got) != 0 {
			t.Errorf("%s: %v %v", name, got, err)
		}
	}
	if Count(nil) != "create=0 update=0 delete=0" || Destructive(nil) {
		t.Errorf("no changes: %q", Count(nil))
	}
	for name, body := range map[string]string{"empty": ``, "not JSON": `tofu`, "a list": `[]`, "a wrong field type": `{"resource_changes":"x"}`} {
		if _, err := Changes([]byte(body)); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}

	// Something that is not a database keeps its address and its plan type.
	other := change("null_resource.x", []string{"create"}, nil, map[string]any{"id": "1"})
	other["type"] = "null_resource"
	got, err := Changes(planJSON(t, other))
	if err != nil || len(got) != 1 || got[0].GetResource() != "null_resource.x" || got[0].GetType() != "null_resource" || got[0].GetAfter() != nil {
		t.Errorf("foreign change: %v %v", got, err)
	}
	if only := []*forgev1.ResourceChange{{Action: "create"}}; Destructive(only) {
		t.Error("a create is not destructive")
	}
}
```

- [ ] **Step 6: Run them and see them fail**

Run: `go test ./internal/resource`
Expected: FAIL to build (`undefined: Changes`).

- [ ] **Step 7: Implement changes.go**

Create `internal/resource/changes.go`:

```go
package resource

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
)

// planDoc is the part of `tofu show -json` Forge reads.
type planDoc struct {
	ResourceChanges []struct {
		Address string `json:"address"`
		Type    string `json:"type"`
		Change  struct {
			Actions []string  `json:"actions"`
			Before  *planSide `json:"before"`
			After   *planSide `json:"after"`
		} `json:"change"`
	} `json:"resource_changes"`
}

type planSide struct {
	Manifest *struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Metadata   struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Spec struct {
			Instances json.Number `json:"instances"`
			Storage   struct {
				Size string `json:"size"`
			} `json:"storage"`
		} `json:"spec"`
	} `json:"manifest"`
}

// database returns the name and settings of s when it is a CloudNativePG cluster.
func (s *planSide) database() (string, *forgev1.ResourceSettings) {
	if s == nil || s.Manifest == nil || s.Manifest.APIVersion != "postgresql.cnpg.io/v1" || s.Manifest.Kind != "Cluster" {
		return "", nil
	}
	m := s.Manifest
	gi, _ := strconv.ParseInt(strings.TrimSuffix(m.Spec.Storage.Size, "Gi"), 10, 32)
	n, _ := strconv.ParseInt(m.Spec.Instances.String(), 10, 32)
	return m.Metadata.Name, &forgev1.ResourceSettings{Name: m.Metadata.Name, Type: TypePostgres, StorageGi: int32(gi), Instances: int32(n)}
}

// action names what a plan's action list does; "" for nothing.
func action(actions []string) string {
	has := func(a string) bool { return slices.Contains(actions, a) }
	switch {
	case has("create") && has("delete"):
		return "replace"
	case has("create"):
		return "create"
	case has("delete"):
		return "delete"
	case has("update"):
		return "update"
	case len(actions) == 0, has("no-op"), has("read"):
		return ""
	default:
		return strings.Join(actions, "+")
	}
}

// Changes reads what a plan (the output of `tofu show -json`) would do,
// sorted by resource and then action. Changes that do nothing are left out.
func Changes(planJSON []byte) ([]*forgev1.ResourceChange, error) {
	if len(planJSON) == 0 {
		return nil, errors.New("the plan is empty")
	}
	var doc planDoc
	dec := json.NewDecoder(strings.NewReader(string(planJSON)))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("the plan is not readable: %w", err)
	}
	var out []*forgev1.ResourceChange
	for _, rc := range doc.ResourceChanges {
		act := action(rc.Change.Actions)
		if act == "" {
			continue
		}
		c := &forgev1.ResourceChange{Action: act, Resource: rc.Address, Type: rc.Type}
		beforeName, before := rc.Change.Before.database()
		afterName, after := rc.Change.After.database()
		if before != nil || after != nil {
			c.Type, c.Before, c.After = TypePostgres, before, after
			if c.Resource = afterName; c.Resource == "" {
				c.Resource = beforeName
			}
		}
		out = append(out, c)
	}
	slices.SortFunc(out, func(a, b *forgev1.ResourceChange) int {
		if c := strings.Compare(a.GetResource(), b.GetResource()); c != 0 {
			return c
		}
		return strings.Compare(a.GetAction(), b.GetAction())
	})
	return out, nil
}

// Count sums changes up: "create=1 update=0 delete=0", with replace=N when N > 0.
func Count(changes []*forgev1.ResourceChange) string {
	n := map[string]int{}
	for _, c := range changes {
		n[c.GetAction()]++
	}
	s := fmt.Sprintf("create=%d update=%d delete=%d", n["create"], n["update"], n["delete"])
	if n["replace"] > 0 {
		s += fmt.Sprintf(" replace=%d", n["replace"])
	}
	return s
}

// Destructive reports whether any change deletes or replaces a resource.
func Destructive(changes []*forgev1.ResourceChange) bool {
	return slices.ContainsFunc(changes, func(c *forgev1.ResourceChange) bool {
		return c.GetAction() == "delete" || c.GetAction() == "replace"
	})
}
```

- [ ] **Step 8: Run the tests**

Run: `go test ./internal/resource`
Expected: PASS.

- [ ] **Step 9: Write the failing manifest tests**

Append to `internal/manifest/manifest_test.go`:

```go
func TestResources(t *testing.T) {
	t.Parallel()
	m, err := Parse([]byte(`apiVersion: forge.dev/v1
service:
  name: payments
  team: billing
resources:
  - name: main
    type: postgres
    storage: 5Gi
    environments:
      production:
        storage: 20Gi
        instances: 2
`))
	if err != nil {
		t.Fatal(err)
	}
	d := m.Declarations()
	if len(d) != 1 || d[0].GetName() != "main" || d[0].GetType() != "postgres" || d[0].GetStorage() != "5Gi" || d[0].GetInstances() != 0 {
		t.Fatalf("declarations: %v", d)
	}
	if o := d[0].GetEnvironments()["production"]; o.GetStorage() != "20Gi" || o.GetInstances() != 2 {
		t.Errorf("override: %v", o)
	}

	// A manifest without resources has none, and writes none back.
	bare, err := Parse([]byte("apiVersion: forge.dev/v1\nservice:\n  name: payments\n  team: billing\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(bare.Declarations()) != 0 {
		t.Errorf("bare: %v", bare.Declarations())
	}
	out, err := bare.Marshal()
	if err != nil || strings.Contains(string(out), "resources") {
		t.Errorf("marshal: %s %v", out, err)
	}

	for name, body := range map[string]string{
		"a bad size":       "resources:\n  - {name: main, type: postgres, storage: 5GB}\n",
		"an unknown field": "resources:\n  - {name: main, type: postgres, version: 18}\n",
		"an unknown env":   "resources:\n  - {name: main, type: postgres, environments: {prod: {instances: 2}}}\n",
		"a huge instances": "resources:\n  - {name: main, type: postgres, instances: 99999999999}\n",
	} {
		_, err := Parse([]byte("apiVersion: forge.dev/v1\nservice:\n  name: payments\n  team: billing\n" + body))
		if err == nil {
			t.Errorf("%s: want an error", name)
		} else if name != "an unknown field" && !strings.Contains(err.Error(), "resources: ") {
			t.Errorf("%s: %v, want it to start with resources:", name, err)
		}
	}
}
```

If `strings` is not yet imported in `manifest_test.go`, add it.

Run: `go test ./internal/manifest -run TestResources`
Expected: FAIL to build (`m.Declarations undefined`).

- [ ] **Step 10: Add resources to the manifest**

In `internal/manifest/manifest.go`:

Add to the imports (keeping the groups: standard library, third party, then this module):

```go
	"math"
```

```go
	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/resource"
```

Change the package comment's first lines to:

```go
// Package manifest reads and writes forge.yaml, the per-repo source of truth
// for a service's catalog entry. Phase 1 covers the catalog fields; Phase 3
// adds run, how a preview environment runs the service; Phase 5 adds
// resources.
```

Add to the `Manifest` struct, after `Run`:

```go
	// Resources are what the service needs besides its own pods.
	Resources []Resource `yaml:"resources,omitempty"`
```

Add after the `RunHealth` method:

```go
// Resource is one entry of resources: today a Postgres database.
type Resource struct {
	Name string `yaml:"name"`
	Type string `yaml:"type"`
	// Storage is whole gibibytes, such as "5Gi" (default 1Gi).
	Storage string `yaml:"storage,omitempty"`
	// Instances is 1 to 3 (default 1 in staging, 2 in production).
	Instances int `yaml:"instances,omitempty"`
	// Environments overrides storage and instances per environment.
	Environments map[string]ResourceOverride `yaml:"environments,omitempty"`
}

// ResourceOverride replaces a resource's values in one environment.
type ResourceOverride struct {
	Storage   string `yaml:"storage,omitempty"`
	Instances int    `yaml:"instances,omitempty"`
}

// count turns a YAML int into the API's int32; a value that does not fit
// becomes -1, which validation refuses.
func count(n int) int32 {
	if n < 0 || n > math.MaxInt32 {
		return -1
	}
	return int32(n)
}

// Declarations returns resources as the API takes them.
func (m *Manifest) Declarations() []*forgev1.ResourceDeclaration {
	out := make([]*forgev1.ResourceDeclaration, 0, len(m.Resources))
	for _, r := range m.Resources {
		d := &forgev1.ResourceDeclaration{Name: r.Name, Type: r.Type, Storage: r.Storage, Instances: count(r.Instances)}
		for env, o := range r.Environments {
			if d.Environments == nil {
				d.Environments = map[string]*forgev1.ResourceOverride{}
			}
			d.Environments[env] = &forgev1.ResourceOverride{Storage: o.Storage, Instances: count(o.Instances)}
		}
		out = append(out, d)
	}
	return out
}
```

In `Validate`, before the final `return nil`:

```go
	if err := resource.Validate(m.Declarations()); err != nil {
		return fmt.Errorf("resources: %w", err)
	}
```

- [ ] **Step 11: Run the tests and lint**

Run: `go test ./internal/manifest ./internal/resource ./internal/cli ./internal/server && make lint`
Expected: PASS, lint clean. (The CLI and server packages read manifests; they must still pass.)

- [ ] **Step 12: Commit**

```bash
git add internal/resource internal/manifest
git commit -m "resource: validate declared resources, settings per environment, plan changes; manifest: resources

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 3: The resource store (memory, PostgreSQL, migration 0003)

**Files:**
- Create: `internal/store/resource.go`, `internal/store/resource_memory.go`, `internal/store/resource_postgres.go`
- Create: `internal/store/migrations/0003_resources.sql`
- Create: `internal/store/resource_suite_test.go`, `internal/store/resource_test.go`
- Modify: `internal/store/memory.go` (the `Memory` struct and `NewMemory`)
- Modify: `internal/store/audit.go` (`Backend`)
- Modify: `internal/store/postgres.go` (one lock key)
- Modify: `internal/store/postgres_test.go` (`TestPostgresMigrate` expects 3 rows)

**Interfaces:**
- Consumes: `forgev1.Resource`, `forgev1.ResourcePlan` (Task 1).
- Produces, in package `store`:

```go
var ErrStateLocked = errors.New("the state is locked")

type PlanRecord struct {
	Seq            int64                 // assigned by the store on first Put; higher is newer
	Plan           *forgev1.ResourcePlan // the public part; Plan.Name is the key
	CreatorSubject string
	Digest         string
	StateVersion   int64
	TokenHash      string    // "" when no run is active
	Deadline       time.Time // of the active run; zero when none
	PlanFile       []byte
	PlanJSON       []byte
	Audit          []byte // the active run's outcome event template, as protojson
}

type PlanQuery struct {
	Service        string // "services/<id>"; "" means any
	Env            string // "" means any
	States         []forgev1.ResourcePlan_State // empty means any
	CreatorSubject string // "" means any
	BeforeSeq      int64  // > 0: only plans with a lower Seq
	Limit          int    // at most this many; must be positive
}

type ResourceState struct {
	Data     []byte
	Version  int64 // counts writes; 0 when nothing was written yet
	LockID   string
	LockInfo []byte
}

type ResourceStore interface {
	GetResourcePlan(ctx context.Context, name string) (*PlanRecord, error)
	ResourcePlanByToken(ctx context.Context, tokenHash string) (*PlanRecord, error)
	ListResourcePlans(ctx context.Context, q PlanQuery) ([]*PlanRecord, error)
	GetResource(ctx context.Context, name string) (*forgev1.Resource, error)
	ListResources(ctx context.Context, service, env string) ([]*forgev1.Resource, error)
	WriteResources(ctx context.Context, fn func(tx ResourceTx) error) error
	GetResourceState(ctx context.Context, key string) (ResourceState, error)
	PutResourceState(ctx context.Context, key, lockID string, data []byte) (int64, error)
	LockResourceState(ctx context.Context, key, lockID string, info []byte) ([]byte, error)
	UnlockResourceState(ctx context.Context, key, lockID string) error
}

type ResourceTx interface {
	GetResourcePlan(ctx context.Context, name string) (*PlanRecord, error)
	PutResourcePlan(ctx context.Context, p *PlanRecord) error
	GetResource(ctx context.Context, name string) (*forgev1.Resource, error)
	PutResource(ctx context.Context, r *forgev1.Resource) error
	DeleteResource(ctx context.Context, name string) error
	ListResources(ctx context.Context, service, env string) ([]*forgev1.Resource, error)
}
```

  - `Backend` now also embeds `ResourceStore`; `*Memory` and `*Postgres` implement it.

- [ ] **Step 1: Write the interface file**

Create `internal/store/resource.go`:

```go
package store

import (
	"context"
	"errors"
	"time"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
)

// ErrStateLocked reports an OpenTofu state held by another lock.
var ErrStateLocked = errors.New("the state is locked")

// PlanRecord is a resource plan with what the API never shows: the plan
// files and the active run's token hash.
type PlanRecord struct {
	// Seq is assigned by the store when the plan is first put; a higher Seq is
	// a newer plan.
	Seq int64
	// Plan is the public part. Plan.Name ("resourcePlans/<id>") is the key.
	Plan *forgev1.ResourcePlan
	// CreatorSubject is the creator's token subject, for "my plans".
	CreatorSubject string
	// Digest identifies the settings the plan was made from.
	Digest string
	// StateVersion is the state's version when the plan was made.
	StateVersion int64
	// TokenHash is the SHA-256 of the active run's token, or "".
	TokenHash string
	// Deadline is when the active run must have ended; zero when none.
	Deadline time.Time
	// PlanFile is OpenTofu's saved plan; PlanJSON is `tofu show -json` of it.
	PlanFile []byte
	PlanJSON []byte
	// Audit is the active run's outcome event template, as protojson.
	Audit []byte
}

// PlanQuery selects plans for ListResourcePlans.
type PlanQuery struct {
	Service        string                       // "services/<id>"; "" means any
	Env            string                       // "" means any
	States         []forgev1.ResourcePlan_State // empty means any
	CreatorSubject string                       // "" means any
	BeforeSeq      int64                        // > 0: only plans with a lower Seq
	Limit          int                          // at most this many; must be positive
}

// ResourceState is the OpenTofu state of one service and environment.
type ResourceState struct {
	Data []byte
	// Version counts writes; 0 when nothing was written yet.
	Version  int64
	LockID   string
	LockInfo []byte
}

// ResourceStore keeps resource plans, resource records and OpenTofu state.
// Records are copied in and out: callers never share them with the store.
type ResourceStore interface {
	// GetResourcePlan returns the plan called name, or ErrNotFound.
	GetResourcePlan(ctx context.Context, name string) (*PlanRecord, error)
	// ResourcePlanByToken returns the plan whose TokenHash is tokenHash, or ErrNotFound.
	ResourcePlanByToken(ctx context.Context, tokenHash string) (*PlanRecord, error)
	// ListResourcePlans returns up to q.Limit matching plans, newest first.
	ListResourcePlans(ctx context.Context, q PlanQuery) ([]*PlanRecord, error)
	// GetResource returns the resource called name, or ErrNotFound.
	GetResource(ctx context.Context, name string) (*forgev1.Resource, error)
	// ListResources returns the resources of service ("services/<id>") and
	// env, sorted by name; "" means any.
	ListResources(ctx context.Context, service, env string) ([]*forgev1.Resource, error)
	// WriteResources runs fn in a transaction; these transactions never
	// overlap. What fn writes takes effect only if fn returns nil. fn's error
	// is returned unchanged. fn must use only tx.
	WriteResources(ctx context.Context, fn func(tx ResourceTx) error) error

	// GetResourceState returns the state under key; the zero value when there is none.
	GetResourceState(ctx context.Context, key string) (ResourceState, error)
	// PutResourceState stores data and returns the new version. It fails with
	// ErrStateLocked when the state is locked under another id.
	PutResourceState(ctx context.Context, key, lockID string, data []byte) (int64, error)
	// LockResourceState locks the state under lockID. When another id holds
	// it, it returns that holder's info and ErrStateLocked.
	LockResourceState(ctx context.Context, key, lockID string, info []byte) ([]byte, error)
	// UnlockResourceState releases the lock held by lockID; "" releases any
	// lock. Another id's lock fails with ErrStateLocked.
	UnlockResourceState(ctx context.Context, key, lockID string) error
}

// ResourceTx is a write transaction on plans and resources. Its reads see
// its own writes.
type ResourceTx interface {
	GetResourcePlan(ctx context.Context, name string) (*PlanRecord, error)
	// PutResourcePlan stores p under p.Plan.Name, replacing any plan there,
	// and sets p.Seq when the plan is new.
	PutResourcePlan(ctx context.Context, p *PlanRecord) error
	GetResource(ctx context.Context, name string) (*forgev1.Resource, error)
	// PutResource stores r under r.Name, replacing any resource there.
	PutResource(ctx context.Context, r *forgev1.Resource) error
	// DeleteResource removes the resource called name, if there is one.
	DeleteResource(ctx context.Context, name string) error
	ListResources(ctx context.Context, service, env string) ([]*forgev1.Resource, error)
}
```

In `internal/store/audit.go`, change `Backend` to:

```go
// Backend is everything forge-api keeps: the catalog, the audit log and resources.
type Backend interface {
	Store
	AuditStore
	ResourceStore
}
```

- [ ] **Step 2: Write the shared suite (failing)**

Create `internal/store/resource_suite_test.go`:

```go
package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
)

// runResourceSuite checks a ResourceStore. open returns an empty store per subtest.
func runResourceSuite(t *testing.T, open func(t *testing.T) ResourceStore) {
	ctx := context.Background()
	at := time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)
	plan := func(id, service, env string, state forgev1.ResourcePlan_State, subject string) *PlanRecord {
		return &PlanRecord{
			Plan: &forgev1.ResourcePlan{
				Name: "resourcePlans/" + id, Service: service, Env: env, State: state,
				Resources:  []*forgev1.ResourceSettings{{Name: "main", Type: "postgres", StorageGi: 5, Instances: 1}},
				Creator:    subject + "@example.com",
				CreateTime: timestamppb.New(at), ExpireTime: timestamppb.New(at.Add(24 * time.Hour)),
			},
			CreatorSubject: subject, Digest: "digest-" + id,
		}
	}
	put := func(t *testing.T, s ResourceStore, recs ...*PlanRecord) {
		t.Helper()
		for _, r := range recs {
			if err := s.WriteResources(ctx, func(tx ResourceTx) error { return tx.PutResourcePlan(ctx, r) }); err != nil {
				t.Fatal(err)
			}
		}
	}
	names := func(recs []*PlanRecord) string {
		out := ""
		for _, r := range recs {
			out += r.Plan.GetName()[len("resourcePlans/"):] + " "
		}
		return out
	}
	res := func(service, env, name string, state forgev1.Resource_State) *forgev1.Resource {
		return &forgev1.Resource{
			Name: "resources/" + service + "/" + env + "/" + name, Service: "services/" + service, Env: env, Type: "postgres",
			Settings: &forgev1.ResourceSettings{Name: name, Type: "postgres", StorageGi: 5, Instances: 1}, State: state,
			Secret: name + "-app", CreateTime: timestamppb.New(at),
		}
	}

	t.Run("a plan keeps every field and gets a sequence number", func(t *testing.T) {
		s := open(t)
		p := plan("aaaaaaaaaaaa", "services/payments", "staging", forgev1.ResourcePlan_STATE_PLANNING, "ana")
		p.TokenHash, p.Deadline = "hash-a", at.Add(15*time.Minute)
		p.PlanFile, p.PlanJSON, p.Audit, p.StateVersion = []byte{0, 1, 2}, []byte(`{"a":1}`), []byte(`{"action":"resource.plan"}`), 7
		put(t, s, p)
		if p.Seq < 1 {
			t.Fatalf("Seq %d, want it set", p.Seq)
		}
		got, err := s.GetResourcePlan(ctx, "resourcePlans/aaaaaaaaaaaa")
		if err != nil {
			t.Fatal(err)
		}
		if !proto.Equal(got.Plan, p.Plan) || got.Seq != p.Seq || got.CreatorSubject != "ana" || got.Digest != "digest-aaaaaaaaaaaa" ||
			got.StateVersion != 7 || got.TokenHash != "hash-a" || !got.Deadline.Equal(p.Deadline) ||
			!bytes.Equal(got.PlanFile, p.PlanFile) || !bytes.Equal(got.PlanJSON, p.PlanJSON) || !bytes.Equal(got.Audit, p.Audit) {
			t.Errorf("got %+v, want %+v", got, p)
		}
		// What comes out is a copy.
		got.Plan.State = forgev1.ResourcePlan_STATE_FAILED
		again, _ := s.GetResourcePlan(ctx, "resourcePlans/aaaaaaaaaaaa")
		if again.Plan.GetState() != forgev1.ResourcePlan_STATE_PLANNING {
			t.Error("the store shares its record with the caller")
		}

		byToken, err := s.ResourcePlanByToken(ctx, "hash-a")
		if err != nil || byToken.Plan.GetName() != p.Plan.GetName() {
			t.Errorf("by token: %v %v", byToken, err)
		}
		for _, hash := range []string{"hash-b", ""} {
			if _, err := s.ResourcePlanByToken(ctx, hash); !errors.Is(err, ErrNotFound) {
				t.Errorf("token %q: %v, want ErrNotFound", hash, err)
			}
		}
		if _, err := s.GetResourcePlan(ctx, "resourcePlans/bbbbbbbbbbbb"); !errors.Is(err, ErrNotFound) {
			t.Errorf("a missing plan: %v", err)
		}
	})

	t.Run("replacing a plan keeps its sequence number and clears what was cleared", func(t *testing.T) {
		s := open(t)
		p := plan("aaaaaaaaaaaa", "services/payments", "staging", forgev1.ResourcePlan_STATE_PLANNING, "ana")
		p.TokenHash, p.Deadline, p.PlanFile = "hash-a", at, []byte("file")
		put(t, s, p)
		seq := p.Seq
		p.Plan.State, p.TokenHash, p.Deadline, p.PlanFile = forgev1.ResourcePlan_STATE_PLANNED, "", time.Time{}, nil
		put(t, s, p)
		got, err := s.GetResourcePlan(ctx, p.Plan.GetName())
		if err != nil {
			t.Fatal(err)
		}
		if got.Seq != seq || got.Plan.GetState() != forgev1.ResourcePlan_STATE_PLANNED || got.TokenHash != "" || !got.Deadline.IsZero() || len(got.PlanFile) != 0 {
			t.Errorf("after the replace: %+v", got)
		}
		if _, err := s.ResourcePlanByToken(ctx, "hash-a"); !errors.Is(err, ErrNotFound) {
			t.Errorf("the old token still finds the plan: %v", err)
		}
	})

	t.Run("list filters and pages, newest first", func(t *testing.T) {
		s := open(t)
		a := plan("aaaaaaaaaaaa", "services/payments", "staging", forgev1.ResourcePlan_STATE_APPLIED, "ana")
		b := plan("bbbbbbbbbbbb", "services/payments", "production", forgev1.ResourcePlan_STATE_PLANNED, "ana")
		c := plan("cccccccccccc", "services/ledger", "staging", forgev1.ResourcePlan_STATE_PLANNED, "bo")
		d := plan("dddddddddddd", "services/payments", "staging", forgev1.ResourcePlan_STATE_PLANNED, "bo")
		put(t, s, a, b, c, d)
		for name, tc := range map[string]struct {
			q    PlanQuery
			want string
		}{
			"everything":      {PlanQuery{Limit: 10}, "dddddddddddd cccccccccccc bbbbbbbbbbbb aaaaaaaaaaaa "},
			"a service":       {PlanQuery{Service: "services/payments", Limit: 10}, "dddddddddddd bbbbbbbbbbbb aaaaaaaaaaaa "},
			"an environment":  {PlanQuery{Service: "services/payments", Env: "staging", Limit: 10}, "dddddddddddd aaaaaaaaaaaa "},
			"states":          {PlanQuery{States: []forgev1.ResourcePlan_State{forgev1.ResourcePlan_STATE_APPLIED, forgev1.ResourcePlan_STATE_FAILED}, Limit: 10}, "aaaaaaaaaaaa "},
			"a creator":       {PlanQuery{CreatorSubject: "bo", Limit: 10}, "dddddddddddd cccccccccccc "},
			"a limit":         {PlanQuery{Limit: 2}, "dddddddddddd cccccccccccc "},
			"before a cursor": {PlanQuery{BeforeSeq: c.Seq, Limit: 10}, "bbbbbbbbbbbb aaaaaaaaaaaa "},
			"nothing":         {PlanQuery{Service: "services/none", Limit: 10}, ""},
		} {
			got, err := s.ListResourcePlans(ctx, tc.q)
			if err != nil || names(got) != tc.want {
				t.Errorf("%s: %q (%v), want %q", name, names(got), err, tc.want)
			}
		}
	})

	t.Run("a failed transaction writes nothing", func(t *testing.T) {
		s := open(t)
		boom := errors.New("boom")
		err := s.WriteResources(ctx, func(tx ResourceTx) error {
			if err := tx.PutResourcePlan(ctx, plan("aaaaaaaaaaaa", "services/payments", "staging", forgev1.ResourcePlan_STATE_PLANNING, "ana")); err != nil {
				return err
			}
			if err := tx.PutResource(ctx, res("payments", "staging", "main", forgev1.Resource_STATE_APPLYING)); err != nil {
				return err
			}
			// Reads inside the transaction see its writes.
			if _, err := tx.GetResourcePlan(ctx, "resourcePlans/aaaaaaaaaaaa"); err != nil {
				return fmt.Errorf("read own plan: %w", err)
			}
			if rs, err := tx.ListResources(ctx, "services/payments", ""); err != nil || len(rs) != 1 {
				return fmt.Errorf("read own resource: %v %w", rs, err)
			}
			return boom
		})
		if !errors.Is(err, boom) {
			t.Fatalf("got %v, want the function's own error", err)
		}
		if _, err := s.GetResourcePlan(ctx, "resourcePlans/aaaaaaaaaaaa"); !errors.Is(err, ErrNotFound) {
			t.Errorf("the plan was written: %v", err)
		}
		if rs, _ := s.ListResources(ctx, "", ""); len(rs) != 0 {
			t.Errorf("the resource was written: %v", rs)
		}
	})

	t.Run("resources are put, listed by name, replaced and deleted", func(t *testing.T) {
		s := open(t)
		err := s.WriteResources(ctx, func(tx ResourceTx) error {
			for _, r := range []*forgev1.Resource{
				res("payments", "staging", "main", forgev1.Resource_STATE_READY),
				res("payments", "production", "main", forgev1.Resource_STATE_APPLYING),
				res("ledger", "staging", "main", forgev1.Resource_STATE_READY),
			} {
				if err := tx.PutResource(ctx, r); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		list := func(service, env string) string {
			rs, err := s.ListResources(ctx, service, env)
			if err != nil {
				t.Fatal(err)
			}
			out := ""
			for _, r := range rs {
				out += r.GetName() + " "
			}
			return out
		}
		if got := list("", ""); got != "resources/ledger/staging/main resources/payments/production/main resources/payments/staging/main " {
			t.Errorf("all: %q", got)
		}
		if got := list("services/payments", ""); got != "resources/payments/production/main resources/payments/staging/main " {
			t.Errorf("a service: %q", got)
		}
		if got := list("services/payments", "staging"); got != "resources/payments/staging/main " {
			t.Errorf("an environment: %q", got)
		}
		want := res("payments", "staging", "main", forgev1.Resource_STATE_READY)
		got, err := s.GetResource(ctx, want.GetName())
		if err != nil || !proto.Equal(got, want) {
			t.Errorf("get: %v %v", got, err)
		}
		err = s.WriteResources(ctx, func(tx ResourceTx) error {
			if err := tx.PutResource(ctx, res("payments", "staging", "main", forgev1.Resource_STATE_FAILED)); err != nil {
				return err
			}
			if r, err := tx.GetResource(ctx, want.GetName()); err != nil || r.GetState() != forgev1.Resource_STATE_FAILED {
				return fmt.Errorf("read own write: %v %w", r, err)
			}
			if err := tx.DeleteResource(ctx, "resources/ledger/staging/main"); err != nil {
				return err
			}
			return tx.DeleteResource(ctx, "resources/none/staging/main") // deleting nothing is fine
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := list("", ""); got != "resources/payments/production/main resources/payments/staging/main " {
			t.Errorf("after the delete: %q", got)
		}
		if _, err := s.GetResource(ctx, "resources/ledger/staging/main"); !errors.Is(err, ErrNotFound) {
			t.Errorf("a deleted resource: %v", err)
		}
	})

	t.Run("state counts writes and honours locks", func(t *testing.T) {
		s := open(t)
		const key = "payments/staging"
		if st, err := s.GetResourceState(ctx, key); err != nil || st.Version != 0 || st.Data != nil || st.LockID != "" {
			t.Fatalf("no state yet: %+v %v", st, err)
		}
		if holder, err := s.LockResourceState(ctx, key, "lock-1", []byte(`{"ID":"lock-1"}`)); err != nil || holder != nil {
			t.Fatalf("first lock: %s %v", holder, err)
		}
		// Locking before the first write makes no version.
		if st, _ := s.GetResourceState(ctx, key); st.Version != 0 || st.LockID != "lock-1" {
			t.Errorf("locked, unwritten: %+v", st)
		}
		// The same id may lock again; another id is told who holds it.
		if _, err := s.LockResourceState(ctx, key, "lock-1", []byte(`{"ID":"lock-1"}`)); err != nil {
			t.Errorf("relock by the holder: %v", err)
		}
		holder, err := s.LockResourceState(ctx, key, "lock-2", []byte(`{"ID":"lock-2"}`))
		if !errors.Is(err, ErrStateLocked) || string(holder) != `{"ID":"lock-1"}` {
			t.Errorf("second lock: %s %v", holder, err)
		}
		if _, err := s.PutResourceState(ctx, key, "lock-2", []byte("x")); !errors.Is(err, ErrStateLocked) {
			t.Errorf("a write under another lock: %v", err)
		}
		if _, err := s.PutResourceState(ctx, key, "", []byte("x")); !errors.Is(err, ErrStateLocked) {
			t.Errorf("a write with no lock id while locked: %v", err)
		}
		v1, err := s.PutResourceState(ctx, key, "lock-1", []byte("one"))
		if err != nil || v1 != 1 {
			t.Fatalf("first write: %d %v", v1, err)
		}
		v2, err := s.PutResourceState(ctx, key, "lock-1", []byte("two"))
		if err != nil || v2 != 2 {
			t.Fatalf("second write: %d %v", v2, err)
		}
		if st, _ := s.GetResourceState(ctx, key); string(st.Data) != "two" || st.Version != 2 || st.LockID != "lock-1" {
			t.Errorf("after two writes: %+v", st)
		}
		if err := s.UnlockResourceState(ctx, key, "lock-2"); !errors.Is(err, ErrStateLocked) {
			t.Errorf("an unlock by another id: %v", err)
		}
		if err := s.UnlockResourceState(ctx, key, "lock-1"); err != nil {
			t.Fatal(err)
		}
		if st, _ := s.GetResourceState(ctx, key); st.LockID != "" || st.LockInfo != nil || st.Version != 2 {
			t.Errorf("after the unlock: %+v", st)
		}
		// Unlocked: a write needs no lock id, and "" force-releases any lock.
		if v, err := s.PutResourceState(ctx, key, "", []byte("three")); err != nil || v != 3 {
			t.Errorf("an unlocked write: %d %v", v, err)
		}
		if _, err := s.LockResourceState(ctx, key, "lock-3", nil); err != nil {
			t.Fatal(err)
		}
		if err := s.UnlockResourceState(ctx, key, ""); err != nil {
			t.Errorf("a forced unlock: %v", err)
		}
		if err := s.UnlockResourceState(ctx, "ledger/staging", "lock-9"); err != nil {
			t.Errorf("unlocking a state that does not exist: %v", err)
		}
		if _, err := s.LockResourceState(ctx, key, "", nil); err == nil {
			t.Error("a lock with no id must fail")
		}
		// Keys are separate.
		if st, _ := s.GetResourceState(ctx, "payments/production"); st.Version != 0 {
			t.Errorf("another key: %+v", st)
		}
	})
}
```

Create `internal/store/resource_test.go`:

```go
package store

import (
	"context"
	"testing"

	"github.com/showmino/forge/internal/store/pgtest"
)

func TestMemoryResources(t *testing.T) {
	t.Parallel()
	runResourceSuite(t, func(*testing.T) ResourceStore { return NewMemory() })
}

func TestPostgresResources(t *testing.T) {
	t.Parallel()
	runResourceSuite(t, func(t *testing.T) ResourceStore {
		p := openTestPostgres(t, pgtest.URL(t))
		if err := p.Migrate(context.Background()); err != nil {
			t.Fatal(err)
		}
		return p
	})
}

func TestBackends(t *testing.T) {
	t.Parallel()
	var _ Backend = NewMemory()
	var _ Backend = (*Postgres)(nil)
}
```

Run: `go test ./internal/store`
Expected: FAIL to build (`*Memory does not implement ResourceStore`).

- [ ] **Step 3: Implement the memory store**

In `internal/store/memory.go`, add to the `Memory` struct after the `audit` field:

```go

	resMu     sync.Mutex
	plans     map[string]*PlanRecord // never changed in place: a write replaces the entry
	planSeq   int64
	resources map[string]*forgev1.Resource // likewise
	states    map[string]ResourceState
```

and change `NewMemory` to:

```go
// NewMemory returns an empty Memory store.
func NewMemory() *Memory {
	return &Memory{
		services: map[string]memEntry{}, requests: map[string]*forgev1.Service{},
		plans: map[string]*PlanRecord{}, resources: map[string]*forgev1.Resource{}, states: map[string]ResourceState{},
	}
}
```

Create `internal/store/resource_memory.go`:

```go
package store

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
)

func clonePlan(p *PlanRecord) *PlanRecord {
	c := *p
	c.Plan = proto.Clone(p.Plan).(*forgev1.ResourcePlan)
	c.PlanFile, c.PlanJSON, c.Audit = slices.Clone(p.PlanFile), slices.Clone(p.PlanJSON), slices.Clone(p.Audit)
	return &c
}

func cloneResource(r *forgev1.Resource) *forgev1.Resource { return proto.Clone(r).(*forgev1.Resource) }

// listResources returns copies of the matching entries of all, sorted by name.
func listResources(all map[string]*forgev1.Resource, service, env string) []*forgev1.Resource {
	var out []*forgev1.Resource
	for _, r := range all {
		if (service == "" || r.GetService() == service) && (env == "" || r.GetEnv() == env) {
			out = append(out, cloneResource(r))
		}
	}
	slices.SortFunc(out, func(a, b *forgev1.Resource) int { return strings.Compare(a.GetName(), b.GetName()) })
	return out
}

func (m *Memory) GetResourcePlan(_ context.Context, name string) (*PlanRecord, error) {
	m.resMu.Lock()
	defer m.resMu.Unlock()
	p, ok := m.plans[name]
	if !ok {
		return nil, ErrNotFound
	}
	return clonePlan(p), nil
}

func (m *Memory) ResourcePlanByToken(_ context.Context, tokenHash string) (*PlanRecord, error) {
	m.resMu.Lock()
	defer m.resMu.Unlock()
	if tokenHash != "" {
		for _, p := range m.plans {
			if p.TokenHash == tokenHash {
				return clonePlan(p), nil
			}
		}
	}
	return nil, ErrNotFound
}

func (m *Memory) ListResourcePlans(_ context.Context, q PlanQuery) ([]*PlanRecord, error) {
	m.resMu.Lock()
	defer m.resMu.Unlock()
	var out []*PlanRecord
	for _, p := range m.plans {
		switch {
		case q.Service != "" && p.Plan.GetService() != q.Service,
			q.Env != "" && p.Plan.GetEnv() != q.Env,
			len(q.States) > 0 && !slices.Contains(q.States, p.Plan.GetState()),
			q.CreatorSubject != "" && p.CreatorSubject != q.CreatorSubject,
			q.BeforeSeq > 0 && p.Seq >= q.BeforeSeq:
			continue
		}
		out = append(out, clonePlan(p))
	}
	slices.SortFunc(out, func(a, b *PlanRecord) int { return int(b.Seq - a.Seq) })
	if len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}

func (m *Memory) GetResource(_ context.Context, name string) (*forgev1.Resource, error) {
	m.resMu.Lock()
	defer m.resMu.Unlock()
	r, ok := m.resources[name]
	if !ok {
		return nil, ErrNotFound
	}
	return cloneResource(r), nil
}

func (m *Memory) ListResources(_ context.Context, service, env string) ([]*forgev1.Resource, error) {
	m.resMu.Lock()
	defer m.resMu.Unlock()
	return listResources(m.resources, service, env), nil
}

// WriteResources runs fn on copies of the maps and keeps them only when fn
// returns nil. Entries are never changed in place, so copying the maps is enough.
func (m *Memory) WriteResources(_ context.Context, fn func(tx ResourceTx) error) error {
	m.resMu.Lock()
	defer m.resMu.Unlock()
	tx := &memResourceTx{plans: maps.Clone(m.plans), resources: maps.Clone(m.resources), seq: m.planSeq}
	if err := fn(tx); err != nil {
		return err
	}
	m.plans, m.resources, m.planSeq = tx.plans, tx.resources, tx.seq
	return nil
}

type memResourceTx struct {
	plans     map[string]*PlanRecord
	resources map[string]*forgev1.Resource
	seq       int64
}

func (t *memResourceTx) GetResourcePlan(_ context.Context, name string) (*PlanRecord, error) {
	p, ok := t.plans[name]
	if !ok {
		return nil, ErrNotFound
	}
	return clonePlan(p), nil
}

func (t *memResourceTx) PutResourcePlan(_ context.Context, p *PlanRecord) error {
	if old, ok := t.plans[p.Plan.GetName()]; ok {
		p.Seq = old.Seq
	} else {
		t.seq++
		p.Seq = t.seq
	}
	t.plans[p.Plan.GetName()] = clonePlan(p)
	return nil
}

func (t *memResourceTx) GetResource(_ context.Context, name string) (*forgev1.Resource, error) {
	r, ok := t.resources[name]
	if !ok {
		return nil, ErrNotFound
	}
	return cloneResource(r), nil
}

func (t *memResourceTx) PutResource(_ context.Context, r *forgev1.Resource) error {
	t.resources[r.GetName()] = cloneResource(r)
	return nil
}

func (t *memResourceTx) DeleteResource(_ context.Context, name string) error {
	delete(t.resources, name)
	return nil
}

func (t *memResourceTx) ListResources(_ context.Context, service, env string) ([]*forgev1.Resource, error) {
	return listResources(t.resources, service, env), nil
}

func (m *Memory) GetResourceState(_ context.Context, key string) (ResourceState, error) {
	m.resMu.Lock()
	defer m.resMu.Unlock()
	st := m.states[key]
	st.Data, st.LockInfo = slices.Clone(st.Data), slices.Clone(st.LockInfo)
	return st, nil
}

func (m *Memory) PutResourceState(_ context.Context, key, lockID string, data []byte) (int64, error) {
	m.resMu.Lock()
	defer m.resMu.Unlock()
	st := m.states[key]
	if st.LockID != "" && st.LockID != lockID {
		return 0, ErrStateLocked
	}
	st.Data = slices.Clone(data)
	st.Version++
	m.states[key] = st
	return st.Version, nil
}

func (m *Memory) LockResourceState(_ context.Context, key, lockID string, info []byte) ([]byte, error) {
	if lockID == "" {
		return nil, errors.New("a state lock needs an id")
	}
	m.resMu.Lock()
	defer m.resMu.Unlock()
	st := m.states[key]
	if st.LockID != "" && st.LockID != lockID {
		return slices.Clone(st.LockInfo), ErrStateLocked
	}
	st.LockID, st.LockInfo = lockID, slices.Clone(info)
	m.states[key] = st
	return nil, nil
}

func (m *Memory) UnlockResourceState(_ context.Context, key, lockID string) error {
	m.resMu.Lock()
	defer m.resMu.Unlock()
	st, ok := m.states[key]
	if !ok {
		return nil
	}
	if lockID != "" && st.LockID != "" && st.LockID != lockID {
		return ErrStateLocked
	}
	st.LockID, st.LockInfo = "", nil
	m.states[key] = st
	return nil
}
```

Run: `go test ./internal/store -run 'TestMemoryResources|TestMemory$'`
Expected: `TestMemoryResources` passes; the package still fails to build until Step 4 if `TestBackends` is compiled. If the build fails only on `*Postgres`, continue to Step 4.

- [ ] **Step 4: Write the migration and the PostgreSQL store**

Create `internal/store/migrations/0003_resources.sql`:

```sql
-- Resources a service declares in forge.yaml: what exists, the plans that
-- change it, and OpenTofu's state for each service and environment.
CREATE TABLE resources (
    name    text PRIMARY KEY,
    service text NOT NULL,
    env     text NOT NULL,
    data    jsonb NOT NULL
);
CREATE INDEX resources_service ON resources (service, env);

CREATE TABLE resource_plans (
    seq             bigserial PRIMARY KEY,
    name            text NOT NULL UNIQUE,
    service         text NOT NULL,
    env             text NOT NULL,
    state           text NOT NULL,
    creator_subject text NOT NULL DEFAULT '',
    data            jsonb NOT NULL,
    digest          text NOT NULL DEFAULT '',
    state_version   bigint NOT NULL DEFAULT 0,
    -- The SHA-256 of the active run's token; never the token.
    token_hash      text NOT NULL DEFAULT '',
    deadline        timestamptz,
    plan_file       bytea,
    plan_json       bytea,
    audit           bytea
);
CREATE INDEX resource_plans_target ON resource_plans (service, env, seq);
CREATE UNIQUE INDEX resource_plans_token ON resource_plans (token_hash) WHERE token_hash <> '';

CREATE TABLE resource_states (
    key       text PRIMARY KEY,
    data      bytea,
    version   bigint NOT NULL DEFAULT 0,
    lock_id   text NOT NULL DEFAULT '',
    lock_info bytea
);
```

In `internal/store/postgres.go`, add a third lock key, so the `const` block reads (gofmt re-aligns the first two lines):

```go
const (
	migrateLock  int64 = 0x666f726765_01 // "forge", 1
	writeLock    int64 = 0x666f726765_02 // "forge", 2
	resourceLock int64 = 0x666f726765_03 // "forge", 3
)
```

Create `internal/store/resource_postgres.go`:

```go
package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/encoding/protojson"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
)

const planColumns = `seq, data, creator_subject, digest, state_version, token_hash, deadline, plan_file, plan_json, audit`

func scanPlan(row pgx.Row) (*PlanRecord, error) {
	var (
		p        PlanRecord
		data     []byte
		deadline *time.Time
	)
	if err := row.Scan(&p.Seq, &data, &p.CreatorSubject, &p.Digest, &p.StateVersion, &p.TokenHash, &deadline, &p.PlanFile, &p.PlanJSON, &p.Audit); err != nil {
		return nil, err
	}
	p.Plan = &forgev1.ResourcePlan{}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(data, p.Plan); err != nil {
		return nil, fmt.Errorf("decode a stored resource plan: %w", err)
	}
	if deadline != nil {
		p.Deadline = *deadline
	}
	return &p, nil
}

func getPlan(ctx context.Context, q querier, where string, arg any) (*PlanRecord, error) {
	p, err := scanPlan(q.QueryRow(ctx, "SELECT "+planColumns+" FROM resource_plans WHERE "+where, arg))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return p, err
}

func decodeResource(data []byte) (*forgev1.Resource, error) {
	r := &forgev1.Resource{}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(data, r); err != nil {
		return nil, fmt.Errorf("decode a stored resource: %w", err)
	}
	return r, nil
}

func getResource(ctx context.Context, q querier, name string) (*forgev1.Resource, error) {
	var data []byte
	err := q.QueryRow(ctx, "SELECT data FROM resources WHERE name = $1", name).Scan(&data)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return decodeResource(data)
}

func queryResources(ctx context.Context, q querier, service, env string) ([]*forgev1.Resource, error) {
	rows, err := q.Query(ctx, `SELECT data FROM resources
		WHERE ($1::text = '' OR service = $1) AND ($2::text = '' OR env = $2) ORDER BY name`, service, env)
	if err != nil {
		return nil, err
	}
	data, err := pgx.CollectRows(rows, pgx.RowTo[[]byte])
	if err != nil {
		return nil, err
	}
	out := make([]*forgev1.Resource, 0, len(data))
	for _, d := range data {
		r, err := decodeResource(d)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

func (p *Postgres) GetResourcePlan(ctx context.Context, name string) (*PlanRecord, error) {
	return getPlan(ctx, p.pool, "name = $1", name)
}

func (p *Postgres) ResourcePlanByToken(ctx context.Context, tokenHash string) (*PlanRecord, error) {
	if tokenHash == "" {
		return nil, ErrNotFound
	}
	return getPlan(ctx, p.pool, "token_hash = $1", tokenHash)
}

func (p *Postgres) ListResourcePlans(ctx context.Context, q PlanQuery) ([]*PlanRecord, error) {
	where := []string{"TRUE"}
	var args []any
	add := func(cond string, v any) {
		args = append(args, v)
		where = append(where, fmt.Sprintf(cond, len(args)))
	}
	if q.Service != "" {
		add("service = $%d", q.Service)
	}
	if q.Env != "" {
		add("env = $%d", q.Env)
	}
	if len(q.States) > 0 {
		states := make([]string, len(q.States))
		for i, s := range q.States {
			states[i] = s.String()
		}
		add("state = ANY($%d)", states)
	}
	if q.CreatorSubject != "" {
		add("creator_subject = $%d", q.CreatorSubject)
	}
	if q.BeforeSeq > 0 {
		add("seq < $%d", q.BeforeSeq)
	}
	args = append(args, q.Limit)
	rows, err := p.pool.Query(ctx, fmt.Sprintf("SELECT %s FROM resource_plans WHERE %s ORDER BY seq DESC LIMIT $%d",
		planColumns, strings.Join(where, " AND "), len(args)), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*PlanRecord
	for rows.Next() {
		rec, err := scanPlan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (p *Postgres) GetResource(ctx context.Context, name string) (*forgev1.Resource, error) {
	return getResource(ctx, p.pool, name)
}

func (p *Postgres) ListResources(ctx context.Context, service, env string) ([]*forgev1.Resource, error) {
	return queryResources(ctx, p.pool, service, env)
}

// WriteResources runs fn in a transaction that first takes a transaction-scoped
// advisory lock, so these transactions run one at a time.
func (p *Postgres) WriteResources(ctx context.Context, fn func(tx ResourceTx) error) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }() // a no-op after Commit
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", resourceLock); err != nil {
		return err
	}
	if err := fn(pgResourceTx{tx}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

type pgResourceTx struct{ tx pgx.Tx }

func (t pgResourceTx) GetResourcePlan(ctx context.Context, name string) (*PlanRecord, error) {
	return getPlan(ctx, t.tx, "name = $1", name)
}

func (t pgResourceTx) PutResourcePlan(ctx context.Context, p *PlanRecord) error {
	data, err := protojson.Marshal(p.Plan)
	if err != nil {
		return err
	}
	var deadline *time.Time
	if !p.Deadline.IsZero() {
		deadline = &p.Deadline
	}
	return t.tx.QueryRow(ctx, `INSERT INTO resource_plans (name, service, env, state, creator_subject, data, digest,
			state_version, token_hash, deadline, plan_file, plan_json, audit)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		ON CONFLICT (name) DO UPDATE SET service = EXCLUDED.service, env = EXCLUDED.env, state = EXCLUDED.state,
			creator_subject = EXCLUDED.creator_subject, data = EXCLUDED.data, digest = EXCLUDED.digest,
			state_version = EXCLUDED.state_version, token_hash = EXCLUDED.token_hash, deadline = EXCLUDED.deadline,
			plan_file = EXCLUDED.plan_file, plan_json = EXCLUDED.plan_json, audit = EXCLUDED.audit
		RETURNING seq`,
		p.Plan.GetName(), p.Plan.GetService(), p.Plan.GetEnv(), p.Plan.GetState().String(), p.CreatorSubject, data, p.Digest,
		p.StateVersion, p.TokenHash, deadline, p.PlanFile, p.PlanJSON, p.Audit).Scan(&p.Seq)
}

func (t pgResourceTx) GetResource(ctx context.Context, name string) (*forgev1.Resource, error) {
	return getResource(ctx, t.tx, name)
}

func (t pgResourceTx) PutResource(ctx context.Context, r *forgev1.Resource) error {
	data, err := protojson.Marshal(r)
	if err != nil {
		return err
	}
	_, err = t.tx.Exec(ctx, `INSERT INTO resources (name, service, env, data) VALUES ($1, $2, $3, $4)
		ON CONFLICT (name) DO UPDATE SET service = EXCLUDED.service, env = EXCLUDED.env, data = EXCLUDED.data`,
		r.GetName(), r.GetService(), r.GetEnv(), data)
	return err
}

func (t pgResourceTx) DeleteResource(ctx context.Context, name string) error {
	_, err := t.tx.Exec(ctx, "DELETE FROM resources WHERE name = $1", name)
	return err
}

func (t pgResourceTx) ListResources(ctx context.Context, service, env string) ([]*forgev1.Resource, error) {
	return queryResources(ctx, t.tx, service, env)
}

func (p *Postgres) GetResourceState(ctx context.Context, key string) (ResourceState, error) {
	var st ResourceState
	err := p.pool.QueryRow(ctx, "SELECT data, version, lock_id, lock_info FROM resource_states WHERE key = $1", key).
		Scan(&st.Data, &st.Version, &st.LockID, &st.LockInfo)
	if errors.Is(err, pgx.ErrNoRows) {
		return ResourceState{}, nil
	}
	return st, err
}

func (p *Postgres) PutResourceState(ctx context.Context, key, lockID string, data []byte) (int64, error) {
	var version int64
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		var held string
		err := tx.QueryRow(ctx, "SELECT lock_id FROM resource_states WHERE key = $1 FOR UPDATE", key).Scan(&held)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if held != "" && held != lockID {
			return ErrStateLocked
		}
		return tx.QueryRow(ctx, `INSERT INTO resource_states (key, data, version) VALUES ($1, $2, 1)
			ON CONFLICT (key) DO UPDATE SET data = EXCLUDED.data, version = resource_states.version + 1
			RETURNING version`, key, data).Scan(&version)
	})
	return version, err
}

func (p *Postgres) LockResourceState(ctx context.Context, key, lockID string, info []byte) ([]byte, error) {
	if lockID == "" {
		return nil, errors.New("a state lock needs an id")
	}
	tag, err := p.pool.Exec(ctx, `INSERT INTO resource_states (key, lock_id, lock_info) VALUES ($1, $2, $3)
		ON CONFLICT (key) DO UPDATE SET lock_id = EXCLUDED.lock_id, lock_info = EXCLUDED.lock_info
		WHERE resource_states.lock_id = '' OR resource_states.lock_id = EXCLUDED.lock_id`, key, lockID, info)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 1 {
		return nil, nil
	}
	var holder []byte
	if err := p.pool.QueryRow(ctx, "SELECT lock_info FROM resource_states WHERE key = $1", key).Scan(&holder); err != nil {
		return nil, err
	}
	return holder, ErrStateLocked
}

func (p *Postgres) UnlockResourceState(ctx context.Context, key, lockID string) error {
	tag, err := p.pool.Exec(ctx, `UPDATE resource_states SET lock_id = '', lock_info = NULL
		WHERE key = $1 AND ($2 = '' OR lock_id = '' OR lock_id = $2)`, key, lockID)
	if err != nil || tag.RowsAffected() == 1 {
		return err
	}
	var exists bool
	if err := p.pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM resource_states WHERE key = $1)", key).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return ErrStateLocked
	}
	return nil
}
```

In `internal/store/postgres_test.go`, `TestPostgresMigrate`: change `n != 2` to `n != 3` and `want 2` to `want 3`.

- [ ] **Step 5: Run the store tests, without and with PostgreSQL**

Run: `go test ./internal/store`
Expected: PASS (the Postgres tests skip).

Run (after `make db` if the container is not running; `docker start forge-test-db` restarts an existing one):
`FORGE_TEST_DATABASE_URL='postgres://postgres:forge@localhost:55432/postgres?sslmode=disable' go test ./internal/store`
Expected: PASS, with `TestPostgresResources` and `TestPostgresMigrate` run, not skipped (check with `-v -run 'TestPostgresResources|TestPostgresMigrate'`).

- [ ] **Step 6: Check everything still builds and lint**

Run: `go build ./... && go test ./... && make lint`
Expected: PASS. (`cmd/forge-api` returns `store.Backend`; both stores now satisfy the larger interface.)

- [ ] **Step 7: Commit**

```bash
git add internal/store
git commit -m "store: resource plans, resource records and OpenTofu state (memory, PostgreSQL, migration 0003)

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 4: The policy engine and the starting rules

**Files:**
- Create: `policy/policy.go`, `policy/resources.rego`, `policy/policy_test.go`
- Modify: `go.mod`, `go.sum`

**Interfaces:**
- Consumes: nothing from other tasks.
- Produces, in package `policy` (`github.com/showmino/forge/policy`):

```go
type Forge struct {
	Service      string     `json:"service"`
	Environment  string     `json:"environment"`
	Resources    []Resource `json:"resources"`
	Actor        Actor      `json:"actor"`
	AllowDestroy bool       `json:"allow_destroy"`
}
type Resource struct {
	Name      string `json:"name"`
	Type      string `json:"type"`
	StorageGi int32  `json:"storage_gi"`
	Instances int32  `json:"instances"`
}
type Actor struct {
	Kind    string `json:"kind"`
	Subject string `json:"subject"`
}
type Result struct{ Deny, Warn []string } // each sorted
func (r Result) Denied() bool
type Engine struct{ /* … */ }
func New(ctx context.Context) (*Engine, error)                  // the embedded rules
func NewFromFS(ctx context.Context, fsys fs.FS) (*Engine, error) // every *.rego at the root of fsys
func (e *Engine) Fingerprint() string                           // 12 hex characters
func (e *Engine) Evaluate(ctx context.Context, planJSON []byte, f Forge) (Result, error)
```

The test fixtures are built by hand in the shape `tofu show -json` gives a `kubernetes_manifest` resource; Plan 2 adds fixtures captured from real OpenTofu runs once the module exists.

- [ ] **Step 1: Add the dependency**

Run: `export PATH=$HOME/.local/go/bin:$HOME/go/bin:$PATH && go get github.com/open-policy-agent/opa@v1.21.1`
Expected: `go.mod` gains `github.com/open-policy-agent/opa v1.21.1`.

- [ ] **Step 2: Write the failing tests**

Create `policy/policy_test.go`:

```go
package policy

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"testing/fstest"
)

// cluster is the object a kubernetes_manifest change holds for a database.
func cluster(ns string, gi, instances int, class string) map[string]any {
	return map[string]any{"manifest": map[string]any{
		"apiVersion": "postgresql.cnpg.io/v1", "kind": "Cluster",
		"metadata": map[string]any{"name": "main", "namespace": ns},
		"spec":     map[string]any{"instances": instances, "storage": map[string]any{"size": fmt.Sprintf("%dGi", gi), "storageClass": class}},
	}}
}

func plan(t *testing.T, actions []string, before, after any) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"format_version": "1.2", "resource_changes": []any{map[string]any{
		"address": "kubernetes_manifest.database", "type": "kubernetes_manifest", "name": "database",
		"change": map[string]any{"actions": actions, "before": before, "after": after},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRules(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e, err := New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	const retain = "longhorn-retain"
	stg := Forge{Service: "payments", Environment: "staging"}
	prod := Forge{Service: "payments", Environment: "production"}
	destroy := stg
	destroy.AllowDestroy = true
	for _, tc := range []struct {
		name       string
		plan       []byte
		f          Forge
		deny, warn string
	}{
		{"a create passes", plan(t, []string{"create"}, nil, cluster("payments-staging", 5, 1, retain)), stg, "", ""},
		{"20Gi passes in staging", plan(t, []string{"create"}, nil, cluster("payments-staging", 20, 1, retain)), stg, "", ""},
		{"21Gi is too much for staging", plan(t, []string{"create"}, nil, cluster("payments-staging", 21, 1, retain)), stg, "storage 21Gi is above the 20Gi limit for staging", ""},
		{"50Gi passes in production", plan(t, []string{"create"}, nil, cluster("payments-production", 50, 2, retain)), prod, "", ""},
		{"200Gi is too much for production", plan(t, []string{"create"}, nil, cluster("payments-production", 200, 2, retain)), prod, "storage 200Gi is above the 50Gi limit for production", ""},
		{"production needs two instances", plan(t, []string{"create"}, nil, cluster("payments-production", 5, 1, retain)), prod, "production needs at least 2 instances, not 1", ""},
		{"four instances are too many", plan(t, []string{"create"}, nil, cluster("payments-production", 5, 4, retain)), prod, "4 instances is above the limit of 3", ""},
		{"another storage class", plan(t, []string{"create"}, nil, cluster("payments-staging", 5, 1, "local-path")), stg, `storage class "local-path" is not longhorn-retain`, ""},
		{"no storage class", plan(t, []string{"create"}, nil, cluster("payments-staging", 5, 1, "")), stg, `storage class "" is not longhorn-retain`, ""},
		{"a shrink", plan(t, []string{"update"}, cluster("payments-staging", 10, 1, retain), cluster("payments-staging", 5, 1, retain)), stg, "storage cannot shrink from 10Gi to 5Gi", ""},
		{"growth passes", plan(t, []string{"update"}, cluster("payments-staging", 5, 1, retain), cluster("payments-staging", 10, 1, retain)), stg, "", ""},
		{"a delete", plan(t, []string{"delete"}, cluster("payments-staging", 5, 1, retain), nil), stg, "the plan deletes database main; plan again with --allow-destroy to remove it", ""},
		{"a delete that was asked for", plan(t, []string{"delete"}, cluster("payments-staging", 5, 1, retain), nil), destroy, "", ""},
		{"a replace is a delete", plan(t, []string{"delete", "create"}, cluster("payments-staging", 5, 1, retain), cluster("payments-staging", 5, 1, retain)), stg, "the plan deletes database main; plan again with --allow-destroy to remove it", ""},
		{"another namespace", plan(t, []string{"create"}, nil, cluster("ledger-staging", 5, 1, retain)), stg, "the plan changes a database in namespace ledger-staging, not payments-staging", ""},
		{"two instances in staging warn", plan(t, []string{"create"}, nil, cluster("payments-staging", 5, 2, retain)), stg, "", "staging has 2 instances; 1 is usually enough"},
		{"a no-op is not checked", plan(t, []string{"no-op"}, cluster("ledger-staging", 500, 9, "x"), cluster("ledger-staging", 500, 9, "x")), stg, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r, err := e.Evaluate(ctx, tc.plan, tc.f)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(r.Deny, "|"); got != tc.deny {
				t.Errorf("deny %q, want %q", got, tc.deny)
			}
			if got := strings.Join(r.Warn, "|"); got != tc.warn {
				t.Errorf("warn %q, want %q", got, tc.warn)
			}
			if r.Denied() != (tc.deny != "") {
				t.Errorf("Denied() = %v", r.Denied())
			}
		})
	}
}

func TestRulesGuardThePlatform(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e, err := New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	stg := Forge{Service: "payments", Environment: "staging"}

	// Anything that is not a database is refused, whatever it is.
	secret, _ := json.Marshal(map[string]any{"resource_changes": []any{map[string]any{
		"address": "kubernetes_manifest.x", "type": "kubernetes_manifest",
		"change": map[string]any{"actions": []string{"create"}, "before": nil, "after": map[string]any{"manifest": map[string]any{
			"apiVersion": "v1", "kind": "Secret", "metadata": map[string]any{"name": "s", "namespace": "payments-staging"}}}},
	}}})
	r, err := e.Evaluate(ctx, secret, stg)
	if err != nil || len(r.Deny) != 1 || r.Deny[0] != "the plan changes kubernetes_manifest.x, which is not a CloudNativePG cluster" {
		t.Errorf("a Secret: %v %v", r, err)
	}
	other, _ := json.Marshal(map[string]any{"resource_changes": []any{map[string]any{
		"address": "null_resource.x", "type": "null_resource",
		"change": map[string]any{"actions": []string{"create"}, "before": nil, "after": map[string]any{"id": "1"}},
	}}})
	if r, err := e.Evaluate(ctx, other, stg); err != nil || !r.Denied() {
		t.Errorf("another resource type: %v %v", r, err)
	}

	// A database the rules cannot read is refused, never waved through.
	odd, _ := json.Marshal(map[string]any{"resource_changes": []any{map[string]any{
		"address": "kubernetes_manifest.database", "type": "kubernetes_manifest",
		"change": map[string]any{"actions": []string{"create"}, "before": nil, "after": map[string]any{"manifest": map[string]any{
			"apiVersion": "postgresql.cnpg.io/v1", "kind": "Cluster",
			"metadata": map[string]any{"name": "main", "namespace": "payments-staging"}, "spec": map[string]any{}}}},
	}}})
	r, err = e.Evaluate(ctx, odd, stg)
	if err != nil || !strings.Contains(strings.Join(r.Deny, "|"), "has no readable storage size or instance count") {
		t.Errorf("an unreadable database: %v %v", r, err)
	}

	// A plan with nothing in it passes: there is nothing to refuse.
	for _, body := range []string{`{"resource_changes":[]}`, `{}`} {
		if r, err := e.Evaluate(ctx, []byte(body), stg); err != nil || r.Denied() || len(r.Warn) != 0 {
			t.Errorf("%s: %v %v", body, r, err)
		}
	}
	for _, body := range []string{``, `tofu`} {
		if _, err := e.Evaluate(ctx, []byte(body), stg); err == nil {
			t.Errorf("%q: want an error", body)
		}
	}
}

func TestEngine(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	a, err := New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	b, err := New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Fingerprint()) != 12 || a.Fingerprint() != b.Fingerprint() {
		t.Errorf("fingerprints %q %q", a.Fingerprint(), b.Fingerprint())
	}
	rule := "package forge.resources\nimport rego.v1\ndeny contains \"no\" if true\nwarn contains \"hm\" if true\n"
	c, err := NewFromFS(ctx, fstest.MapFS{"x.rego": {Data: []byte(rule)}})
	if err != nil {
		t.Fatal(err)
	}
	if c.Fingerprint() == a.Fingerprint() {
		t.Error("other rules must have another fingerprint")
	}
	if r, err := c.Evaluate(ctx, []byte(`{}`), Forge{}); err != nil || len(r.Deny) != 1 || r.Deny[0] != "no" || r.Warn[0] != "hm" {
		t.Errorf("custom rules: %v %v", r, err)
	}
	if _, err := NewFromFS(ctx, fstest.MapFS{"bad.rego": {Data: []byte("package forge.resources\ndeny contains msg if {")}}); err == nil {
		t.Error("rules that do not compile must be an error")
	}
	if _, err := NewFromFS(ctx, fstest.MapFS{}); err == nil {
		t.Error("no rules must be an error")
	}
	// Rules whose messages are not strings are an error, not a pass.
	nums := "package forge.resources\nimport rego.v1\ndeny contains 1 if true\nwarn contains \"x\" if false\n"
	d, err := NewFromFS(ctx, fstest.MapFS{"n.rego": {Data: []byte(nums)}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Evaluate(ctx, []byte(`{}`), Forge{}); err == nil {
		t.Error("a non-string message must be an error")
	}
}
```

Run: `go test ./policy`
Expected: FAIL to build (`undefined: New`).

- [ ] **Step 3: Write the rules**

Create `policy/resources.rego`:

```rego
# Rules for resource plans.
# Input: {"plan": <the output of tofu show -json>, "forge": {service,
# environment, resources, actor, allow_destroy}}.
# A message in deny refuses the plan; a message in warn is shown with it.
package forge.resources

import rego.v1

# The most storage a database may ask for, in Gi, per environment.
max_storage_gi := {"staging": 20, "production": 50}

# The most instances a database may have.
max_instances := 3

# changes are the plan's resource changes that do something.
changes contains rc if {
	some rc in input.plan.resource_changes
	some action in rc.change.actions
	not action in {"no-op", "read"}
}

# manifest is the Kubernetes object a change ends with, or the one it starts
# from when the change deletes it.
manifest(rc) := rc.change.after.manifest if {
	rc.change.after != null
} else := rc.change.before.manifest

is_database(rc) if {
	rc.type == "kubernetes_manifest"
	m := manifest(rc)
	m.apiVersion == "postgresql.cnpg.io/v1"
	m.kind == "Cluster"
}

gi(size) := to_number(trim_suffix(size, "Gi"))

namespace := sprintf("%s-%s", [input.forge.service, input.forge.environment])

# readable is true for a database whose size and instance count the rules can read.
readable(m) if {
	regex.match(`^[1-9][0-9]*Gi$`, m.spec.storage.size)
	is_number(m.spec.instances)
}

# Only databases, and only in the service's own namespace. This guards the
# platform: if the module or the runner ever tried to change something else,
# the plan is refused.
deny contains msg if {
	some rc in changes
	not is_database(rc)
	msg := sprintf("the plan changes %s, which is not a CloudNativePG cluster", [rc.address])
}

deny contains msg if {
	some rc in changes
	is_database(rc)
	ns := manifest(rc).metadata.namespace
	ns != namespace
	msg := sprintf("the plan changes a database in namespace %s, not %s", [ns, namespace])
}

# A database the rules cannot read is refused, never waved through.
deny contains msg if {
	some rc in changes
	is_database(rc)
	rc.change.after != null
	not readable(rc.change.after.manifest)
	msg := sprintf("the plan's database %s has no readable storage size or instance count", [rc.address])
}

deny contains msg if {
	some rc in changes
	"delete" in rc.change.actions
	not input.forge.allow_destroy
	msg := sprintf("the plan deletes database %s; plan again with --allow-destroy to remove it", [manifest(rc).metadata.name])
}

deny contains msg if {
	some rc in changes
	is_database(rc)
	rc.change.after != null
	size := gi(rc.change.after.manifest.spec.storage.size)
	limit := max_storage_gi[input.forge.environment]
	size > limit
	msg := sprintf("storage %dGi is above the %dGi limit for %s", [size, limit, input.forge.environment])
}

deny contains msg if {
	some rc in changes
	is_database(rc)
	rc.change.before != null
	rc.change.after != null
	was := gi(rc.change.before.manifest.spec.storage.size)
	now := gi(rc.change.after.manifest.spec.storage.size)
	now < was
	msg := sprintf("storage cannot shrink from %dGi to %dGi", [was, now])
}

deny contains msg if {
	some rc in changes
	is_database(rc)
	rc.change.after != null
	input.forge.environment == "production"
	n := rc.change.after.manifest.spec.instances
	n < 2
	msg := sprintf("production needs at least 2 instances, not %d", [n])
}

deny contains msg if {
	some rc in changes
	is_database(rc)
	rc.change.after != null
	n := rc.change.after.manifest.spec.instances
	n > max_instances
	msg := sprintf("%d instances is above the limit of %d", [n, max_instances])
}

deny contains msg if {
	some rc in changes
	is_database(rc)
	rc.change.after != null
	class := object.get(rc.change.after.manifest.spec.storage, "storageClass", "")
	class != "longhorn-retain"
	msg := sprintf("storage class %q is not longhorn-retain", [class])
}

warn contains msg if {
	some rc in changes
	is_database(rc)
	rc.change.after != null
	input.forge.environment == "staging"
	n := rc.change.after.manifest.spec.instances
	n > 1
	msg := sprintf("staging has %d instances; 1 is usually enough", [n])
}
```

- [ ] **Step 4: Write the engine**

Create `policy/policy.go`:

```go
// Package policy checks resource plans against Forge's Rego rules. The rules
// are the .rego files next to this file, built into the binary.
package policy

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"slices"

	"github.com/open-policy-agent/opa/v1/rego"
)

//go:embed *.rego
var embedded embed.FS

// Forge is what a rule knows besides the plan.
type Forge struct {
	Service      string     `json:"service"`
	Environment  string     `json:"environment"`
	Resources    []Resource `json:"resources"`
	Actor        Actor      `json:"actor"`
	AllowDestroy bool       `json:"allow_destroy"`
}

// Resource is one declared resource, worked out for the environment.
type Resource struct {
	Name      string `json:"name"`
	Type      string `json:"type"`
	StorageGi int32  `json:"storage_gi"`
	Instances int32  `json:"instances"`
}

// Actor is who asked for the plan.
type Actor struct {
	Kind    string `json:"kind"`
	Subject string `json:"subject"`
}

// Result is what the rules said about one plan. Both lists are sorted.
type Result struct {
	Deny []string
	Warn []string
}

// Denied reports whether any rule refused the plan.
func (r Result) Denied() bool { return len(r.Deny) > 0 }

// Engine evaluates a compiled rule set.
type Engine struct {
	query       rego.PreparedEvalQuery
	fingerprint string
}

// New compiles the rules built into the binary.
func New(ctx context.Context) (*Engine, error) { return NewFromFS(ctx, embedded) }

// NewFromFS compiles every .rego file at the root of fsys.
func NewFromFS(ctx context.Context, fsys fs.FS) (*Engine, error) {
	names, err := fs.Glob(fsys, "*.rego")
	if err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return nil, errors.New("policy: no .rego files")
	}
	slices.Sort(names)
	sum := sha256.New()
	opts := []func(*rego.Rego){rego.Query("deny = data.forge.resources.deny; warn = data.forge.resources.warn")}
	for _, n := range names {
		src, err := fs.ReadFile(fsys, n)
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(sum, "%s\x00%d\x00", n, len(src))
		sum.Write(src)
		opts = append(opts, rego.Module(n, string(src)))
	}
	q, err := rego.New(opts...).PrepareForEval(ctx)
	if err != nil {
		return nil, fmt.Errorf("policy: compile the rules: %w", err)
	}
	return &Engine{query: q, fingerprint: hex.EncodeToString(sum.Sum(nil))[:12]}, nil
}

// Fingerprint identifies the rule set: the first 12 hex characters of the
// SHA-256 of its files.
func (e *Engine) Fingerprint() string { return e.fingerprint }

// Evaluate checks planJSON, the output of `tofu show -json`, with f. An
// error means the plan could not be checked; it must then not be applied.
func (e *Engine) Evaluate(ctx context.Context, planJSON []byte, f Forge) (Result, error) {
	var plan any
	if err := json.Unmarshal(planJSON, &plan); err != nil {
		return Result{}, fmt.Errorf("policy: the plan is not JSON: %w", err)
	}
	if f.Resources == nil {
		f.Resources = []Resource{}
	}
	rs, err := e.query.Eval(ctx, rego.EvalInput(map[string]any{"plan": plan, "forge": f}))
	if err != nil {
		return Result{}, fmt.Errorf("policy: evaluate: %w", err)
	}
	if len(rs) != 1 {
		return Result{}, fmt.Errorf("policy: the rules gave %d results, want 1", len(rs))
	}
	var out Result
	for _, part := range []struct {
		name string
		dst  *[]string
	}{{"deny", &out.Deny}, {"warn", &out.Warn}} {
		set, ok := rs[0].Bindings[part.name].([]any)
		if !ok {
			return Result{}, fmt.Errorf("policy: %s is not a set of messages", part.name)
		}
		for _, m := range set {
			s, ok := m.(string)
			if !ok {
				return Result{}, fmt.Errorf("policy: a %s message is not a string", part.name)
			}
			*part.dst = append(*part.dst, s)
		}
		slices.Sort(*part.dst)
	}
	return out, nil
}
```

- [ ] **Step 5: Run the tests and tidy**

Run: `go mod tidy && go test ./policy && go build ./... && make lint`
Expected: PASS, lint clean. If `go mod tidy` moves OPA from an `// indirect` line to the direct `require` block, that is the intended result.

- [ ] **Step 6: Commit**

```bash
git add policy go.mod go.sum
git commit -m "policy: Rego rules for resource plans and the engine that evaluates them

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 5: Run types and the fake runner

**Files:**
- Create: `internal/run/run.go`
- Create: `internal/envtest/runner.go`, `internal/envtest/runner_test.go`

**Interfaces:**
- Consumes: nothing from other tasks.
- Produces, in package `run` (`github.com/showmino/forge/internal/run`):

```go
const ( ModePlan = "plan"; ModeApply = "apply" )
var ErrLocked = errors.New("the state is locked")

type Spec struct {
	Plan, Mode, Service, Env, Token string // Plan is "resourcePlans/<id>"; Service is the bare id
	Deadline time.Time
}
type Status struct { Done, Failed bool; Log string }
type Launcher interface {
	Start(ctx context.Context, s Spec) error
	Status(ctx context.Context, plan, mode string) (Status, error)
	Stop(ctx context.Context, plan, mode string) error
}
type Resource struct {
	Name      string `json:"name"`
	Type      string `json:"type"`
	StorageGi int32  `json:"storage_gi"`
	Instances int32  `json:"instances"`
}
type Variables struct {
	Service     string     `json:"service"`
	Environment string     `json:"environment"`
	Namespace   string     `json:"namespace"`
	Resources   []Resource `json:"resources"`
}
type Work struct { Plan, Mode string; Variables Variables; PlanFile []byte }
type Result struct { PlanFile, PlanJSON []byte; Secrets map[string]string; Error string }
type API interface {
	Claim(ctx context.Context, token string) (*Work, error)
	Event(ctx context.Context, token, step, message string) error
	Complete(ctx context.Context, token string, r Result) error
	ReadState(ctx context.Context, token string) ([]byte, error)
	WriteState(ctx context.Context, token, lockID string, data []byte) error
	LockState(ctx context.Context, token, lockID string, info []byte) ([]byte, error)
	UnlockState(ctx context.Context, token, lockID string) error
}
```

- Produces, in package `envtest`: `type Runner struct` implementing `run.Launcher`, with exported knobs `API run.API`, `StartErr error`, `FailPlan`, `FailApply`, `Crash string`, `Hang`, `Silent bool`, `StorageClass string`, `PlanJSON []byte`, and methods `Started() []run.Spec`, `Databases() []string`, `Close()`.

- [ ] **Step 1: Write the run package**

Create `internal/run/run.go`:

```go
// Package run holds what forge-api and a resource runner share: how a run
// is started and watched (Launcher), and what a runner may ask of forge-api
// with its one-time token (API).
package run

import (
	"context"
	"errors"
	"time"
)

// Modes of a run.
const (
	ModePlan  = "plan"
	ModeApply = "apply"
)

// ErrLocked reports an OpenTofu state locked under another id.
var ErrLocked = errors.New("the state is locked")

// Spec is what a Launcher needs to start a runner.
type Spec struct {
	// Plan is the plan's name, "resourcePlans/<id>".
	Plan string
	// Mode is ModePlan or ModeApply.
	Mode string
	// Service is the service id, without "services/".
	Service string
	Env     string
	// Token authenticates the runner to the API. Never log it.
	Token    string
	Deadline time.Time
}

// Status is how a started runner is doing.
type Status struct {
	// Done is true once the runner's process has ended.
	Done bool
	// Failed is true when it ended without being able to report a result.
	Failed bool
	// Log is the end of its output, when it failed.
	Log string
}

// Launcher starts and watches runners (Kubernetes Jobs in production).
type Launcher interface {
	// Start starts a runner for s and returns once it is started.
	Start(ctx context.Context, s Spec) error
	// Status reports on the runner started for plan and mode.
	Status(ctx context.Context, plan, mode string) (Status, error)
	// Stop ends that runner, if it still runs.
	Stop(ctx context.Context, plan, mode string) error
}

// Resource is one resource the module should make.
type Resource struct {
	Name      string `json:"name"`
	Type      string `json:"type"`
	StorageGi int32  `json:"storage_gi"`
	Instances int32  `json:"instances"`
}

// Variables are the OpenTofu module's input.
type Variables struct {
	Service     string     `json:"service"`
	Environment string     `json:"environment"`
	Namespace   string     `json:"namespace"`
	Resources   []Resource `json:"resources"`
}

// Work is what a runner is asked to do.
type Work struct {
	Plan      string
	Mode      string
	Variables Variables
	// PlanFile is the saved plan to apply; empty for a plan run.
	PlanFile []byte
}

// Result is what a runner hands back.
type Result struct {
	// PlanFile is OpenTofu's saved plan and PlanJSON `tofu show -json` of
	// it; both set by a plan run.
	PlanFile []byte
	PlanJSON []byte
	// Secrets maps a resource's name to the Secret holding its connection
	// string; set by an apply run.
	Secrets map[string]string
	// Error says why the run failed; empty when it worked.
	Error string
}

// API is what a runner calls, authenticated by its run token.
type API interface {
	// Claim returns the run's work.
	Claim(ctx context.Context, token string) (*Work, error)
	// Event adds a progress line to the run's operation.
	Event(ctx context.Context, token, step, message string) error
	// Complete ends the run with r. It works once.
	Complete(ctx context.Context, token string, r Result) error
	// ReadState returns the OpenTofu state, nil when there is none.
	ReadState(ctx context.Context, token string) ([]byte, error)
	// WriteState stores the state; only an apply run may.
	WriteState(ctx context.Context, token, lockID string, data []byte) error
	// LockState locks the state; when it is held by another id it returns
	// the holder's info and ErrLocked.
	LockState(ctx context.Context, token, lockID string, info []byte) ([]byte, error)
	// UnlockState releases the lock.
	UnlockState(ctx context.Context, token, lockID string) error
}
```

- [ ] **Step 2: Write the failing tests for the fake runner**

Create `internal/envtest/runner_test.go`:

```go
package envtest

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/showmino/forge/internal/run"
)

// runAPI is a run.API that serves one piece of work and records the rest.
type runAPI struct {
	mu      sync.Mutex
	work    run.Work
	state   []byte
	lock    string
	events  []string
	results []run.Result
	done    chan struct{}
}

func newRunAPI(w run.Work) *runAPI { return &runAPI{work: w, done: make(chan struct{}, 4)} }

func (a *runAPI) Claim(context.Context, string) (*run.Work, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	w := a.work
	return &w, nil
}

func (a *runAPI) Event(_ context.Context, _, step, message string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, step+": "+message)
	return nil
}

func (a *runAPI) Complete(_ context.Context, _ string, r run.Result) error {
	a.mu.Lock()
	a.results = append(a.results, r)
	a.mu.Unlock()
	a.done <- struct{}{}
	return nil
}

func (a *runAPI) ReadState(context.Context, string) ([]byte, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.state, nil
}

func (a *runAPI) WriteState(_ context.Context, _, lockID string, data []byte) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.lock != lockID {
		return run.ErrLocked
	}
	a.state = data
	return nil
}

func (a *runAPI) LockState(_ context.Context, _, lockID string, _ []byte) ([]byte, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lock = lockID
	return nil, nil
}

func (a *runAPI) UnlockState(context.Context, string, string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lock = ""
	return nil
}

// last waits for the next result.
func (a *runAPI) last(t *testing.T) run.Result {
	t.Helper()
	select {
	case <-a.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the run did not complete")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.results[len(a.results)-1]
}

func vars(resources ...run.Resource) run.Variables {
	return run.Variables{Service: "payments", Environment: "staging", Namespace: "payments-staging", Resources: resources}
}

// actions returns "<action> <name>" for every change of a fake plan.
func actions(t *testing.T, planJSON []byte) string {
	t.Helper()
	var doc struct {
		ResourceChanges []struct {
			Address string `json:"address"`
			Change  struct {
				Actions []string `json:"actions"`
			} `json:"change"`
		} `json:"resource_changes"`
	}
	if err := json.Unmarshal(planJSON, &doc); err != nil {
		t.Fatalf("plan JSON: %v\n%s", err, planJSON)
	}
	var out []string
	for _, rc := range doc.ResourceChanges {
		out = append(out, strings.Join(rc.Change.Actions, "+")+" "+rc.Address)
	}
	return strings.Join(out, ", ")
}

func TestRunnerPlansAndApplies(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	main5 := run.Resource{Name: "main", Type: "postgres", StorageGi: 5, Instances: 1}
	api := newRunAPI(run.Work{Plan: "resourcePlans/aaaaaaaaaaaa", Mode: run.ModePlan, Variables: vars(main5)})
	r := &Runner{API: api}
	t.Cleanup(r.Close)

	step := func(mode string, w run.Work) run.Result {
		t.Helper()
		api.mu.Lock()
		api.work = w
		api.mu.Unlock()
		if err := r.Start(ctx, run.Spec{Plan: w.Plan, Mode: mode, Service: "payments", Env: "staging", Token: "tok"}); err != nil {
			t.Fatal(err)
		}
		return api.last(t)
	}

	// Nothing exists: the plan creates the database.
	plan := step(run.ModePlan, run.Work{Plan: "resourcePlans/aaaaaaaaaaaa", Mode: run.ModePlan, Variables: vars(main5)})
	if plan.Error != "" || actions(t, plan.PlanJSON) != `create kubernetes_manifest.database["main"]` || len(plan.PlanFile) == 0 {
		t.Fatalf("first plan: %+v", plan)
	}
	if !strings.Contains(string(plan.PlanJSON), `"storageClass":"longhorn-retain"`) || !strings.Contains(string(plan.PlanJSON), `"namespace":"payments-staging"`) {
		t.Errorf("plan JSON: %s", plan.PlanJSON)
	}
	if len(r.Databases()) != 0 {
		t.Errorf("a plan must change nothing: %v", r.Databases())
	}

	// Applying it makes the database, writes the state and names the Secret.
	applied := step(run.ModeApply, run.Work{Plan: "resourcePlans/aaaaaaaaaaaa", Mode: run.ModeApply, Variables: vars(main5), PlanFile: plan.PlanFile})
	if applied.Error != "" || applied.Secrets["main"] != "main-app" {
		t.Fatalf("apply: %+v", applied)
	}
	if got := r.Databases(); !slices.Equal(got, []string{"payments-staging/main 5Gi x1"}) {
		t.Errorf("databases: %v", got)
	}
	if !strings.Contains(string(api.state), `"serial":1`) || api.lock != "" {
		t.Errorf("state %s, lock %q", api.state, api.lock)
	}

	// The same settings again: nothing to do.
	same := step(run.ModePlan, run.Work{Plan: "resourcePlans/bbbbbbbbbbbb", Mode: run.ModePlan, Variables: vars(main5)})
	if actions(t, same.PlanJSON) != `no-op kubernetes_manifest.database["main"]` {
		t.Errorf("unchanged plan: %s", same.PlanJSON)
	}

	// More storage is an update; an empty list deletes.
	main10 := main5
	main10.StorageGi = 10
	grow := step(run.ModePlan, run.Work{Plan: "resourcePlans/cccccccccccc", Mode: run.ModePlan, Variables: vars(main10)})
	if actions(t, grow.PlanJSON) != `update kubernetes_manifest.database["main"]` {
		t.Errorf("grow plan: %s", grow.PlanJSON)
	}
	step(run.ModeApply, run.Work{Plan: "resourcePlans/cccccccccccc", Mode: run.ModeApply, Variables: vars(main10), PlanFile: grow.PlanFile})
	if got := r.Databases(); !slices.Equal(got, []string{"payments-staging/main 10Gi x1"}) {
		t.Errorf("after the update: %v", got)
	}
	if !strings.Contains(string(api.state), `"serial":2`) {
		t.Errorf("state after two applies: %s", api.state)
	}
	gone := step(run.ModePlan, run.Work{Plan: "resourcePlans/dddddddddddd", Mode: run.ModePlan, Variables: vars()})
	if actions(t, gone.PlanJSON) != `delete kubernetes_manifest.database["main"]` {
		t.Errorf("delete plan: %s", gone.PlanJSON)
	}
	removed := step(run.ModeApply, run.Work{Plan: "resourcePlans/dddddddddddd", Mode: run.ModeApply, Variables: vars(), PlanFile: gone.PlanFile})
	if len(r.Databases()) != 0 || len(removed.Secrets) != 0 {
		t.Errorf("after the delete: %v %v", r.Databases(), removed.Secrets)
	}

	if st, err := r.Status(ctx, "resourcePlans/dddddddddddd", run.ModeApply); err != nil || !st.Done || st.Failed {
		t.Errorf("status of a finished run: %+v %v", st, err)
	}
	if _, err := r.Status(ctx, "resourcePlans/none", run.ModePlan); err == nil {
		t.Error("status of an unknown run must fail")
	}
	if n := len(r.Started()); n != 7 {
		t.Errorf("%d runs started, want 7", n)
	}
}

func TestRunnerKnobs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	spec := run.Spec{Plan: "resourcePlans/aaaaaaaaaaaa", Mode: run.ModePlan, Token: "tok"}
	work := run.Work{Plan: spec.Plan, Mode: run.ModePlan, Variables: vars(run.Resource{Name: "main", Type: "postgres", StorageGi: 5, Instances: 1})}
	waitDone := func(r *Runner) run.Status {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if st, err := r.Status(ctx, spec.Plan, spec.Mode); err == nil && st.Done {
				return st
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatal("the run did not end")
		return run.Status{}
	}

	t.Run("a start error", func(t *testing.T) {
		r := &Runner{API: newRunAPI(work), StartErr: errors.New("no nodes")}
		if err := r.Start(ctx, spec); err == nil || err.Error() != "no nodes" {
			t.Errorf("got %v", err)
		}
	})
	t.Run("a failing plan reports its error", func(t *testing.T) {
		api := newRunAPI(work)
		r := &Runner{API: api, FailPlan: "tofu: boom"}
		t.Cleanup(r.Close)
		_ = r.Start(ctx, spec)
		if res := api.last(t); res.Error != "tofu: boom" || res.PlanJSON != nil {
			t.Errorf("result: %+v", res)
		}
		if st := waitDone(r); st.Failed {
			t.Errorf("a reported failure is a clean exit: %+v", st)
		}
	})
	t.Run("a crash ends without a result", func(t *testing.T) {
		api := newRunAPI(work)
		r := &Runner{API: api, Crash: "OOMKilled"}
		t.Cleanup(r.Close)
		_ = r.Start(ctx, spec)
		if st := waitDone(r); !st.Failed || st.Log != "OOMKilled" || len(api.results) != 0 {
			t.Errorf("status %+v, results %v", st, api.results)
		}
	})
	t.Run("a silent run ends cleanly without a result", func(t *testing.T) {
		api := newRunAPI(work)
		r := &Runner{API: api, Silent: true}
		t.Cleanup(r.Close)
		_ = r.Start(ctx, spec)
		if st := waitDone(r); st.Failed || len(api.results) != 0 {
			t.Errorf("status %+v, results %v", st, api.results)
		}
	})
	t.Run("a hanging run ends only when stopped", func(t *testing.T) {
		r := &Runner{API: newRunAPI(work), Hang: true}
		t.Cleanup(r.Close)
		_ = r.Start(ctx, spec)
		time.Sleep(20 * time.Millisecond)
		if st, _ := r.Status(ctx, spec.Plan, spec.Mode); st.Done {
			t.Fatalf("it ended by itself: %+v", st)
		}
		if err := r.Stop(ctx, spec.Plan, spec.Mode); err != nil {
			t.Fatal(err)
		}
		if st := waitDone(r); !st.Failed {
			t.Errorf("a stopped run: %+v", st)
		}
	})
	t.Run("a given plan and storage class", func(t *testing.T) {
		api := newRunAPI(work)
		r := &Runner{API: api, PlanJSON: []byte("not json")}
		t.Cleanup(r.Close)
		_ = r.Start(ctx, spec)
		if res := api.last(t); string(res.PlanJSON) != "not json" || string(res.PlanFile) != "not json" {
			t.Errorf("result: %+v", res)
		}
		api2 := newRunAPI(work)
		r2 := &Runner{API: api2, StorageClass: "local-path"}
		t.Cleanup(r2.Close)
		_ = r2.Start(ctx, spec)
		if res := api2.last(t); !strings.Contains(string(res.PlanJSON), `"storageClass":"local-path"`) {
			t.Errorf("plan: %s", res.PlanJSON)
		}
	})
}
```

Run: `go test ./internal/envtest -run TestRunner`
Expected: FAIL to build (`undefined: Runner`).

- [ ] **Step 3: Implement the fake runner**

Create `internal/envtest/runner.go`:

```go
package envtest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/showmino/forge/internal/run"
)

// Runner is a fake run.Launcher. It plays the runner and OpenTofu: a plan
// run compares the wanted resources with the databases it keeps in memory,
// in the shape `tofu show -json` gives a kubernetes_manifest; an apply run
// makes that plan's changes and writes the state. Set the knobs before Start.
type Runner struct {
	// API is forge-api's side of a run.
	API run.API
	// StartErr fails Start.
	StartErr error
	// FailPlan and FailApply make a run of that mode report this error.
	FailPlan, FailApply string
	// Crash makes a run end without reporting: Status says failed, with this log.
	Crash string
	// Hang keeps a run going until it is stopped.
	Hang bool
	// Silent makes a run end cleanly without reporting a result.
	Silent bool
	// StorageClass is what the fake module asks for (default longhorn-retain).
	StorageClass string
	// PlanJSON, when set, is handed back as the plan in place of a computed one.
	PlanJSON []byte

	mu        sync.Mutex
	databases map[string]run.Resource // "<namespace>/<name>"
	runs      map[string]*fakeRun     // "<plan>/<mode>"
	started   []run.Spec
	wg        sync.WaitGroup
}

type fakeRun struct {
	status run.Status
	stop   context.CancelFunc
}

// knobs are the Runner's settings as one run sees them.
type knobs struct {
	failPlan, failApply, crash, class string
	hang, silent                      bool
	planJSON                          []byte
	api                               run.API
}

// Start implements run.Launcher.
func (r *Runner) Start(_ context.Context, s run.Spec) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.StartErr != nil {
		return r.StartErr
	}
	if r.runs == nil {
		r.runs, r.databases = map[string]*fakeRun{}, map[string]run.Resource{}
	}
	k := knobs{failPlan: r.FailPlan, failApply: r.FailApply, crash: r.Crash, class: r.StorageClass,
		hang: r.Hang, silent: r.Silent, planJSON: r.PlanJSON, api: r.API}
	if k.class == "" {
		k.class = "longhorn-retain"
	}
	ctx, cancel := context.WithCancel(context.Background())
	fr := &fakeRun{stop: cancel}
	r.runs[s.Plan+"/"+s.Mode] = fr
	r.started = append(r.started, s)
	r.wg.Go(func() {
		defer cancel()
		st := r.play(ctx, k, s)
		r.mu.Lock()
		fr.status = st
		r.mu.Unlock()
	})
	return nil
}

// Status implements run.Launcher.
func (r *Runner) Status(_ context.Context, plan, mode string) (run.Status, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fr, ok := r.runs[plan+"/"+mode]
	if !ok {
		return run.Status{}, fmt.Errorf("no %s run for %s", mode, plan)
	}
	return fr.status, nil
}

// Stop implements run.Launcher.
func (r *Runner) Stop(_ context.Context, plan, mode string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if fr, ok := r.runs[plan+"/"+mode]; ok {
		fr.stop()
	}
	return nil
}

// Close stops every run and waits for them to end.
func (r *Runner) Close() {
	r.mu.Lock()
	for _, fr := range r.runs {
		fr.stop()
	}
	r.mu.Unlock()
	r.wg.Wait()
}

// Started returns the specs of every run started, in order.
func (r *Runner) Started() []run.Spec {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.started)
}

// Databases returns what exists, sorted: "<namespace>/<name> <n>Gi x<instances>".
func (r *Runner) Databases() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for key, d := range r.databases {
		out = append(out, fmt.Sprintf("%s %dGi x%d", key, d.StorageGi, d.Instances))
	}
	slices.Sort(out)
	return out
}

func failed(format string, a ...any) run.Status {
	return run.Status{Done: true, Failed: true, Log: fmt.Sprintf(format, a...)}
}

// play is one run, from start to exit.
func (r *Runner) play(ctx context.Context, k knobs, s run.Spec) run.Status {
	switch {
	case k.hang:
		<-ctx.Done()
		return failed("stopped")
	case k.crash != "":
		return failed("%s", k.crash)
	case k.silent:
		return run.Status{Done: true}
	}
	work, err := k.api.Claim(ctx, s.Token)
	if err != nil {
		return failed("claim: %v", err)
	}
	_ = k.api.Event(ctx, s.Token, "init", "OpenTofu initialised")
	var res run.Result
	if work.Mode == run.ModeApply {
		_ = k.api.Event(ctx, s.Token, "apply", "applying the saved plan")
		res = r.apply(ctx, k, s.Token, work)
	} else {
		_ = k.api.Event(ctx, s.Token, "plan", "planning")
		res = r.plan(k, work)
	}
	if err := k.api.Complete(ctx, s.Token, res); err != nil {
		return failed("complete: %v", err)
	}
	return run.Status{Done: true}
}

// object is the kubernetes_manifest value of a database.
func object(namespace string, d run.Resource, class string) map[string]any {
	return map[string]any{"manifest": map[string]any{
		"apiVersion": "postgresql.cnpg.io/v1", "kind": "Cluster",
		"metadata": map[string]any{"name": d.Name, "namespace": namespace},
		"spec": map[string]any{
			"instances": d.Instances,
			"storage":   map[string]any{"size": fmt.Sprintf("%dGi", d.StorageGi), "storageClass": class},
		},
	}}
}

type fakeChange struct {
	Address string `json:"address"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Change  struct {
		Actions []string `json:"actions"`
		Before  any      `json:"before"`
		After   any      `json:"after"`
	} `json:"change"`
}

func newChange(name string, actions []string, before, after any) fakeChange {
	c := fakeChange{Address: fmt.Sprintf("kubernetes_manifest.database[%q]", name), Type: "kubernetes_manifest", Name: "database"}
	c.Change.Actions, c.Change.Before, c.Change.After = actions, before, after
	return c
}

// plan compares what is wanted with what exists.
func (r *Runner) plan(k knobs, work *run.Work) run.Result {
	if k.failPlan != "" {
		return run.Result{Error: k.failPlan}
	}
	if k.planJSON != nil {
		return run.Result{PlanFile: k.planJSON, PlanJSON: k.planJSON}
	}
	ns := work.Variables.Namespace
	r.mu.Lock()
	defer r.mu.Unlock()
	changes := []fakeChange{}
	wanted := map[string]bool{}
	for _, d := range work.Variables.Resources {
		wanted[d.Name] = true
		cur, exists := r.databases[ns+"/"+d.Name]
		switch {
		case !exists:
			changes = append(changes, newChange(d.Name, []string{"create"}, nil, object(ns, d, k.class)))
		case cur != d:
			changes = append(changes, newChange(d.Name, []string{"update"}, object(ns, cur, k.class), object(ns, d, k.class)))
		default:
			changes = append(changes, newChange(d.Name, []string{"no-op"}, object(ns, cur, k.class), object(ns, cur, k.class)))
		}
	}
	for key, cur := range r.databases {
		if strings.HasPrefix(key, ns+"/") && !wanted[cur.Name] {
			changes = append(changes, newChange(cur.Name, []string{"delete"}, object(ns, cur, k.class), nil))
		}
	}
	slices.SortFunc(changes, func(a, b fakeChange) int { return strings.Compare(a.Address, b.Address) })
	b, err := json.Marshal(map[string]any{"format_version": "1.2", "resource_changes": changes})
	if err != nil {
		return run.Result{Error: err.Error()}
	}
	return run.Result{PlanFile: b, PlanJSON: b}
}

// apply makes the changes of the saved plan and writes the state.
func (r *Runner) apply(ctx context.Context, k knobs, token string, work *run.Work) run.Result {
	if k.failApply != "" {
		return run.Result{Error: k.failApply}
	}
	var doc struct {
		ResourceChanges []struct {
			Change struct {
				Actions []string `json:"actions"`
				Before  *side    `json:"before"`
				After   *side    `json:"after"`
			} `json:"change"`
		} `json:"resource_changes"`
	}
	if err := json.Unmarshal(work.PlanFile, &doc); err != nil {
		return run.Result{Error: "the saved plan is not readable: " + err.Error()}
	}
	lockID := "lock-" + work.Plan
	if _, err := k.api.LockState(ctx, token, lockID, []byte(`{"ID":"`+lockID+`"}`)); err != nil {
		return run.Result{Error: "lock the state: " + err.Error()}
	}
	defer func() { _ = k.api.UnlockState(ctx, token, lockID) }()
	old, err := k.api.ReadState(ctx, token)
	if err != nil {
		return run.Result{Error: "read the state: " + err.Error()}
	}
	var prev struct {
		Serial int `json:"serial"`
	}
	if len(old) > 0 {
		if err := json.Unmarshal(old, &prev); err != nil {
			return run.Result{Error: "the state is not readable: " + err.Error()}
		}
	}

	ns := work.Variables.Namespace
	r.mu.Lock()
	for _, rc := range doc.ResourceChanges {
		acts := rc.Change.Actions
		if slices.Contains(acts, "delete") && rc.Change.Before != nil {
			delete(r.databases, rc.Change.Before.key())
		}
		if (slices.Contains(acts, "create") || slices.Contains(acts, "update")) && rc.Change.After != nil {
			r.databases[rc.Change.After.key()] = rc.Change.After.resource()
		}
	}
	names := []string{}
	secrets := map[string]string{}
	for key, d := range r.databases {
		if strings.HasPrefix(key, ns+"/") {
			names = append(names, d.Name)
			secrets[d.Name] = d.Name + "-app"
		}
	}
	r.mu.Unlock()
	slices.Sort(names)

	state, err := json.Marshal(map[string]any{"version": 4, "serial": prev.Serial + 1, "resources": names})
	if err != nil {
		return run.Result{Error: err.Error()}
	}
	if err := k.api.WriteState(ctx, token, lockID, state); err != nil {
		if errors.Is(err, run.ErrLocked) {
			return run.Result{Error: "the state is locked by another run"}
		}
		return run.Result{Error: "write the state: " + err.Error()}
	}
	return run.Result{Secrets: secrets}
}

// side is the before or after of a fake change.
type side struct {
	Manifest struct {
		Metadata struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"metadata"`
		Spec struct {
			Instances int32 `json:"instances"`
			Storage   struct {
				Size string `json:"size"`
			} `json:"storage"`
		} `json:"spec"`
	} `json:"manifest"`
}

func (s *side) key() string { return s.Manifest.Metadata.Namespace + "/" + s.Manifest.Metadata.Name }

func (s *side) resource() run.Resource {
	var gi int32
	_, _ = fmt.Sscanf(s.Manifest.Spec.Storage.Size, "%dGi", &gi)
	return run.Resource{Name: s.Manifest.Metadata.Name, Type: "postgres", StorageGi: gi, Instances: s.Manifest.Spec.Instances}
}
```

- [ ] **Step 4: Run the tests**

Run: `go test -race ./internal/envtest ./internal/run && make lint`
Expected: PASS, lint clean.

- [ ] **Step 5: Commit**

```bash
git add internal/run internal/envtest
git commit -m "run: the launcher and runner API types; envtest: a fake runner that plays OpenTofu

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 6: Planning: `Resources`, runs, the runner API, outcomes

This is the core. A plan request saves a plan record, starts an Operation, and the Operation starts a runner and waits. The runner calls back with its token. Whoever ends the run (the runner's `Complete`, a failure seen by the Operation, or later the sweep) does so through `endRun`, which changes the plan's state once and appends the one outcome event.

**Files:**
- Create: `internal/server/resources.go`, `internal/server/resource_run.go`
- Create: `internal/server/resources_test.go`
- Modify: `internal/server/operations.go` (no outcome event for operations whose owner writes its own)
- Modify: `internal/server/audit.go` (one helper)
- Modify: `internal/server/authn.go` (`workloadProcedures`, one message)
- Modify: `internal/server/server.go` (`Options.Resources`, the handler)

**Interfaces:**
- Consumes:
  - Task 1: the proto types, `resourceNamePrefix`, `planNamePrefix`, `opResourcePlan`, `opResourceApply`.
  - Task 2: `resource.Resolve`, `resource.Digest`, `resource.Changes`, `resource.Count`.
  - Task 3: `store.ResourceStore`, `store.ResourceTx`, `store.PlanRecord`, `store.PlanQuery`, `store.ErrStateLocked`, `store.ErrNotFound`.
  - Task 4: `policy.Engine`, `policy.Forge`, `policy.Resource`, `policy.Actor`.
  - Task 5: `run.Launcher`, `run.API`, `run.Spec`, `run.Work`, `run.Result`, `run.Variables`, `run.Resource`, `run.ModePlan`, `run.ModeApply`, `run.ErrLocked`; `envtest.Runner`.
  - Existing in package `server`: `require`, `workloadOf`, `workloadDenied`, `actorOf`, `requestKey`, `parseName`, `validEnv`, `notFound`, `invalidArg`, `storeErr`, `clip`, `auditText`, `auditMessageMax`, `auditFrom`, `details`, `flag`, `auditor`, `Operations`, `OpError`, `OpCodeCancelled`, `OpCodeInternal`, `servicePrefix`, `defaultPageSize`, `maxPageSize`.
- Produces, in package `server`:

```go
type ServiceNamespaces interface {
	ServiceNamespaceFree(ctx context.Context, service, env string) error
	EnsureServiceNamespace(ctx context.Context, service, env string) error
}
type DatabaseHealth interface {
	DatabaseHealth(ctx context.Context, service, env, name string) (*forgev1.ResourceHealth, error)
}
type ResourceOptions struct {
	Store      store.ResourceStore
	Catalog    *Catalog
	Operations *Operations
	Policy     *policy.Engine
	Launcher   run.Launcher
	Namespaces ServiceNamespaces // nil: no namespace checks
	Health     DatabaseHealth    // nil: health is unknown
	RunTimeout time.Duration     // default 15m
	PlanTTL    time.Duration     // default 24h
	Poll       time.Duration     // default 2s
	Grace      time.Duration     // default 10s
	Now        func() time.Time
	Log        *slog.Logger
}
func NewResources(o ResourceOptions) (*Resources, error)
// *Resources implements run.API and, after Task 7, forgev1connect.ResourceServiceHandler.
const OpCodePolicyDenied = "policy_denied"
```

  - Used by Task 7 and 8, unexported: `(*Resources).ready`, `view`, `startRun`, `endRun`, `evaluate(ctx, rec, planJSON, actor policy.Actor)`, `planTarget`, `resourceName`, `precondition`, `parsePlanName`, `newRunToken`, `modeOf`, `runEnd`, `(*Resources).live` (the map of watched runs, guarded by `mu`), `noteAuditDetail`.
  - `Options.Resources *Resources` in `server.New`.

- [ ] **Step 1: Let an operation's owner write the outcome event**

In `internal/server/operations.go`:

Add after the `opNameKey` type:

```go
// noOutcomeKey marks a context whose operation appends no outcome event.
type noOutcomeKey struct{}

// withoutOutcome returns ctx marked so an operation started with it appends
// no outcome event: its owner appends one itself. Resource runs do, because
// a run can end after forge-api restarted and its operation is gone.
func withoutOutcome(ctx context.Context) context.Context {
	return context.WithValue(ctx, noOutcomeKey{}, true)
}
```

In the `operation` struct, replace the `auditEv` line with these two (the second is new; gofmt aligns them):

```go
	auditEv   *forgev1.AuditEvent // the outcome event's template; set before the work starts
	noOutcome bool                // the owner appends the outcome event
```

In `Start`, after the line `o.auditEv.Details, o.auditEv.Client, o.auditEv.Result, o.auditEv.Name, o.auditEv.Time = nil, nil, nil, "", nil`, add:

```go
	o.noOutcome = ctx.Value(noOutcomeKey{}) != nil
```

In `run`, change `if o.auditEv != nil {` to:

```go
	if o.auditEv != nil && !o.noOutcome {
```

Append to `internal/server/operations_test.go`:

```go
func TestOperationWithoutOutcome(t *testing.T) {
	t.Parallel()
	st := store.NewMemory()
	ops := NewOperations(nil)
	ops.setAuditor(&auditor{store: st, log: slog.New(slog.DiscardHandler)})
	op := ops.Start(withoutOutcome(context.Background()), "resource.plan", "resources/payments/staging", "ana",
		func(context.Context, func(string, string)) error { return nil })
	wait(t, ops, op.GetName())
	loud := ops.Start(context.Background(), "deploy", "deployments/payments/staging", "ana",
		func(context.Context, func(string, string)) error { return nil })
	wait(t, ops, loud.GetName())
	waitEvents(t, st, 1)
	time.Sleep(50 * time.Millisecond) // an outcome of the quiet one would have landed by now
	evs := allEvents(t, st)
	if len(evs) != 1 || evs[0].GetOperation() != loud.GetName() {
		t.Errorf("events: %v", evs)
	}
}
```

If `operations_test.go` does not import `log/slog` or `store` yet, add them.

Run: `go test ./internal/server -run 'TestOperation'`
Expected: PASS.

- [ ] **Step 2: Add an audit helper**

In `internal/server/audit.go`, after `noteAuditOperation`:

```go
// noteAuditDetail adds one detail to the call's event. Like the details of
// describe, value must be an identifier or a setting, never free text.
func noteAuditDetail(ctx context.Context, key, value string) {
	if r := auditFrom(ctx); r != nil && value != "" {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.ev.Details == nil {
			r.ev.Details = map[string]string{}
		}
		r.ev.Details[key] = auditText(value, auditFieldMax)
	}
}
```

- [ ] **Step 3: Write the test fixture and the first failing tests**

Create `internal/server/resources_test.go`:

```go
package server

import (
	"context"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/gen/forge/v1/forgev1connect"
	"github.com/showmino/forge/internal/envtest"
	"github.com/showmino/forge/internal/run"
	"github.com/showmino/forge/internal/store"
	"github.com/showmino/forge/policy"
)

// resourceFixture is a Forge API with resources on a fake runner.
// Tokens: "writer" (ana), "reader" (bo), "ci" (payments' CI), "ci-billing".
type resourceFixture struct {
	res    *Resources
	runner *envtest.Runner
	store  *store.Memory
	ops    *Operations
	cat    *Catalog
	client forgev1connect.ResourceServiceClient
	opc    forgev1connect.OperationServiceClient
}

func newResourceFixture(t *testing.T, tweak func(*ResourceOptions)) *resourceFixture {
	t.Helper()
	st := store.NewMemory()
	f := &resourceFixture{runner: &envtest.Runner{}, store: st, ops: NewOperations(nil), cat: NewCatalogWithStore(st, nil)}
	for _, id := range []string{"payments", "billing"} {
		register(t, f.cat, id)
	}
	pol, err := policy.New(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	o := ResourceOptions{
		Store: st, Catalog: f.cat, Operations: f.ops, Policy: pol, Launcher: f.runner,
		Poll: 2 * time.Millisecond, Grace: 30 * time.Millisecond,
	}
	if tweak != nil {
		tweak(&o)
	}
	if f.res, err = NewResources(o); err != nil {
		t.Fatal(err)
	}
	f.runner.API = f.res
	t.Cleanup(f.runner.Close)
	h, err := New(Options{
		Authenticator: StaticAuthenticator{
			"writer": {Subject: "ana", Email: "ana@example.com", Groups: []string{"platform"}},
			"reader": {Subject: "bo", Email: "bo@example.com", Groups: []string{"dev"}},
			"ci": {Subject: "github:showmino/payments", Permissions: []string{PermDeploy("payments", "staging")},
				Workload: &Workload{Repository: "showmino/payments", Service: "payments", SHA: envSHA}},
			"ci-billing": {Subject: "github:showmino/billing", Permissions: []string{PermDeploy("billing", "staging")},
				Workload: &Workload{Repository: "showmino/billing", Service: "billing", SHA: envSHA}},
		},
		Authorizer: Authorizer{WriterGroups: []string{"platform"}},
		Catalog:    f.cat, Operations: f.ops, Resources: f.res, Audit: st,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	f.client = forgev1connect.NewResourceServiceClient(srv.Client(), srv.URL)
	f.opc = forgev1connect.NewOperationServiceClient(srv.Client(), srv.URL)
	return f
}

// database declares one Postgres database.
func database(storage string, instances int32) []*forgev1.ResourceDeclaration {
	return []*forgev1.ResourceDeclaration{{Name: "main", Type: "postgres", Storage: storage, Instances: instances}}
}

// startPlan asks for a plan and returns the response.
func (f *resourceFixture) startPlan(t *testing.T, token, service, env string, decls []*forgev1.ResourceDeclaration, allowDestroy bool) *forgev1.PlanResourcesResponse {
	t.Helper()
	resp, err := f.client.PlanResources(context.Background(), as(token, &forgev1.PlanResourcesRequest{
		Service: service, Env: env, Resources: decls, AllowDestroy: allowDestroy,
	}))
	if err != nil {
		t.Fatalf("plan %s (%s): %v", service, env, err)
	}
	return resp.Msg
}

// plan asks for a plan, waits for its operation, and returns both as they ended.
func (f *resourceFixture) plan(t *testing.T, token, env string, decls []*forgev1.ResourceDeclaration, allowDestroy bool) (*forgev1.ResourcePlan, *forgev1.Operation) {
	t.Helper()
	resp := f.startPlan(t, token, "services/payments", env, decls, allowDestroy)
	op := waitCIOp(t, f.ops, resp.GetOperation().GetName())
	return f.getPlan(t, resp.GetPlan().GetName()), op
}

func (f *resourceFixture) getPlan(t *testing.T, name string) *forgev1.ResourcePlan {
	t.Helper()
	got, err := f.client.GetResourcePlan(context.Background(), as("writer", &forgev1.GetResourcePlanRequest{Name: name}))
	if err != nil {
		t.Fatal(err)
	}
	return got.Msg.GetPlan()
}

// outcomes returns the outcome events about plan, oldest first.
func (f *resourceFixture) outcomes(t *testing.T, plan string) []*forgev1.AuditEvent {
	t.Helper()
	var out []*forgev1.AuditEvent
	for _, e := range allEvents(t, f.store) {
		if e.GetKind() == "outcome" && e.GetDetails()["plan"] == plan {
			out = append(out, e)
		}
	}
	return out
}

func wantCode(t *testing.T, what string, err error, code connect.Code, contains string) {
	t.Helper()
	if connect.CodeOf(err) != code || err == nil || !strings.Contains(err.Error(), contains) {
		t.Errorf("%s: %v, want %s containing %q", what, err, code, contains)
	}
}

func TestPlanResources(t *testing.T) {
	t.Parallel()
	f := newResourceFixture(t, nil)
	p, op := f.plan(t, "writer", "staging", database("5Gi", 0), false)

	if op.GetError() != nil || op.GetKind() != "resource.plan" || op.GetTarget() != "resources/payments/staging" || op.GetCreator() != "ana@example.com" {
		t.Fatalf("operation: %v", op)
	}
	if steps := eventSteps(op); !strings.Contains(steps, "launch") || !strings.Contains(steps, "init") || !strings.Contains(steps, "policy") {
		t.Errorf("events: %s", steps)
	}
	if p.GetState() != forgev1.ResourcePlan_STATE_PLANNED || p.GetService() != "services/payments" || p.GetEnv() != "staging" ||
		p.GetCreator() != "ana@example.com" || p.GetOperation() != op.GetName() || !strings.HasPrefix(p.GetName(), "resourcePlans/") {
		t.Errorf("plan: %v", p)
	}
	if rs := p.GetResources(); len(rs) != 1 || rs[0].GetStorageGi() != 5 || rs[0].GetInstances() != 1 {
		t.Errorf("settings: %v", rs)
	}
	if cs := p.GetChanges(); len(cs) != 1 || cs[0].GetAction() != "create" || cs[0].GetResource() != "main" || cs[0].GetAfter().GetStorageGi() != 5 {
		t.Errorf("changes: %v", cs)
	}
	if pol := p.GetPolicy(); pol == nil || pol.GetDenied() || len(pol.GetRules()) != 12 {
		t.Errorf("policy: %v", pol)
	}
	if got := p.GetExpireTime().AsTime().Sub(p.GetCreateTime().AsTime()); got != 24*time.Hour {
		t.Errorf("expires after %s, want 24h", got)
	}
	if len(f.runner.Databases()) != 0 {
		t.Errorf("a plan must change nothing: %v", f.runner.Databases())
	}

	// The audit log has the request and one outcome, by the same person.
	var req *forgev1.AuditEvent
	for _, e := range allEvents(t, f.store) {
		if e.GetKind() == "request" && e.GetAction() == "resource.plan" {
			req = e
		}
	}
	if req == nil || req.GetActor().GetEmail() != "ana@example.com" || req.GetService() != "services/payments" ||
		req.GetTarget() != "resources/payments/staging" || req.GetOperation() != op.GetName() ||
		req.GetDetails()["environment"] != "staging" || req.GetDetails()["plan"] != p.GetName() || req.GetResult().GetCode() != "ok" {
		t.Errorf("request event: %v", req)
	}
	outs := f.outcomes(t, p.GetName())
	if len(outs) != 1 {
		t.Fatalf("%d outcome events, want 1: %v", len(outs), outs)
	}
	out := outs[0]
	if out.GetAction() != "resource.plan" || out.GetActor().GetEmail() != "ana@example.com" || out.GetService() != "services/payments" ||
		out.GetTarget() != "resources/payments/staging" || out.GetOperation() != op.GetName() || out.GetResult().GetCode() != "ok" {
		t.Errorf("outcome event: %v", out)
	}
	if d := out.GetDetails(); d["environment"] != "staging" || d["policy"] != "passed" || d["policy_rules"] != p.GetPolicy().GetRules() ||
		d["changes"] != "create=1 update=0 delete=0" || d["allow_destroy"] != "" {
		t.Errorf("outcome details: %v", d)
	}

	// The record keeps the plan files; the API has no field to show them.
	rec, err := f.store.GetResourcePlan(context.Background(), p.GetName())
	if err != nil || len(rec.PlanFile) == 0 || len(rec.PlanJSON) == 0 || rec.TokenHash != "" || !rec.Deadline.IsZero() || rec.CreatorSubject != "ana" {
		t.Errorf("record: %+v %v", rec, err)
	}
}

func TestPlanDenied(t *testing.T) {
	t.Parallel()
	f := newResourceFixture(t, nil)
	p, op := f.plan(t, "writer", "production", database("200Gi", 2), false)
	if op.GetError().GetCode() != "policy_denied" || !strings.Contains(op.GetError().GetMessage(), "storage 200Gi is above the 50Gi limit for production") {
		t.Errorf("operation error: %v", op.GetError())
	}
	if p.GetState() != forgev1.ResourcePlan_STATE_DENIED || !p.GetPolicy().GetDenied() ||
		!slices.Equal(p.GetPolicy().GetDeny(), []string{"storage 200Gi is above the 50Gi limit for production"}) {
		t.Errorf("plan: %v", p)
	}
	rec, _ := f.store.GetResourcePlan(context.Background(), p.GetName())
	if len(rec.PlanFile) != 0 || len(rec.PlanJSON) != 0 {
		t.Error("a denied plan keeps no plan file")
	}
	outs := f.outcomes(t, p.GetName())
	if len(outs) != 1 || outs[0].GetResult().GetCode() != "failed" || outs[0].GetDetails()["policy"] != "denied" ||
		!strings.HasPrefix(outs[0].GetResult().GetMessage(), "policy_denied: storage 200Gi") {
		t.Errorf("outcome: %v", outs)
	}

	// A warning does not block.
	w, op := f.plan(t, "writer", "staging", database("5Gi", 2), false)
	if op.GetError() != nil || w.GetState() != forgev1.ResourcePlan_STATE_PLANNED || len(w.GetPolicy().GetWarn()) != 1 {
		t.Errorf("warned plan: %v %v", w, op.GetError())
	}
}

func TestPlanAccess(t *testing.T) {
	t.Parallel()
	f := newResourceFixture(t, nil)
	ctx := context.Background()
	req := func(service, env string) *forgev1.PlanResourcesRequest {
		return &forgev1.PlanResourcesRequest{Service: service, Env: env, Resources: database("5Gi", 0)}
	}

	_, err := f.client.PlanResources(ctx, as("reader", req("services/payments", "staging")))
	wantCode(t, "a reader", err, connect.CodePermissionDenied, "services.write")
	_, err = f.client.PlanResources(ctx, connect.NewRequest(req("services/payments", "staging")))
	wantCode(t, "no token", err, connect.CodeUnauthenticated, "")

	// CI plans its own service, in either environment, and never with allow_destroy.
	for _, env := range []string{"staging", "production"} {
		resp := f.startPlan(t, "ci", "services/payments", env, database("5Gi", 2), false)
		if op := waitCIOp(t, f.ops, resp.GetOperation().GetName()); op.GetCreator() != "github:showmino/payments" {
			t.Errorf("%s: creator %q", env, op.GetCreator())
		}
		// It may read its own plan and follow its own operation.
		if _, err := f.client.GetResourcePlan(ctx, as("ci", &forgev1.GetResourcePlanRequest{Name: resp.GetPlan().GetName()})); err != nil {
			t.Errorf("CI reads its plan: %v", err)
		}
		if _, err := f.opc.GetOperation(ctx, as("ci", &forgev1.GetOperationRequest{Name: resp.GetOperation().GetName()})); err != nil {
			t.Errorf("CI reads its operation: %v", err)
		}
		_, err := f.client.GetResourcePlan(ctx, as("ci-billing", &forgev1.GetResourcePlanRequest{Name: resp.GetPlan().GetName()}))
		wantCode(t, "another repo's CI reads the plan", err, connect.CodePermissionDenied, "only read the plans of services/billing")
	}
	_, err = f.client.PlanResources(ctx, as("ci-billing", req("services/payments", "staging")))
	wantCode(t, "another repo's CI", err, connect.CodePermissionDenied, "only plan the resources of services/billing")
	destroy := req("services/payments", "staging")
	destroy.AllowDestroy = true
	_, err = f.client.PlanResources(ctx, as("ci", destroy))
	wantCode(t, "CI with allow_destroy", err, connect.CodePermissionDenied, "not plan with allow_destroy")
	for _, call := range []func() error{
		func() error {
			_, err := f.client.ListResourcePlans(ctx, as("ci", &forgev1.ListResourcePlansRequest{}))
			return err
		},
		func() error {
			_, err := f.client.ApplyResourcePlan(ctx, as("ci", &forgev1.ApplyResourcePlanRequest{Name: "resourcePlans/0123456789ab"}))
			return err
		},
		func() error {
			_, err := f.client.ListResources(ctx, as("ci", &forgev1.ListResourcesRequest{}))
			return err
		},
	} {
		wantCode(t, "CI on another call", call(), connect.CodePermissionDenied, "CI token")
	}

	_, err = f.client.PlanResources(ctx, as("writer", req("services/nope", "staging")))
	wantCode(t, "an unknown service", err, connect.CodeNotFound, "services/nope")
	_, err = f.client.PlanResources(ctx, as("writer", req("services/payments", "qa")))
	wantCode(t, "an unknown environment", err, connect.CodeInvalidArgument, "staging or production")
	bad := req("services/payments", "staging")
	bad.Resources = database("5GB", 0)
	_, err = f.client.PlanResources(ctx, as("writer", bad))
	wantCode(t, "a bad size", err, connect.CodeInvalidArgument, `resources: main: storage "5GB"`)
	_, err = f.client.GetResourcePlan(ctx, as("writer", &forgev1.GetResourcePlanRequest{Name: "plans/x"}))
	wantCode(t, "a bad plan name", err, connect.CodeInvalidArgument, "resourcePlans/<id>")
	_, err = f.client.GetResourcePlan(ctx, as("writer", &forgev1.GetResourcePlanRequest{Name: "resourcePlans/0123456789ab"}))
	wantCode(t, "a missing plan", err, connect.CodeNotFound, "resourcePlans/0123456789ab")

	// Resources off: every call says so.
	var off *Resources
	_, err = off.PlanResources(writerCtx(), connect.NewRequest(req("services/payments", "staging")))
	wantCode(t, "resources off", err, connect.CodeFailedPrecondition, "resources are not configured on this Forge API")
	_, err = off.GetResourcePlan(writerCtx(), connect.NewRequest(&forgev1.GetResourcePlanRequest{}))
	wantCode(t, "resources off", err, connect.CodeFailedPrecondition, "resources are not configured")
	_, err = off.ListResourcePlans(writerCtx(), connect.NewRequest(&forgev1.ListResourcePlansRequest{}))
	wantCode(t, "resources off", err, connect.CodeFailedPrecondition, "resources are not configured")
}

func TestPlanOneAtATimeAndRequestID(t *testing.T) {
	t.Parallel()
	f := newResourceFixture(t, nil)
	f.runner.Hang = true
	ctx := context.Background()
	req := &forgev1.PlanResourcesRequest{Service: "services/payments", Env: "staging", Resources: database("5Gi", 0), RequestId: "req-1"}
	first, err := f.client.PlanResources(ctx, as("writer", req))
	if err != nil {
		t.Fatal(err)
	}
	// The same request id returns the same operation and plan.
	again, err := f.client.PlanResources(ctx, as("writer", req))
	if err != nil || again.Msg.GetOperation().GetName() != first.Msg.GetOperation().GetName() || again.Msg.GetPlan().GetName() != first.Msg.GetPlan().GetName() {
		t.Errorf("replay: %v %v", again, err)
	}
	// Another plan for the same target is refused and names the running operation.
	other := &forgev1.PlanResourcesRequest{Service: "services/payments", Env: "staging", Resources: database("6Gi", 0)}
	_, err = f.client.PlanResources(ctx, as("writer", other))
	wantCode(t, "a second plan", err, connect.CodeFailedPrecondition, first.Msg.GetOperation().GetName()+" is already running for resources/payments/staging")
	// Another environment is free.
	prod := f.startPlan(t, "writer", "services/payments", "production", database("5Gi", 2), false)
	if prod.GetPlan().GetName() == first.Msg.GetPlan().GetName() {
		t.Error("production got staging's plan")
	}
}

func TestPlanCancel(t *testing.T) {
	t.Parallel()
	f := newResourceFixture(t, nil)
	f.runner.Hang = true
	ctx := context.Background()
	resp := f.startPlan(t, "writer", "services/payments", "staging", database("5Gi", 0), false)
	// Wait until the runner started, then cancel.
	deadline := time.Now().Add(5 * time.Second)
	for len(f.runner.Started()) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if _, err := f.opc.CancelOperation(ctx, as("writer", &forgev1.CancelOperationRequest{Name: resp.GetOperation().GetName()})); err != nil {
		t.Fatal(err)
	}
	op := waitCIOp(t, f.ops, resp.GetOperation().GetName())
	if op.GetError().GetCode() != "cancelled" {
		t.Errorf("operation: %v", op.GetError())
	}
	p := f.getPlan(t, resp.GetPlan().GetName())
	if p.GetState() != forgev1.ResourcePlan_STATE_FAILED || p.GetMessage() != "the run was cancelled" {
		t.Errorf("plan: %v", p)
	}
	outs := f.outcomes(t, p.GetName())
	if len(outs) != 1 || outs[0].GetResult().GetCode() != "cancelled" {
		t.Errorf("outcomes: %v", outs)
	}
	// The runner was stopped and the target is free again.
	stopped := false
	for end := time.Now().Add(5 * time.Second); !stopped && time.Now().Before(end); time.Sleep(time.Millisecond) {
		st, err := f.runner.Status(ctx, p.GetName(), run.ModePlan)
		stopped = err == nil && st.Done
	}
	if !stopped {
		t.Error("the runner was not stopped")
	}
	f.runner.Hang = false
	if p2, op2 := f.plan(t, "writer", "staging", database("5Gi", 0), false); op2.GetError() != nil || p2.GetState() != forgev1.ResourcePlan_STATE_PLANNED {
		t.Errorf("a plan after the cancel: %v %v", p2, op2.GetError())
	}
}

func TestPlanFailures(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		set     func(*envtest.Runner)
		tweak   func(*ResourceOptions)
		code    string
		message string
	}{
		"OpenTofu fails":             {func(r *envtest.Runner) { r.FailPlan = "tofu: boom" }, nil, "run_failed", "tofu: boom"},
		"the runner crashes":         {func(r *envtest.Runner) { r.Crash = "OOMKilled" }, nil, "runner_failed", "the runner failed: OOMKilled"},
		"the runner says nothing":    {func(r *envtest.Runner) { r.Silent = true }, nil, "runner_failed", "the runner ended without a result"},
		"the runner cannot start":    {func(r *envtest.Runner) { r.StartErr = context.DeadlineExceeded }, nil, "launch_failed", "the runner could not be started"},
		"the run takes too long":     {func(r *envtest.Runner) { r.Hang = true }, func(o *ResourceOptions) { o.RunTimeout = 40 * time.Millisecond }, "run_timeout", "the run did not finish within 40ms"},
		"control characters removed": {func(r *envtest.Runner) { r.FailPlan = "bad\x1b[31m thing\x00" }, nil, "run_failed", "bad�[31m thing"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newResourceFixture(t, tc.tweak)
			tc.set(f.runner)
			p, op := f.plan(t, "writer", "staging", database("5Gi", 0), false)
			if op.GetError().GetCode() != tc.code || !strings.Contains(op.GetError().GetMessage(), tc.message) {
				t.Errorf("operation error: %v", op.GetError())
			}
			if p.GetState() != forgev1.ResourcePlan_STATE_FAILED || !strings.Contains(p.GetMessage(), tc.message) || p.GetPolicy() != nil {
				t.Errorf("plan: %v", p)
			}
			outs := f.outcomes(t, p.GetName())
			if len(outs) != 1 || outs[0].GetResult().GetCode() != "failed" || !strings.HasPrefix(outs[0].GetResult().GetMessage(), tc.code+": ") {
				t.Errorf("outcomes: %v", outs)
			}
			// The target is free again.
			f.runner.FailPlan, f.runner.Crash, f.runner.Silent, f.runner.StartErr, f.runner.Hang = "", "", false, nil, false
			f.startPlan(t, "writer", "services/payments", "staging", database("5Gi", 0), false)
		})
	}
}

func TestPlanUnreadableResult(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		planJSON []byte
		message  string
	}{
		"not JSON": {[]byte("tofu says hi"), "the plan could not be read"},
		"a list":   {[]byte("[]"), "the plan could not be read"},
		"empty":    {[]byte{}, "the runner returned no plan"},
		"too big":  {append([]byte(`{"x":"`), append(make([]byte, 9<<20), `"}`...)...), "the plan is larger than"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newResourceFixture(t, nil)
			f.runner.PlanJSON = tc.planJSON
			p, op := f.plan(t, "writer", "staging", database("5Gi", 0), false)
			if p.GetState() != forgev1.ResourcePlan_STATE_FAILED || !strings.Contains(p.GetMessage(), tc.message) || op.GetError().GetCode() != "run_failed" {
				t.Errorf("plan %v, operation error %v", p, op.GetError())
			}
			rec, _ := f.store.GetResourcePlan(context.Background(), p.GetName())
			if len(rec.PlanFile) != 0 || len(rec.PlanJSON) != 0 {
				t.Error("a failed plan keeps no plan file")
			}
		})
	}
	// A plan with nothing in it is a plan with no changes.
	f := newResourceFixture(t, nil)
	f.runner.PlanJSON = []byte(`{}`)
	p, op := f.plan(t, "writer", "staging", database("5Gi", 0), false)
	if p.GetState() != forgev1.ResourcePlan_STATE_PLANNED || len(p.GetChanges()) != 0 || op.GetError() != nil {
		t.Errorf("an empty plan: %v %v", p, op.GetError())
	}
}

func TestRunTokens(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	f := newResourceFixture(t, func(o *ResourceOptions) { o.Now = clock; o.RunTimeout = time.Hour })
	f.runner.Hang = true // the test plays the runner itself
	ctx := context.Background()
	resp := f.startPlan(t, "writer", "services/payments", "staging", database("5Gi", 0), false)
	deadline := time.Now().Add(5 * time.Second)
	for len(f.runner.Started()) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	spec := f.runner.Started()[0]
	if len(spec.Token) != 64 || spec.Plan != resp.GetPlan().GetName() || spec.Mode != run.ModePlan || spec.Service != "payments" || spec.Env != "staging" {
		t.Fatalf("spec: %+v", spec)
	}
	// Only the token's hash is stored.
	rec, _ := f.store.GetResourcePlan(ctx, spec.Plan)
	if rec.TokenHash == "" || rec.TokenHash == spec.Token || strings.Contains(string(rec.Audit), spec.Token) {
		t.Errorf("stored token hash %q", rec.TokenHash)
	}

	for _, bad := range []string{"", "nope", strings.Repeat("0", 64)} {
		_, err := f.res.Claim(ctx, bad)
		wantCode(t, "claim with a wrong token", err, connect.CodeUnauthenticated, "the run token is not valid")
		wantCode(t, "complete with a wrong token", f.res.Complete(ctx, bad, run.Result{Error: "x"}), connect.CodeUnauthenticated, "not valid")
		wantCode(t, "event with a wrong token", f.res.Event(ctx, bad, "s", "m"), connect.CodeUnauthenticated, "not valid")
		_, err = f.res.ReadState(ctx, bad)
		wantCode(t, "state with a wrong token", err, connect.CodeUnauthenticated, "not valid")
	}

	work, err := f.res.Claim(ctx, spec.Token)
	if err != nil {
		t.Fatal(err)
	}
	if work.Plan != spec.Plan || work.Mode != run.ModePlan || work.PlanFile != nil || work.Variables.Namespace != "payments-staging" ||
		work.Variables.Service != "payments" || work.Variables.Environment != "staging" ||
		!slices.Equal(work.Variables.Resources, []run.Resource{{Name: "main", Type: "postgres", StorageGi: 5, Instances: 1}}) {
		t.Errorf("work: %+v", work)
	}
	// A plan run may lock and read the state, never write it.
	if _, err := f.res.LockState(ctx, spec.Token, "lock-1", []byte(`{"ID":"lock-1"}`)); err != nil {
		t.Errorf("lock: %v", err)
	}
	if data, err := f.res.ReadState(ctx, spec.Token); err != nil || data != nil {
		t.Errorf("read state: %s %v", data, err)
	}
	wantCode(t, "a plan run writing state", f.res.WriteState(ctx, spec.Token, "lock-1", []byte("x")), connect.CodeFailedPrecondition, "a plan run may not write the state")
	if err := f.res.UnlockState(ctx, spec.Token, "lock-1"); err != nil {
		t.Errorf("unlock: %v", err)
	}
	if err := f.res.Event(ctx, spec.Token, "plan", "planning\x07"); err != nil {
		t.Errorf("event: %v", err)
	}

	// Past the deadline the token is dead.
	mu.Lock()
	now = now.Add(2 * time.Hour)
	mu.Unlock()
	_, err = f.res.Claim(ctx, spec.Token)
	wantCode(t, "an expired token", err, connect.CodeUnauthenticated, "not valid")
	mu.Lock()
	now = now.Add(-2 * time.Hour)
	mu.Unlock()

	// Completing works once.
	planJSON := []byte(`{"resource_changes":[]}`)
	if err := f.res.Complete(ctx, spec.Token, run.Result{PlanFile: planJSON, PlanJSON: planJSON}); err != nil {
		t.Fatal(err)
	}
	op := waitCIOp(t, f.ops, resp.GetOperation().GetName())
	if op.GetError() != nil || !strings.Contains(eventSteps(op), "plan") {
		t.Errorf("operation: %v", op)
	}
	wantCode(t, "a second complete", f.res.Complete(ctx, spec.Token, run.Result{Error: "late"}), connect.CodeUnauthenticated, "not valid")
	_, err = f.res.Claim(ctx, spec.Token)
	wantCode(t, "a claim after the end", err, connect.CodeUnauthenticated, "not valid")
	if p := f.getPlan(t, spec.Plan); p.GetState() != forgev1.ResourcePlan_STATE_PLANNED {
		t.Errorf("plan: %v", p)
	}
	if outs := f.outcomes(t, spec.Plan); len(outs) != 1 || outs[0].GetResult().GetCode() != "ok" {
		t.Errorf("outcomes: %v", outs)
	}
}

func TestCompleteOnce(t *testing.T) {
	t.Parallel()
	f := newResourceFixture(t, nil)
	f.runner.Hang = true
	ctx := context.Background()
	resp := f.startPlan(t, "writer", "services/payments", "staging", database("5Gi", 0), false)
	deadline := time.Now().Add(5 * time.Second)
	for len(f.runner.Started()) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	token := f.runner.Started()[0].Token
	planJSON := []byte(`{"resource_changes":[]}`)
	var wg sync.WaitGroup
	var okMu sync.Mutex
	ok := 0
	for range 8 {
		wg.Go(func() {
			if err := f.res.Complete(ctx, token, run.Result{PlanFile: planJSON, PlanJSON: planJSON}); err == nil {
				okMu.Lock()
				ok++
				okMu.Unlock()
			}
		})
	}
	wg.Wait()
	waitCIOp(t, f.ops, resp.GetOperation().GetName())
	if ok != 1 {
		t.Errorf("%d of 8 completes worked, want 1", ok)
	}
	if outs := f.outcomes(t, resp.GetPlan().GetName()); len(outs) != 1 {
		t.Errorf("%d outcome events, want 1", len(outs))
	}
}

func TestListAndViewPlans(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	f := newResourceFixture(t, func(o *ResourceOptions) { o.Now = clock })
	ctx := context.Background()
	a, _ := f.plan(t, "writer", "staging", database("5Gi", 0), false)
	b, _ := f.plan(t, "ci", "staging", database("6Gi", 0), false)
	c, _ := f.plan(t, "writer", "production", database("200Gi", 2), false) // denied

	list := func(token string, req *forgev1.ListResourcePlansRequest) []string {
		t.Helper()
		resp, err := f.client.ListResourcePlans(ctx, as(token, req))
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, p := range resp.Msg.GetPlans() {
			names = append(names, p.GetName())
		}
		return names
	}
	if got := list("reader", &forgev1.ListResourcePlansRequest{}); !slices.Equal(got, []string{c.GetName(), b.GetName(), a.GetName()}) {
		t.Errorf("all, newest first: %v", got)
	}
	if got := list("writer", &forgev1.ListResourcePlansRequest{Service: "payments", Env: "staging"}); !slices.Equal(got, []string{b.GetName(), a.GetName()}) {
		t.Errorf("staging: %v", got)
	}
	if got := list("writer", &forgev1.ListResourcePlansRequest{Mine: true}); !slices.Equal(got, []string{c.GetName(), a.GetName()}) {
		t.Errorf("mine: %v", got)
	}
	if got := list("writer", &forgev1.ListResourcePlansRequest{States: []forgev1.ResourcePlan_State{forgev1.ResourcePlan_STATE_DENIED}}); !slices.Equal(got, []string{c.GetName()}) {
		t.Errorf("denied: %v", got)
	}
	// Paging.
	first, err := f.client.ListResourcePlans(ctx, as("writer", &forgev1.ListResourcePlansRequest{PageSize: 2}))
	if err != nil || len(first.Msg.GetPlans()) != 2 || first.Msg.GetNextPageToken() == "" {
		t.Fatalf("first page: %v %v", first, err)
	}
	second, err := f.client.ListResourcePlans(ctx, as("writer", &forgev1.ListResourcePlansRequest{PageSize: 2, PageToken: first.Msg.GetNextPageToken()}))
	if err != nil || len(second.Msg.GetPlans()) != 1 || second.Msg.GetPlans()[0].GetName() != a.GetName() || second.Msg.GetNextPageToken() != "" {
		t.Errorf("second page: %v %v", second, err)
	}
	_, err = f.client.ListResourcePlans(ctx, as("writer", &forgev1.ListResourcePlansRequest{PageToken: "!!"}))
	wantCode(t, "a bad page token", err, connect.CodeInvalidArgument, "page_token")
	_, err = f.client.ListResourcePlans(ctx, as("writer", &forgev1.ListResourcePlansRequest{PageSize: -1}))
	wantCode(t, "a negative page size", err, connect.CodeInvalidArgument, "page_size")
	_, err = f.client.ListResourcePlans(ctx, as("writer", &forgev1.ListResourcePlansRequest{Env: "qa"}))
	wantCode(t, "a bad environment", err, connect.CodeInvalidArgument, "staging or production")

	// A day later a plan that was ready reads as expired, before any sweep ran.
	mu.Lock()
	now = now.Add(25 * time.Hour)
	mu.Unlock()
	if p := f.getPlan(t, a.GetName()); p.GetState() != forgev1.ResourcePlan_STATE_EXPIRED {
		t.Errorf("after a day: %v", p.GetState())
	}
	if p := f.getPlan(t, c.GetName()); p.GetState() != forgev1.ResourcePlan_STATE_DENIED {
		t.Errorf("a denied plan stays denied: %v", p.GetState())
	}
	if got := list("writer", &forgev1.ListResourcePlansRequest{States: []forgev1.ResourcePlan_State{forgev1.ResourcePlan_STATE_PLANNED}}); len(got) != 0 {
		t.Errorf("expired plans listed as planned: %v", got)
	}
}
```

Run: `go test ./internal/server -run 'TestPlan|TestRunTokens|TestCompleteOnce|TestListAndViewPlans'`
Expected: FAIL to build (`undefined: Resources`).

- [ ] **Step 4: Implement resources.go**

Create `internal/server/resources.go`:

```go
package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/resource"
	"github.com/showmino/forge/internal/run"
	"github.com/showmino/forge/internal/store"
	"github.com/showmino/forge/policy"
)

// Error codes a resource operation may end with, besides cancelled and internal.
const (
	OpCodePolicyDenied = "policy_denied"
	opCodeRunFailed    = "run_failed"
	opCodeRunTimeout   = "run_timeout"
	opCodeRunnerFailed = "runner_failed"
	opCodeLaunchFailed = "launch_failed"
)

// maxRunResult is the most bytes of plan file or plan JSON a runner may hand back.
const maxRunResult = 8 << 20

// ServiceNamespaces checks and makes the namespace <service>-<env>.
type ServiceNamespaces interface {
	// ServiceNamespaceFree fails when the namespace exists and is not Forge's.
	ServiceNamespaceFree(ctx context.Context, service, env string) error
	// EnsureServiceNamespace makes the namespace, with Forge's labels, when it is missing.
	EnsureServiceNamespace(ctx context.Context, service, env string) error
}

// DatabaseHealth reads a database's live health from the cluster.
type DatabaseHealth interface {
	DatabaseHealth(ctx context.Context, service, env, name string) (*forgev1.ResourceHealth, error)
}

// ResourceOptions configure resources.
type ResourceOptions struct {
	Store      store.ResourceStore
	Catalog    *Catalog
	Operations *Operations
	Policy     *policy.Engine
	Launcher   run.Launcher
	// Namespaces checks and makes service namespaces; nil skips both.
	Namespaces ServiceNamespaces
	// Health reads live health; nil reports "unknown".
	Health DatabaseHealth
	// RunTimeout is how long a run may take (default 15m).
	RunTimeout time.Duration
	// PlanTTL is how long a plan may wait to be applied (default 24h).
	PlanTTL time.Duration
	// Poll is how often a run's launcher is asked for its status (default 2s).
	Poll time.Duration
	// Grace is how long after a runner ended cleanly its result may still
	// arrive (default 10s).
	Grace time.Duration
	Now   func() time.Time
	Log   *slog.Logger
}

// Resources serves ResourceService and is the runner's API. A nil
// *Resources answers FAILED_PRECONDITION.
type Resources struct {
	o     ResourceOptions
	audit atomic.Pointer[auditor]

	mu sync.Mutex
	// running maps "<service>/<env>" to the operation running there.
	running map[string]string
	// requests maps a caller's PlanResources request_id (requestKey) to its plan.
	requests map[string]string
	// live maps a plan to the run an operation of this process is watching.
	live map[string]*liveRun
}

// liveRun connects a run to the operation that watches it.
type liveRun struct {
	emit  func(step, message string)
	ended chan struct{}
	once  sync.Once
}

// NewResources returns the resource service.
func NewResources(o ResourceOptions) (*Resources, error) {
	if o.Store == nil || o.Catalog == nil || o.Operations == nil || o.Policy == nil || o.Launcher == nil {
		return nil, errors.New("resources: Store, Catalog, Operations, Policy and Launcher are required")
	}
	for _, d := range []struct {
		v   *time.Duration
		def time.Duration
	}{{&o.RunTimeout, 15 * time.Minute}, {&o.PlanTTL, 24 * time.Hour}, {&o.Poll, 2 * time.Second}, {&o.Grace, 10 * time.Second}} {
		if *d.v == 0 {
			*d.v = d.def
		}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	return &Resources{o: o, running: map[string]string{}, requests: map[string]string{}, live: map[string]*liveRun{}}, nil
}

func (r *Resources) ready() error {
	if r == nil {
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("resources are not configured on this Forge API"))
	}
	return nil
}

// setAuditor makes ended runs append their outcome to the audit log.
func (r *Resources) setAuditor(a *auditor) { r.audit.Store(a) }

func precondition(format string, a ...any) error {
	return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(format, a...))
}

var planIDRE = regexp.MustCompile(`^[0-9a-f]{12}$`)

// parsePlanName accepts "resourcePlans/<id>" or "<id>".
func parsePlanName(name string) (string, error) {
	id := strings.TrimPrefix(name, planNamePrefix)
	if !planIDRE.MatchString(id) {
		return "", invalidArg("%q is not a plan name (resourcePlans/<id>)", name)
	}
	return planNamePrefix + id, nil
}

func newPlanID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// newRunToken returns a run token and the hash forge-api stores. The token
// goes to the launcher only: never log it.
func newRunToken() (token, hash string) {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	token = hex.EncodeToString(b)
	return token, hashToken(token)
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func subjectOf(ctx context.Context) string {
	if p, ok := PrincipalFrom(ctx); ok {
		return p.Subject
	}
	return ""
}

// modeOf is the run a plan in state s is in: plan, apply, or "" for none.
func modeOf(s forgev1.ResourcePlan_State) string {
	switch s {
	case forgev1.ResourcePlan_STATE_PLANNING:
		return run.ModePlan
	case forgev1.ResourcePlan_STATE_APPLYING:
		return run.ModeApply
	default:
		return ""
	}
}

// view is the plan as the API shows it: a ready plan past its expiry reads
// as expired, also before the sweep has marked it.
func (r *Resources) view(rec *store.PlanRecord) *forgev1.ResourcePlan {
	p := proto.Clone(rec.Plan).(*forgev1.ResourcePlan)
	if p.GetState() == forgev1.ResourcePlan_STATE_PLANNED && !r.o.Now().Before(p.GetExpireTime().AsTime()) {
		p.State = forgev1.ResourcePlan_STATE_EXPIRED
	}
	return p
}

// planAccess checks that the caller may plan service's resources: a writer,
// or the service's own CI without allow_destroy.
func planAccess(ctx context.Context, service string, allowDestroy bool) error {
	w := workloadOf(ctx)
	if w == nil {
		return require(ctx, PermServicesWrite)
	}
	if service != w.Service {
		return workloadDenied(w, "only plan the resources of %s%s, not of %s%s", servicePrefix, w.Service, servicePrefix, service)
	}
	if allowDestroy {
		return workloadDenied(w, "not plan with allow_destroy")
	}
	return nil
}

// PlanResources saves a plan record and starts the run that fills it.
func (r *Resources) PlanResources(ctx context.Context, req *connect.Request[forgev1.PlanResourcesRequest]) (*connect.Response[forgev1.PlanResourcesResponse], error) {
	if err := r.ready(); err != nil {
		return nil, err
	}
	m := req.Msg
	name, err := parseName(m.GetService())
	if err != nil {
		return nil, err
	}
	svc, env := strings.TrimPrefix(name, servicePrefix), m.GetEnv()
	if err := planAccess(ctx, svc, m.GetAllowDestroy()); err != nil {
		return nil, err
	}
	if err := validEnv(env); err != nil {
		return nil, err
	}
	if ok, err := r.o.Catalog.has(ctx, name); err != nil {
		return nil, err
	} else if !ok {
		return nil, notFound(name)
	}
	settings, err := resource.Resolve(m.GetResources(), env)
	if err != nil {
		return nil, invalidArg("resources: %v", err)
	}
	if r.o.Namespaces != nil {
		if err := r.o.Namespaces.ServiceNamespaceFree(ctx, svc, env); err != nil {
			return nil, err
		}
	}

	target := svc + "/" + env
	r.mu.Lock()
	defer r.mu.Unlock()
	if key := m.GetRequestId(); key != "" {
		if planName, ok := r.requests[requestKey(ctx, key)]; ok {
			if rec, err := r.o.Store.GetResourcePlan(ctx, planName); err == nil {
				op, _ := r.o.Operations.Get(rec.Plan.GetOperation())
				return connect.NewResponse(&forgev1.PlanResourcesResponse{Operation: op, Plan: r.view(rec)}), nil
			}
		}
	}
	if running, ok := r.running[target]; ok {
		return nil, precondition("%s is already running for %s%s", running, resourceNamePrefix, target)
	}
	now := r.o.Now()
	token, hash := newRunToken()
	rec := &store.PlanRecord{
		Plan: &forgev1.ResourcePlan{
			Name: planNamePrefix + newPlanID(), Service: name, Env: env, State: forgev1.ResourcePlan_STATE_PLANNING,
			Resources: settings, AllowDestroy: m.GetAllowDestroy(), Creator: actorOf(ctx),
			CreateTime: timestamppb.New(now), ExpireTime: timestamppb.New(now.Add(r.o.PlanTTL)),
		},
		CreatorSubject: subjectOf(ctx), Digest: resource.Digest(settings),
		TokenHash: hash, Deadline: now.Add(r.o.RunTimeout),
	}
	noteAuditDetail(ctx, "plan", rec.Plan.GetName())
	op, err := r.startRun(ctx, rec, run.ModePlan, token, nil)
	if err != nil {
		return nil, err
	}
	if key := m.GetRequestId(); key != "" {
		r.requests[requestKey(ctx, key)] = rec.Plan.GetName()
	}
	return connect.NewResponse(&forgev1.PlanResourcesResponse{Operation: op, Plan: r.view(rec)}), nil
}

// GetResourcePlan returns one plan: to every member, and to a CI workload
// when the plan is its own service's.
func (r *Resources) GetResourcePlan(ctx context.Context, req *connect.Request[forgev1.GetResourcePlanRequest]) (*connect.Response[forgev1.GetResourcePlanResponse], error) {
	if err := r.ready(); err != nil {
		return nil, err
	}
	w := workloadOf(ctx)
	if w == nil {
		if err := require(ctx, PermServicesRead); err != nil {
			return nil, err
		}
	}
	name, err := parsePlanName(req.Msg.GetName())
	if err != nil {
		return nil, err
	}
	rec, err := r.o.Store.GetResourcePlan(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return nil, notFound(name)
	}
	if err != nil {
		return nil, storeErr(err)
	}
	if w != nil && rec.Plan.GetService() != servicePrefix+w.Service {
		return nil, workloadDenied(w, "only read the plans of %s%s", servicePrefix, w.Service)
	}
	return connect.NewResponse(&forgev1.GetResourcePlanResponse{Plan: r.view(rec)}), nil
}

// ListResourcePlans returns plans, newest first.
func (r *Resources) ListResourcePlans(ctx context.Context, req *connect.Request[forgev1.ListResourcePlansRequest]) (*connect.Response[forgev1.ListResourcePlansResponse], error) {
	if err := r.ready(); err != nil {
		return nil, err
	}
	if err := require(ctx, PermServicesRead); err != nil {
		return nil, err
	}
	m := req.Msg
	size := int(m.GetPageSize())
	switch {
	case size < 0:
		return nil, invalid("page_size must not be negative")
	case size == 0:
		size = defaultPageSize
	case size > maxPageSize:
		size = maxPageSize
	}
	q := store.PlanQuery{Env: m.GetEnv(), Limit: size + 1}
	if m.GetEnv() != "" {
		if err := validEnv(m.GetEnv()); err != nil {
			return nil, err
		}
	}
	if m.GetService() != "" {
		name, err := parseName(m.GetService())
		if err != nil {
			return nil, err
		}
		q.Service = name
	}
	if m.GetMine() {
		q.CreatorSubject = subjectOf(ctx)
	}
	if t := m.GetPageToken(); t != "" {
		b, err := base64.RawURLEncoding.DecodeString(t)
		if err == nil {
			q.BeforeSeq, err = strconv.ParseInt(string(b), 10, 64)
		}
		if err != nil || q.BeforeSeq < 1 {
			return nil, invalid("page_token is malformed")
		}
	}
	// A ready plan past its expiry reads as expired, so the state filter is
	// applied to what the caller will see: stored "planned" plans are fetched
	// too when expired ones are wanted, and every plan is filtered as viewed.
	want := m.GetStates()
	q.States = slices.Clone(want)
	if slices.Contains(want, forgev1.ResourcePlan_STATE_EXPIRED) && !slices.Contains(want, forgev1.ResourcePlan_STATE_PLANNED) {
		q.States = append(q.States, forgev1.ResourcePlan_STATE_PLANNED)
	}
	resp := &forgev1.ListResourcePlansResponse{}
	var seqs []int64
	for len(resp.Plans) <= size {
		recs, err := r.o.Store.ListResourcePlans(ctx, q)
		if err != nil {
			return nil, storeErr(err)
		}
		for _, rec := range recs {
			if p := r.view(rec); len(want) == 0 || slices.Contains(want, p.GetState()) {
				resp.Plans = append(resp.Plans, p)
				seqs = append(seqs, rec.Seq)
			}
		}
		if len(recs) < q.Limit {
			break // the store has no more
		}
		q.BeforeSeq = recs[len(recs)-1].Seq
	}
	if len(resp.Plans) > size {
		resp.Plans = resp.Plans[:size]
		resp.NextPageToken = base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(seqs[size-1], 10)))
	}
	return connect.NewResponse(resp), nil
}
```

- [ ] **Step 5: Implement resource_run.go**

Create `internal/server/resource_run.go`:

```go
package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/resource"
	"github.com/showmino/forge/internal/run"
	"github.com/showmino/forge/internal/store"
	"github.com/showmino/forge/policy"
)

// runEnd is how a run ended.
type runEnd struct {
	// code is "" when the run worked, else the operation error code.
	code    string
	message string
	res     run.Result
}

// planTarget returns the service id and "<service>/<env>" of a plan.
func planTarget(p *forgev1.ResourcePlan) (svc, target string) {
	svc = strings.TrimPrefix(p.GetService(), servicePrefix)
	return svc, svc + "/" + p.GetEnv()
}

// outcomeTemplate is the outcome event of the run a call starts, without its
// result: the call's own event so far, or forge-api's when there is none.
func outcomeTemplate(ctx context.Context, action, op, service, target string) *forgev1.AuditEvent {
	ev := &forgev1.AuditEvent{Action: action, Actor: &forgev1.AuditActor{Kind: actorSystem}}
	if rec := auditFrom(ctx); rec != nil {
		ev = rec.snapshot()
	}
	ev.Kind, ev.Operation, ev.Service, ev.Target = auditOutcome, op, service, target
	ev.Details, ev.Client, ev.Result, ev.Name, ev.Time = nil, nil, nil, "", nil
	return ev
}

// startRun starts the operation that runs rec in mode, and stores rec with
// the operation's name and the outcome template. write, when set, runs in
// the same transaction first; its error aborts the run. r.mu must be held.
func (r *Resources) startRun(ctx context.Context, rec *store.PlanRecord, mode, token string, write func(tx store.ResourceTx) error) (*forgev1.Operation, error) {
	kind := opResourcePlan
	if mode == run.ModeApply {
		kind = opResourceApply
	}
	planName := rec.Plan.GetName()
	svc, target := planTarget(rec.Plan)
	spec := run.Spec{Plan: planName, Mode: mode, Service: svc, Env: rec.Plan.GetEnv(), Token: token, Deadline: rec.Deadline}
	lr := &liveRun{ended: make(chan struct{})}
	// The work waits at the gate until the record is stored.
	gate := make(chan error, 1)
	op := r.o.Operations.Start(withoutOutcome(ctx), kind, resourceNamePrefix+target, actorOf(ctx), func(ctx context.Context, emit func(string, string)) error {
		if err := <-gate; err != nil {
			return err // nothing was claimed: the target is not this run's to free
		}
		defer r.unlock(target, planName)
		r.mu.Lock()
		lr.emit = emit
		r.live[planName] = lr
		r.mu.Unlock()
		return r.watchRun(ctx, spec, lr, emit)
	})
	rec.Plan.Operation = op.GetName()
	tmpl, err := protojson.Marshal(outcomeTemplate(ctx, kind, op.GetName(), rec.Plan.GetService(), resourceNamePrefix+target))
	if err == nil {
		rec.Audit = tmpl
		err = r.o.Store.WriteResources(ctx, func(tx store.ResourceTx) error {
			if write != nil {
				if err := write(tx); err != nil {
					return err
				}
			}
			return tx.PutResourcePlan(ctx, rec)
		})
	}
	if err != nil {
		gate <- &OpError{Code: OpCodeInternal, Message: "the run could not be recorded"}
		return nil, storeErr(err)
	}
	r.running[target] = op.GetName()
	gate <- nil
	return op, nil
}

func (r *Resources) unlock(target, planName string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.running, target)
	delete(r.live, planName)
}

// liveEmit adds an event to the operation watching plan's run, if there is one.
func (r *Resources) liveEmit(planName, step, message string) {
	r.mu.Lock()
	lr := r.live[planName]
	r.mu.Unlock()
	if lr != nil && lr.emit != nil {
		lr.emit(step, message)
	}
}

// signal tells the operation watching plan's run that the run ended.
func (r *Resources) signal(planName string) {
	r.mu.Lock()
	lr := r.live[planName]
	r.mu.Unlock()
	if lr != nil {
		lr.once.Do(func() { close(lr.ended) })
	}
}

// watchRun starts the runner and waits until the run ended: by the runner's
// Complete, by the runner dying, by the deadline, or by a cancel.
func (r *Resources) watchRun(ctx context.Context, spec run.Spec, lr *liveRun, emit func(string, string)) error {
	if spec.Mode == run.ModeApply && r.o.Namespaces != nil {
		if err := r.o.Namespaces.EnsureServiceNamespace(ctx, spec.Service, spec.Env); err != nil {
			return r.failRun(ctx, spec, opCodeLaunchFailed, "namespace "+spec.Service+"-"+spec.Env+": "+clip(err.Error()))
		}
		emit("namespace", spec.Service+"-"+spec.Env)
	}
	if err := r.o.Launcher.Start(ctx, spec); err != nil {
		return r.failRun(ctx, spec, opCodeLaunchFailed, "the runner could not be started: "+clip(err.Error()))
	}
	emit("launch", "the "+spec.Mode+" runner started")
	deadline := time.NewTimer(r.o.RunTimeout)
	defer deadline.Stop()
	tick := time.NewTicker(r.o.Poll)
	defer tick.Stop()
	stop := func() { _ = r.o.Launcher.Stop(context.WithoutCancel(ctx), spec.Plan, spec.Mode) }
	var exited time.Time // when the runner was first seen to have ended cleanly
	for {
		select {
		case <-lr.ended:
			return r.runError(ctx, spec.Plan, spec.Mode)
		case <-ctx.Done():
			stop()
			return r.failRun(ctx, spec, OpCodeCancelled, "the run was cancelled")
		case <-deadline.C:
			stop()
			return r.failRun(ctx, spec, opCodeRunTimeout, fmt.Sprintf("the run did not finish within %s", r.o.RunTimeout))
		case <-tick.C:
			st, err := r.o.Launcher.Status(ctx, spec.Plan, spec.Mode)
			if err != nil || !st.Done {
				continue
			}
			if st.Failed {
				return r.failRun(ctx, spec, opCodeRunnerFailed, "the runner failed: "+clip(st.Log))
			}
			// It ended cleanly: its result is on its way, or it never sent one.
			if exited.IsZero() {
				exited = time.Now()
			} else if time.Since(exited) >= r.o.Grace {
				return r.failRun(ctx, spec, opCodeRunnerFailed, "the runner ended without a result")
			}
		}
	}
}

// failRun ends the run as failed and returns the operation's error. When
// the run had already ended another way, that ending decides.
func (r *Resources) failRun(ctx context.Context, spec run.Spec, code, message string) error {
	message = auditText(message, auditMessageMax)
	won, err := r.endRun(ctx, spec.Plan, spec.Mode, runEnd{code: code, message: message})
	if err != nil {
		return &OpError{Code: OpCodeInternal, Message: "the run's end could not be recorded: " + clip(err.Error())}
	}
	if won {
		return &OpError{Code: code, Message: message}
	}
	return r.runError(ctx, spec.Plan, spec.Mode)
}

// runError is the operation error of a run that ended, read from the plan.
func (r *Resources) runError(ctx context.Context, planName, mode string) error {
	rec, err := r.o.Store.GetResourcePlan(context.WithoutCancel(ctx), planName)
	if err != nil {
		return &OpError{Code: OpCodeInternal, Message: "the plan could not be read: " + clip(err.Error())}
	}
	p := rec.Plan
	switch p.GetState() {
	case forgev1.ResourcePlan_STATE_PLANNED:
		if mode == run.ModePlan {
			return nil
		}
	case forgev1.ResourcePlan_STATE_APPLIED:
		return nil
	case forgev1.ResourcePlan_STATE_DENIED:
		return &OpError{Code: OpCodePolicyDenied, Message: deniedMessage(p.GetPolicy())}
	case forgev1.ResourcePlan_STATE_FAILED, forgev1.ResourcePlan_STATE_APPLY_FAILED:
		return &OpError{Code: opCodeRunFailed, Message: p.GetMessage()}
	}
	return &OpError{Code: OpCodeInternal, Message: fmt.Sprintf("%s is %s after its %s run", planName, planStateWord(p.GetState()), mode)}
}

// planStateWord is a state as people read it: "apply_failed".
func planStateWord(s forgev1.ResourcePlan_State) string {
	return strings.ToLower(strings.TrimPrefix(s.String(), "STATE_"))
}

// deniedMessage is the first refusal, and how many more there are.
func deniedMessage(p *forgev1.PolicyResult) string {
	deny := p.GetDeny()
	switch len(deny) {
	case 0:
		return "denied by policy"
	case 1:
		return deny[0]
	default:
		return fmt.Sprintf("%s (and %d more)", deny[0], len(deny)-1)
	}
}

// templateActor is the actor of a stored outcome template: who started the run.
func templateActor(template []byte) policy.Actor {
	ev := &forgev1.AuditEvent{}
	if protojson.Unmarshal(template, ev) != nil {
		return policy.Actor{}
	}
	return policy.Actor{Kind: ev.GetActor().GetKind(), Subject: ev.GetActor().GetSubject()}
}

// evaluate checks planJSON against policy for the plan in rec, asked for by actor.
func (r *Resources) evaluate(ctx context.Context, rec *store.PlanRecord, planJSON []byte, actor policy.Actor) (*forgev1.PolicyResult, error) {
	svc, _ := planTarget(rec.Plan)
	f := policy.Forge{Service: svc, Environment: rec.Plan.GetEnv(), AllowDestroy: rec.Plan.GetAllowDestroy(), Actor: actor}
	for _, s := range rec.Plan.GetResources() {
		f.Resources = append(f.Resources, policy.Resource{Name: s.GetName(), Type: s.GetType(), StorageGi: s.GetStorageGi(), Instances: s.GetInstances()})
	}
	res, err := r.o.Policy.Evaluate(ctx, planJSON, f)
	if err != nil {
		return nil, err
	}
	return &forgev1.PolicyResult{Denied: res.Denied(), Deny: res.Deny, Warn: res.Warn, Rules: r.o.Policy.Fingerprint()}, nil
}

// planCheck is what a plan run's result turned out to be.
type planCheck struct {
	changes      []*forgev1.ResourceChange
	policy       *forgev1.PolicyResult
	stateVersion int64
}

// checkPlan reads and checks a plan run's result. A result that cannot be
// read or checked turns end into a failure: such a plan is never ready.
func (r *Resources) checkPlan(ctx context.Context, rec *store.PlanRecord, end *runEnd) planCheck {
	fail := func(format string, a ...any) planCheck {
		end.code, end.message = opCodeRunFailed, auditText(fmt.Sprintf(format, a...), auditMessageMax)
		return planCheck{}
	}
	res := end.res
	switch {
	case len(res.PlanFile) == 0 || len(res.PlanJSON) == 0:
		return fail("the runner returned no plan")
	case len(res.PlanFile) > maxRunResult || len(res.PlanJSON) > maxRunResult:
		return fail("the plan is larger than %d MiB", maxRunResult>>20)
	}
	changes, err := resource.Changes(res.PlanJSON)
	if err != nil {
		return fail("the plan could not be read: %v", err)
	}
	pol, err := r.evaluate(ctx, rec, res.PlanJSON, templateActor(rec.Audit))
	if err != nil {
		return fail("policy could not be evaluated: %v", err)
	}
	_, target := planTarget(rec.Plan)
	st, err := r.o.Store.GetResourceState(ctx, target)
	if err != nil {
		return fail("the state could not be read: %v", err)
	}
	return planCheck{changes: changes, policy: pol, stateVersion: st.Version}
}

// endRun moves a plan out of its running state for mode. It works once: the
// first caller changes the plan (and, after an apply, the resources), appends
// the outcome event and wakes the operation watching the run. It reports
// whether this call was that first one.
func (r *Resources) endRun(ctx context.Context, planName, mode string, end runEnd) (bool, error) {
	ctx = context.WithoutCancel(ctx)
	rec, err := r.o.Store.GetResourcePlan(ctx, planName)
	if err != nil {
		return false, err
	}
	if modeOf(rec.Plan.GetState()) != mode {
		return false, nil
	}
	var check planCheck
	if mode == run.ModePlan && end.code == "" {
		check = r.checkPlan(ctx, rec, &end)
	}
	now := timestamppb.New(r.o.Now())
	var final *store.PlanRecord
	err = r.o.Store.WriteResources(ctx, func(tx store.ResourceTx) error {
		cur, err := tx.GetResourcePlan(ctx, planName)
		if err != nil {
			return err
		}
		if modeOf(cur.Plan.GetState()) != mode || cur.TokenHash != rec.TokenHash {
			return nil // it ended another way first
		}
		p := cur.Plan
		cur.TokenHash, cur.Deadline, cur.Audit = "", time.Time{}, nil
		cur.PlanFile, cur.PlanJSON = nil, nil
		switch {
		case mode == run.ModePlan && end.code != "":
			p.State, p.Message = forgev1.ResourcePlan_STATE_FAILED, end.message
		case mode == run.ModePlan && check.policy.GetDenied():
			p.State, p.Changes, p.Policy = forgev1.ResourcePlan_STATE_DENIED, check.changes, check.policy
		case mode == run.ModePlan:
			p.State, p.Changes, p.Policy = forgev1.ResourcePlan_STATE_PLANNED, check.changes, check.policy
			cur.PlanFile, cur.PlanJSON, cur.StateVersion = end.res.PlanFile, end.res.PlanJSON, check.stateVersion
		case end.code != "":
			p.State, p.Message = forgev1.ResourcePlan_STATE_APPLY_FAILED, end.message
			if err := failResources(ctx, tx, p, now); err != nil {
				return err
			}
		default:
			p.State, p.ApplyTime = forgev1.ResourcePlan_STATE_APPLIED, now
			if err := applyResources(ctx, tx, p, end.res.Secrets, now); err != nil {
				return err
			}
		}
		final = cur
		return tx.PutResourcePlan(ctx, cur)
	})
	if err != nil || final == nil {
		return false, err
	}
	r.appendOutcome(ctx, final.Plan, rec.Audit, mode, end)
	switch p := final.Plan; p.GetState() {
	case forgev1.ResourcePlan_STATE_PLANNED:
		r.liveEmit(planName, "policy", "passed; "+resource.Count(p.GetChanges()))
	case forgev1.ResourcePlan_STATE_DENIED:
		r.liveEmit(planName, "policy", "denied: "+deniedMessage(p.GetPolicy()))
	case forgev1.ResourcePlan_STATE_APPLIED:
		r.liveEmit(planName, "apply", appliedHint(p.GetChanges()))
	}
	r.signal(planName)
	return true, nil
}

// appliedHint tells the caller what an apply means for the app.
func appliedHint(changes []*forgev1.ResourceChange) string {
	for _, c := range changes {
		if c.GetAction() != "delete" {
			return "Deploy to give the app DATABASE_URL."
		}
	}
	return "The next deploy drops DATABASE_URL."
}

// resourceName is "resources/<service>/<env>/<name>".
func resourceName(p *forgev1.ResourcePlan, name string) string {
	_, target := planTarget(p)
	return resourceNamePrefix + target + "/" + name
}

// applyResources records what an applied plan left behind.
func applyResources(ctx context.Context, tx store.ResourceTx, p *forgev1.ResourcePlan, secrets map[string]string, now *timestamppb.Timestamp) error {
	for _, c := range p.GetChanges() {
		name := resourceName(p, c.GetResource())
		if c.GetAction() == "delete" {
			if err := tx.DeleteResource(ctx, name); err != nil {
				return err
			}
			continue
		}
		if c.GetAfter() == nil {
			continue
		}
		res := &forgev1.Resource{Name: name, Service: p.GetService(), Env: p.GetEnv(), CreateTime: now}
		if old, err := tx.GetResource(ctx, name); err == nil {
			res.CreateTime = old.GetCreateTime()
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		res.Type, res.Settings, res.State = c.GetAfter().GetType(), c.GetAfter(), forgev1.Resource_STATE_READY
		res.Secret, res.LastPlan, res.UpdateTime = secrets[c.GetResource()], p.GetName(), now
		if err := tx.PutResource(ctx, res); err != nil {
			return err
		}
	}
	return nil
}

// failResources marks the resources a failed apply touched.
func failResources(ctx context.Context, tx store.ResourceTx, p *forgev1.ResourcePlan, now *timestamppb.Timestamp) error {
	for _, c := range p.GetChanges() {
		res, err := tx.GetResource(ctx, resourceName(p, c.GetResource()))
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		res.State, res.LastPlan, res.UpdateTime = forgev1.Resource_STATE_FAILED, p.GetName(), now
		if err := tx.PutResource(ctx, res); err != nil {
			return err
		}
	}
	return nil
}

// appendOutcome appends the run's outcome event, built from the template
// stored when the run started.
func (r *Resources) appendOutcome(ctx context.Context, p *forgev1.ResourcePlan, template []byte, mode string, end runEnd) {
	ev := &forgev1.AuditEvent{}
	if err := protojson.Unmarshal(template, ev); err != nil || ev.GetAction() == "" {
		action := opResourcePlan
		if mode == run.ModeApply {
			action = opResourceApply
		}
		_, target := planTarget(p)
		ev = &forgev1.AuditEvent{Action: action, Actor: &forgev1.AuditActor{Kind: actorSystem},
			Service: p.GetService(), Target: resourceNamePrefix + target, Operation: p.GetOperation()}
	}
	ev.Kind = auditOutcome
	verdict, changes := "", ""
	if pol := p.GetPolicy(); pol != nil {
		verdict, changes = "passed", resource.Count(p.GetChanges())
		if pol.GetDenied() {
			verdict = "denied"
		}
	}
	ev.Details = details("environment", p.GetEnv(), "plan", p.GetName(), "policy", verdict,
		"policy_rules", p.GetPolicy().GetRules(), "allow_destroy", flag(p.GetAllowDestroy()), "changes", changes)
	switch {
	case p.GetState() == forgev1.ResourcePlan_STATE_DENIED:
		ev.Result = &forgev1.AuditResult{Code: "failed", Message: auditText(OpCodePolicyDenied+": "+deniedMessage(p.GetPolicy()), auditMessageMax)}
	case end.code == "":
		ev.Result = &forgev1.AuditResult{Code: "ok"}
	case end.code == OpCodeCancelled:
		ev.Result = &forgev1.AuditResult{Code: "cancelled", Message: auditText(end.message, auditMessageMax)}
	default:
		ev.Result = &forgev1.AuditResult{Code: "failed", Message: auditText(end.code+": "+end.message, auditMessageMax)}
	}
	r.audit.Load().append(ctx, ev)
}

// The runner's API. Every call carries the run's token.

var errRunToken = connect.NewError(connect.CodeUnauthenticated, errors.New("the run token is not valid"))

// authRun returns the plan and mode of the run token belongs to. A token
// works only while its run is going and before the run's deadline.
func (r *Resources) authRun(ctx context.Context, token string) (*store.PlanRecord, string, error) {
	if token == "" {
		return nil, "", errRunToken
	}
	rec, err := r.o.Store.ResourcePlanByToken(ctx, hashToken(token))
	if errors.Is(err, store.ErrNotFound) {
		return nil, "", errRunToken
	}
	if err != nil {
		return nil, "", storeErr(err)
	}
	mode := modeOf(rec.Plan.GetState())
	if mode == "" || !r.o.Now().Before(rec.Deadline) {
		return nil, "", errRunToken
	}
	return rec, mode, nil
}

// Claim implements run.API.
func (r *Resources) Claim(ctx context.Context, token string) (*run.Work, error) {
	rec, mode, err := r.authRun(ctx, token)
	if err != nil {
		return nil, err
	}
	svc, target := planTarget(rec.Plan)
	// One run at a time per target: a lock still held is a dead run's.
	if err := r.o.Store.UnlockResourceState(ctx, target, ""); err != nil {
		return nil, storeErr(err)
	}
	w := &run.Work{Plan: rec.Plan.GetName(), Mode: mode, Variables: run.Variables{
		Service: svc, Environment: rec.Plan.GetEnv(), Namespace: svc + "-" + rec.Plan.GetEnv(), Resources: []run.Resource{},
	}}
	for _, s := range rec.Plan.GetResources() {
		w.Variables.Resources = append(w.Variables.Resources, run.Resource{Name: s.GetName(), Type: s.GetType(), StorageGi: s.GetStorageGi(), Instances: s.GetInstances()})
	}
	if mode == run.ModeApply {
		w.PlanFile = rec.PlanFile
	}
	return w, nil
}

// Event implements run.API.
func (r *Resources) Event(ctx context.Context, token, step, message string) error {
	rec, _, err := r.authRun(ctx, token)
	if err != nil {
		return err
	}
	r.liveEmit(rec.Plan.GetName(), auditText(step, 32), auditText(message, auditMessageMax))
	return nil
}

// Complete implements run.API.
func (r *Resources) Complete(ctx context.Context, token string, res run.Result) error {
	rec, mode, err := r.authRun(ctx, token)
	if err != nil {
		return err
	}
	end := runEnd{res: res}
	if res.Error != "" {
		end.code, end.message = opCodeRunFailed, auditText(res.Error, auditMessageMax)
	}
	won, err := r.endRun(ctx, rec.Plan.GetName(), mode, end)
	if err != nil {
		return storeErr(err)
	}
	if !won {
		return errRunToken
	}
	return nil
}

// ReadState implements run.API.
func (r *Resources) ReadState(ctx context.Context, token string) ([]byte, error) {
	rec, _, err := r.authRun(ctx, token)
	if err != nil {
		return nil, err
	}
	_, target := planTarget(rec.Plan)
	st, err := r.o.Store.GetResourceState(ctx, target)
	return st.Data, storeErr(err)
}

// WriteState implements run.API.
func (r *Resources) WriteState(ctx context.Context, token, lockID string, data []byte) error {
	rec, mode, err := r.authRun(ctx, token)
	if err != nil {
		return err
	}
	if mode != run.ModeApply {
		return precondition("a plan run may not write the state")
	}
	_, target := planTarget(rec.Plan)
	_, err = r.o.Store.PutResourceState(ctx, target, lockID, data)
	if errors.Is(err, store.ErrStateLocked) {
		return run.ErrLocked
	}
	return storeErr(err)
}

// LockState implements run.API.
func (r *Resources) LockState(ctx context.Context, token, lockID string, info []byte) ([]byte, error) {
	rec, _, err := r.authRun(ctx, token)
	if err != nil {
		return nil, err
	}
	_, target := planTarget(rec.Plan)
	holder, err := r.o.Store.LockResourceState(ctx, target, lockID, info)
	if errors.Is(err, store.ErrStateLocked) {
		return holder, run.ErrLocked
	}
	return nil, storeErr(err)
}

// UnlockState implements run.API.
func (r *Resources) UnlockState(ctx context.Context, token, lockID string) error {
	rec, _, err := r.authRun(ctx, token)
	if err != nil {
		return err
	}
	_, target := planTarget(rec.Plan)
	err = r.o.Store.UnlockResourceState(ctx, target, lockID)
	if errors.Is(err, store.ErrStateLocked) {
		return run.ErrLocked
	}
	return storeErr(err)
}
```

- [ ] **Step 6: Wire the service, and let CI plan**

In `internal/server/server.go`:

Add to `Options`, after `Deploys`:

```go
	// Resources serves ResourceService; nil answers FAILED_PRECONDITION. It
	// must share Catalog and Operations with these Options.
	Resources *Resources
```

In `New`, after `o.Operations.setAuditor(aud)`:

```go
	if o.Resources != nil {
		o.Resources.setAuditor(aud)
	}
```

and after the `NewAuditServiceHandler` line:

```go
	mux.Handle(forgev1connect.NewResourceServiceHandler(o.Resources, opts))
```

`*Resources` must implement all six handler methods for this to compile. Three arrive in Task 7; add them now as stubs at the end of `internal/server/resources.go`, to be replaced there:

```go
// ApplyResourcePlan, ListResources and GetResource are implemented in resource_apply.go.

func (r *Resources) ApplyResourcePlan(ctx context.Context, _ *connect.Request[forgev1.ApplyResourcePlanRequest]) (*connect.Response[forgev1.ApplyResourcePlanResponse], error) {
	if err := r.ready(); err != nil {
		return nil, err
	}
	if err := require(ctx, PermServicesWrite); err != nil {
		return nil, err
	}
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("apply is not built yet"))
}

func (r *Resources) ListResources(ctx context.Context, _ *connect.Request[forgev1.ListResourcesRequest]) (*connect.Response[forgev1.ListResourcesResponse], error) {
	if err := r.ready(); err != nil {
		return nil, err
	}
	if err := require(ctx, PermServicesRead); err != nil {
		return nil, err
	}
	return connect.NewResponse(&forgev1.ListResourcesResponse{}), nil
}

func (r *Resources) GetResource(ctx context.Context, req *connect.Request[forgev1.GetResourceRequest]) (*connect.Response[forgev1.GetResourceResponse], error) {
	if err := r.ready(); err != nil {
		return nil, err
	}
	if err := require(ctx, PermServicesRead); err != nil {
		return nil, err
	}
	return nil, notFound(req.Msg.GetName())
}
```

In `internal/server/authn.go`, change `workloadProcedures` and its comment to:

```go
// workloadProcedures are the only RPCs a CI caller may make; their handlers
// narrow each one to the caller's own service, staging deploys, resource
// plans and operations.
var workloadProcedures = []string{
	forgev1connect.DeployServiceStartDeployProcedure,
	forgev1connect.DeployServiceGetDeploymentProcedure,
	forgev1connect.OperationServiceGetOperationProcedure,
	forgev1connect.OperationServiceWatchOperationProcedure,
	forgev1connect.ResourceServicePlanResourcesProcedure,
	forgev1connect.ResourceServiceGetResourcePlanProcedure,
}
```

and the refusal in `authenticate` to:

```go
				fmt.Errorf("CI token %s may only start and follow its own staging deploy and plan its own resources, not call %s", p.Subject, procedure))
```

Then run `grep -rn "may only start and follow" internal` and update any test that matches the old sentence in full; tests that match only `CI token` or `may only start and follow its own staging deploy` keep passing.

- [ ] **Step 7: Run the tests**

Run: `go test -race ./internal/server`
Expected: PASS, including every test of Step 3 and `TestAuditCoversEveryProcedure`.

If `TestPlanFailures/the_run_takes_too_long` is flaky under `-race`, raise `RunTimeout` there (and the expected text) to `80ms`; do not raise `Poll`.

- [ ] **Step 8: Lint and commit**

Run: `make lint`
Expected: clean.

```bash
git add internal/server
git commit -m "server: plan resources through a runner, with policy, one outcome per run, and CI plans

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 7: Applying a plan, resource records, and the delete guard

**Files:**
- Create: `internal/server/resource_apply.go`
- Modify: `internal/server/resources.go` (remove the three stubs of Task 6)
- Modify: `internal/server/audit.go` (one helper)
- Modify: `internal/server/catalog.go` (the `Catalog` struct and `DeleteService`)
- Modify: `internal/server/server.go` (hand the resource store to the catalog)
- Modify: `internal/server/resources_test.go` (new tests)

**Interfaces:**
- Consumes (Task 6): `(*Resources).ready`, `view`, `startRun(ctx, rec, mode, token, write)`, `evaluate(ctx, rec, planJSON, actor)`, `precondition`, `parsePlanName`, `newRunToken`, `planTarget`, `resourceName`, `noteAuditDetail`; existing `noteAuditService`, `actorUser`.
- Produces:
  - `(*Resources).ApplyResourcePlan`, `ListResources`, `GetResource`: with Task 6, `*Resources` is a full `forgev1connect.ResourceServiceHandler`.
  - `Catalog.DeleteService` answers `failed_precondition` while the service has resources.

- [ ] **Step 1: Write the failing tests**

Append to `internal/server/resources_test.go` (add `"errors"` and `"testing/fstest"` to its imports):

```go
// fakeCluster records namespace calls and reports a fixed health.
type fakeCluster struct {
	mu      sync.Mutex
	ensured []string
	health  *forgev1.ResourceHealth
	err     error
}

func (c *fakeCluster) ServiceNamespaceFree(context.Context, string, string) error { return nil }

func (c *fakeCluster) EnsureServiceNamespace(_ context.Context, service, env string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ensured = append(c.ensured, service+"-"+env)
	return nil
}

func (c *fakeCluster) DatabaseHealth(context.Context, string, string, string) (*forgev1.ResourceHealth, error) {
	return c.health, c.err
}

// startApply asks for an apply as a writer.
func (f *resourceFixture) startApply(req *forgev1.ApplyResourcePlanRequest) (*forgev1.ApplyResourcePlanResponse, error) {
	resp, err := f.client.ApplyResourcePlan(context.Background(), as("writer", req))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

// apply applies plan and waits for its operation.
func (f *resourceFixture) apply(t *testing.T, plan string) *forgev1.Operation {
	t.Helper()
	resp, err := f.startApply(&forgev1.ApplyResourcePlanRequest{Name: plan})
	if err != nil {
		t.Fatalf("apply %s: %v", plan, err)
	}
	return waitCIOp(t, f.ops, resp.GetOperation().GetName())
}

func (f *resourceFixture) getResource(t *testing.T, name string) *forgev1.Resource {
	t.Helper()
	got, err := f.client.GetResource(context.Background(), as("reader", &forgev1.GetResourceRequest{Name: name}))
	if err != nil {
		t.Fatal(err)
	}
	return got.Msg.GetResource()
}

func TestApply(t *testing.T) {
	t.Parallel()
	cluster := &fakeCluster{health: &forgev1.ResourceHealth{Phase: "Cluster in healthy state", ReadyInstances: 1, Instances: 1}}
	f := newResourceFixture(t, func(o *ResourceOptions) { o.Namespaces, o.Health = cluster, cluster })
	ctx := context.Background()
	p, _ := f.plan(t, "writer", "staging", database("5Gi", 0), false)
	if len(cluster.ensured) != 0 {
		t.Errorf("a plan made a namespace: %v", cluster.ensured)
	}
	op := f.apply(t, p.GetName())
	if op.GetError() != nil || op.GetKind() != "resource.apply" || op.GetTarget() != "resources/payments/staging" {
		t.Fatalf("operation: %v", op)
	}
	if steps := eventSteps(op); !strings.HasPrefix(steps, "namespace,launch") || !strings.HasSuffix(steps, "apply") {
		t.Errorf("events: %s", steps)
	}
	if last := op.GetEvents()[len(op.GetEvents())-1]; last.GetMessage() != "Deploy to give the app DATABASE_URL." {
		t.Errorf("last event: %q", last.GetMessage())
	}
	if !slices.Equal(cluster.ensured, []string{"payments-staging"}) {
		t.Errorf("namespaces: %v", cluster.ensured)
	}
	if got := f.runner.Databases(); !slices.Equal(got, []string{"payments-staging/main 5Gi x1"}) {
		t.Errorf("databases: %v", got)
	}

	applied := f.getPlan(t, p.GetName())
	if applied.GetState() != forgev1.ResourcePlan_STATE_APPLIED || applied.GetApplier() != "ana@example.com" || applied.GetApplyTime() == nil || applied.GetOperation() != op.GetName() {
		t.Errorf("plan: %v", applied)
	}
	rec, _ := f.store.GetResourcePlan(ctx, p.GetName())
	if len(rec.PlanFile) != 0 || len(rec.PlanJSON) != 0 || rec.TokenHash != "" {
		t.Errorf("an applied plan keeps no plan file and no token: %+v", rec)
	}
	if st, _ := f.store.GetResourceState(ctx, "payments/staging"); st.Version != 1 || st.LockID != "" {
		t.Errorf("state: %+v", st)
	}

	res := f.getResource(t, "resources/payments/staging/main")
	if res.GetState() != forgev1.Resource_STATE_READY || res.GetSecret() != "main-app" || res.GetType() != "postgres" ||
		res.GetService() != "services/payments" || res.GetEnv() != "staging" || res.GetLastPlan() != p.GetName() ||
		res.GetSettings().GetStorageGi() != 5 || res.GetSettings().GetInstances() != 1 || res.GetCreateTime() == nil {
		t.Errorf("resource: %v", res)
	}
	if h := res.GetHealth(); h.GetPhase() != "Cluster in healthy state" || h.GetReadyInstances() != 1 {
		t.Errorf("health: %v", h)
	}

	// The audit log: the apply request names the service, and one outcome follows.
	var req *forgev1.AuditEvent
	for _, e := range allEvents(t, f.store) {
		if e.GetKind() == "request" && e.GetAction() == "resource.apply" {
			req = e
		}
	}
	if req == nil || req.GetService() != "services/payments" || req.GetTarget() != "resources/payments/staging" || req.GetOperation() != op.GetName() ||
		req.GetActor().GetEmail() != "ana@example.com" || req.GetResult().GetCode() != "ok" {
		t.Fatalf("request event: %v", req)
	}
	if d := req.GetDetails(); d["plan"] != p.GetName() || d["environment"] != "staging" || d["policy"] != "passed" || len(d["policy_rules"]) != 12 {
		t.Errorf("request details: %v", d)
	}
	var outs []*forgev1.AuditEvent
	for _, e := range f.outcomes(t, p.GetName()) {
		if e.GetAction() == "resource.apply" {
			outs = append(outs, e)
		}
	}
	if len(outs) != 1 || outs[0].GetResult().GetCode() != "ok" || outs[0].GetActor().GetEmail() != "ana@example.com" ||
		outs[0].GetDetails()["changes"] != "create=1 update=0 delete=0" || outs[0].GetDetails()["policy"] != "passed" {
		t.Errorf("apply outcomes: %v", outs)
	}

	// Planning the same again changes nothing, and nothing cannot be applied.
	same, _ := f.plan(t, "writer", "staging", database("5Gi", 0), false)
	if len(same.GetChanges()) != 0 || same.GetState() != forgev1.ResourcePlan_STATE_PLANNED {
		t.Errorf("unchanged plan: %v", same)
	}
	_, err := f.startApply(&forgev1.ApplyResourcePlanRequest{Name: same.GetName()})
	wantCode(t, "a plan with no changes", err, connect.CodeFailedPrecondition, "nothing to apply")

	// More storage is an update of the same resource.
	grow, _ := f.plan(t, "writer", "staging", database("10Gi", 0), false)
	if cs := grow.GetChanges(); len(cs) != 1 || cs[0].GetAction() != "update" || cs[0].GetBefore().GetStorageGi() != 5 || cs[0].GetAfter().GetStorageGi() != 10 {
		t.Fatalf("grow plan: %v", grow)
	}
	if op := f.apply(t, grow.GetName()); op.GetError() != nil {
		t.Fatalf("grow apply: %v", op.GetError())
	}
	grown := f.getResource(t, "resources/payments/staging/main")
	if grown.GetSettings().GetStorageGi() != 10 || grown.GetLastPlan() != grow.GetName() || !grown.GetCreateTime().AsTime().Equal(res.GetCreateTime().AsTime()) {
		t.Errorf("after growing: %v", grown)
	}
}

func TestApplyRefusals(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	f := newResourceFixture(t, func(o *ResourceOptions) { o.Now = clock })
	ctx := context.Background()

	_, err := f.startApply(&forgev1.ApplyResourcePlanRequest{Name: "resourcePlans/0123456789ab"})
	wantCode(t, "a missing plan", err, connect.CodeNotFound, "resourcePlans/0123456789ab")
	_, err = f.startApply(&forgev1.ApplyResourcePlanRequest{Name: "x"})
	wantCode(t, "a bad name", err, connect.CodeInvalidArgument, "resourcePlans/<id>")

	denied, _ := f.plan(t, "writer", "production", database("200Gi", 2), false)
	_, err = f.startApply(&forgev1.ApplyResourcePlanRequest{Name: denied.GetName()})
	wantCode(t, "a denied plan", err, connect.CodeFailedPrecondition, "was denied by policy")

	f.runner.FailPlan = "tofu: boom"
	failed, _ := f.plan(t, "writer", "staging", database("5Gi", 0), false)
	f.runner.FailPlan = ""
	_, err = f.startApply(&forgev1.ApplyResourcePlanRequest{Name: failed.GetName()})
	wantCode(t, "a failed plan", err, connect.CodeFailedPrecondition, "failed; run 'forge resource plan' again")

	// Two plans made from the same state: applying one makes the other stale.
	a, _ := f.plan(t, "writer", "staging", database("5Gi", 0), false)
	b, _ := f.plan(t, "writer", "staging", database("6Gi", 0), false)

	_, err = f.client.ApplyResourcePlan(ctx, as("reader", &forgev1.ApplyResourcePlanRequest{Name: a.GetName()}))
	wantCode(t, "a reader", err, connect.CodePermissionDenied, "services.write")

	// forge.yaml changed since the plan.
	_, err = f.startApply(&forgev1.ApplyResourcePlanRequest{Name: a.GetName(), CheckResources: true, Resources: database("7Gi", 0)})
	wantCode(t, "a changed manifest", err, connect.CodeFailedPrecondition, "forge.yaml changed since")
	_, err = f.startApply(&forgev1.ApplyResourcePlanRequest{Name: a.GetName(), CheckResources: true, Resources: database("7GB", 0)})
	wantCode(t, "a bad manifest", err, connect.CodeInvalidArgument, "resources: main: storage")
	// Without check_resources the list is ignored.
	started := len(f.runner.Started())
	dry, err := f.startApply(&forgev1.ApplyResourcePlanRequest{Name: a.GetName(), Resources: database("7Gi", 0), ValidateOnly: true})
	if err != nil || dry.GetOperation() != nil || dry.GetPlan().GetState() != forgev1.ResourcePlan_STATE_PLANNED {
		t.Fatalf("dry run: %v %v", dry, err)
	}
	if _, err := f.startApply(&forgev1.ApplyResourcePlanRequest{Name: a.GetName(), CheckResources: true, Resources: database("5Gi", 0), ValidateOnly: true}); err != nil {
		t.Errorf("a dry run with the same manifest: %v", err)
	}
	if len(f.runner.Started()) != started || f.getPlan(t, a.GetName()).GetState() != forgev1.ResourcePlan_STATE_PLANNED {
		t.Error("a dry run started a run or changed the plan")
	}
	var dryEvent *forgev1.AuditEvent
	for _, e := range allEvents(t, f.store) {
		if e.GetAction() == "resource.apply" && e.GetDetails()["validate_only"] == "true" && e.GetResult().GetCode() == "ok" {
			dryEvent = e
		}
	}
	if dryEvent == nil || dryEvent.GetDetails()["policy"] != "passed" || dryEvent.GetService() != "services/payments" {
		t.Errorf("the dry run's event: %v", dryEvent)
	}

	if op := f.apply(t, a.GetName()); op.GetError() != nil {
		t.Fatalf("apply: %v", op.GetError())
	}
	_, err = f.startApply(&forgev1.ApplyResourcePlanRequest{Name: a.GetName()})
	wantCode(t, "an applied plan", err, connect.CodeFailedPrecondition, "was already applied")
	_, err = f.startApply(&forgev1.ApplyResourcePlanRequest{Name: b.GetName()})
	wantCode(t, "a stale plan", err, connect.CodeFailedPrecondition, "the resources changed since")

	// A day later a ready plan has expired.
	c, _ := f.plan(t, "writer", "staging", database("6Gi", 0), false)
	mu.Lock()
	now = now.Add(25 * time.Hour)
	mu.Unlock()
	_, err = f.startApply(&forgev1.ApplyResourcePlanRequest{Name: c.GetName()})
	wantCode(t, "an expired plan", err, connect.CodeFailedPrecondition, "expired; run 'forge resource plan' again")
}

func TestApplyChecksPolicyAgain(t *testing.T) {
	t.Parallel()
	f := newResourceFixture(t, nil)
	ctx := context.Background()
	p, _ := f.plan(t, "writer", "staging", database("5Gi", 0), false)

	// The rules changed since the plan: a second API on the same store, with stricter rules.
	rules := "package forge.resources\nimport rego.v1\ndeny contains \"frozen: no changes this week\" if true\nwarn contains \"x\" if false\n"
	strict, err := policy.NewFromFS(ctx, fstest.MapFS{"freeze.rego": {Data: []byte(rules)}})
	if err != nil {
		t.Fatal(err)
	}
	other, err := NewResources(ResourceOptions{Store: f.store, Catalog: f.cat, Operations: f.ops, Policy: strict, Launcher: f.runner})
	if err != nil {
		t.Fatal(err)
	}
	started := len(f.runner.Started())
	_, err = other.ApplyResourcePlan(writerCtx(), connect.NewRequest(&forgev1.ApplyResourcePlanRequest{Name: p.GetName()}))
	wantCode(t, "newer rules deny", err, connect.CodePermissionDenied, "is denied by policy: frozen: no changes this week")
	if len(f.runner.Started()) != started {
		t.Error("a denied apply started a run")
	}
	got := f.getPlan(t, p.GetName())
	if got.GetState() != forgev1.ResourcePlan_STATE_DENIED || got.GetPolicy().GetRules() != strict.Fingerprint() ||
		!slices.Equal(got.GetPolicy().GetDeny(), []string{"frozen: no changes this week"}) {
		t.Errorf("plan after the refusal: %v", got)
	}
	// Denied for good, also for the API with the old rules.
	_, err = f.startApply(&forgev1.ApplyResourcePlanRequest{Name: p.GetName()})
	wantCode(t, "a plan denied at apply", err, connect.CodeFailedPrecondition, "was denied by policy")
}

func TestApplyOnce(t *testing.T) {
	t.Parallel()
	f := newResourceFixture(t, nil)
	p, _ := f.plan(t, "writer", "staging", database("5Gi", 0), false)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var ops []string
	for range 8 {
		wg.Go(func() {
			resp, err := f.startApply(&forgev1.ApplyResourcePlanRequest{Name: p.GetName()})
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				ops = append(ops, resp.GetOperation().GetName())
			} else if connect.CodeOf(err) != connect.CodeFailedPrecondition {
				t.Errorf("a refused apply: %v", err)
			}
		})
	}
	wg.Wait()
	if len(ops) != 1 {
		t.Fatalf("%d of 8 applies started, want 1", len(ops))
	}
	waitCIOp(t, f.ops, ops[0])
	applies := 0
	for _, s := range f.runner.Started() {
		if s.Mode == run.ModeApply {
			applies++
		}
	}
	outs := 0
	for _, e := range f.outcomes(t, p.GetName()) {
		if e.GetAction() == "resource.apply" {
			outs++
		}
	}
	if applies != 1 || outs != 1 {
		t.Errorf("%d apply runs and %d outcomes, want 1 and 1", applies, outs)
	}
}

func TestApplyFailure(t *testing.T) {
	t.Parallel()
	f := newResourceFixture(t, nil)
	p, _ := f.plan(t, "writer", "staging", database("5Gi", 0), false)
	f.runner.FailApply = "tofu: no capacity"
	op := f.apply(t, p.GetName())
	if op.GetError().GetCode() != "run_failed" || op.GetError().GetMessage() != "tofu: no capacity" {
		t.Errorf("operation: %v", op.GetError())
	}
	if got := f.getPlan(t, p.GetName()); got.GetState() != forgev1.ResourcePlan_STATE_APPLY_FAILED || got.GetMessage() != "tofu: no capacity" {
		t.Errorf("plan: %v", got)
	}
	if res := f.getResource(t, "resources/payments/staging/main"); res.GetState() != forgev1.Resource_STATE_FAILED || res.GetLastPlan() != p.GetName() {
		t.Errorf("resource: %v", res)
	}
	_, err := f.startApply(&forgev1.ApplyResourcePlanRequest{Name: p.GetName()})
	wantCode(t, "a failed apply again", err, connect.CodeFailedPrecondition, "failed; run 'forge resource plan' again")

	// A new plan and apply repair it.
	f.runner.FailApply = ""
	again, _ := f.plan(t, "writer", "staging", database("5Gi", 0), false)
	if op := f.apply(t, again.GetName()); op.GetError() != nil {
		t.Fatalf("repair: %v", op.GetError())
	}
	if res := f.getResource(t, "resources/payments/staging/main"); res.GetState() != forgev1.Resource_STATE_READY || res.GetSecret() != "main-app" {
		t.Errorf("after the repair: %v", res)
	}
}

func TestDestroy(t *testing.T) {
	t.Parallel()
	f := newResourceFixture(t, nil)
	ctx := context.Background()
	p, _ := f.plan(t, "writer", "staging", database("5Gi", 0), false)
	f.apply(t, p.GetName())

	// Removing the database from forge.yaml is refused unless asked for.
	refused, op := f.plan(t, "writer", "staging", nil, false)
	if refused.GetState() != forgev1.ResourcePlan_STATE_DENIED || op.GetError().GetCode() != "policy_denied" ||
		!strings.Contains(op.GetError().GetMessage(), "the plan deletes database main") {
		t.Fatalf("a delete without allow_destroy: %v %v", refused, op.GetError())
	}
	gone, op := f.plan(t, "writer", "staging", nil, true)
	if gone.GetState() != forgev1.ResourcePlan_STATE_PLANNED || !gone.GetAllowDestroy() || op.GetError() != nil ||
		len(gone.GetChanges()) != 1 || gone.GetChanges()[0].GetAction() != "delete" || gone.GetChanges()[0].GetBefore().GetStorageGi() != 5 {
		t.Fatalf("a delete with allow_destroy: %v %v", gone, op.GetError())
	}

	// While the destroy runs the resource is "deleting"; a cancel leaves it "failed".
	f.runner.Hang = true
	resp, err := f.startApply(&forgev1.ApplyResourcePlanRequest{Name: gone.GetName()})
	if err != nil {
		t.Fatal(err)
	}
	if res := f.getResource(t, "resources/payments/staging/main"); res.GetState() != forgev1.Resource_STATE_DELETING {
		t.Errorf("during the destroy: %v", res.GetState())
	}
	if _, err := f.opc.CancelOperation(ctx, as("writer", &forgev1.CancelOperationRequest{Name: resp.GetOperation().GetName()})); err != nil {
		t.Fatal(err)
	}
	if op := waitCIOp(t, f.ops, resp.GetOperation().GetName()); op.GetError().GetCode() != "cancelled" {
		t.Errorf("cancelled apply: %v", op.GetError())
	}
	if res := f.getResource(t, "resources/payments/staging/main"); res.GetState() != forgev1.Resource_STATE_FAILED {
		t.Errorf("after the cancel: %v", res.GetState())
	}
	if got := f.getPlan(t, gone.GetName()); got.GetState() != forgev1.ResourcePlan_STATE_APPLY_FAILED {
		t.Errorf("plan after the cancel: %v", got.GetState())
	}

	// Plan and apply the destroy again: the database and its record go.
	f.runner.Hang = false
	gone2, _ := f.plan(t, "writer", "staging", nil, true)
	op = f.apply(t, gone2.GetName())
	if op.GetError() != nil {
		t.Fatalf("destroy: %v", op.GetError())
	}
	if last := op.GetEvents()[len(op.GetEvents())-1]; last.GetMessage() != "The next deploy drops DATABASE_URL." {
		t.Errorf("last event: %q", last.GetMessage())
	}
	if len(f.runner.Databases()) != 0 {
		t.Errorf("databases: %v", f.runner.Databases())
	}
	_, err = f.client.GetResource(ctx, as("reader", &forgev1.GetResourceRequest{Name: "resources/payments/staging/main"}))
	wantCode(t, "a destroyed resource", err, connect.CodeNotFound, "resources/payments/staging/main")
	var out *forgev1.AuditEvent
	for _, e := range f.outcomes(t, gone2.GetName()) {
		if e.GetAction() == "resource.apply" {
			out = e
		}
	}
	if out == nil || out.GetDetails()["allow_destroy"] != "true" || out.GetDetails()["changes"] != "create=0 update=0 delete=1" {
		t.Errorf("destroy outcome: %v", out)
	}
}

func TestResourceReads(t *testing.T) {
	t.Parallel()
	cluster := &fakeCluster{err: errors.New("the API server is away")}
	f := newResourceFixture(t, func(o *ResourceOptions) { o.Health = cluster })
	ctx := context.Background()
	for _, env := range []string{"staging", "production"} {
		p, _ := f.plan(t, "writer", env, database("5Gi", 2), false)
		f.apply(t, p.GetName())
	}
	list := func(req *forgev1.ListResourcesRequest) []string {
		t.Helper()
		resp, err := f.client.ListResources(ctx, as("reader", req))
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, r := range resp.Msg.GetResources() {
			names = append(names, r.GetName())
			// A cluster that cannot be read gives "unknown", not an error.
			if r.GetHealth().GetPhase() != "unknown" {
				t.Errorf("%s health: %v", r.GetName(), r.GetHealth())
			}
		}
		return names
	}
	if got := list(&forgev1.ListResourcesRequest{}); !slices.Equal(got, []string{"resources/payments/production/main", "resources/payments/staging/main"}) {
		t.Errorf("all: %v", got)
	}
	if got := list(&forgev1.ListResourcesRequest{Service: "payments", Env: "staging"}); !slices.Equal(got, []string{"resources/payments/staging/main"}) {
		t.Errorf("staging: %v", got)
	}
	if got := list(&forgev1.ListResourcesRequest{Service: "services/billing"}); len(got) != 0 {
		t.Errorf("billing: %v", got)
	}
	_, err := f.client.ListResources(ctx, as("reader", &forgev1.ListResourcesRequest{Env: "qa"}))
	wantCode(t, "a bad environment", err, connect.CodeInvalidArgument, "staging or production")
	for _, name := range []string{"main", "resources/payments/main", "resources/payments/qa/main", "resources/Payments/staging/main", "resources/payments/staging/"} {
		_, err := f.client.GetResource(ctx, as("reader", &forgev1.GetResourceRequest{Name: name}))
		wantCode(t, "name "+name, err, connect.CodeInvalidArgument, "")
	}
	_, err = f.client.GetResource(ctx, as("reader", &forgev1.GetResourceRequest{Name: "resources/payments/staging/other"}))
	wantCode(t, "a missing resource", err, connect.CodeNotFound, "resources/payments/staging/other")

	var off *Resources
	_, err = off.ApplyResourcePlan(writerCtx(), connect.NewRequest(&forgev1.ApplyResourcePlanRequest{}))
	wantCode(t, "resources off", err, connect.CodeFailedPrecondition, "resources are not configured")
	_, err = off.ListResources(writerCtx(), connect.NewRequest(&forgev1.ListResourcesRequest{}))
	wantCode(t, "resources off", err, connect.CodeFailedPrecondition, "resources are not configured")
	_, err = off.GetResource(writerCtx(), connect.NewRequest(&forgev1.GetResourceRequest{}))
	wantCode(t, "resources off", err, connect.CodeFailedPrecondition, "resources are not configured")

	// No health reader at all: also "unknown".
	plain := newResourceFixture(t, nil)
	p, _ := plain.plan(t, "writer", "staging", database("5Gi", 0), false)
	plain.apply(t, p.GetName())
	if h := plain.getResource(t, "resources/payments/staging/main").GetHealth(); h.GetPhase() != "unknown" {
		t.Errorf("no health reader: %v", h)
	}
}

func TestDeleteServiceGuard(t *testing.T) {
	t.Parallel()
	f := newResourceFixture(t, nil)
	p, _ := f.plan(t, "writer", "staging", database("5Gi", 0), false)
	f.apply(t, p.GetName())

	del := func(name string, dryRun bool) error {
		_, err := f.cat.DeleteService(writerCtx(), connect.NewRequest(&forgev1.DeleteServiceRequest{Name: name, ValidateOnly: dryRun}))
		return err
	}
	for _, dryRun := range []bool{true, false} {
		err := del("services/payments", dryRun)
		wantCode(t, "a service with a database", err, connect.CodeFailedPrecondition, "services/payments still has resources (resources/payments/staging/main)")
		if !strings.Contains(err.Error(), "forge resource plan --env <env> --allow-destroy") {
			t.Errorf("the refusal must say how to remove them: %v", err)
		}
	}
	if err := del("services/billing", false); err != nil {
		t.Errorf("a service without resources: %v", err)
	}
	gone, _ := f.plan(t, "writer", "staging", nil, true)
	f.apply(t, gone.GetName())
	if err := del("services/payments", false); err != nil {
		t.Errorf("after the destroy: %v", err)
	}
}
```

Run: `go test ./internal/server -run 'TestApply|TestDestroy|TestResourceReads|TestDeleteServiceGuard'`
Expected: FAIL (`TestApply`: `unimplemented: apply is not built yet`).

- [ ] **Step 2: Implement resource_apply.go**

Delete the three stub methods and their comment from the end of `internal/server/resources.go`.

In `internal/server/audit.go`, after `noteAuditDetail`, add:

```go
// noteAuditTarget replaces the call's target, when the request did not name it.
func noteAuditTarget(ctx context.Context, target string) {
	if r := auditFrom(ctx); r != nil && target != "" {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.ev.Target = auditText(target, auditFieldMax)
	}
}
```

Create `internal/server/resource_apply.go`:

```go
package server

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/manifest"
	"github.com/showmino/forge/internal/resource"
	"github.com/showmino/forge/internal/run"
	"github.com/showmino/forge/internal/store"
	"github.com/showmino/forge/policy"
)

// applicable checks everything about a saved plan that needs no policy: its
// state and age, that it changes something, and that neither forge.yaml nor
// the real state moved since it was made.
func (r *Resources) applicable(ctx context.Context, rec *store.PlanRecord, m *forgev1.ApplyResourcePlanRequest) error {
	p := r.view(rec)
	name := p.GetName()
	switch p.GetState() {
	case forgev1.ResourcePlan_STATE_PLANNED:
	case forgev1.ResourcePlan_STATE_DENIED:
		return precondition("%s was denied by policy", name)
	case forgev1.ResourcePlan_STATE_APPLIED:
		return precondition("%s was already applied", name)
	case forgev1.ResourcePlan_STATE_FAILED, forgev1.ResourcePlan_STATE_APPLY_FAILED:
		return precondition("%s failed; run 'forge resource plan' again", name)
	case forgev1.ResourcePlan_STATE_EXPIRED:
		return precondition("%s expired; run 'forge resource plan' again", name)
	case forgev1.ResourcePlan_STATE_PLANNING:
		return precondition("%s is still being made", name)
	case forgev1.ResourcePlan_STATE_APPLYING:
		return precondition("%s is already being applied", name)
	default:
		return precondition("%s is in state %s", name, planStateWord(p.GetState()))
	}
	if len(p.GetChanges()) == 0 {
		return precondition("%s has no changes: nothing to apply", name)
	}
	if m.GetCheckResources() {
		settings, err := resource.Resolve(m.GetResources(), p.GetEnv())
		if err != nil {
			return invalidArg("resources: %v", err)
		}
		if resource.Digest(settings) != rec.Digest {
			return precondition("forge.yaml changed since %s was made; run 'forge resource plan' again", name)
		}
	}
	_, target := planTarget(p)
	st, err := r.o.Store.GetResourceState(ctx, target)
	if err != nil {
		return storeErr(err)
	}
	if st.Version != rec.StateVersion {
		return precondition("the resources changed since %s was made; run 'forge resource plan' again", name)
	}
	return nil
}

// deny marks a ready plan as denied by pol: rules that changed since the
// plan was made refused it.
func (r *Resources) deny(ctx context.Context, planName string, pol *forgev1.PolicyResult) error {
	return r.o.Store.WriteResources(ctx, func(tx store.ResourceTx) error {
		cur, err := tx.GetResourcePlan(ctx, planName)
		if err != nil {
			return err
		}
		if cur.Plan.GetState() != forgev1.ResourcePlan_STATE_PLANNED {
			return nil
		}
		cur.Plan.State, cur.Plan.Policy = forgev1.ResourcePlan_STATE_DENIED, pol
		cur.PlanFile, cur.PlanJSON = nil, nil
		return tx.PutResourcePlan(ctx, cur)
	})
}

// markResources records, as an apply starts, what it is about to make or delete.
func markResources(ctx context.Context, tx store.ResourceTx, p *forgev1.ResourcePlan, now *timestamppb.Timestamp) error {
	for _, c := range p.GetChanges() {
		name := resourceName(p, c.GetResource())
		old, err := tx.GetResource(ctx, name)
		missing := errors.Is(err, store.ErrNotFound)
		if err != nil && !missing {
			return err
		}
		var res *forgev1.Resource
		switch {
		case c.GetAction() == "delete" && !missing:
			res = old
			res.State = forgev1.Resource_STATE_DELETING
		case c.GetAction() == "create" && missing && c.GetAfter() != nil:
			res = &forgev1.Resource{Name: name, Service: p.GetService(), Env: p.GetEnv(), Type: c.GetAfter().GetType(),
				Settings: c.GetAfter(), State: forgev1.Resource_STATE_APPLYING, CreateTime: now}
		default:
			continue
		}
		res.LastPlan, res.UpdateTime = p.GetName(), now
		if err := tx.PutResource(ctx, res); err != nil {
			return err
		}
	}
	return nil
}

// ApplyResourcePlan checks a saved plan, checks policy again, and starts the
// run that applies exactly that plan.
func (r *Resources) ApplyResourcePlan(ctx context.Context, req *connect.Request[forgev1.ApplyResourcePlanRequest]) (*connect.Response[forgev1.ApplyResourcePlanResponse], error) {
	if err := r.ready(); err != nil {
		return nil, err
	}
	if err := require(ctx, PermServicesWrite); err != nil {
		return nil, err
	}
	m := req.Msg
	planName, err := parsePlanName(m.GetName())
	if err != nil {
		return nil, err
	}
	rec, err := r.o.Store.GetResourcePlan(ctx, planName)
	if errors.Is(err, store.ErrNotFound) {
		return nil, notFound(planName)
	}
	if err != nil {
		return nil, storeErr(err)
	}
	_, target := planTarget(rec.Plan)
	noteAuditService(ctx, rec.Plan.GetService())
	noteAuditTarget(ctx, resourceNamePrefix+target)
	noteAuditDetail(ctx, "plan", planName)
	noteAuditDetail(ctx, "environment", rec.Plan.GetEnv())
	if err := r.applicable(ctx, rec, m); err != nil {
		return nil, err
	}

	// The rules may have changed since the plan was made: today's decide.
	pol, err := r.evaluate(ctx, rec, rec.PlanJSON, policy.Actor{Kind: actorUser, Subject: subjectOf(ctx)})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("policy could not be evaluated, so %s was not applied: %w", planName, err))
	}
	noteAuditDetail(ctx, "policy_rules", pol.GetRules())
	if pol.GetDenied() {
		noteAuditDetail(ctx, "policy", "denied")
		if err := r.deny(ctx, planName, pol); err != nil {
			return nil, storeErr(err)
		}
		return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("%s is denied by policy: %s", planName, strings.Join(pol.GetDeny(), "; ")))
	}
	noteAuditDetail(ctx, "policy", "passed")
	if m.GetValidateOnly() {
		return connect.NewResponse(&forgev1.ApplyResourcePlanResponse{Plan: r.view(rec)}), nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if running, ok := r.running[target]; ok {
		return nil, precondition("%s is already running for %s%s", running, resourceNamePrefix, target)
	}
	now := r.o.Now()
	token, hash := newRunToken()
	rec.Plan.State, rec.Plan.Applier, rec.Plan.Policy = forgev1.ResourcePlan_STATE_APPLYING, actorOf(ctx), pol
	rec.TokenHash, rec.Deadline = hash, now.Add(r.o.RunTimeout)
	op, err := r.startRun(ctx, rec, run.ModeApply, token, func(tx store.ResourceTx) error {
		// The plan may have been applied, denied or expired since it was read.
		cur, err := tx.GetResourcePlan(ctx, planName)
		if err != nil {
			return err
		}
		if cur.Plan.GetState() != forgev1.ResourcePlan_STATE_PLANNED {
			return precondition("%s is no longer ready to apply", planName)
		}
		return markResources(ctx, tx, rec.Plan, timestamppb.New(now))
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&forgev1.ApplyResourcePlanResponse{Operation: op, Plan: r.view(rec)}), nil
}

var resourceShortRE = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,28}[a-z0-9])?$`)

// parseResourceName checks "resources/<service>/<env>/<name>".
func parseResourceName(name string) (string, error) {
	bad := invalidArg("%q is not a resource name (resources/<service>/<env>/<name>)", name)
	rest, ok := strings.CutPrefix(name, resourceNamePrefix)
	parts := strings.Split(rest, "/")
	if !ok || len(parts) != 3 || !manifest.ValidID(parts[0]) || validEnv(parts[1]) != nil || !resourceShortRE.MatchString(parts[2]) {
		return "", bad
	}
	return name, nil
}

// withHealth sets res.Health from the cluster: "unknown" when it cannot be read.
func (r *Resources) withHealth(ctx context.Context, res *forgev1.Resource) {
	res.Health = &forgev1.ResourceHealth{Phase: "unknown"}
	if r.o.Health == nil {
		return
	}
	svc := strings.TrimPrefix(res.GetService(), servicePrefix)
	h, err := r.o.Health.DatabaseHealth(ctx, svc, res.GetEnv(), res.GetSettings().GetName())
	if err != nil || h == nil {
		if err != nil {
			r.o.Log.Warn("reading a database's health", "resource", res.GetName(), "error", err)
		}
		return
	}
	res.Health = h
}

// ListResources returns resources with their live health.
func (r *Resources) ListResources(ctx context.Context, req *connect.Request[forgev1.ListResourcesRequest]) (*connect.Response[forgev1.ListResourcesResponse], error) {
	if err := r.ready(); err != nil {
		return nil, err
	}
	if err := require(ctx, PermServicesRead); err != nil {
		return nil, err
	}
	m := req.Msg
	service := ""
	if m.GetService() != "" {
		name, err := parseName(m.GetService())
		if err != nil {
			return nil, err
		}
		service = name
	}
	if m.GetEnv() != "" {
		if err := validEnv(m.GetEnv()); err != nil {
			return nil, err
		}
	}
	rs, err := r.o.Store.ListResources(ctx, service, m.GetEnv())
	if err != nil {
		return nil, storeErr(err)
	}
	for _, res := range rs {
		r.withHealth(ctx, res)
	}
	return connect.NewResponse(&forgev1.ListResourcesResponse{Resources: rs}), nil
}

// GetResource returns one resource with its live health.
func (r *Resources) GetResource(ctx context.Context, req *connect.Request[forgev1.GetResourceRequest]) (*connect.Response[forgev1.GetResourceResponse], error) {
	if err := r.ready(); err != nil {
		return nil, err
	}
	if err := require(ctx, PermServicesRead); err != nil {
		return nil, err
	}
	name, err := parseResourceName(req.Msg.GetName())
	if err != nil {
		return nil, err
	}
	res, err := r.o.Store.GetResource(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return nil, notFound(name)
	}
	if err != nil {
		return nil, storeErr(err)
	}
	r.withHealth(ctx, res)
	return connect.NewResponse(&forgev1.GetResourceResponse{Resource: res}), nil
}
```

- [ ] **Step 3: Guard DeleteService**

In `internal/server/catalog.go`, add a field to the `Catalog` struct:

```go
	// resources, when set, is asked before a service is deleted: a service
	// that still has resources stays.
	resources store.ResourceStore
```

In `DeleteService`, after the `parseName` check and before `err = c.store.Write(`:

```go
	if c.resources != nil {
		rs, err := c.resources.ListResources(ctx, name, "")
		if err != nil {
			return nil, storeErr(err)
		}
		if len(rs) > 0 {
			names := make([]string, len(rs))
			for i, r := range rs {
				names[i] = r.GetName()
			}
			return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(
				"%s still has resources (%s); remove them from forge.yaml, then run 'forge resource plan --env <env> --allow-destroy' and apply the plan",
				name, strings.Join(names, ", ")))
		}
	}
```

In `internal/server/server.go`, change the block added in Task 6 to:

```go
	if o.Resources != nil {
		o.Resources.setAuditor(aud)
		o.Catalog.resources = o.Resources.o.Store
	}
```

- [ ] **Step 4: Run the tests**

Run: `go test -race ./internal/server`
Expected: PASS.

- [ ] **Step 5: Lint and commit**

Run: `make lint`
Expected: clean.

```bash
git add internal/server
git commit -m "server: apply a saved resource plan, resource records with live health, and keep services that have resources

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 8: Deadlines, expiry and runs that end after a restart

**Files:**
- Create: `internal/server/resource_sweep.go`
- Create: `internal/server/resource_sweep_test.go`

**Interfaces:**
- Consumes (Task 6): `(*Resources).endRun`, `(*Resources).live`, `modeOf`, `runEnd`, `opCodeRunTimeout`; `newResourceFixture`, `database` and the fixture's helpers from `resources_test.go`.
- Produces: `func (r *Resources) Sweep(ctx context.Context)` and `func (r *Resources) RunSweeper(ctx context.Context, every time.Duration)`. Plan 2 starts `RunSweeper` in `cmd/forge-api`.

- [ ] **Step 1: Write the failing tests**

Create `internal/server/resource_sweep_test.go`:

```go
package server

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/envtest"
	"github.com/showmino/forge/internal/run"
	"github.com/showmino/forge/internal/store"
)

// testClock is a clock tests move by hand.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newTestClock() *testClock {
	return &testClock{now: time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)}
}

func TestSweepExpiresPlans(t *testing.T) {
	t.Parallel()
	clk := newTestClock()
	f := newResourceFixture(t, func(o *ResourceOptions) { o.Now = clk.Now })
	ctx := context.Background()
	ready, _ := f.plan(t, "writer", "staging", database("5Gi", 0), false)
	denied, _ := f.plan(t, "writer", "production", database("200Gi", 2), false)
	events := len(allEvents(t, f.store))

	f.res.Sweep(ctx) // nothing is due yet
	if rec, _ := f.store.GetResourcePlan(ctx, ready.GetName()); rec.Plan.GetState() != forgev1.ResourcePlan_STATE_PLANNED || len(rec.PlanFile) == 0 {
		t.Fatalf("swept too early: %+v", rec)
	}
	clk.Add(24 * time.Hour)
	f.res.Sweep(ctx)
	rec, _ := f.store.GetResourcePlan(ctx, ready.GetName())
	if rec.Plan.GetState() != forgev1.ResourcePlan_STATE_EXPIRED || len(rec.PlanFile) != 0 || len(rec.PlanJSON) != 0 {
		t.Errorf("an expired plan: %+v", rec)
	}
	if len(rec.Plan.GetChanges()) != 1 || rec.Plan.GetPolicy() == nil {
		t.Error("an expired plan keeps its summary and policy result")
	}
	if rec, _ := f.store.GetResourcePlan(ctx, denied.GetName()); rec.Plan.GetState() != forgev1.ResourcePlan_STATE_DENIED {
		t.Errorf("a denied plan: %v", rec.Plan.GetState())
	}
	if got := len(allEvents(t, f.store)); got != events {
		t.Errorf("expiry wrote %d audit events", got-events)
	}
	_, err := f.startApply(&forgev1.ApplyResourcePlanRequest{Name: ready.GetName()})
	wantCode(t, "an expired plan", err, connect.CodeFailedPrecondition, "expired")
}

func TestSweepEndsDeadRuns(t *testing.T) {
	t.Parallel()
	clk := newTestClock()
	f := newResourceFixture(t, func(o *ResourceOptions) { o.Now = clk.Now })
	ctx := context.Background()

	// A run whose forge-api died: the record says "applying", nobody watches it.
	tmpl, err := protojson.Marshal(&forgev1.AuditEvent{Kind: "outcome", Action: "resource.apply",
		Actor:   &forgev1.AuditActor{Kind: "user", Subject: "ana", Email: "ana@example.com"},
		Service: "services/payments", Target: "resources/payments/staging", Operation: "operations/op_0123456789ab"})
	if err != nil {
		t.Fatal(err)
	}
	main := &forgev1.ResourceSettings{Name: "main", Type: "postgres", StorageGi: 5, Instances: 1}
	dead := &store.PlanRecord{
		Plan: &forgev1.ResourcePlan{
			Name: "resourcePlans/aaaaaaaaaaaa", Service: "services/payments", Env: "staging", State: forgev1.ResourcePlan_STATE_APPLYING,
			Resources: []*forgev1.ResourceSettings{main}, Changes: []*forgev1.ResourceChange{{Action: "create", Resource: "main", Type: "postgres", After: main}},
			Policy: &forgev1.PolicyResult{Rules: "0123456789ab"}, Operation: "operations/op_0123456789ab",
			CreateTime: timestamppb.New(clk.Now()), ExpireTime: timestamppb.New(clk.Now().Add(24 * time.Hour)),
		},
		TokenHash: hashToken("dead-token"), Deadline: clk.Now().Add(15 * time.Minute), Audit: tmpl, PlanFile: []byte("plan"),
	}
	err = f.store.WriteResources(ctx, func(tx store.ResourceTx) error {
		if err := tx.PutResourcePlan(ctx, dead); err != nil {
			return err
		}
		return tx.PutResource(ctx, &forgev1.Resource{Name: "resources/payments/staging/main", Service: "services/payments", Env: "staging",
			Type: "postgres", Settings: main, State: forgev1.Resource_STATE_APPLYING})
	})
	if err != nil {
		t.Fatal(err)
	}

	f.res.Sweep(ctx) // before the deadline: the run may still report
	if rec, _ := f.store.GetResourcePlan(ctx, dead.Plan.GetName()); rec.Plan.GetState() != forgev1.ResourcePlan_STATE_APPLYING {
		t.Fatalf("swept before the deadline: %v", rec.Plan.GetState())
	}
	clk.Add(16 * time.Minute)
	// Past the deadline its token is dead too.
	wantCode(t, "a late complete", f.res.Complete(ctx, "dead-token", run.Result{}), connect.CodeUnauthenticated, "not valid")
	f.res.Sweep(ctx)
	f.res.Sweep(ctx) // a second sweep finds nothing to do

	rec, _ := f.store.GetResourcePlan(ctx, dead.Plan.GetName())
	if rec.Plan.GetState() != forgev1.ResourcePlan_STATE_APPLY_FAILED || rec.Plan.GetMessage() != "the run did not finish before its deadline" ||
		rec.TokenHash != "" || len(rec.PlanFile) != 0 {
		t.Errorf("the dead run's plan: %+v", rec)
	}
	if res := f.getResource(t, "resources/payments/staging/main"); res.GetState() != forgev1.Resource_STATE_FAILED {
		t.Errorf("its resource: %v", res.GetState())
	}
	outs := f.outcomes(t, dead.Plan.GetName())
	if len(outs) != 1 {
		t.Fatalf("%d outcome events, want 1: %v", len(outs), outs)
	}
	if o := outs[0]; o.GetResult().GetCode() != "failed" || o.GetResult().GetMessage() != "run_timeout: the run did not finish before its deadline" ||
		o.GetActor().GetEmail() != "ana@example.com" || o.GetOperation() != "operations/op_0123456789ab" || o.GetAction() != "resource.apply" {
		t.Errorf("outcome: %v", o)
	}
}

func TestSweepLeavesWatchedRuns(t *testing.T) {
	t.Parallel()
	clk := newTestClock()
	f := newResourceFixture(t, func(o *ResourceOptions) { o.Now = clk.Now })
	f.runner.Hang = true
	resp := f.startPlan(t, "writer", "services/payments", "staging", database("5Gi", 0), false)
	deadline := time.Now().Add(5 * time.Second)
	for len(f.runner.Started()) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	clk.Add(time.Hour) // past the run's deadline by the fake clock; its operation still watches it
	f.res.Sweep(context.Background())
	if p := f.getPlan(t, resp.GetPlan().GetName()); p.GetState() != forgev1.ResourcePlan_STATE_PLANNING {
		t.Errorf("the sweep ended a run its operation watches: %v", p.GetState())
	}
	if outs := f.outcomes(t, resp.GetPlan().GetName()); len(outs) != 0 {
		t.Errorf("outcomes: %v", outs)
	}
}

func TestCompleteAfterRestart(t *testing.T) {
	t.Parallel()
	f := newResourceFixture(t, nil)
	f.runner.Hang = true // the runner outlives forge-api; the test plays it
	ctx := context.Background()
	resp := f.startPlan(t, "writer", "services/payments", "staging", database("5Gi", 0), false)
	deadline := time.Now().Add(5 * time.Second)
	for len(f.runner.Started()) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	token := f.runner.Started()[0].Token

	// "Restart": a new API on the same store, with no memory of the run.
	after, err := NewResources(ResourceOptions{Store: f.store, Catalog: f.cat, Operations: NewOperations(nil), Policy: f.res.o.Policy, Launcher: &envtest.Runner{}})
	if err != nil {
		t.Fatal(err)
	}
	after.setAuditor(&auditor{store: f.store, log: slog.New(slog.DiscardHandler)})

	work, err := after.Claim(ctx, token)
	if err != nil || work.Mode != run.ModePlan {
		t.Fatalf("claim after the restart: %+v %v", work, err)
	}
	if err := after.Event(ctx, token, "plan", "still planning"); err != nil {
		t.Errorf("an event with no operation to show it: %v", err)
	}
	planJSON := []byte(`{"resource_changes":[{"address":"kubernetes_manifest.database[\"main\"]","type":"kubernetes_manifest",
		"change":{"actions":["create"],"before":null,"after":{"manifest":{"apiVersion":"postgresql.cnpg.io/v1","kind":"Cluster",
		"metadata":{"name":"main","namespace":"payments-staging"},"spec":{"instances":1,"storage":{"size":"5Gi","storageClass":"longhorn-retain"}}}}}}]}`)
	if err := after.Complete(ctx, token, run.Result{PlanFile: planJSON, PlanJSON: planJSON}); err != nil {
		t.Fatal(err)
	}

	p := f.getPlan(t, resp.GetPlan().GetName())
	if p.GetState() != forgev1.ResourcePlan_STATE_PLANNED || len(p.GetChanges()) != 1 {
		t.Fatalf("plan: %v", p)
	}
	outs := f.outcomes(t, p.GetName())
	if len(outs) != 1 || outs[0].GetResult().GetCode() != "ok" || outs[0].GetActor().GetEmail() != "ana@example.com" ||
		outs[0].GetOperation() != resp.GetOperation().GetName() {
		t.Fatalf("outcomes: %v", outs)
	}

	// The old API's operation, still watching, ends without a second outcome.
	if _, err := f.opc.CancelOperation(ctx, as("writer", &forgev1.CancelOperationRequest{Name: resp.GetOperation().GetName()})); err != nil {
		t.Fatal(err)
	}
	waitCIOp(t, f.ops, resp.GetOperation().GetName())
	if outs := f.outcomes(t, p.GetName()); len(outs) != 1 {
		t.Errorf("%d outcome events after the old operation ended, want 1", len(outs))
	}
	if got := f.getPlan(t, p.GetName()); got.GetState() != forgev1.ResourcePlan_STATE_PLANNED {
		t.Errorf("the plan changed: %v", got.GetState())
	}

	// A nil service sweeps nothing and does not panic.
	var off *Resources
	off.Sweep(ctx)
}
```

Run: `go test ./internal/server -run 'TestSweep|TestCompleteAfterRestart'`
Expected: FAIL to build (`f.res.Sweep undefined`).

- [ ] **Step 2: Implement the sweep**

Create `internal/server/resource_sweep.go`:

```go
package server

import (
	"context"
	"time"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/store"
)

// sweepLimit is the most plans one sweep looks at; the rest wait for the next.
const sweepLimit = 1000

// RunSweeper sweeps now and then every interval, until ctx ends.
func (r *Resources) RunSweeper(ctx context.Context, every time.Duration) {
	if r == nil {
		return
	}
	r.Sweep(ctx)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.Sweep(ctx)
		}
	}
}

// Sweep ends the runs that passed their deadline with nobody watching them
// (forge-api restarted, or the runner never reported), and marks the plans
// nobody applied within their time as expired.
func (r *Resources) Sweep(ctx context.Context) {
	if r == nil {
		return
	}
	recs, err := r.o.Store.ListResourcePlans(ctx, store.PlanQuery{Limit: sweepLimit, States: []forgev1.ResourcePlan_State{
		forgev1.ResourcePlan_STATE_PLANNING, forgev1.ResourcePlan_STATE_APPLYING, forgev1.ResourcePlan_STATE_PLANNED,
	}})
	if err != nil {
		r.o.Log.Error("resource sweep: listing plans", "error", err)
		return
	}
	now := r.o.Now()
	for _, rec := range recs {
		name := rec.Plan.GetName()
		mode := modeOf(rec.Plan.GetState())
		switch {
		case mode != "":
			// A run an operation of this process watches is ended by that operation.
			if now.Before(rec.Deadline) || r.isLive(name) {
				continue
			}
			if _, err := r.endRun(ctx, name, mode, runEnd{code: opCodeRunTimeout, message: "the run did not finish before its deadline"}); err != nil {
				r.o.Log.Error("resource sweep: ending a run", "plan", name, "error", err)
			}
		case !now.Before(rec.Plan.GetExpireTime().AsTime()):
			if err := r.expire(ctx, name); err != nil {
				r.o.Log.Error("resource sweep: expiring a plan", "plan", name, "error", err)
			}
		}
	}
}

// isLive reports whether an operation of this process is watching plan's run.
func (r *Resources) isLive(planName string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.live[planName]
	return ok
}

// expire marks a plan nobody applied in time. Its plan file goes; its
// summary and policy result stay.
func (r *Resources) expire(ctx context.Context, planName string) error {
	return r.o.Store.WriteResources(ctx, func(tx store.ResourceTx) error {
		cur, err := tx.GetResourcePlan(ctx, planName)
		if err != nil {
			return err
		}
		if cur.Plan.GetState() != forgev1.ResourcePlan_STATE_PLANNED {
			return nil
		}
		cur.Plan.State = forgev1.ResourcePlan_STATE_EXPIRED
		cur.PlanFile, cur.PlanJSON = nil, nil
		return tx.PutResourcePlan(ctx, cur)
	})
}
```

- [ ] **Step 3: Run the tests**

Run: `go test -race ./internal/server`
Expected: PASS.

- [ ] **Step 4: Lint and commit**

Run: `make lint`
Expected: clean.

```bash
git add internal/server
git commit -m "server: end resource runs that passed their deadline and expire old plans

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 9: `forge resource plan`, `apply`, `list` and `get`

**Files:**
- Create: `internal/cli/resource.go`
- Create: `internal/cli/resource_test.go`
- Create (by `make update-golden`): `internal/cli/testdata/resource_plan_table.golden`, `resource_plan_denied.golden`, `resource_list_table.golden`
- Modify: `internal/cli/root.go` (register the command)
- Modify: `internal/cli/prompt.go` (a yes/no prompt)
- Modify: `internal/cli/cli_test.go` (the harness serves resources)

**Interfaces:**
- Consumes:
  - Task 1: `client.Client.Resources` and the proto types.
  - Task 2: `manifest.Manifest.Declarations()`, `manifest.Load`.
  - Tasks 4 to 7: `server.NewResources`, `server.ResourceOptions`, `policy.New`, `envtest.Runner`.
  - Existing in package `cli`: `group`, `noArgs`, `f.Format`, `f.Manifest`, `f.withTimeout`, `f.Client`, `f.StreamClient`, `f.watchOperation`, `opFailure`, `f.printStarted`, `f.confirmDestructive`, `f.interactive`, `f.readLine`, `f.infof`, `f.Now`, `age`, `lowerEnum`, `golden`.
- Produces: the command `forge resource` (alias `resources`) with `plan`, `apply`, `list` (alias `ls`) and `get`.

- [ ] **Step 1: Let the test harness serve resources**

In `internal/cli/cli_test.go`:

Add a field to `harness`, after `deploys`:

```go
	resources *server.Resources
```

In `startAPI`, add to the `server.Options` literal after `Deploys: h.deploys,`:

```go
		Resources:     h.resources,
```

- [ ] **Step 2: Write the failing tests**

Create `internal/cli/resource_test.go`:

```go
package cli

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/showmino/forge/internal/envtest"
	"github.com/showmino/forge/internal/server"
	"github.com/showmino/forge/policy"
)

// serveResources restarts the harness API with resources on a fake runner.
func (h *harness) serveResources() *envtest.Runner {
	h.t.Helper()
	pol, err := policy.New(context.Background())
	if err != nil {
		h.t.Fatal(err)
	}
	runner := &envtest.Runner{}
	h.resources, err = server.NewResources(server.ResourceOptions{
		Store: h.audit, Catalog: h.catalog, Operations: h.ops, Policy: pol, Launcher: runner,
		Poll: 2 * time.Millisecond, Grace: 30 * time.Millisecond,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	runner.API = h.resources
	h.t.Cleanup(runner.Close)
	h.startAPI(h.authn)
	return runner
}

// manifestWith writes forge.yaml for service payments with resources.
func (h *harness) manifestWith(resources string) {
	h.t.Helper()
	body := "apiVersion: forge.dev/v1\nservice:\n  name: payments\n  team: billing\n" + resources
	if err := os.WriteFile(filepath.Join(h.dir, "forge.yaml"), []byte(body), 0o600); err != nil {
		h.t.Fatal(err)
	}
}

const (
	database5Gi = "resources:\n  - name: main\n    type: postgres\n    storage: 5Gi\n"
	noDatabase  = ""
)

var planNameRE = regexp.MustCompile(`resourcePlans/[0-9a-f]{12}`)

// scrub replaces plan names, which are random, with a fixed one.
func scrub(s string) string { return planNameRE.ReplaceAllString(s, "resourcePlans/PLAN") }

func TestResourceLifecycle(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	runner := h.serveResources()
	h.ok("service", "register", "payments", "--team", "billing")

	if r := h.fails(2, "resource", "plan", "--env", "staging"); !strings.Contains(r.err, "no forge.yaml here") {
		t.Errorf("no manifest: %v", r)
	}
	h.manifestWith(database5Gi)
	h.fails(2, "resource", "plan")                // --env is required
	h.fails(2, "resource", "plan", "--env", "qa") // the API refuses it

	if r := h.ok("resource", "list"); r.out != "" || !strings.Contains(r.err, "No resources.") {
		t.Errorf("empty list: %v", r)
	}
	if r := h.ok("resource", "list", "-o", "json"); !strings.Contains(r.out, `"resources": []`) {
		t.Errorf("empty list as JSON: %s", r.out)
	}

	r := h.ok("resource", "plan", "--env", "staging")
	golden(t, "resource_plan_table", scrub(r.out))
	for _, step := range []string{"launch", "init", "plan", "policy"} {
		if !strings.Contains(r.err, "  "+step) {
			t.Errorf("no %s event:\n%s", step, r.err)
		}
	}
	plan := planNameRE.FindString(r.out)
	if plan == "" {
		t.Fatalf("no plan name in:\n%s", r.out)
	}
	if len(runner.Databases()) != 0 {
		t.Errorf("a plan made a database: %v", runner.Databases())
	}

	if r := h.ok("resource", "apply", plan, "--dry-run"); scrub(r.out) != "Would apply resourcePlans/PLAN to payments (staging)\n" {
		t.Errorf("dry run: %q", r.out)
	}
	// Without a terminal and without --yes, nothing is applied.
	if r := h.fails(2, "resource", "apply", plan); !strings.Contains(r.err, "--yes") {
		t.Errorf("no confirmation: %v", r)
	}
	h.fails(2, "resource", "apply")                              // a plan or --env
	h.fails(2, "resource", "apply", plan, "--env", "staging")    // not both
	h.fails(2, "resource", "apply", "nope", "--yes")             // not a plan name
	h.fails(3, "resource", "apply", "0123456789ab", "--yes")     // no such plan
	h.fails(3, "resource", "apply", "--env", "production", "-y") // no plan of mine there

	r = h.ok("resource", "apply", "--env", "staging", "--yes")
	if scrub(r.out) != "Applied resourcePlans/PLAN to payments (staging)\n" || !strings.Contains(r.err, "Deploy to give the app DATABASE_URL.") {
		t.Errorf("apply: %v", r)
	}
	if got := runner.Databases(); !slices.Equal(got, []string{"payments-staging/main 5Gi x1"}) {
		t.Errorf("databases: %v", got)
	}

	golden(t, "resource_list_table", h.ok("resource", "list").out)
	if r := h.ok("resource", "list", "payments", "--env", "staging", "-o", "name"); r.out != "resources/payments/staging/main\n" {
		t.Errorf("list -o name: %q", r.out)
	}
	if r := h.ok("resource", "list", "--env", "production"); !strings.Contains(r.err, "No resources.") {
		t.Errorf("production: %v", r)
	}
	r = h.ok("resource", "get", "payments/staging/main")
	for _, want := range []string{"resources/payments/staging/main", "ready", "main-app", "5Gi", plan, "unknown"} {
		if !strings.Contains(r.out, want) {
			t.Errorf("get lacks %q:\n%s", want, r.out)
		}
	}
	if r := h.ok("resource", "get", "resources/payments/staging/main", "-o", "json"); !strings.Contains(r.out, `"state": "STATE_READY"`) {
		t.Errorf("get as JSON: %s", r.out)
	}
	h.fails(3, "resource", "get", "payments/staging/nope")
	h.fails(2, "resource", "get", "nope")

	// A plan is used once (exit 6), and planning again finds nothing to do.
	if r := h.fails(6, "resource", "apply", plan, "--yes"); !strings.Contains(r.err, "was already applied") {
		t.Errorf("a second apply: %v", r)
	}
	r = h.ok("resource", "plan", "--env", "staging")
	if !strings.Contains(r.out, "  No changes\n") || strings.Contains(r.out, "Apply it with") {
		t.Errorf("an unchanged plan:\n%s", r.out)
	}
	if r := h.fails(6, "resource", "apply", "--env", "staging", "--yes"); !strings.Contains(r.err, "nothing to apply") {
		t.Errorf("applying no changes: %v", r)
	}

	// forge.yaml changed after the plan: apply refuses.
	h.manifestWith(strings.Replace(database5Gi, "5Gi", "10Gi", 1))
	r = h.ok("resource", "plan", "--env", "staging", "-o", "json")
	if !strings.Contains(r.out, `"state": "STATE_PLANNED"`) || !strings.Contains(r.out, `"action": "update"`) {
		t.Errorf("plan as JSON: %s", r.out)
	}
	grow := planNameRE.FindString(r.out)
	h.manifestWith(strings.Replace(database5Gi, "5Gi", "12Gi", 1))
	if r := h.fails(6, "resource", "apply", grow, "--yes"); !strings.Contains(r.err, "forge.yaml changed since") {
		t.Errorf("a changed manifest: %v", r)
	}
	h.manifestWith(strings.Replace(database5Gi, "5Gi", "10Gi", 1))
	r = h.ok("resource", "apply", grow, "--yes", "--wait=false")
	if !strings.Contains(r.err, "forge op watch") {
		t.Errorf("--wait=false: %v", r)
	}
}

func TestResourcePlanDenied(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.serveResources()
	h.ok("service", "register", "payments", "--team", "billing")
	h.manifestWith(strings.Replace(database5Gi, "5Gi", "200Gi", 1))
	r := h.fails(5, "resource", "plan", "--env", "production")
	golden(t, "resource_plan_denied", scrub(r.out))
	if !strings.Contains(r.err, "was denied by policy") {
		t.Errorf("stderr: %s", r.err)
	}
	plan := planNameRE.FindString(r.out)
	if r := h.fails(6, "resource", "apply", plan, "--yes"); !strings.Contains(r.err, "was denied by policy") {
		t.Errorf("applying a denied plan: %v", r)
	}
	// A warning is shown and does not fail the command.
	h.manifestWith(database5Gi + "    instances: 2\n")
	if r := h.ok("resource", "plan", "--env", "staging"); !strings.Contains(r.out, "  ! staging has 2 instances; 1 is usually enough\nPolicy: passed\n") {
		t.Errorf("a warning:\n%s", r.out)
	}
}

func TestResourceDestroy(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	runner := h.serveResources()
	h.ok("service", "register", "payments", "--team", "billing")
	h.manifestWith(database5Gi)
	h.ok("resource", "plan", "--env", "staging")
	h.ok("resource", "apply", "--env", "staging", "--yes")

	// The database left forge.yaml: refused unless asked for.
	h.manifestWith(noDatabase)
	if r := h.fails(5, "resource", "plan", "--env", "staging"); !strings.Contains(r.out, "✗ the plan deletes database main; plan again with --allow-destroy to remove it") {
		t.Errorf("a delete without --allow-destroy:\n%s", r.out)
	}
	r := h.ok("resource", "plan", "--env", "staging", "--allow-destroy")
	if !strings.Contains(r.out, "  - postgres main   storage 5Gi, instances 1\n") {
		t.Errorf("destroy plan:\n%s", r.out)
	}

	// At a terminal, apply asks for the resource's name.
	h.tty = true
	h.stdin = "nope\n"
	if r := h.fails(2, "resource", "apply", "--env", "staging"); !strings.Contains(r.err, "Type its name to confirm") || !strings.Contains(r.err, "did not match") {
		t.Errorf("a wrong name: %v", r)
	}
	if len(runner.Databases()) != 1 {
		t.Fatal("the database went without confirmation")
	}
	h.stdin = "main\n"
	r = h.ok("resource", "apply", "--env", "staging")
	if !strings.Contains(r.err, "The next deploy drops DATABASE_URL.") {
		t.Errorf("destroy: %v", r)
	}
	if len(runner.Databases()) != 0 {
		t.Errorf("databases: %v", runner.Databases())
	}
	if r := h.ok("resource", "list"); !strings.Contains(r.err, "No resources.") {
		t.Errorf("after the destroy: %v", r)
	}
}

func TestResourceApplyAsksYesOrNo(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	runner := h.serveResources()
	h.ok("service", "register", "payments", "--team", "billing")
	h.manifestWith(database5Gi)
	h.ok("resource", "plan", "--env", "staging")
	h.tty = true
	h.stdin = "n\n"
	r := h.fails(2, "resource", "apply", "--env", "staging")
	// The plan is shown before the question.
	if !strings.Contains(r.err, "+ postgres main") || !strings.Contains(r.err, "Apply this plan? [y/N]") || len(runner.Databases()) != 0 {
		t.Errorf("answering no: %v", r)
	}
	h.stdin = "y\n"
	h.ok("resource", "apply", "--env", "staging")
	if len(runner.Databases()) != 1 {
		t.Errorf("answering yes: %v", runner.Databases())
	}
}

func TestResourceAccessAndOff(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.ok("service", "register", "payments", "--team", "billing")
	h.manifestWith(database5Gi)
	// Resources are off on this API: exit 6 with the reason.
	if r := h.fails(6, "resource", "list"); !strings.Contains(r.err, "resources are not configured on this Forge API") {
		t.Errorf("resources off: %v", r)
	}
	h.fails(6, "resource", "plan", "--env", "staging")

	h.serveResources()
	h.env["FORGE_TOKEN"] = readerToken
	if r := h.fails(5, "resource", "plan", "--env", "staging"); !strings.Contains(r.err, "services.write") {
		t.Errorf("a reader plans: %v", r)
	}
	h.ok("resource", "list")

	// -f reads another manifest.
	h.env["FORGE_TOKEN"] = writerToken
	other := filepath.Join(t.TempDir(), "other.yaml")
	if err := os.WriteFile(other, []byte("apiVersion: forge.dev/v1\nservice:\n  name: payments\n  team: billing\n"+strings.Replace(database5Gi, "5Gi", "7Gi", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := h.ok("resource", "plan", "--env", "staging", "-f", other); !strings.Contains(r.out, "storage 7Gi") {
		t.Errorf("-f: %s", r.out)
	}
	h.fails(2, "resource", "plan", "--env", "staging", "-f", filepath.Join(t.TempDir(), "missing.yaml"))
	// A manifest the CLI itself can refuse never reaches the API.
	h.manifestWith(strings.Replace(database5Gi, "5Gi", "5GB", 1))
	if r := h.fails(2, "resource", "plan", "--env", "staging"); !strings.Contains(r.err, "resources: main: storage") {
		t.Errorf("a bad manifest: %v", r)
	}
}
```

Run: `go test ./internal/cli -run TestResource`
Expected: FAIL (`unknown command "resource"`, exit 2 where 0 was wanted).

- [ ] **Step 3: Add the yes/no prompt**

Append to `internal/cli/prompt.go`:

```go
// confirm asks a yes/no question before a change that is not destructive.
// --yes skips it. Without a terminal (or with --no-input) it fails with exit
// code 2 rather than hanging.
func (f *Factory) confirm(question string, yes bool) error {
	if yes {
		return nil
	}
	if !f.interactive() {
		return exitcode.Usagef("refusing to continue without confirmation").
			WithHint("pass --yes to confirm, or --dry-run to preview")
	}
	fmt.Fprintf(f.IO.ErrOut, "%s [y/N]: ", question)
	answer, err := f.readLine()
	if err != nil {
		return exitcode.Usagef("no confirmation received")
	}
	if a := strings.ToLower(answer); a != "y" && a != "yes" {
		return exitcode.Usagef("not confirmed; nothing was changed")
	}
	return nil
}
```

- [ ] **Step 4: Implement the commands**

Create `internal/cli/resource.go`:

```go
package cli

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/exitcode"
	"github.com/showmino/forge/internal/manifest"
	"github.com/showmino/forge/internal/output"
	"github.com/showmino/forge/pkg/client"
)

func newResourceCmd(f *Factory) *cobra.Command {
	cmd := group("resource", "Plan and apply the resources a service declares", "resources")
	cmd.Long = `A service declares what it needs besides its own pods under 'resources' in
forge.yaml: today, one Postgres database. 'forge resource plan' shows what
would change in one environment and checks it against policy; 'forge resource
apply' then makes exactly that change. A plan is used once and expires after
24 hours. Deploy after an apply to give the app DATABASE_URL.`
	cmd.AddCommand(newResourcePlanCmd(f), newResourceApplyCmd(f), newResourceListCmd(f), newResourceGetCmd(f))
	return cmd
}

// loadManifest reads file, or the forge.yaml of the working directory when
// file is empty; nil when there is none there.
func (f *Factory) loadManifest(file string) (*manifest.Manifest, error) {
	if file != "" {
		m, err := manifest.Load(file)
		if err != nil {
			return nil, exitcode.Usagef("%v", err)
		}
		return m, nil
	}
	m, _, err := f.Manifest()
	if err != nil {
		return nil, exitcode.Usagef("forge.yaml: %v", err)
	}
	return m, nil
}

var planIDRE = regexp.MustCompile(`^[0-9a-f]{12}$`)

// planName accepts "<id>" or "resourcePlans/<id>".
func planName(s string) (string, error) {
	id := strings.TrimPrefix(s, "resourcePlans/")
	if !planIDRE.MatchString(id) {
		return "", exitcode.Usagef("%q is not a plan name (resourcePlans/<id>)", s).WithHint("see the output of 'forge resource plan'")
	}
	return "resourcePlans/" + id, nil
}

var changeSymbols = map[string]string{"create": "+", "update": "~", "delete": "-", "replace": "±"}

// settingsText reads a change's settings: "storage 5Gi → 10Gi, instances 1".
func settingsText(before, after *forgev1.ResourceSettings) string {
	one := func(s *forgev1.ResourceSettings) string {
		return fmt.Sprintf("storage %dGi, instances %d", s.GetStorageGi(), s.GetInstances())
	}
	switch {
	case before == nil && after == nil:
		return ""
	case before == nil:
		return one(after)
	case after == nil:
		return one(before)
	}
	storage := fmt.Sprintf("storage %dGi", after.GetStorageGi())
	if before.GetStorageGi() != after.GetStorageGi() {
		storage = fmt.Sprintf("storage %dGi → %dGi", before.GetStorageGi(), after.GetStorageGi())
	}
	instances := fmt.Sprintf("instances %d", after.GetInstances())
	if before.GetInstances() != after.GetInstances() {
		instances = fmt.Sprintf("instances %d → %d", before.GetInstances(), after.GetInstances())
	}
	return storage + ", " + instances
}

// planText is a plan as people read it.
func planText(p *forgev1.ResourcePlan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Plan %s for %s (%s)", p.GetName(), strings.TrimPrefix(p.GetService(), "services/"), p.GetEnv())
	switch p.GetState() {
	case forgev1.ResourcePlan_STATE_PLANNED:
	case forgev1.ResourcePlan_STATE_DENIED:
		b.WriteString(": denied by policy")
	default:
		b.WriteString(": " + strings.ReplaceAll(lowerEnum(p.GetState(), "STATE_"), "_", " "))
	}
	b.WriteString("\n")
	for _, c := range p.GetChanges() {
		symbol := changeSymbols[c.GetAction()]
		if symbol == "" {
			symbol = "?"
		}
		fmt.Fprintf(&b, "  %s %s %s   %s\n", symbol, c.GetType(), c.GetResource(), settingsText(c.GetBefore(), c.GetAfter()))
	}
	pol := p.GetPolicy()
	if pol != nil && len(p.GetChanges()) == 0 {
		b.WriteString("  No changes\n")
	}
	if p.GetMessage() != "" {
		fmt.Fprintf(&b, "  %s\n", p.GetMessage())
	}
	for _, m := range pol.GetDeny() {
		fmt.Fprintf(&b, "  ✗ %s\n", m)
	}
	for _, m := range pol.GetWarn() {
		fmt.Fprintf(&b, "  ! %s\n", m)
	}
	if pol != nil && !pol.GetDenied() {
		b.WriteString("Policy: passed\n")
	}
	if p.GetState() == forgev1.ResourcePlan_STATE_PLANNED && len(p.GetChanges()) > 0 {
		fmt.Fprintf(&b, "Apply it with: forge resource apply %s\n", p.GetName())
	}
	return b.String()
}

func (f *Factory) printResourcePlan(format output.Format, p *forgev1.ResourcePlan) error {
	if format != output.Table {
		return output.Print(f.IO.Out, format, output.View{Names: []string{p.GetName()}, Data: p})
	}
	_, err := fmt.Fprint(f.IO.Out, planText(p))
	return err
}

func newResourcePlanCmd(f *Factory) *cobra.Command {
	var env, file string
	var allowDestroy, wait bool
	cmd := &cobra.Command{
		Use:   "plan --env staging|production",
		Short: "Show what forge.yaml's resources would change, and check it against policy",
		Long: `Plan the resources in forge.yaml for one environment. Nothing changes: the
plan is saved, checked against policy, and printed with its name. Exits 5
when policy denies it. A plan that deletes a database is denied unless it is
made with --allow-destroy.`,
		Example: `  forge resource plan --env staging
  forge resource plan --env production -f ./forge.yaml
  forge resource plan --env staging --allow-destroy`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if env == "" {
				return exitcode.Usagef("--env is required (staging or production)")
			}
			m, err := f.loadManifest(file)
			if err != nil {
				return err
			}
			if m == nil {
				return exitcode.Usagef("no forge.yaml here").WithHint("run inside a service repo, or pass -f <file>")
			}
			format, err := f.Format()
			if err != nil {
				return err
			}
			ctx, cancel := f.withTimeout(cmd.Context())
			defer cancel()
			c, err := f.StreamClient(ctx)
			if err != nil {
				return err
			}
			resp, err := c.Resources.PlanResources(ctx, connect.NewRequest(&forgev1.PlanResourcesRequest{
				Service: "services/" + m.Service.Name, Env: env, Resources: m.Declarations(),
				AllowDestroy: allowDestroy, RequestId: client.NewRequestID(),
			}))
			if err != nil {
				return err
			}
			if !wait {
				return f.printStarted(format, resp.Msg.GetOperation())
			}
			op, err := f.watchOperation(ctx, c, resp.Msg.GetOperation().GetName())
			if err != nil {
				return err
			}
			got, err := c.Resources.GetResourcePlan(ctx, connect.NewRequest(&forgev1.GetResourcePlanRequest{Name: resp.Msg.GetPlan().GetName()}))
			if err != nil {
				return err
			}
			p := got.Msg.GetPlan()
			if err := f.printResourcePlan(format, p); err != nil {
				return err
			}
			if p.GetState() == forgev1.ResourcePlan_STATE_DENIED {
				return exitcode.New(exitcode.PermissionDenied, "%s was denied by policy", p.GetName())
			}
			if err := opFailure(op); err != nil {
				return err
			}
			if len(m.Resources) > 0 {
				f.previewNote(ctx, c, m.Service.Name)
			}
			return nil
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&env, "env", "", "staging or production")
	fl.StringVarP(&file, "file", "f", "", "manifest to read (default: forge.yaml here or in a parent directory)")
	fl.BoolVar(&allowDestroy, "allow-destroy", false, "let the plan delete resources")
	fl.BoolVar(&wait, "wait", true, "watch the plan until it is made")
	return cmd
}

// previewNote says, when the service has previews, that they get no database.
func (f *Factory) previewNote(ctx context.Context, c *client.Client, svc string) {
	resp, err := c.Environments.ListEnvironments(ctx, connect.NewRequest(&forgev1.ListEnvironmentsRequest{Service: "services/" + svc}))
	if err == nil && len(resp.Msg.GetEnvironments()) > 0 {
		f.infof("Previews of %s get no database and no DATABASE_URL.", svc)
	}
}

// confirmApply asks before an apply: the name of every resource the plan
// deletes or replaces, or yes/no when it deletes nothing.
func (f *Factory) confirmApply(p *forgev1.ResourcePlan, yes bool) error {
	svc := strings.TrimPrefix(p.GetService(), "services/")
	destructive := false
	for _, c := range p.GetChanges() {
		if c.GetAction() != "delete" && c.GetAction() != "replace" {
			continue
		}
		destructive = true
		if err := f.confirmDestructive("delete", "resources/"+svc+"/"+p.GetEnv()+"/"+c.GetResource(), yes); err != nil {
			return err
		}
	}
	if destructive {
		return nil
	}
	return f.confirm("Apply this plan?", yes)
}

func newResourceApplyCmd(f *Factory) *cobra.Command {
	var env, file string
	var yes, dryRun, wait bool
	cmd := &cobra.Command{
		Use:   "apply [plan] [--env staging|production]",
		Short: "Apply a saved plan",
		Long: `Apply exactly the plan 'forge resource plan' saved. Give its name, or --env
to apply your own newest plan that is ready for that environment. The plan is
shown and confirmed first; a plan that deletes a database asks for the
resource's name. Apply refuses a plan that was used, expired, or made before
forge.yaml or the resources changed (exit 6), and one policy now denies
(exit 5).`,
		Example: `  forge resource apply --env staging
  forge resource apply resourcePlans/7f3a91c2b04e --yes
  forge resource apply --env production --dry-run`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			switch {
			case len(args) == 0 && env == "":
				return exitcode.Usagef("give a plan name, or --env to apply your newest plan")
			case len(args) == 1 && env != "":
				return exitcode.Usagef("give a plan name or --env, not both")
			}
			format, err := f.Format()
			if err != nil {
				return err
			}
			// A manifest is optional here: with one, the API also checks that
			// forge.yaml did not change since the plan.
			m, err := f.loadManifest(file)
			if err != nil {
				return err
			}
			ctx, cancel := f.withTimeout(cmd.Context())
			defer cancel()
			c, err := f.StreamClient(ctx)
			if err != nil {
				return err
			}
			var name string
			if len(args) == 1 {
				if name, err = planName(args[0]); err != nil {
					return err
				}
			} else {
				if m == nil {
					return exitcode.Usagef("--env needs a forge.yaml to know the service").WithHint("run inside a service repo, pass -f <file>, or give a plan name")
				}
				resp, err := c.Resources.ListResourcePlans(ctx, connect.NewRequest(&forgev1.ListResourcePlansRequest{
					Service: "services/" + m.Service.Name, Env: env, Mine: true, PageSize: 1,
					States: []forgev1.ResourcePlan_State{forgev1.ResourcePlan_STATE_PLANNED},
				}))
				if err != nil {
					return err
				}
				if len(resp.Msg.GetPlans()) == 0 {
					return exitcode.New(exitcode.NotFound, "you have no plan ready to apply for %s (%s)", m.Service.Name, env).
						WithHint("run 'forge resource plan --env " + env + "' first")
				}
				name = resp.Msg.GetPlans()[0].GetName()
			}
			got, err := c.Resources.GetResourcePlan(ctx, connect.NewRequest(&forgev1.GetResourcePlanRequest{Name: name}))
			if err != nil {
				return err
			}
			p := got.Msg.GetPlan()
			svc := strings.TrimPrefix(p.GetService(), "services/")
			req := &forgev1.ApplyResourcePlanRequest{Name: name, ValidateOnly: dryRun}
			if m != nil && m.Service.Name == svc {
				req.Resources, req.CheckResources = m.Declarations(), true
			}
			if dryRun {
				resp, err := c.Resources.ApplyResourcePlan(ctx, connect.NewRequest(req))
				if err != nil {
					return err
				}
				if format != output.Table {
					return f.printResourcePlan(format, resp.Msg.GetPlan())
				}
				_, err = fmt.Fprintf(f.IO.Out, "Would apply %s to %s (%s)\n", name, svc, p.GetEnv())
				return err
			}
			if !yes {
				fmt.Fprint(f.IO.ErrOut, planText(p))
			}
			if err := f.confirmApply(p, yes); err != nil {
				return err
			}
			resp, err := c.Resources.ApplyResourcePlan(ctx, connect.NewRequest(req))
			if err != nil {
				return err
			}
			if !wait {
				return f.printStarted(format, resp.Msg.GetOperation())
			}
			op, err := f.watchOperation(ctx, c, resp.Msg.GetOperation().GetName())
			if err != nil {
				return err
			}
			if err := opFailure(op); err != nil {
				return exitcode.From(err).WithHint(fmt.Sprintf("see 'forge resource list %s --env %s'; plan and apply again to repair it", svc, p.GetEnv()))
			}
			if format != output.Table {
				done, err := c.Resources.GetResourcePlan(ctx, connect.NewRequest(&forgev1.GetResourcePlanRequest{Name: name}))
				if err != nil {
					return err
				}
				return f.printResourcePlan(format, done.Msg.GetPlan())
			}
			_, err = fmt.Fprintf(f.IO.Out, "Applied %s to %s (%s)\n", name, svc, p.GetEnv())
			return err
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&env, "env", "", "apply your newest ready plan for this environment")
	fl.StringVarP(&file, "file", "f", "", "manifest to check the plan against (default: forge.yaml here or in a parent directory)")
	fl.BoolVarP(&yes, "yes", "y", false, "skip the confirmation prompt")
	fl.BoolVar(&dryRun, "dry-run", false, "run every check without applying")
	fl.BoolVar(&wait, "wait", true, "watch the apply until it ends")
	return cmd
}

// healthText reads a resource's live health: "Cluster in healthy state (2/2 ready)".
func healthText(h *forgev1.ResourceHealth) string {
	if h == nil || h.GetPhase() == "" || h.GetPhase() == "unknown" {
		return "unknown"
	}
	return fmt.Sprintf("%s (%d/%d ready)", h.GetPhase(), h.GetReadyInstances(), h.GetInstances())
}

func newResourceListCmd(f *Factory) *cobra.Command {
	var env string
	cmd := &cobra.Command{
		Use:     "list [service] [--env staging|production]",
		Aliases: []string{"ls"},
		Short:   "List resources with their state and live health",
		Long: `List resources. Without a service name, inside a service repo it shows that
service, and elsewhere every service.`,
		Example: `  forge resource list
  forge resource list payments --env production -o json`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc := ""
			if len(args) == 1 {
				svc = strings.TrimPrefix(args[0], "services/")
			} else {
				m, _, err := f.Manifest()
				if err != nil {
					return err
				}
				if m != nil {
					svc = m.Service.Name
				}
			}
			format, err := f.Format()
			if err != nil {
				return err
			}
			ctx, cancel := f.withTimeout(cmd.Context())
			defer cancel()
			c, err := f.Client(ctx, true)
			if err != nil {
				return err
			}
			req := &forgev1.ListResourcesRequest{Env: env}
			if svc != "" {
				req.Service = "services/" + svc
			}
			resp, err := c.Resources.ListResources(ctx, connect.NewRequest(req))
			if err != nil {
				return err
			}
			rs := resp.Msg.GetResources()
			if len(rs) == 0 && format == output.Table {
				f.infof("No resources.")
				return nil
			}
			v := output.View{Headers: []string{"service", "env", "name", "type", "storage", "instances", "state", "health"}, Data: resp.Msg}
			if len(rs) == 0 {
				v.Data = map[string]any{"resources": []any{}} // keep '.resources[]' working in jq
			}
			for _, r := range rs {
				s := r.GetSettings()
				v.Rows = append(v.Rows, []string{
					strings.TrimPrefix(r.GetService(), "services/"), r.GetEnv(), s.GetName(), r.GetType(),
					fmt.Sprintf("%dGi", s.GetStorageGi()), fmt.Sprint(s.GetInstances()),
					lowerEnum(r.GetState(), "STATE_"), healthText(r.GetHealth()),
				})
				v.Names = append(v.Names, r.GetName())
			}
			return output.Print(f.IO.Out, format, v)
		},
	}
	cmd.Flags().StringVar(&env, "env", "", "only this environment")
	return cmd
}

func newResourceGetCmd(f *Factory) *cobra.Command {
	return &cobra.Command{
		Use:     "get <service>/<env>/<name>",
		Short:   "Show one resource",
		Example: "  forge resource get payments/staging/main\n  forge resource get resources/payments/production/main -o json",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := f.Format()
			if err != nil {
				return err
			}
			short := strings.TrimPrefix(args[0], "resources/")
			if strings.Count(short, "/") != 2 {
				return exitcode.Usagef("%q is not a resource name (<service>/<env>/<name>)", args[0]).WithHint("see 'forge resource list'")
			}
			ctx, cancel := f.withTimeout(cmd.Context())
			defer cancel()
			c, err := f.Client(ctx, true)
			if err != nil {
				return err
			}
			resp, err := c.Resources.GetResource(ctx, connect.NewRequest(&forgev1.GetResourceRequest{Name: "resources/" + short}))
			if err != nil {
				return err
			}
			r := resp.Msg.GetResource()
			if format != output.Table {
				return output.Print(f.IO.Out, format, output.View{Names: []string{r.GetName()}, Data: r})
			}
			s := r.GetSettings()
			rows := [][]string{
				{"name", r.GetName()},
				{"service", r.GetService()},
				{"env", r.GetEnv()},
				{"type", r.GetType()},
				{"storage", fmt.Sprintf("%dGi", s.GetStorageGi())},
				{"instances", fmt.Sprint(s.GetInstances())},
				{"state", lowerEnum(r.GetState(), "STATE_")},
				{"health", healthText(r.GetHealth())},
				{"secret", r.GetSecret()},
				{"last plan", r.GetLastPlan()},
			}
			if r.GetUpdateTime() != nil {
				rows = append(rows, []string{"updated", r.GetUpdateTime().AsTime().UTC().Format(time.RFC3339) + " (" + age(f.Now().Sub(r.GetUpdateTime().AsTime())) + " ago)"})
			}
			return output.Print(f.IO.Out, format, output.View{Headers: []string{"field", "value"}, Rows: rows})
		},
	}
}
```

In `internal/cli/root.go`, add `newResourceCmd(f)` to the core command list, after `newDeployCmd(f)`:

```go
	for _, c := range []*cobra.Command{newServiceCmd(f), newTemplateCmd(f), newCreateCmd(f), newEnvCmd(f), newDeployCmd(f), newResourceCmd(f), newOpCmd(f), newAuditCmd(f)} {
```

- [ ] **Step 5: Create the golden files and read them**

Run: `go test ./internal/cli -run 'TestResourceLifecycle|TestResourcePlanDenied' -update`

Then read the three files. They must be exactly:

`internal/cli/testdata/resource_plan_table.golden`:

```
Plan resourcePlans/PLAN for payments (staging)
  + postgres main   storage 5Gi, instances 1
Policy: passed
Apply it with: forge resource apply resourcePlans/PLAN
```

`internal/cli/testdata/resource_plan_denied.golden`:

```
Plan resourcePlans/PLAN for payments (production): denied by policy
  + postgres main   storage 200Gi, instances 2
  ✗ storage 200Gi is above the 50Gi limit for production
```

`internal/cli/testdata/resource_list_table.golden` (one header row and one resource row; the columns are aligned by the table writer):

```
SERVICE    ENV       NAME   TYPE       STORAGE   INSTANCES   STATE   HEALTH
payments   staging   main   postgres   5Gi       1           ready   unknown
```

If a file differs in more than column spacing, the code is wrong, not the golden: fix the code and run the update again.

- [ ] **Step 6: Run the CLI tests and check the help**

Run: `go test -race ./internal/cli`
Expected: PASS.

Run: `make build && ./bin/forge resource --help && ./bin/forge --help | grep resource`
Expected: the help lists `apply`, `get`, `list`, `plan`, and `forge --help` shows `resource` among the core commands.

- [ ] **Step 7: Lint and commit**

Run: `make lint && make test`
Expected: clean, PASS.

```bash
git add internal/cli
git commit -m "cli: forge resource plan, apply, list and get

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 10: Docs

**Files:**
- Modify: `docs/index.md`
- Modify: `.claude/skills/forge/SKILL.md`
- Modify: `README.md` (only if it lists the CLI's commands)

**Interfaces:**
- Consumes: what Tasks 1 to 9 built.
- Produces: docs that say what exists now and what Plan 2 still owes.

- [ ] **Step 1: Update `docs/index.md`**

1. In the command table, change the `resource` row's example to `forge resource plan --env staging` (the command reads `forge.yaml` from the repo; `-f` names another file).
2. After the paragraph that starts "**The `forge.yaml` manifest**", add:

````markdown
A service declares its resources there. This version has one type, a Postgres database, at most one per service:

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

`forge resource plan --env <env>` saves a plan and checks it against policy; `forge resource apply` runs exactly that plan. See `docs/specs/2026-10-10-phase-5-m2-resources-policy-design.md`.
````

3. In the delivery section, change the sentence about Phase 5 so it ends: "milestone 2 brings resources and policy: its API, policy rules and CLI are built and tested against a fake runner (`docs/plans/2026-10-10-phase-5-m2a-resources-api-policy.md`); the runner that makes real databases, and the exit gate on k3s, are next."

- [ ] **Step 2: Update `.claude/skills/forge/SKILL.md`**

1. In the conventions table, add a row:

```markdown
| A resource run (plan or apply) ends through `Resources.endRun`, which changes the plan once and appends the one outcome event | Operations started with `withoutOutcome(ctx)`; never append a resource outcome anywhere else |
```

2. In the same table, add a row:

```markdown
| Policy rules are `policy/*.rego`, package `forge.resources`, built into the binary | `policy.New`; every rule has a passing and a failing case in `policy/policy_test.go` |
```

3. In the status section, replace "milestone 2: resources (OpenTofu jobs) and policy" with: "milestone 2, plan 1 of 3 done: `ResourceService`, the resource store (migration 0003), Rego policy (`policy/`), `forge resource plan/apply/list/get`, tested against `envtest.Runner`. Resources are OFF in `cmd/forge-api` until plan 2 (the Job launcher, runner API on a second port, `forge-runner` image, OpenTofu module, `database.secret` in the chart, k3s manifests) and plan 3 (rollout and exit gate)."

- [ ] **Step 3: Check `README.md`**

Run: `grep -n "forge audit\|forge deploy" README.md`
If the README lists commands, add `forge resource plan --env staging` and `forge resource apply --env staging` in the same style. If it does not, leave it.

- [ ] **Step 4: Check the docs against the code**

Run: `./bin/forge resource plan --help | head -5 && grep -n "resource" docs/index.md | head -20`
Expected: every flag and command the docs name exists in the help.

- [ ] **Step 5: Commit**

```bash
git add docs/index.md .claude/skills/forge/SKILL.md README.md
git commit -m "docs: resources in forge.yaml and forge resource; what the runner plan still owes

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

## After the last task

- The controller runs the whole-branch review, then opens a pull request from `phase-5-resources` to `main` and merges it (the owner's standing rule allows this for showmino/forge).
- Nothing is rolled out to k3s in this plan: resources stay off in `cmd/forge-api`. Migration `0003` only adds three empty tables the next time forge-api is rolled out.
- Plan 2 (the runner) is written next, from the same spec.
