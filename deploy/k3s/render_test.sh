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
clusterrole_no_secret_read() { section ClusterRole | grep -A1 -F 'secrets' | grep -qxF '    verbs: [create]'; }
role_pull_secret_only() { section Role | grep -qF 'resourceNames: [mino-ghcr]'; }

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
check "no previews: output unchanged"       unchanged
check "no previews, App: output unchanged"  unchanged_app
check "previews: forge-api settings"        preview_settings
check "previews: forge-api service account" has 'serviceAccountName: forge-api'
check "previews: SA has the pull secret"    sa_pull_secret
check "previews: gateway on the IP"         gateway_ip
check "previews: routes only from forge namespaces" gateway_routes
check "previews: no wildcard in RBAC"       rbac_no_wildcard
check "previews: no secret read in the ClusterRole" clusterrole_no_secret_read
check "previews: Role limited to the pull secret" role_pull_secret_only
check "previews: no #previews markers"      sh -c "! printf '%s' \"\$1\" | grep -qF '#previews'" _ "$prev"
check "previews: valid YAML for kubectl"    sh -c "printf '%s' \"\$1\" | kubectl apply --dry-run=client -f - >/dev/null" _ "$prev"
check "previews need an App"                sh -c "! FORGE_PREVIEW_IP=10.150.1.222 sh \"$dir/render.sh\" --lb 10.150.1.220 10.150.1.221 >/dev/null 2>&1"
check "bad preview IP refused"              sh -c "! FORGE_GITHUB_APP_ID=1 FORGE_PREVIEW_IP='1.2.3.4;x' sh \"$dir/render.sh\" --lb 10.150.1.220 10.150.1.221 >/dev/null 2>&1"
check "bad pull secret refused"             sh -c "! FORGE_GITHUB_APP_ID=1 FORGE_PREVIEW_IP=10.150.1.222 FORGE_PREVIEW_PULL_SECRET='A_b' sh \"$dir/render.sh\" --lb 10.150.1.220 10.150.1.221 >/dev/null 2>&1"
exit $fail
