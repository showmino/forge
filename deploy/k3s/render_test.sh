#!/usr/bin/env sh
# Checks render.sh output with and without a GitHub App and previews. Run: sh deploy/k3s/render_test.sh
set -eu
dir=$(dirname "$0")
fail=0
# FORGE_GITHUB_ORG is required by render.sh itself (Dex always needs an org);
# default it here so every check below need not set it, same as showmino's
# own live.env does. A check that exercises the org explicitly overrides it.
export FORGE_GITHUB_ORG=${FORGE_GITHUB_ORG:-showmino}
# FORGE_PREVIEW_PULL_SECRET is required whenever previews are on; default it
# too, for the same reason.
export FORGE_PREVIEW_PULL_SECRET=${FORGE_PREVIEW_PULL_SECRET:-mino-ghcr}
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
check "no FORGE_GITHUB_ORG: render.sh refuses" sh -c "! FORGE_GITHUB_ORG= FORGE_GITHUB_APP_ID=5239131 sh \"\$1\" --lb 10.150.1.220 10.150.1.221 >/dev/null 2>&1" _ "$dir/render.sh"
check "no previews: no FORGE_PREVIEW_PULL_SECRET needed" sh -c "env -u FORGE_PREVIEW_PULL_SECRET sh \"\$1\" --lb 10.150.1.220 10.150.1.221 >/dev/null 2>&1" _ "$dir/render.sh"
check "previews, no FORGE_PREVIEW_PULL_SECRET: render.sh refuses" sh -c "! env -u FORGE_PREVIEW_PULL_SECRET FORGE_GITHUB_APP_ID=5239131 FORGE_PREVIEW_IP=10.150.1.222 sh \"\$1\" --lb 10.150.1.220 10.150.1.221 >/dev/null 2>&1" _ "$dir/render.sh"

# --cloud-lb: a MetalLB-free LoadBalancer exposure mode (a cloud provider or
# minikube tunnel assigns the address; the operator points its own hostnames
# at it afterward).
cloudlb=$(sh "$dir/render.sh" --cloud-lb dex.example.test api.example.test)
check "cloud-lb: no metallb annotation" sh -c "! printf '%s' \"\$1\" | grep -q 'metallb.universe.tf'" _ "$cloudlb"
check "cloud-lb: plain LoadBalancer Services" sh -c "printf '%s' \"\$1\" | grep -q 'type: LoadBalancer' && printf '%s' \"\$1\" | grep -qF 'host: dex.example.test'" _ "$cloudlb"
check "cloud-lb: rejects an IP where a hostname is expected" sh -c "! sh \"\$1\" --cloud-lb 10.0.0.1 api.example.test >/dev/null 2>&1" _ "$dir/render.sh"
check "cloud-lb: exact render pinned" sh -c "sh \"\$1\" --cloud-lb dex.example.test api.example.test | cmp -s - \"$dir/testdata/cloudlb.golden\"" _ "$dir/render.sh"

# The bare-domain (Traefik) mode's Ingress class: traefik by default,
# overridable for a cluster whose ingress controller uses another class.
check "ingress class defaults to traefik" sh -c "sh \"\$1\" example.test | grep -q 'ingressClassName: traefik'" _ "$dir/render.sh"
check "ingress class is overridable" sh -c "FORGE_INGRESS_CLASS=nginx sh \"\$1\" example.test | grep -q 'ingressClassName: nginx'" _ "$dir/render.sh"

# CloudNativePG's storage class: unset by default (the cluster's own default
# StorageClass), settable for a cluster that needs one named explicitly.
check "no storage class by default" sh -c "! sh \"\$1\" --lb 10.150.1.220 10.150.1.221 | grep -q storageClass" _ "$dir/render.sh"
check "storage class is settable" sh -c "FORGE_DB_STORAGE_CLASS=longhorn-retain sh \"\$1\" --lb 10.150.1.220 10.150.1.221 | grep -q 'storageClass: longhorn-retain'" _ "$dir/render.sh"

