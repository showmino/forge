#!/usr/bin/env sh
# Checks render.sh output with and without a GitHub App and previews. Run: sh deploy/k3s/render_test.sh
set -eu
dir=$(dirname "$0")
fail=0
check() { # check <description> <command...>
  desc=$1; shift
  if "$@"; then echo "ok   $desc"; else echo "FAIL $desc"; fail=1; fi
}

without=$(sh "$dir/render.sh" --lb 10.150.1.220 10.150.1.221)
with=$(FORGE_GITHUB_APP_ID=5239131 sh "$dir/render.sh" --lb 10.150.1.220 10.150.1.221)
onepw=$(FORGE_GITHUB_APP_ID=5239131 FORGE_ONEPASSWORD_ITEM=vaults/Kube/items/forge-github-app sh "$dir/render.sh" --lb 10.150.1.220 10.150.1.221)
prev=$(FORGE_GITHUB_APP_ID=5239131 FORGE_PREVIEW_IP=10.150.1.222 sh "$dir/render.sh" --lb 10.150.1.220 10.150.1.221)

# section <kind>: the lines of $prev from "kind: <kind>" to the next "---".
section() { printf '%s\n' "$prev" | awk -v k="kind: $1" '$0 == k {on=1} on && /^---/ {on=0} on'; }
has() { printf '%s\n' "$prev" | grep -qF -- "$1"; }
unchanged() { sh "$dir/render.sh" --lb 10.150.1.220 10.150.1.221 | cmp -s - "$dir/testdata/lb.golden"; }
unchanged_app() { FORGE_GITHUB_APP_ID=5239131 sh "$dir/render.sh" --lb 10.150.1.220 10.150.1.221 | cmp -s - "$dir/testdata/lb-app.golden"; }
preview_settings() {
  has 'FORGE_PREVIEWS, value: "1"' && has 'FORGE_PREVIEW_DOMAIN, value: preview.10.150.1.222.nip.io' &&
    has 'FORGE_PREVIEW_PULL_SECRET, value: forge/mino-ghcr'
}
sa_pull_secret() { section ServiceAccount | grep -qF -- '- name: mino-ghcr'; }
gateway_ip() {
  section Gateway | grep -qF 'metallb.universe.tf/loadBalancerIPs: 10.150.1.222' &&
    section Gateway | grep -qF 'hostname: "*.preview.10.150.1.222.nip.io"' &&
    section Gateway | grep -qF 'gatewayClassName: istio'
}
gateway_routes() { section Gateway | grep -qF 'from: Selector' && section Gateway | grep -qF 'app.kubernetes.io/managed-by: forge'; }
rbac_no_wildcard() { ! { section ClusterRole; section Role; } | grep -qF '*'; }
# The RBAC is pinned exactly: any added verb, resource or rule is a review.
rbac_exact() { { section ClusterRole; section Role; } | cmp -s - "$dir/testdata/previews-rbac.golden"; }

