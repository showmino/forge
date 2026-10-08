#!/usr/bin/env bash
# Exercise every forge command against a live API and check exit codes.
#
#   bash .claude/skills/forge/scripts/cli-smoke.sh bin/forge <context> > smoke.log; tail -1 smoke.log
#
# Needs a logged-in context whose Dex has the password connector: it logs out
# and back in with approve-device-login.py. It registers and deletes the
# services payments, ledger, billing-db and billing-api in that catalog, and
# runs 'forge template' and 'forge create --dry-run' (needs templates and the
# GitHub App configured on the API; nothing is created).
# Each case: t <expected-exit> <description> -- <command...>. Exits 1 on any FAIL.
set -u
[ $# -eq 2 ] || { echo "usage: $0 <forge-binary> <context>" >&2; exit 2; }
F=$(realpath "$1"); CTX=$2
HERE=$(cd "$(dirname "$0")" && pwd)
pass=0; fail=0
t() {
  local want=$1 desc=$2; shift 3
  local out rc
  out=$("$@" 2>&1 </dev/null); rc=$?
  if [ "$rc" = "$want" ]; then pass=$((pass+1)); s=PASS; else fail=$((fail+1)); s="FAIL(want $want)"; fi
  printf '\n### [%s] exit=%s  %s\n$ %s\n%s\n' "$s" "$rc" "$desc" "$*" "$(printf '%s' "$out" | head -${LINES_MAX:-12})"
}
f() { "$F" --context "$CTX" "$@"; }

echo "#### catalog CRUD"
f service delete ledger --yes >/dev/null 2>&1; f service delete billing-db --yes >/dev/null 2>&1
f service register payments --team billing --owner admin@example.com --lifecycle production >/dev/null 2>&1  # catalog is in memory
t 0 "list (table)" -- f service list
t 0 "register full" -- f service register ledger --team billing --owner ana@example.com --depends-on payments --label tier=1 --repo https://github.com/showmino/ledger --display-name Ledger --lifecycle production
t 6 "register duplicate" -- f service register ledger --team billing
t 2 "register invalid name" -- f service register Bad_Name --team billing
t 2 "register missing team" -- f service register nameless
t 2 "register bad lifecycle" -- f service register lc --team billing --lifecycle bogus
t 0 "register dry-run" -- f service register dryrun --team billing --dry-run
t 3 "dry-run did not create" -- f service get dryrun
t 6 "register unknown dependency" -- f service register billing-db --team billing --depends-on nope
t 0 "get table" -- f service get ledger
LINES_MAX=30 t 0 "get json" -- f service get ledger -o json
t 0 "get yaml" -- f service get ledger -o yaml
t 0 "get name" -- f service get ledger -o name
t 3 "get missing" -- f service get nope
t 2 "get no arg outside repo" -- f service get
t 0 "list json" -- f service list -o json
t 0 "list name" -- f service list -o name
t 0 "update owners/lifecycle/labels" -- f service update ledger --add-owner bo@example.com --remove-owner ana@example.com --lifecycle deprecated --label tier=2
t 0 "update dry-run" -- f service update ledger --team other --dry-run
t 0 "dry-run did not change team" -- f service get ledger -o yaml
t 2 "update nothing" -- f service update ledger
t 2 "update bad lifecycle" -- f service update ledger --lifecycle bogus
t 2 "update to depend on itself" -- f service update ledger --depends-on ledger
t 6 "update to unknown dependency" -- f service update ledger --depends-on nope
t 3 "update missing" -- f service update nope --team x
t 0 "owners" -- f service owners ledger
t 0 "owners json" -- f service owners ledger -o json
t 0 "deps" -- f service deps ledger
t 0 "deps of dependency" -- f service deps payments
t 2 "delete without --yes and no tty" -- f service delete ledger
t 0 "delete dry-run" -- f service delete ledger --dry-run
t 0 "still there" -- f service get ledger -o name
t 6 "delete service others depend on" -- f service delete payments --yes
t 0 "delete" -- f service delete ledger --yes
t 3 "deleted" -- f service get ledger
t 3 "delete again" -- f service delete ledger --yes

echo; echo "#### templates and create (no side effects)"
t 0 "template list" -- f template list
t 0 "template list json" -- f template list -o json
t 0 "template get go-grpc" -- f template get go-grpc
t 3 "template get missing" -- f template get nope
t 3 "template get unknown version" -- f template get go-grpc@v0.0.1
t 0 "create dry run" -- f create smoke-dry --template go-grpc --team platform --dry-run
t 6 "create name taken on GitHub" -- f create forge-templates --template go-grpc --team platform --dry-run
t 6 "create name taken in catalog" -- f create payments --template go-grpc --team platform --dry-run
t 2 "create bad parameter" -- f create smoke-dry --template go-grpc --team platform --set port=http --dry-run
t 2 "create unknown parameter" -- f create smoke-dry --template go-grpc --team platform --set nope=1 --dry-run
t 2 "create without --team (no tty)" -- f create smoke-dry --template go-grpc

echo; echo "#### output and global flags"
t 2 "bad -o" -- f service list -o xml
t 0 "quiet" -- f service list -q
t 0 "verbose" -- f service list -v
t 8 "timeout" -- f service list --timeout 1ns
t 1 "unreachable api (with token)" -- env FORGE_TOKEN=x "$F" --api-url http://127.0.0.1:9 service list
t 2 "unknown command" -- f servce list
t 2 "unknown flag" -- f service list --bogus
t 2 "extra arg" -- f service list extra

echo; echo "#### context"
t 0 "context list" -- "$F" context list
t 0 "context list json" -- "$F" context list -o json
t 0 "context current" -- "$F" context current
t 3 "context use missing" -- "$F" context use nope
t 2 "context set without api" -- "$F" context set newctx
t 2 "--context missing (bad flag value)" -- "$F" --context nope service list

echo; echo "#### auth"
t 0 "auth status" -- f auth status
t 0 "auth status json" -- f auth status -o json
t 0 "auth token" -- bash -c "\"$F\" --context $CTX auth token | cut -c1-20"
t 2 "login refused with FORGE_TOKEN" -- env FORGE_TOKEN=x "$F" --context "$CTX" auth login
t 4 "bad FORGE_TOKEN" -- env FORGE_TOKEN=nope "$F" --context "$CTX" service list
t 0 "logout" -- f auth logout
t 4 "after logout" -- f service list
t 4 "auth token after logout" -- f auth token
t 0 "login again" -- python3 -I "$HERE/approve-device-login.py" "$F" --context "$CTX"

echo; echo "#### init and forge.yaml"
D=$(mktemp -d)/billing-api; mkdir -p "$D"; cd "$D"
t 2 "init without team" -- f init
t 0 "init" -- f init --team billing --owner ana@example.com
t 0 "forge.yaml" -- cat forge.yaml
t 6 "init again" -- f init --team billing
t 0 "init --force" -- f init --team billing --lifecycle production --force
t 0 "register from forge.yaml" -- f service register
t 0 "get defaults to manifest" -- f service get
t 0 "update defaults to manifest" -- f service update --lifecycle deprecated
mkdir -p sub && cd sub
t 0 "get from subdirectory" -- f service get -o name
cd ..
t 0 "doctor in repo" -- f doctor
printf 'apiVersion: forge.dev/v1\nservice:\n  name: billing-api\n  tem: billing\n' > forge.yaml
t 2 "invalid forge.yaml" -- f service get
t 1 "doctor with invalid forge.yaml" -- f doctor
t 2 "list with invalid forge.yaml" -- f service list
cd /; f service delete billing-api --yes >/dev/null 2>&1

echo; echo "#### utilities"
t 0 "doctor" -- f doctor
t 0 "version" -- f version
t 0 "version json" -- f version -o json
t 0 "completion bash" -- bash -c "\"$F\" completion bash | head -3"
t 0 "help" -- bash -c "\"$F\" --help | head -5"

echo; echo "== $pass passed, $fail failed"
[ "$fail" -eq 0 ]