# The previews/deploys Gateway: its class is overridable, and a domain can
# replace FORGE_PREVIEW_IP's MetalLB-derived nip.io hostname entirely.
gwclass=$(FORGE_GITHUB_APP_ID=5239131 FORGE_GATEWAY_CLASS=envoy-gateway FORGE_PREVIEW_IP=10.150.1.222 sh "$dir/render.sh" --lb 10.150.1.220 10.150.1.221)
check "gateway class is overridable" sh -c "printf '%s' \"\$1\" | grep -q 'gatewayClassName: envoy-gateway'" _ "$gwclass"
gwdomain=$(FORGE_GITHUB_APP_ID=5239131 FORGE_GATEWAY_DOMAIN=minikube.test sh "$dir/render.sh" --cloud-lb dex.example.test api.example.test)
check "gateway domain: no metallb annotation" sh -c "! printf '%s' \"\$1\" | grep -q 'metallb.universe.tf'" _ "$gwdomain"
check "gateway domain: preview hostname" sh -c "printf '%s' \"\$1\" | grep -qF 'hostname: \"*.preview.minikube.test\"'" _ "$gwdomain"
check "gateway domain: forge-api's own FORGE_PREVIEW_DOMAIN" sh -c "printf '%s' \"\$1\" | grep -qF 'FORGE_PREVIEW_DOMAIN, value: preview.minikube.test'" _ "$gwdomain"
check "FORGE_PREVIEW_IP and FORGE_GATEWAY_DOMAIN together are refused" sh -c "! FORGE_GITHUB_APP_ID=5239131 FORGE_PREVIEW_IP=10.150.1.222 FORGE_GATEWAY_DOMAIN=minikube.test sh \"\$1\" --lb 10.150.1.220 10.150.1.221 >/dev/null 2>&1" _ "$dir/render.sh"
check "App: deploy repo defaults from org" sh -c "printf '%s' \"\$1\" | grep -q 'FORGE_DEPLOY_REPO, value: showmino/forge-deploys'" _ "$(FORGE_GITHUB_APP_ID=5239131 FORGE_DEPLOYS=1 FORGE_PREVIEW_IP=10.150.1.222 FORGE_DEPLOY_CHART_REVISION=0.20.0 sh "$dir/render.sh" --lb 10.150.1.220 10.150.1.221)"
check "App: cli repo defaults from org" sh -c "printf '%s' \"\$1\" | grep -q 'FORGE_CLI_REPO, value: showmino/forge'" _ "$with"
check "deploys.yaml has no literal showmino" sh -c "! FORGE_GITHUB_ORG=someorg FORGE_GITHUB_APP_ID=5239131 FORGE_DEPLOYS=1 FORGE_PREVIEW_IP=10.150.1.222 FORGE_DEPLOY_CHART_REVISION=0.20.0 sh \"\$1\" --lb 10.150.1.220 10.150.1.221 2>/dev/null | awk '/^# Deploys:/ {on=1} on' | grep -q showmino" _ "$dir/render.sh"
check "deploys.yaml follows a different org" sh -c "printf '%s' \"\$1\" | grep -q 'someorg/forge-deploys' && printf '%s' \"\$1\" | grep -q 'someorg/forge'" _ "$(FORGE_GITHUB_ORG=someorg FORGE_GITHUB_APP_ID=5239131 FORGE_DEPLOYS=1 FORGE_PREVIEW_IP=10.150.1.222 FORGE_DEPLOY_CHART_REVISION=0.20.0 sh "$dir/render.sh" --lb 10.150.1.220 10.150.1.221)"
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
check "database: CNPG cluster forge-db" sh -c "printf '%s' \"\$1\" | grep -qF 'kind: Cluster' && printf '%s' \"\$1\" | grep -qF 'name: forge-db' && printf '%s' \"\$1\" | grep -qF 'instances: 2' && printf '%s' \"\$1\" | grep -qF 'imageName: ghcr.io/cloudnative-pg/postgresql:18.4-system-trixie'" _ "$with"
check "database: dex database"          sh -c "printf '%s' \"\$1\" | grep -qF 'kind: Database' && printf '%s' \"\$1\" | grep -qF 'name: forge-db-dex' && printf '%s' \"\$1\" | grep -qF 'name: dex'" _ "$with"
check "database: rendered without an App too" sh -c "printf '%s' \"\$1\" | grep -qF 'kind: Cluster'" _ "$without"
check "Dex: storage in Postgres"        sh -c "printf '%s' \"\$1\" | grep -qF 'type: postgres' && printf '%s' \"\$1\" | grep -qF 'host: forge-db-rw.forge.svc' && printf '%s' \"\$1\" | grep -qF 'password: \$DEX_DB_PASSWORD' && printf '%s' \"\$1\" | grep -qF 'secretKeyRef: {name: forge-db-app, key: password}'" _ "$with"
check "Dex: no memory storage"          sh -c "! printf '%s' \"\$1\" | grep -qF 'type: memory'" _ "$with"
check "forge-api: database URL from the Secret" sh -c "printf '%s' \"\$1\" | grep -qF 'name: FORGE_DATABASE_URL' && printf '%s' \"\$1\" | grep -qF 'secretKeyRef: {name: forge-db-app, key: uri}'" _ "$with"
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
rev=0.20.0
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
res=$(FORGE_RESOURCES=1 FORGE_RUNNER_IMAGE=ghcr.io/showmino/forge-runner:abc1234 FORGE_DEPLOYS=1 FORGE_DEPLOY_CHART_REVISION=$rev FORGE_GITHUB_APP_ID=5239131 FORGE_PREVIEW_IP=10.150.1.222 sh "$dir/render.sh" --lb 10.150.1.220 10.150.1.221)
# resources_section: everything resources.yaml.tmpl adds (from its first line to the end).
resources_section() { printf '%s\n' "$res" | awk '/^# Resources:/ {on=1} on'; }
# The namespace, the permissions, the admission policy, the Service and the
# NetworkPolicy are pinned exactly: any change to them is a review.
resources_exact() { resources_section | cmp -s - "$dir/testdata/resources.golden"; }
resources_settings() {
  printf '%s\n' "$res" | grep -qF 'FORGE_RESOURCES, value: "1"' &&
    printf '%s\n' "$res" | grep -qF 'FORGE_RUNNER_IMAGE, value: "ghcr.io/showmino/forge-runner:abc1234"' &&
    printf '%s\n' "$res" | grep -qF '{name: runner, containerPort: 8081}'
}
# Without resources nothing of them is rendered: the deploys render is what it was.
no_resources() { ! printf '%s\n' "$dep" | grep -q -e FORGE_RESOURCES -e FORGE_RUNNER -e forge-runner -e 8081 -e NetworkPolicy; }
runner_api_private() {
  resources_section | awk '$0 == "kind: Service" {on=1} on && /^---/ {on=0} on' | grep -qF 'type: ClusterIP' &&
    ! printf '%s\n' "$res" | awk '$0 == "kind: Service" {on=1} on && /^---/ {on=0} on' | grep -B8 -A8 'LoadBalancer' | grep -qF 8081
}

check "resources: forge-api settings and port" resources_settings
check "resources: nothing without it"       no_resources
check "resources: objects and RBAC exactly as reviewed" resources_exact
check "resources: the runner API is cluster-internal" runner_api_private
check "resources: no #resources markers"    sh -c "! printf '%s' \"\$1\" | grep -qF '#resources'" _ "$res"
check "resources: valid YAML for kubectl"   sh -c "printf '%s' \"\$1\" | kubectl apply --dry-run=client -f - >/dev/null" _ "$res"
check "resources need deploys"              sh -c "! FORGE_RESOURCES=1 FORGE_RUNNER_IMAGE=forge-runner:dev FORGE_GITHUB_APP_ID=1 FORGE_PREVIEW_IP=10.150.1.222 sh \"$dir/render.sh\" --lb 10.150.1.220 10.150.1.221 >/dev/null 2>&1"
check "resources need a runner image"       sh -c "! FORGE_RESOURCES=1 FORGE_DEPLOYS=1 FORGE_DEPLOY_CHART_REVISION=$rev FORGE_GITHUB_APP_ID=1 FORGE_PREVIEW_IP=10.150.1.222 sh \"$dir/render.sh\" --lb 10.150.1.220 10.150.1.221 >/dev/null 2>&1"
check "bad runner image refused"            sh -c "! FORGE_RESOURCES=1 FORGE_RUNNER_IMAGE='x; rm' FORGE_DEPLOYS=1 FORGE_DEPLOY_CHART_REVISION=$rev FORGE_GITHUB_APP_ID=1 FORGE_PREVIEW_IP=10.150.1.222 sh \"$dir/render.sh\" --lb 10.150.1.220 10.150.1.221 >/dev/null 2>&1"
check "bad FORGE_RESOURCES refused"         sh -c "! FORGE_RESOURCES=yes FORGE_RUNNER_IMAGE=forge-runner:dev FORGE_DEPLOYS=1 FORGE_DEPLOY_CHART_REVISION=$rev FORGE_GITHUB_APP_ID=1 FORGE_PREVIEW_IP=10.150.1.222 sh \"$dir/render.sh\" --lb 10.150.1.220 10.150.1.221 >/dev/null 2>&1"