check "no App: no App settings"       sh -c "! printf '%s' \"\$1\" | grep -q -e FORGE_GITHUB_APP -e forge-github-app" _ "$without"
check "no App: no #app markers"       sh -c "! printf '%s' \"\$1\" | grep -q '#app'" _ "$without"
check "App: id"                       sh -c "printf '%s' \"\$1\" | grep -q 'FORGE_GITHUB_APP_ID, value: \"5239131\"'" _ "$with"
check "App: key file"                 sh -c "printf '%s' \"\$1\" | grep -q 'FORGE_GITHUB_APP_KEY_FILE, value: /etc/forge/github-app/github_app_private_key'" _ "$with"
check "App: org and template repo"    sh -c "printf '%s' \"\$1\" | grep -q 'FORGE_GITHUB_ORG, value: showmino' && printf '%s' \"\$1\" | grep -q 'FORGE_TEMPLATES_REPO, value: showmino/forge-templates'" _ "$with"
check "App: secret volume, read-only" sh -c "printf '%s' \"\$1\" | grep -q 'secretName: forge-github-app' && printf '%s' \"\$1\" | grep -q 'readOnly: true'" _ "$with"
check "App: key readable by the API"  sh -c "printf '%s' \"\$1\" | grep -q 'fsGroup: 65532' && printf '%s' \"\$1\" | grep -q 'defaultMode: 0440'" _ "$with"
check "App: no #app markers"          sh -c "! printf '%s' \"\$1\" | grep -q '#app'" _ "$with"
check "memory limit 256Mi"            sh -c "printf '%s' \"\$1\" | grep -A12 'name: forge-api$' | grep -q 'limits: {memory: 256Mi}' || printf '%s' \"\$1\" | grep -q 'limits: {memory: 256Mi}'" _ "$with"
check "valid YAML for kubectl"        sh -c "printf '%s' \"\$1\" | kubectl apply --dry-run=client -f - >/dev/null" _ "$with"
check "no 1Password item by default"   sh -c "! printf '%s' \"\$1\" | grep -q OnePasswordItem" _ "$with"
check "1Password item syncs the Secret" sh -c "printf '%s' \"\$1\" | grep -q 'kind: OnePasswordItem' && printf '%s' \"\$1\" | grep -q 'itemPath: vaults/Kube/items/forge-github-app' && printf '%s' \"\$1\" | grep -A3 'kind: OnePasswordItem' | grep -q 'name: forge-github-app'" _ "$onepw"
check "bad 1Password item refused"     sh -c "! FORGE_GITHUB_APP_ID=1 FORGE_ONEPASSWORD_ITEM='x; rm' sh \"$dir/render.sh\" --lb 10.150.1.220 10.150.1.221 >/dev/null 2>&1"
check "1Password item needs an App"    sh -c "! FORGE_ONEPASSWORD_ITEM=vaults/Kube/items/x sh \"$dir/render.sh\" --lb 10.150.1.220 10.150.1.221 >/dev/null 2>&1"
check "bad App id refused"            sh -c "! FORGE_GITHUB_APP_ID='5; rm' sh \"$dir/render.sh\" --lb 10.150.1.220 10.150.1.221 >/dev/null 2>&1"
check "Dex: no password login"          sh -c "! printf '%s' \"\$1\" | grep -q -e staticPasswords -e enablePasswordDB -e 'hash:'" _ "$with"
check "Dex: GitHub connector for the org" sh -c "printf '%s' \"\$1\" | grep -qF 'type: github' && printf '%s' \"\$1\" | grep -qF -- '- name: showmino' && printf '%s' \"\$1\" | grep -qF 'teamNameField: slug' && printf '%s' \"\$1\" | grep -qF 'redirectURI: http://dex.10.150.1.220.nip.io/dex/callback'" _ "$with"
check "Dex: OAuth credentials only from the Secret" sh -c "printf '%s' \"\$1\" | grep -qF 'clientSecret: \$GITHUB_CLIENT_SECRET' && printf '%s' \"\$1\" | grep -qF 'secretKeyRef: {name: dex-github, key: client_id}' && printf '%s' \"\$1\" | grep -qF 'secretKeyRef: {name: dex-github, key: client_secret}'" _ "$with"
check "writer groups default to platform-engineering" sh -c "printf '%s' \"\$1\" | grep -qF 'FORGE_WRITER_GROUPS, value: \"showmino:platform-engineering\"'" _ "$without"
check "writer groups can be set"        sh -c "FORGE_WRITER_GROUPS=showmino:a,showmino:b sh \"$dir/render.sh\" --lb 10.150.1.220 10.150.1.221 | grep -qF 'FORGE_WRITER_GROUPS, value: \"showmino:a,showmino:b\"'"
check "empty writer groups refused"     sh -c "! FORGE_WRITER_GROUPS= sh \"$dir/render.sh\" --lb 10.150.1.220 10.150.1.221 >/dev/null 2>&1"
check "bad writer groups refused"       sh -c "! FORGE_WRITER_GROUPS='a;rm' sh \"$dir/render.sh\" --lb 10.150.1.220 10.150.1.221 >/dev/null 2>&1"
check "comma-only writer groups refused" sh -c "! FORGE_WRITER_GROUPS=, sh \"$dir/render.sh\" --lb 10.150.1.220 10.150.1.221 >/dev/null 2>&1"
check "trailing-comma writer groups refused" sh -c "! FORGE_WRITER_GROUPS='showmino:a,' sh \"$dir/render.sh\" --lb 10.150.1.220 10.150.1.221 >/dev/null 2>&1"
check "Dex org follows FORGE_GITHUB_ORG" sh -c "out=\$(FORGE_GITHUB_ORG=acme sh \"$dir/render.sh\" --lb 10.150.1.220 10.150.1.221) && printf '%s' \"\$out\" | grep -qF -- '- name: acme' && printf '%s' \"\$out\" | grep -qF 'value: \"acme:platform-engineering\"'"
check "bad Dex org refused"             sh -c "! FORGE_DEX_GITHUB_ORG='a/b' sh \"$dir/render.sh\" --lb 10.150.1.220 10.150.1.221 >/dev/null 2>&1"
check "no Dex 1Password item by default" sh -c "! printf '%s' \"\$1\" | grep -A3 'kind: OnePasswordItem' | grep -qF 'name: dex-github'" _ "$onepw"
check "Dex 1Password item syncs dex-github" sh -c "FORGE_DEX_ONEPASSWORD_ITEM=vaults/Kube/items/forge-dex-github sh \"$dir/render.sh\" --lb 10.150.1.220 10.150.1.221 | grep -A3 'kind: OnePasswordItem' | grep -qF 'name: dex-github' && FORGE_DEX_ONEPASSWORD_ITEM=vaults/Kube/items/forge-dex-github sh \"$dir/render.sh\" --lb 10.150.1.220 10.150.1.221 | grep -qF 'itemPath: vaults/Kube/items/forge-dex-github'"
check "bad Dex 1Password item refused"  sh -c "! FORGE_DEX_ONEPASSWORD_ITEM='x; rm' sh \"$dir/render.sh\" --lb 10.150.1.220 10.150.1.221 >/dev/null 2>&1"
check "no previews: output unchanged"       unchanged
check "no previews, App: output unchanged"  unchanged_app
check "previews: forge-api settings"        preview_settings
check "previews: forge-api service account" has 'serviceAccountName: forge-api'
check "previews: SA has the pull secret"    sa_pull_secret
check "previews: gateway on the IP"         gateway_ip
check "previews: routes only from forge namespaces" gateway_routes
check "previews: no wildcard in RBAC"       rbac_no_wildcard
check "previews: RBAC exactly as reviewed (no secret read cluster-wide; Role only gets the pull secret)" rbac_exact
check "previews: no #previews markers"      sh -c "! printf '%s' \"\$1\" | grep -qF '#previews'" _ "$prev"
check "previews: valid YAML for kubectl"    sh -c "printf '%s' \"\$1\" | kubectl apply --dry-run=client -f - >/dev/null" _ "$prev"
check "previews need an App"                sh -c "! FORGE_PREVIEW_IP=10.150.1.222 sh \"$dir/render.sh\" --lb 10.150.1.220 10.150.1.221 >/dev/null 2>&1"
check "bad preview IP refused"              sh -c "! FORGE_GITHUB_APP_ID=1 FORGE_PREVIEW_IP='1.2.3.4;x' sh \"$dir/render.sh\" --lb 10.150.1.220 10.150.1.221 >/dev/null 2>&1"
check "bad pull secret refused"             sh -c "! FORGE_GITHUB_APP_ID=1 FORGE_PREVIEW_IP=10.150.1.222 FORGE_PREVIEW_PULL_SECRET='A_b' sh \"$dir/render.sh\" --lb 10.150.1.220 10.150.1.221 >/dev/null 2>&1"
rev=0123456789abcdef0123456789abcdef01234567
dep=$(FORGE_DEPLOYS=1 FORGE_DEPLOY_CHART_REVISION=$rev FORGE_GITHUB_APP_ID=5239131 FORGE_PREVIEW_IP=10.150.1.222 sh "$dir/render.sh" --lb 10.150.1.220 10.150.1.221)
# deploys_section: everything deploys.yaml.tmpl adds (from its first line to the end).
deploys_section() { printf '%s\n' "$dep" | awk '/^# Deploys:/ {on=1} on'; }
gateway_listeners() {
  printf '%s\n' "$dep" | awk '$0 == "kind: Gateway" {on=1} on' | grep -qF 'name: staging' &&
    printf '%s\n' "$dep" | grep -qF 'hostname: "*.staging.10.150.1.222.nip.io"' &&
    printf '%s\n' "$dep" | grep -qF 'hostname: "*.production.10.150.1.222.nip.io"'
}
# gateway_of <render>: the Gateway section of a render.
gateway_of() { printf '%s\n' "$1" | awk '$0 == "kind: Gateway" {on=1} on && /^---/ {on=0} on'; }
listener_selectors() {
  gateway_of "$dep" | grep -qF 'forge.dev/deploy-env: staging' &&
    gateway_of "$dep" | grep -qF 'forge.dev/deploy-env: production'
}
no_listener_selectors() { ! gateway_of "$prev" | grep -qF 'forge.dev/deploy-env'; }
deploys_exact() { deploys_section | cmp -s - "$dir/testdata/deploys.golden"; }
# previews.golden is the previews render without deploys.
prev_unchanged() { printf '%s\n' "$prev" | cmp -s - "$dir/testdata/previews.golden"; }
deny_destination() { printf '%s\n' "$dep" | grep -qF 'namespace: "!docs-dev-staging"}'; }

