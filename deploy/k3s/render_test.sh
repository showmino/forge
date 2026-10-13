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
check "database: CNPG cluster forge-db" sh -c "printf '%s' \"\$1\" | grep -qF 'kind: Cluster' && printf '%s' \"\$1\" | grep -qF 'name: forge-db' && printf '%s' \"\$1\" | grep -qF 'instances: 2' && printf '%s' \"\$1\" | grep -qF 'imageName: ghcr.io/cloudnative-pg/postgresql:18.4-system-trixie' && printf '%s' \"\$1\" | grep -qF 'storageClass: longhorn-retain'" _ "$with"
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
# The chart revision must be one whose chart makes DNS records: this checkout's.
head_rev=$(git -C "$dir" rev-parse HEAD)
pub_render() { FORGE_PUBLIC_DOMAINS=$1 FORGE_DEPLOYS=1 FORGE_DEPLOY_CHART_REVISION=${2:-$head_rev} FORGE_GITHUB_APP_ID=5239131 FORGE_PREVIEW_IP=10.150.1.222 sh "$dir/render.sh" --lb 10.150.1.220 10.150.1.221; }
pub=$(pub_render showmino.com=showmino-com,deogratias.cloud=deogratias-cloud)
dep_head=$(FORGE_DEPLOYS=1 FORGE_DEPLOY_CHART_REVISION=$head_rev FORGE_GITHUB_APP_ID=5239131 FORGE_PREVIEW_IP=10.150.1.222 sh "$dir/render.sh" --lb 10.150.1.220 10.150.1.221)
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
argo_render() { FORGE_ARGOCD_URL=$1 FORGE_DEPLOYS=1 FORGE_DEPLOY_CHART_REVISION=$head_rev FORGE_GITHUB_APP_ID=5239131 FORGE_PREVIEW_IP=10.150.1.222 sh "$dir/render.sh" --lb 10.150.1.220 10.150.1.221; }
argo_on() { out=$(argo_render https://argocd.showmino.com) && printf '%s\n' "$out" | grep -qF '{name: FORGE_ARGOCD_URL, value: "https://argocd.showmino.com"}' && ! printf '%s\n' "$out" | grep -q '#argocd'; }
argo_off() { ! printf '%s\n' "$dep_head" | grep -q -e FORGE_ARGOCD_URL -e '#argocd'; }
check "argocd: forge-api gets the address"        argo_on
check "argocd: nothing rendered without it"       argo_off
for bad in argocd.showmino.com https://argocd.showmino.com/ 'https://a.com/x' 'https://a.com|b' 'javascript:alert(1)'; do
  check "argocd: \"$bad\" refused" sh -c "! FORGE_ARGOCD_URL='$bad' FORGE_DEPLOYS=1 FORGE_DEPLOY_CHART_REVISION=$head_rev FORGE_GITHUB_APP_ID=1 FORGE_PREVIEW_IP=10.150.1.222 sh \"$dir/render.sh\" --lb 10.150.1.220 10.150.1.221 >/dev/null 2>&1"
done
check "argocd: needs deploys"                     sh -c "! FORGE_ARGOCD_URL=https://argocd.showmino.com FORGE_GITHUB_APP_ID=1 FORGE_PREVIEW_IP=10.150.1.222 sh \"$dir/render.sh\" --lb 10.150.1.220 10.150.1.221 >/dev/null 2>&1"
check "public: a chart revision without DNS records refused" sh -c "! FORGE_PUBLIC_DOMAINS=example.com=example-com FORGE_DEPLOYS=1 FORGE_DEPLOY_CHART_REVISION=18b12326d3df2c1a11b9f664f378b579eb472aaf FORGE_GITHUB_APP_ID=1 FORGE_PREVIEW_IP=10.150.1.222 sh \"$dir/render.sh\" --lb 10.150.1.220 10.150.1.221 >/dev/null 2>&1"
# What forge-api (ParsePublicDomains) refuses, the render refuses too.
for bad in example.com example.com= =example-com Example.com=example-com example.com=example.com localhost=local \
  a.com=a-com,a.com=other a..com=a-com example.com=1zone example.com=zone- example.com=-zone -example.com=z example.c=z example.123=z \
  example.com=example-com,,other.io=other-io x.com=x-com,y.com=x-com-root; do
  check "public: \"$bad\" refused" bad_pub "$bad"
done
check "public: a bad tunnel name refused"         sh -c "! FORGE_PUBLIC_DNS_TUNNEL='a|b' FORGE_PUBLIC_DOMAINS=example.com=example-com FORGE_DEPLOYS=1 FORGE_DEPLOY_CHART_REVISION=$head_rev FORGE_GITHUB_APP_ID=1 FORGE_PREVIEW_IP=10.150.1.222 sh \"$dir/render.sh\" --lb 10.150.1.220 10.150.1.221 >/dev/null 2>&1"
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
exit $fail