# Cluster capacity: off unless FORGE_CLUSTERS=1 (the goldens above are renders without it).
cl=$(FORGE_CLUSTERS=1 FORGE_CLUSTER_NAME=homelab FORGE_GRAFANA_URL=http://10.150.1.202 FORGE_PROMETHEUS_URL=http://prometheus-prometheus.monitoring.svc:9090 FORGE_REMOTE_CLUSTERS=aks-production,lab FORGE_GITHUB_APP_ID=5239131 FORGE_PREVIEW_IP=10.150.1.222 sh "$dir/render.sh" --lb 10.150.1.220 10.150.1.221)
cl_nografana=$(FORGE_CLUSTERS=1 FORGE_GITHUB_APP_ID=5239131 FORGE_PREVIEW_IP=10.150.1.222 sh "$dir/render.sh" --lb 10.150.1.220 10.150.1.221)
# clusters_section: everything clusters.yaml.tmpl adds (from its first line to the end).
clusters_section() { printf '%s\n' "$cl" | awk '/^# Cluster capacity:/ {on=1} on'; }
# The permissions are pinned exactly: any change to them is a review.
clusters_exact() { clusters_section | cmp -s - "$dir/testdata/clusters.golden"; }
clusters_settings() {
  printf '%s\n' "$cl" | grep -qF 'FORGE_CLUSTERS, value: "1"' &&
    printf '%s\n' "$cl" | grep -qF 'FORGE_CLUSTER_NAME, value: "homelab"' &&
    printf '%s\n' "$cl" | grep -qF 'FORGE_GRAFANA_URL, value: "http://10.150.1.202"' &&
    printf '%s\n' "$cl" | grep -qF 'FORGE_PROMETHEUS_URL, value: "http://prometheus-prometheus.monitoring.svc:9090"' &&
    printf '%s\n' "$cl" | grep -qF 'FORGE_REMOTE_CLUSTERS, value: "aks-production,lab"'
}
clusters_default() {
  printf '%s\n' "$cl_nografana" | grep -qF 'FORGE_CLUSTER_NAME, value: "default"' &&
    ! printf '%s\n' "$cl_nografana" | grep -q -e FORGE_GRAFANA_URL -e FORGE_PROMETHEUS_URL -e FORGE_REMOTE_CLUSTERS -e forge-api-remote-clusters
}
no_clusters() { ! printf '%s\n' "$prev" | grep -q -e FORGE_CLUSTER -e FORGE_GRAFANA -e FORGE_PROMETHEUS -e forge-api-clusters; }

check "clusters: forge-api settings"        clusters_settings
check "clusters: default name, no Grafana"  clusters_default
check "clusters: nothing without it"        no_clusters
check "clusters: RBAC exactly as reviewed"  clusters_exact
check "clusters: no markers left"           sh -c "! printf '%s' \"\$1\" | grep -q -e '#clusters' -e '#grafana' -e '#prometheus' -e '#remoteclusters'" _ "$cl"
check "clusters: namespaces only with Prometheus" sh -c "! printf '%s' \"\$1\" | awk '/^# Cluster capacity:/ {on=1} on' | grep -qF 'resources: [namespaces]'" _ "$cl_nografana"
check "clusters: valid YAML for kubectl"    sh -c "printf '%s' \"\$1\" | kubectl apply --dry-run=client -f - >/dev/null" _ "$cl"
check "clusters need previews"              sh -c "! FORGE_CLUSTERS=1 FORGE_GITHUB_APP_ID=1 sh \"$dir/render.sh\" --lb 10.150.1.220 10.150.1.221 >/dev/null 2>&1"
check "bad FORGE_CLUSTERS refused"          sh -c "! FORGE_CLUSTERS=yes FORGE_GITHUB_APP_ID=1 FORGE_PREVIEW_IP=10.150.1.222 sh \"$dir/render.sh\" --lb 10.150.1.220 10.150.1.221 >/dev/null 2>&1"
check "bad FORGE_CLUSTER_NAME refused"      sh -c "! FORGE_CLUSTERS=1 FORGE_CLUSTER_NAME='Home|Lab' FORGE_GITHUB_APP_ID=1 FORGE_PREVIEW_IP=10.150.1.222 sh \"$dir/render.sh\" --lb 10.150.1.220 10.150.1.221 >/dev/null 2>&1"
check "bad FORGE_GRAFANA_URL refused"       sh -c "! FORGE_CLUSTERS=1 FORGE_GRAFANA_URL='javascript:alert(1)' FORGE_GITHUB_APP_ID=1 FORGE_PREVIEW_IP=10.150.1.222 sh \"$dir/render.sh\" --lb 10.150.1.220 10.150.1.221 >/dev/null 2>&1"
check "FORGE_GRAFANA_URL needs clusters"    sh -c "! FORGE_GRAFANA_URL=https://grafana.example.com FORGE_GITHUB_APP_ID=1 FORGE_PREVIEW_IP=10.150.1.222 sh \"$dir/render.sh\" --lb 10.150.1.220 10.150.1.221 >/dev/null 2>&1"
check "bad FORGE_PROMETHEUS_URL refused"    sh -c "! FORGE_CLUSTERS=1 FORGE_PROMETHEUS_URL='http://p|x' FORGE_GITHUB_APP_ID=1 FORGE_PREVIEW_IP=10.150.1.222 sh \"$dir/render.sh\" --lb 10.150.1.220 10.150.1.221 >/dev/null 2>&1"
check "FORGE_PROMETHEUS_URL needs clusters" sh -c "! FORGE_PROMETHEUS_URL=http://prometheus:9090 FORGE_GITHUB_APP_ID=1 FORGE_PREVIEW_IP=10.150.1.222 sh \"$dir/render.sh\" --lb 10.150.1.220 10.150.1.221 >/dev/null 2>&1"
for bad in 'AKS' 'aks,' ',aks' 'aks,,lab' 'aks lab' 'aks,aks' 'default' 'aks|x'; do
  check "FORGE_REMOTE_CLUSTERS='$bad' refused" sh -c "! FORGE_CLUSTERS=1 FORGE_REMOTE_CLUSTERS='$bad' FORGE_GITHUB_APP_ID=1 FORGE_PREVIEW_IP=10.150.1.222 sh \"$dir/render.sh\" --lb 10.150.1.220 10.150.1.221 >/dev/null 2>&1"
done
check "FORGE_REMOTE_CLUSTERS needs clusters" sh -c "! FORGE_REMOTE_CLUSTERS=aks FORGE_GITHUB_APP_ID=1 FORGE_PREVIEW_IP=10.150.1.222 sh \"$dir/render.sh\" --lb 10.150.1.220 10.150.1.221 >/dev/null 2>&1"

# Backups of forge-db: off unless FORGE_DB_BACKUP_DEST is set (the goldens above are renders without it).
bk_dest=https://stminoforgesdcproduction.blob.core.windows.net/forge-db
bk=$(FORGE_DB_BACKUP_DEST=$bk_dest FORGE_DB_BACKUP_ITEM=vaults/production/items/forge-backup-storage sh "$dir/render.sh" --lb 10.150.1.220 10.150.1.221)
# backups_section: what backups.yaml.tmpl adds, to its last line.
backups_section() { printf '%s\n' "$bk" | awk '/^# Backups of forge-db/ {on=1} on {print} on && /summary: a forge-db volume is more than 80% full$/ {exit}'; }
backups_exact() { backups_section | cmp -s - "$dir/testdata/backups.golden"; }
backups_plugin() {
  printf '%s\n' "$bk" | awk '$0 == "kind: Cluster" {on=1} on && /^---/ {on=0} on' | grep -qF 'parameters: {barmanObjectName: forge-db-azure}' &&
    printf '%s\n' "$bk" | grep -qF 'isWALArchiver: true' &&
    printf '%s\n' "$bk" | grep -qF 'Backed up to Azure Blob' && ! printf '%s\n' "$bk" | grep -qF 'No backups yet'
}
no_backups() { ! printf '%s\n' "$without" | grep -q -e plugins: -e forge-db-azure -e ScheduledBackup -e forge-db-backup -e 'Backed up to Azure' -e '#nobackups'; }
# refuses <message> <env...>: render.sh fails, and says <message>.
refuses() { msg=$1; shift; ! env "$@" sh "$dir/render.sh" --lb 10.150.1.220 10.150.1.221 >/tmp/render-refuses.$$ 2>&1 && grep -qF -- "$msg" /tmp/render-refuses.$$; }
bk_deploys="FORGE_DEPLOYS=1 FORGE_DEPLOY_CHART_REVISION=$rev FORGE_GITHUB_APP_ID=1 FORGE_PREVIEW_IP=10.150.1.222"

check "backups: objects exactly as reviewed"  backups_exact
check "backups: forge-db names the plugin"    backups_plugin
check "backups: nothing without it"           no_backups
check "backups: no #backups markers"          sh -c "! printf '%s' \"\$1\" | grep -q -e '#backups' -e '#nobackups'" _ "$bk"
check "backups need the 1Password item"       refuses 'needs FORGE_DB_BACKUP_ITEM' FORGE_DB_BACKUP_DEST=$bk_dest
check "backups need a destination"            refuses 'needs FORGE_DB_BACKUP_DEST' FORGE_DB_BACKUP_ITEM=vaults/Kube/items/x
check "bad FORGE_DB_BACKUP_DEST refused"      refuses 'invalid FORGE_DB_BACKUP_DEST' FORGE_DB_BACKUP_DEST=https://evil.example.com/x FORGE_DB_BACKUP_ITEM=vaults/Kube/items/x
check "short container refused"               refuses 'invalid FORGE_DB_BACKUP_DEST' FORGE_DB_BACKUP_DEST=https://stminoforgesdcproduction.blob.core.windows.net/x FORGE_DB_BACKUP_ITEM=vaults/Kube/items/x
check "bad FORGE_DB_BACKUP_ITEM refused"      refuses 'invalid FORGE_DB_BACKUP_ITEM' FORGE_DB_BACKUP_DEST=$bk_dest 'FORGE_DB_BACKUP_ITEM=vaults/Kube/items/x|y'
check "dotted FORGE_DB_BACKUP_ITEM refused"   refuses 'invalid FORGE_DB_BACKUP_ITEM' FORGE_DB_BACKUP_DEST=$bk_dest FORGE_DB_BACKUP_ITEM=vaults/Kube/items/a/../b
bk_server() {
  FORGE_DB_BACKUP_DEST=$bk_dest FORGE_DB_BACKUP_ITEM=vaults/production/items/forge-backup-storage FORGE_DB_BACKUP_SERVER_NAME=forge-db-r20261025 sh "$dir/render.sh" --lb 10.150.1.220 10.150.1.221 |
    grep -qF 'parameters: {barmanObjectName: forge-db-azure, serverName: forge-db-r20261025}'
}
check "backups under another server name"     bk_server
check "server name needs backups"             refuses 'needs FORGE_DB_BACKUP_DEST' FORGE_DB_BACKUP_SERVER_NAME=forge-db-r1
check "bad server name refused"               refuses 'invalid FORGE_DB_BACKUP_SERVER_NAME' FORGE_DB_BACKUP_DEST=$bk_dest FORGE_DB_BACKUP_ITEM=vaults/Kube/items/x 'FORGE_DB_BACKUP_SERVER_NAME=a}b'
# shellcheck disable=SC2086 # bk_deploys is a list of assignments on purpose
check "backup item must be denied to services" refuses 'add it to FORGE_SECRET_DENIED_ITEMS' $bk_deploys FORGE_SECRET_VAULTS=Kube FORGE_SECRET_DENIED_ITEMS=other FORGE_DB_BACKUP_DEST=$bk_dest FORGE_DB_BACKUP_ITEM=vaults/Kube/items/forge-backup-storage
# shellcheck disable=SC2086
check "a denied backup item renders"          sh -c "env $bk_deploys FORGE_SECRET_VAULTS=Kube FORGE_SECRET_DENIED_ITEMS=other,forge-backup-storage FORGE_DB_BACKUP_DEST=$bk_dest FORGE_DB_BACKUP_ITEM=vaults/Kube/items/forge-backup-storage sh \"$dir/render.sh\" --lb 10.150.1.220 10.150.1.221 >/dev/null 2>&1"
rm -f /tmp/render-refuses.$$

# The web dashboard: off unless FORGE_WEB_ORIGINS is set (the goldens above are renders without it).
web=$(FORGE_WEB_ORIGINS=http://forge-web.staging.10.150.1.222.nip.io,http://forge-web.production.10.150.1.222.nip.io FORGE_GITHUB_APP_ID=5239131 sh "$dir/render.sh" --lb 10.150.1.220 10.150.1.221)
web_has() { printf '%s\n' "$web" | grep -qF -- "$1"; }
web_settings() {
  web_has '      - id: forge-web' && web_has '        public: true' &&
    web_has 'redirectURIs: ["http://forge-web.staging.10.150.1.222.nip.io/auth/callback", "http://forge-web.production.10.150.1.222.nip.io/auth/callback"]' &&
    web_has 'allowedOrigins: ["http://forge-web.staging.10.150.1.222.nip.io", "http://forge-web.production.10.150.1.222.nip.io"]' &&
    web_has '{name: FORGE_OIDC_WEB_CLIENT_ID, value: forge-web}' &&
    web_has '{name: FORGE_CORS_ORIGINS, value: "http://forge-web.staging.10.150.1.222.nip.io,http://forge-web.production.10.150.1.222.nip.io"}'
}
no_web() { ! printf '%s\n' "$with" | grep -q -e forge-web -e allowedOrigins -e FORGE_CORS -e '#web'; }
web_only_adds() { [ "$(printf '%s\n' "$web" | grep -v -e 'id: forge-web' -e 'redirectURIs: \["http' -e allowedOrigins -e 'name: Forge dashboard' -e FORGE_OIDC_WEB_CLIENT_ID -e FORGE_CORS_ORIGINS -e '^        public: true$' | cksum)" = "$(printf '%s\n' "$with" | grep -v '^        public: true$' | cksum)" ]; }
bad_web() { ! FORGE_WEB_ORIGINS=$1 sh "$dir/render.sh" --lb 10.150.1.220 10.150.1.221 >/dev/null 2>&1; }
check "web: Dex client, allowed origins, forge-api settings" web_settings
check "web: nothing rendered without FORGE_WEB_ORIGINS" no_web
check "web: adds lines and changes no others"   web_only_adds
check "web: an origin with a path refused"      bad_web http://forge-web.example.com/app
check "web: a trailing slash refused"           bad_web http://forge-web.example.com/
check "web: a wildcard refused"                 bad_web 'http://*.example.com'
check "web: an origin without a scheme refused" bad_web forge-web.example.com
check "web: an empty item refused"              bad_web http://a.example.com,,http://b.example.com
check "web: upper case refused"                 bad_web http://Forge-Web.example.com
check "web: a port that is not a number refused" bad_web http://forge-web.example.com:xx
check "web: the scheme's own port refused"      bad_web http://forge-web.example.com:80
check "web: a port is fine"                     sh -c "FORGE_WEB_ORIGINS=http://localhost:3000 sh \"$dir/render.sh\" --lb 10.150.1.220 10.150.1.221 | grep -qF 'allowedOrigins: [\"http://localhost:3000\"]'"

# Public addresses: off unless FORGE_PUBLIC_DOMAINS is set ($dep is a render with deploys and without it).
# The dns.yaml-presence guard now fetches the published chart from
# ghcr.io, which doesn't exist until this phase's own release tag ships
# it; a stub `helm` on PATH (ahead of the real one) simulates that fetch
# deterministically for every check below, without real network access.
# `helm pull ... --version X -d DIR`: X ending "-no-dns" writes a tarball
# without templates/dns.yaml; X ending "-unreachable" fails outright
# (simulating no network or auth); anything else writes one that has it.
fake_helm_dir=$(mktemp -d)
# no_helm_dir: just the utilities render.sh itself needs, symlinked from
# wherever they really live, guaranteed to exclude helm regardless of where
# it happens to live on this machine (unlike a fixed /usr/bin:/bin, which
# could include it and make the "no helm on PATH" check below hit the real
# registry instead).
no_helm_dir=$(mktemp -d)
for util in sh grep sed mktemp cat dirname printf tr tar awk; do
  p=$(command -v "$util" 2>/dev/null) && ln -s "$p" "$no_helm_dir/$util"
done
trap 'rm -rf "$fake_helm_dir" "$no_helm_dir"' EXIT
cat > "$fake_helm_dir/helm" <<'HELMSTUB'
#!/usr/bin/env sh
set -eu
[ "${1:-}" = pull ] || { echo "fake helm: only 'pull' is stubbed" >&2; exit 1; }
shift
version= outdir=.
while [ $# -gt 0 ]; do
  case $1 in
    --version) version=$2; shift 2 ;;
    -d) outdir=$2; shift 2 ;;
    *) shift ;;
  esac
done
case $version in
  *-unreachable) echo "Error: failed to do request: dial tcp: no route to host" >&2; exit 1 ;;
esac
work=$(mktemp -d)
mkdir -p "$work/forge-chart/templates"
case $version in
  *-no-dns) : > "$work/forge-chart/templates/other.yaml" ;;
  *) : > "$work/forge-chart/templates/dns.yaml" ;;