check "deploys: forge-api setting"          sh -c "printf '%s' \"\$1\" | grep -qF 'FORGE_DEPLOYS, value: \"1\"'" _ "$dep"
check "deploys: no FORGE_DEPLOYS without it" sh -c "! printf '%s' \"\$1\" | grep -qF FORGE_DEPLOYS" _ "$prev"
check "deploys: gateway listeners"          gateway_listeners
check "deploys: listeners select the staging and production namespaces" listener_selectors
check "deploys: no env selectors without it" no_listener_selectors
check "deploys: no listeners without it"    sh -c "! printf '%s' \"\$1\" | grep -qF '.staging.'" _ "$prev"
check "deploys: Argo objects and RBAC exactly as reviewed" deploys_exact
check "deploys: deny destination for docs-dev-staging" deny_destination
check "deploys: chart pinned to the revision" sh -c "printf '%s' \"\$1\" | grep -qF 'targetRevision: $rev'" _ "$dep"
check "deploys: no #deploys markers"        sh -c "! printf '%s' \"\$1\" | grep -qF '#deploys'" _ "$dep"
check "deploys: valid YAML for kubectl"     sh -c "printf '%s' \"\$1\" | kubectl apply --dry-run=client -f - >/dev/null" _ "$dep"
check "previews output unchanged by deploys support" prev_unchanged
check "deploys need previews"               sh -c "! FORGE_DEPLOYS=1 FORGE_DEPLOY_CHART_REVISION=$rev FORGE_GITHUB_APP_ID=1 sh \"$dir/render.sh\" --lb 10.150.1.220 10.150.1.221 >/dev/null 2>&1"
check "deploys need a pinned chart revision" sh -c "! FORGE_DEPLOYS=1 FORGE_GITHUB_APP_ID=1 FORGE_PREVIEW_IP=10.150.1.222 sh \"$dir/render.sh\" --lb 10.150.1.220 10.150.1.221 >/dev/null 2>&1"
check "deploys refuse a branch as revision" sh -c "! FORGE_DEPLOYS=1 FORGE_DEPLOY_CHART_REVISION=main FORGE_GITHUB_APP_ID=1 FORGE_PREVIEW_IP=10.150.1.222 sh \"$dir/render.sh\" --lb 10.150.1.220 10.150.1.221 >/dev/null 2>&1"
check "bad FORGE_DEPLOYS refused"           sh -c "! FORGE_DEPLOYS=yes FORGE_GITHUB_APP_ID=1 FORGE_PREVIEW_IP=10.150.1.222 sh \"$dir/render.sh\" --lb 10.150.1.220 10.150.1.221 >/dev/null 2>&1"
exit $fail