esac
tar czf "$outdir/forge-chart-$version.tgz" -C "$work" forge-chart
rm -rf "$work"
HELMSTUB
chmod +x "$fake_helm_dir/helm"
pub_render() { PATH="$fake_helm_dir:$PATH" FORGE_PUBLIC_DOMAINS=$1 FORGE_DEPLOYS=1 FORGE_DEPLOY_CHART_REVISION=${2:-$rev} FORGE_GITHUB_APP_ID=5239131 FORGE_PREVIEW_IP=10.150.1.222 sh "$dir/render.sh" --lb 10.150.1.220 10.150.1.221; }
pub=$(pub_render showmino.com=showmino-com,deogratias.cloud=deogratias-cloud)
dep_head=$(FORGE_DEPLOYS=1 FORGE_DEPLOY_CHART_REVISION=$rev FORGE_GITHUB_APP_ID=5239131 FORGE_PREVIEW_IP=10.150.1.222 sh "$dir/render.sh" --lb 10.150.1.220 10.150.1.221)
pub_has() { printf '%s\n' "$pub" | grep -qF -- "$1"; }
pub_settings() {
  pub_has '{name: FORGE_PUBLIC_DOMAINS, value: "showmino.com=showmino-com,deogratias.cloud=deogratias-cloud"}' &&
    pub_has '{name: FORGE_PUBLIC_DNS_TUNNEL, value: "production"}' &&
    pub_has '    - {group: kro.run, kind: CloudflareRecordsApplication}'
}
pub_listeners() {
  pub_has '    - name: public-showmino-com' && pub_has '      hostname: "*.showmino.com"' &&
    pub_has '    - name: public-showmino-com-root' && pub_has '      hostname: "showmino.com"' &&
    pub_has '    - name: public-deogratias-cloud' && pub_has '      hostname: "*.deogratias.cloud"' &&
    pub_has '    - name: public-deogratias-cloud-root' && pub_has '      hostname: "deogratias.cloud"' &&
    [ "$(printf '%s\n' "$pub" | grep -cF -- '- {key: forge.dev/deploy-env, operator: Exists}')" = 4 ] &&
    ! pub_has '#public'
}
no_pub() { ! printf '%s\n' "$dep" | grep -q -e FORGE_PUBLIC -e 'name: public' -e CloudflareRecordsApplication -e '#public' -e 'Public addresses'; }
pub_only_adds() { d=$(mktemp) && p=$(mktemp) && printf '%s\n' "$dep_head" > "$d" && printf '%s\n' "$pub" > "$p"; n=$(diff "$d" "$p" | grep -c '^<'); rm -f "$d" "$p"; [ "$n" = 0 ]; }
bad_pub() { ! pub_render "$1" >/dev/null 2>&1; }
check "public: forge-api settings and the project entry" pub_settings
check "public: two listeners per domain (its hosts, its root), for Forge's deployments only" pub_listeners
check "public: nothing rendered without FORGE_PUBLIC_DOMAINS" no_pub
check "public: adds lines and changes no others" pub_only_adds
check "public: needs deploys"                    sh -c "! FORGE_PUBLIC_DOMAINS=example.com=example-com FORGE_GITHUB_APP_ID=1 FORGE_PREVIEW_IP=10.150.1.222 sh \"$dir/render.sh\" --lb 10.150.1.220 10.150.1.221 >/dev/null 2>&1"

# Argo CD links: off unless FORGE_ARGOCD_URL is set.
argo_render() { FORGE_ARGOCD_URL=$1 FORGE_DEPLOYS=1 FORGE_DEPLOY_CHART_REVISION=$rev FORGE_GITHUB_APP_ID=5239131 FORGE_PREVIEW_IP=10.150.1.222 sh "$dir/render.sh" --lb 10.150.1.220 10.150.1.221; }
argo_on() { out=$(argo_render https://argocd.showmino.com) && printf '%s\n' "$out" | grep -qF '{name: FORGE_ARGOCD_URL, value: "https://argocd.showmino.com"}' && ! printf '%s\n' "$out" | grep -q '#argocd'; }
argo_off() { ! printf '%s\n' "$dep_head" | grep -q -e FORGE_ARGOCD_URL -e '#argocd'; }
check "argocd: forge-api gets the address"        argo_on
check "argocd: nothing rendered without it"       argo_off
for bad in argocd.showmino.com https://argocd.showmino.com/ 'https://a.com/x' 'https://a.com|b' 'javascript:alert(1)'; do
  check "argocd: \"$bad\" refused" sh -c "! FORGE_ARGOCD_URL='$bad' FORGE_DEPLOYS=1 FORGE_DEPLOY_CHART_REVISION=$rev FORGE_GITHUB_APP_ID=1 FORGE_PREVIEW_IP=10.150.1.222 sh \"$dir/render.sh\" --lb 10.150.1.220 10.150.1.221 >/dev/null 2>&1"
done
check "argocd: needs deploys"                     sh -c "! FORGE_ARGOCD_URL=https://argocd.showmino.com FORGE_GITHUB_APP_ID=1 FORGE_PREVIEW_IP=10.150.1.222 sh \"$dir/render.sh\" --lb 10.150.1.220 10.150.1.221 >/dev/null 2>&1"

# run.secrets vaults: off unless FORGE_SECRET_VAULTS is set.
vaults_render() { FORGE_SECRET_VAULTS=$1 FORGE_DEPLOYS=1 FORGE_DEPLOY_CHART_REVISION=$rev FORGE_GITHUB_APP_ID=5239131 FORGE_PREVIEW_IP=10.150.1.222 sh "$dir/render.sh" --lb 10.150.1.220 10.150.1.221; }
vaults_on() { out=$(vaults_render Apps,Shared) && printf '%s\n' "$out" | grep -qF '{name: FORGE_SECRET_VAULTS, value: "Apps,Shared"}' && ! printf '%s\n' "$out" | grep -q '#vaults'; }
denied_on() { out=$(FORGE_SECRET_DENIED_ITEMS=forge-github-app,ghcr-secret vaults_render Kube) && printf '%s\n' "$out" | grep -qF '{name: FORGE_SECRET_DENIED_ITEMS, value: "forge-github-app,ghcr-secret"}'; }
check "vaults: forge-api gets the denied items"   denied_on
check "vaults: denied items need vaults"         sh -c "! FORGE_SECRET_DENIED_ITEMS=x FORGE_DEPLOYS=1 FORGE_DEPLOY_CHART_REVISION=$rev FORGE_GITHUB_APP_ID=1 FORGE_PREVIEW_IP=10.150.1.222 sh \"$dir/render.sh\" --lb 10.150.1.220 10.150.1.221 >/dev/null 2>&1"
check "vaults: a bad denied item refused"        sh -c "! FORGE_SECRET_VAULTS=Kube FORGE_SECRET_DENIED_ITEMS='a|b' FORGE_DEPLOYS=1 FORGE_DEPLOY_CHART_REVISION=$rev FORGE_GITHUB_APP_ID=1 FORGE_PREVIEW_IP=10.150.1.222 sh \"$dir/render.sh\" --lb 10.150.1.220 10.150.1.221 >/dev/null 2>&1"
vaults_off() { ! printf '%s\n' "$dep_head" | grep -q -e FORGE_SECRET_VAULTS -e '#vaults'; }
options_kinds() { for k in '{group: onepassword.com, kind: OnePasswordItem}' '{group: batch, kind: CronJob}' '{group: keda.sh, kind: ScaledObject}'; do printf '%s\n' "$dep_head" | grep -qF -- "$k" || return 1; done; }
check "vaults: forge-api gets the list"           vaults_on
check "vaults: nothing rendered without it"       vaults_off
check "options: the project allows their kinds"   options_kinds
for bad in 'My Vault' 'Apps,' ',Apps' 'Apps|x' 'a/b'; do
  check "vaults: \"$bad\" refused" sh -c "! FORGE_SECRET_VAULTS='$bad' FORGE_DEPLOYS=1 FORGE_DEPLOY_CHART_REVISION=$rev FORGE_GITHUB_APP_ID=1 FORGE_PREVIEW_IP=10.150.1.222 sh \"$dir/render.sh\" --lb 10.150.1.220 10.150.1.221 >/dev/null 2>&1"
done

# The secret backend: off (default onepassword, nothing rendered) unless
# FORGE_SECRET_BACKEND names a different one, in which case forge-api and the
# chart's own helm parameter both get it, so they cannot drift apart.
secretbackend_render() { FORGE_SECRET_BACKEND=$1 FORGE_DEPLOYS=1 FORGE_DEPLOY_CHART_REVISION=$rev FORGE_GITHUB_APP_ID=5239131 FORGE_PREVIEW_IP=10.150.1.222 sh "$dir/render.sh" --lb 10.150.1.220 10.150.1.221; }
secretbackend_on() {
  out=$(secretbackend_render external) &&
    printf '%s\n' "$out" | grep -qF '{name: FORGE_SECRET_BACKEND, value: "external"}' &&
    printf '%s\n' "$out" | grep -qF '{name: secretBackend, value: "external"}' &&
    ! printf '%s\n' "$out" | grep -q '#secretbackend'
}
secretbackend_off() { ! printf '%s\n' "$dep_head" | grep -q -e FORGE_SECRET_BACKEND -e '{name: secretBackend' -e '#secretbackend'; }
check "secret backend: forge-api and the chart both get it" secretbackend_on
check "secret backend: nothing rendered without it"         secretbackend_off
check "secret backend: needs deploys" sh -c "! FORGE_SECRET_BACKEND=external FORGE_GITHUB_APP_ID=1 FORGE_PREVIEW_IP=10.150.1.222 sh \"$dir/render.sh\" --lb 10.150.1.220 10.150.1.221 >/dev/null 2>&1"
check "secret backend: a bad value refused" sh -c "! FORGE_SECRET_BACKEND='my backend' FORGE_DEPLOYS=1 FORGE_DEPLOY_CHART_REVISION=$rev FORGE_GITHUB_APP_ID=1 FORGE_PREVIEW_IP=10.150.1.222 sh \"$dir/render.sh\" --lb 10.150.1.220 10.150.1.221 >/dev/null 2>&1"

# The chart revision is a published chart's SemVer now, not a commit SHA.
check "chart revision accepts a semver" sh -c "FORGE_GITHUB_APP_ID=5239131 FORGE_DEPLOYS=1 FORGE_PREVIEW_IP=10.150.1.222 FORGE_DEPLOY_CHART_REVISION=0.20.0 sh \"\$1\" --lb 10.150.1.220 10.150.1.221 | grep -q 'targetRevision: 0.20.0'" _ "$dir/render.sh"
# An OCI source with no path, chart or ref is an invalid Argo CD Application
# spec (every generated Application would go straight to ComparisonError).
check "OCI chart source has a path" sh -c "FORGE_GITHUB_APP_ID=5239131 FORGE_DEPLOYS=1 FORGE_PREVIEW_IP=10.150.1.222 FORGE_DEPLOY_CHART_REVISION=0.20.0 sh \"\$1\" --lb 10.150.1.220 10.150.1.221 | grep -A8 'repoURL: oci://ghcr.io/showmino/forge-chart$' | grep -q '  path: \.$'" _ "$dir/render.sh"
check "chart revision refuses the old SHA shape" sh -c "! FORGE_GITHUB_APP_ID=5239131 FORGE_DEPLOYS=1 FORGE_PREVIEW_IP=10.150.1.222 FORGE_DEPLOY_CHART_REVISION=2b84f5e1676aa92090e43737790655e6436cb070 sh \"\$1\" --lb 10.150.1.220 10.150.1.221 >/dev/null 2>&1" _ "$dir/render.sh"
check "public: a chart revision without DNS records refused" sh -c "PATH=\"$fake_helm_dir:\$PATH\" FORGE_PUBLIC_DOMAINS=example.com=example-com FORGE_DEPLOYS=1 FORGE_DEPLOY_CHART_REVISION=0.0.0-no-dns FORGE_GITHUB_APP_ID=1 FORGE_PREVIEW_IP=10.150.1.222 sh \"$dir/render.sh\" --lb 10.150.1.220 10.150.1.221 2>&1 1>/dev/null | grep -qF 'has no templates/dns.yaml'"
check "public: an unreachable registry refused, not silently passed" sh -c "PATH=\"$fake_helm_dir:\$PATH\" FORGE_PUBLIC_DOMAINS=example.com=example-com FORGE_DEPLOYS=1 FORGE_DEPLOY_CHART_REVISION=0.0.0-unreachable FORGE_GITHUB_APP_ID=1 FORGE_PREVIEW_IP=10.150.1.222 sh \"$dir/render.sh\" --lb 10.150.1.220 10.150.1.221 2>&1 1>/dev/null | grep -qF 'could not fetch'"
check "public: no helm on PATH refused clearly" sh -c "PATH=\"$no_helm_dir\" FORGE_PUBLIC_DOMAINS=example.com=example-com FORGE_DEPLOYS=1 FORGE_DEPLOY_CHART_REVISION=0.20.0 FORGE_GITHUB_APP_ID=1 FORGE_PREVIEW_IP=10.150.1.222 sh \"$dir/render.sh\" --lb 10.150.1.220 10.150.1.221 2>&1 1>/dev/null | grep -qF 'needs helm on PATH'"
# What forge-api (ParsePublicDomains) refuses, the render refuses too.
for bad in example.com example.com= =example-com Example.com=example-com example.com=example.com localhost=local \
  a.com=a-com,a.com=other a..com=a-com example.com=1zone example.com=zone- example.com=-zone -example.com=z example.c=z example.123=z \
  example.com=example-com,,other.io=other-io x.com=x-com,y.com=x-com-root; do
  check "public: \"$bad\" refused" bad_pub "$bad"
done
check "public: a bad tunnel name refused"         sh -c "! FORGE_PUBLIC_DNS_TUNNEL='a|b' FORGE_PUBLIC_DOMAINS=example.com=example-com FORGE_DEPLOYS=1 FORGE_DEPLOY_CHART_REVISION=$rev FORGE_GITHUB_APP_ID=1 FORGE_PREVIEW_IP=10.150.1.222 sh \"$dir/render.sh\" --lb 10.150.1.220 10.150.1.221 >/dev/null 2>&1"
tunnel_ignored() { [ "$(FORGE_PUBLIC_DNS_TUNNEL='x|;s|forge|HACKED' FORGE_GITHUB_APP_ID=5239131 sh "$dir/render.sh" --lb 10.150.1.220 10.150.1.221 | cksum)" = "$(printf '%s\n' "$with" | cksum)" ]; }
check "public: the tunnel name is ignored without public domains" tunnel_ignored

# The login's public name: off unless FORGE_LOGIN_URL is set.
login=$(FORGE_LOGIN_URL=https://forge-login.showmino.com FORGE_GITHUB_APP_ID=5239131 sh "$dir/render.sh" --lb 10.150.1.220 10.150.1.221)
login_has() { printf '%s\n' "$login" | grep -qF -- "$1"; }
login_settings() {
  login_has '    issuer: https://forge-login.showmino.com/dex' &&
    login_has '{name: FORGE_OIDC_ISSUER, value: "https://forge-login.showmino.com/dex"}' &&
    ! login_has 'http://dex.10.150.1.220.nip.io/dex'
}
login_only_changes_urls() { [ "$(printf '%s\n' "$login" | sed 's|https://forge-login.showmino.com|http://dex.10.150.1.220.nip.io|g' | cksum)" = "$(printf '%s\n' "$with" | cksum)" ]; }
bad_login() { ! FORGE_LOGIN_URL=$1 sh "$dir/render.sh" --lb 10.150.1.220 10.150.1.221 >/dev/null 2>&1; }
check "login: the issuer and forge-api use the public name" login_settings
check "login: nothing else changes"                 login_only_changes_urls
check "login: a path refused"                       bad_login https://login.example.com/dex
check "login: a name without a scheme refused"      bad_login login.example.com
check "login: upper case refused"                   bad_login https://Login.example.com
check "login: sed characters refused"               bad_login 'https://a.example.com|x'

# Forge's look on Dex's pages: off unless FORGE_DEX_THEME=1.
theme=$(FORGE_DEX_THEME=1 FORGE_GITHUB_APP_ID=5239131 sh "$dir/render.sh" --lb 10.150.1.220 10.150.1.221)
theme_files="$dir/../platform/chart/files/dex-theme"
# theme_doc: the rendered documents, parsed as YAML: kind/name -> object, as JSON.
theme_doc() { printf '%s\n' "$theme" | python3 -I -c 'import json,sys,yaml; print(json.dumps({d["kind"]+"/"+d["metadata"]["name"]: d for d in yaml.safe_load_all(sys.stdin) if d}))'; }
theme_configmap() {
  theme_doc | python3 -I -c '
import base64,json,sys
cm=json.load(sys.stdin)["ConfigMap/dex-theme"]
assert sorted(cm["data"]) == ["header.html","logo.svg","styles.css"], cm["data"].keys()
assert sorted(cm["binaryData"]) == ["archivo.woff2","favicon.png","plex-sans-400.woff2","plex-sans-600.woff2"], cm["binaryData"].keys()
d=sys.argv[1]
assert base64.b64decode(cm["binaryData"]["archivo.woff2"]) == open(d+"/archivo.woff2","rb").read()
assert cm["data"]["styles.css"] == open(d+"/styles.css").read()
assert cm["data"]["header.html"] == open(d+"/header.html").read()
' "$theme_files"
}
theme_dex() {
  theme_doc | python3 -I -c '
import json,sys,yaml
docs=json.load(sys.stdin)
cfg=yaml.safe_load(docs["ConfigMap/dex-config"]["data"]["config.yaml"])
assert cfg["frontend"] == {"dir":"/srv/dex/web","theme":"forge","issuer":"Forge","logoURL":"theme/logo.svg"}, cfg["frontend"]
pod=docs["Deployment/dex"]["spec"]["template"]
assert len(pod["metadata"]["annotations"]["checksum/dex-theme"]) == 16
c=pod["spec"]["containers"][0]
mounts={m["mountPath"]: m for m in c["volumeMounts"]}
assert mounts["/srv/dex/web/themes/forge"]["name"] == "theme"
assert mounts["/srv/dex/web/templates/header.html"] == {"name":"theme","mountPath":"/srv/dex/web/templates/header.html","subPath":"header.html"}
assert {"name":"theme","configMap":{"name":"dex-theme"}} in pod["spec"]["volumes"]
'
}
theme_no_markers() { ! printf '%s\n' "$theme" | grep -q '#theme'; }
theme_off() { ! printf '%s\n' "$with" | grep -q -e dex-theme -e 'frontend:' -e '#theme'; }
theme_sum_follows_files() {
  copy=$(mktemp -d) && cp -r "$dir/.." "$copy/deploy" && printf '/* changed */\n' >> "$copy/deploy/platform/chart/files/dex-theme/styles.css" &&
    changed=$(FORGE_DEX_THEME=1 FORGE_GITHUB_APP_ID=5239131 sh "$copy/deploy/k3s/render.sh" --lb 10.150.1.220 10.150.1.221 | grep 'checksum/dex-theme') &&
    rm -rf "$copy" && [ "$changed" != "$(printf '%s\n' "$theme" | grep 'checksum/dex-theme')" ]
}
check "theme: the ConfigMap holds the theme's files as they are" theme_configmap
check "theme: Dex's frontend, the mounts and the checksum"     theme_dex
check "theme: no #theme markers left"                         theme_no_markers
check "theme: nothing of it without FORGE_DEX_THEME"           theme_off
check "theme: a changed file changes the checksum"            theme_sum_follows_files
# kubectl apply (and Argo CD's client-side sync) keep the whole object in an
# annotation, and all of an object's annotations must stay under 262144 bytes:
# keep the theme well below that.
theme_small() { [ "$(printf '%s\n' "$theme" | awk '/^kind: ConfigMap$/ {k=1} /^---$/ {k=0} k' | awk '/^  name: dex-theme$/ {on=1} on' | wc -c)" -lt 230000 ]; }
# header.html is Dex's own with a few changes, written for one Dex version: a
# new Dex image means comparing it with that version's header.html first.
theme_header_version() {
  v=$(sed -n 's/^{{\/\* Dex \(v[0-9.]*\)'"'"'s header.html.*/\1/p' "$theme_files/header.html")
  [ -n "$v" ] && grep -q "image: ghcr.io/dexidp/dex:$v\$" "$dir/forge.yaml.tmpl" &&
    grep -q "image: ghcr.io/dexidp/dex:$v\$" "$dir/../platform/chart/templates/dex.yaml"
}
check "theme: the ConfigMap stays under 230000 bytes"         theme_small
check "theme: header.html is for the Dex image in use"         theme_header_version
check "theme: a bad FORGE_DEX_THEME refused"                  sh -c "! FORGE_DEX_THEME=yes sh \"\$1\" --lb 10.150.1.220 10.150.1.221 >/dev/null 2>&1" _ "$dir/render.sh"
exit $fail
