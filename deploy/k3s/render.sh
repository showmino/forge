#!/usr/bin/env sh
# Render the k3s manifests.
#
# FORGE_GITHUB_ORG is required by every render (Dex's GitHub connector needs
# it regardless of the App).
#
# Stock k3s, behind the bundled Traefik ingress (dex.<domain>, forge.<domain>):
#   FORGE_GITHUB_ORG=<org> deploy/k3s/render.sh 192.168.1.50.nip.io | kubectl apply -f -
#
# No Traefik: Dex and forge-api on their own LoadBalancer IPs, such as from MetalLB
# (dex.<dex-ip>.nip.io, forge.<api-ip>.nip.io):
#   FORGE_GITHUB_ORG=<org> deploy/k3s/render.sh --lb 10.150.1.220 10.150.1.221 | kubectl apply -f -
#
# No Traefik, no MetalLB (a cloud provider's own LoadBalancer, or `minikube
# tunnel`): Dex and forge-api each get a plain LoadBalancer Service with no
# requested IP, and you point hostnames you already control at whatever
# address the cluster assigns. Those hostnames must resolve from inside the
# cluster too, the same requirement the nip.io modes already have: forge-api
# discovers Dex's OIDC issuer at startup and fails to start if it can't:
#   FORGE_GITHUB_ORG=<org> deploy/k3s/render.sh --cloud-lb dex.example.com forge.example.com | kubectl apply -f -
#
# FORGE_API_IMAGE picks the image (default forge-api:dev):
#   FORGE_GITHUB_ORG=<org> FORGE_API_IMAGE=ghcr.io/showmino/forge-api:v0.1.0 deploy/k3s/render.sh forge.lan | kubectl apply -f -
#
# With the Forge GitHub App (templates and 'forge create'), create the Secret
# first, then render with the App's ID (FORGE_TEMPLATES_REPO, FORGE_REPO,
# FORGE_DEPLOY_REPO and FORGE_CHART_REPO default to <org>/forge-templates,
# <org>/forge, <org>/forge-deploys and <org>/forge-chart):
#   kubectl -n forge create secret generic forge-github-app --from-file=github_app_private_key=<key.pem>
#   FORGE_GITHUB_ORG=<org> FORGE_GITHUB_APP_ID=5239131 FORGE_API_IMAGE=... deploy/k3s/render.sh --lb <dex-ip> <api-ip> | kubectl apply -f -
# or let the 1Password operator create that Secret from an item with a
# github_app_private_key field:
#   FORGE_ONEPASSWORD_ITEM=vaults/Kube/items/forge-github-app FORGE_GITHUB_APP_ID=5239131 ... | kubectl apply -f -
#
# Login is through GitHub (Dex's GitHub connector): only members of
# FORGE_DEX_GITHUB_ORG (default FORGE_GITHUB_ORG) log in. The teams in
# FORGE_WRITER_GROUPS (default <org>:platform-engineering; never empty) are
# forge-api's admins, who may change everything; the members of a service's
# owner team may work on that service. Dex reads the OAuth App's client_id and client_secret from
# the Secret dex-github, which the 1Password operator can create:
#   FORGE_DEX_ONEPASSWORD_ITEM=vaults/Kube/items/forge-dex-github ... | kubectl apply -f -
#
# Preview environments (forge env) need the App too, plus the IP of their
# Gateway and FORGE_PREVIEW_PULL_SECRET, the pull secret in namespace forge
# (required once previews are configured, no default); lines in
# forge.yaml.tmpl with a trailing "previews" marker exist only then:
#   FORGE_PREVIEW_IP=10.150.1.222 FORGE_GITHUB_APP_ID=5239131 ... | kubectl apply -f -
# FORGE_GATEWAY_DOMAIN replaces FORGE_PREVIEW_IP with a domain you already
# control (mutually exclusive with it): the cluster's own LoadBalancer
# implementation assigns the address, with no MetalLB annotation, and
# previews/staging/production are reached under *.preview.<domain> etc.
# instead of *.preview.<ip>.nip.io. FORGE_GATEWAY_CLASS (default istio)
# names the Gateway API implementation's class either way:
#   FORGE_GATEWAY_DOMAIN=minikube.test FORGE_GATEWAY_CLASS=envoy-gateway FORGE_GITHUB_APP_ID=5239131 ... | kubectl apply -f -
#
# The web dashboard (Phase 6) needs its addresses, comma-separated, each a
# scheme and a host; lines with a trailing "web" marker exist only then:
#   FORGE_WEB_ORIGINS=http://forge-web.staging.<domain>,http://forge-web.production.<domain> ... | kubectl apply -f -
# It adds Dex's public client forge-web and lets those pages call forge-api
# from a browser. Dex reads its config at start: restart it after a change.
#
# FORGE_DEX_THEME=1 gives Dex's pages (sign-in, the CLI's device code, errors)
# the dashboard's look, from deploy/platform/chart/files/dex-theme:
#   FORGE_DEX_THEME=1 ... | kubectl apply -f -
#
# Public addresses: with FORGE_PUBLIC_DOMAINS a service is reached under a
# real domain, payments.example.com in production and
# payments-staging.example.com in staging. Each item is a domain and the name
# of its DNS zone for the platform's CloudflareRecordsApplication, the default
# domain first; lines with a trailing "public" marker exist only then:
#   FORGE_PUBLIC_DOMAINS=showmino.com=showmino-com,deogratias.cloud=deogratias-cloud ... | kubectl apply -f -
# It needs FORGE_DEPLOYS, a chart revision that has the DNS record, and the
# Cloudflare tunnel sending the domains' hosts to the Gateway
# (FORGE_PREVIEW_IP). FORGE_PUBLIC_DNS_TUNNEL (default production) names the
# tunnel the records point at.
#
# Deploys (forge deploy) need previews, plus the published chart's version
# (a vX.Y.Z release tag publishes it to oci://ghcr.io/<org>/forge-chart;
# FORGE_DEPLOY_CHART_REVISION is that version without the leading v, e.g.
# 0.20.0, not a commit SHA); lines with a trailing "deploys" marker exist
# only then:
#   FORGE_DEPLOYS=1 FORGE_DEPLOY_CHART_REVISION=0.20.0 FORGE_PREVIEW_IP=... | kubectl apply -f -
# Once deploys are on, every re-render must include FORGE_DEPLOYS=1 and the
# current FORGE_DEPLOY_CHART_REVISION (read it with `kubectl -n argocd get
# applicationset forge-services -o jsonpath='{.spec.template.spec.sources[0].targetRevision}'`);
# a render without them removes the staging and production routes. Changing the
# revision re-renders every deployed service.
# FORGE_ARGOCD_URL (e.g. https://argocd.example.com) gives each deployment a
# link to its Application in Argo CD's web UI. FORGE_SECRET_VAULTS (e.g. Apps)
# lists the secret groups a service's run.secrets may read, and
# FORGE_SECRET_DENIED_ITEMS the items in them no service may (the platform's own).
# FORGE_SECRET_BACKEND picks which operator populates a run.secrets entry's
# Secret (default onepassword, which then also refuses any item that is not
# vaults/<vault>/items/<item>); set both forge-api's check and the chart's own
# secretBackend value, so they cannot drift apart.
#
# Resources (forge resource) need deploys, plus the image of the runner Job;
# lines with a trailing "resources" marker exist only then:
#   FORGE_RESOURCES=1 FORGE_RUNNER_IMAGE=ghcr.io/showmino/forge-runner:<tag> FORGE_DEPLOYS=1 ... | kubectl apply -f -
# The chart revision must be one whose chart knows database.secret. Once
# resources are on, every re-render must include both settings: a render
# without them removes the runner's namespace and permissions.
#
# Cluster capacity (forge cluster, for admins) reads the nodes, the pods and
# metrics-server with forge-api's ServiceAccount, so it needs previews; lines
# with a trailing "clusters" marker exist only then:
#   FORGE_CLUSTERS=1 FORGE_CLUSTER_NAME=default FORGE_GRAFANA_URL=https://grafana.example.com ...
# FORGE_CLUSTER_NAME (default "default") is its name in clusters/<name>;
# FORGE_GRAFANA_URL, optional, is linked from it. FORGE_PROMETHEUS_URL,
# optional, adds node disk and volume use (for example
# http://prometheus-prometheus.monitoring.svc:9090).
set -eu

usage() {
  echo "usage: $0 <domain>                 (for example 192.168.1.50.nip.io)" >&2
  echo "       $0 --lb <dex-ip> <api-ip>   (for example --lb 10.150.1.220 10.150.1.221)" >&2
  echo "       $0 --cloud-lb <dex-host> <api-host>   (for example --cloud-lb dex.example.com forge.example.com)" >&2
  exit 2
}

dir=$(dirname "$0")
image=${FORGE_API_IMAGE:-forge-api:dev}
ingress_class=${FORGE_INGRESS_CLASS:-traefik}
case $ingress_class in
  *[!A-Za-z0-9.-]*) echo "invalid FORGE_INGRESS_CLASS: $ingress_class" >&2; exit 2 ;;
esac
# CloudNativePG's storage class: unset uses the cluster's own default
# StorageClass; a name that doesn't exist on the target cluster is
# Kubernetes' own error to surface, not something render.sh checks for.
db_storage_class=${FORGE_DB_STORAGE_CLASS:-}
if [ -n "$db_storage_class" ]; then
  case $db_storage_class in
    *[!A-Za-z0-9.-]*) echo "invalid FORGE_DB_STORAGE_CLASS: $db_storage_class" >&2; exit 2 ;;
  esac
  dbstorage_marks='s/ *#dbstorage$//'
else
  dbstorage_marks='/#dbstorage$/d'
fi
# Backups of forge-db to Azure Blob (backups.yaml.tmpl): both or neither.
db_backup_dest=${FORGE_DB_BACKUP_DEST:-}
db_backup_item=${FORGE_DB_BACKUP_ITEM:-}
if [ -n "$db_backup_dest$db_backup_item" ]; then
  # The character-class guards first: grep anchors per line, not per string.
  case $db_backup_dest in
    *[!a-z0-9.:/-]*) echo "invalid FORGE_DB_BACKUP_DEST: $db_backup_dest (like https://<account>.blob.core.windows.net/<container>)" >&2; exit 2 ;;
  esac
  printf '%s' "$db_backup_dest" | grep -Eq '^https://[a-z0-9]{3,24}\.blob\.core\.windows\.net/[a-z0-9]([a-z0-9-]{1,61}[a-z0-9])?$' ||
    { echo "invalid FORGE_DB_BACKUP_DEST: $db_backup_dest (like https://<account>.blob.core.windows.net/<container>)" >&2; exit 2; }
  case $db_backup_item in
    vaults/*/items/*) ;;
    *) echo "FORGE_DB_BACKUP_DEST needs FORGE_DB_BACKUP_ITEM, a 1Password item like vaults/Kube/items/forge-backup-storage" >&2; exit 2 ;;
  esac
  case $db_backup_item in
    *[!A-Za-z0-9./_-]*) echo "invalid FORGE_DB_BACKUP_ITEM: $db_backup_item" >&2; exit 2 ;;
  esac
  backups_marks='s/ *#backups$//'
else
  backups_marks='/#backups$/d'
fi
dex_ip= api_ip=

case ${1:-} in
  --lb)
    [ $# -eq 3 ] || usage
    dex_ip=$2 api_ip=$3
    for ip in "$dex_ip" "$api_ip"; do
      case $ip in
        ''|*[!0-9.]*) echo "invalid IP: $ip" >&2; exit 2 ;;
      esac
    done
    dex_host=dex.$dex_ip.nip.io api_host=forge.$api_ip.nip.io
    exposure=$dir/loadbalancer.yaml.tmpl
    ;;
  --cloud-lb)
    [ $# -eq 3 ] || usage
    dex_host=$2 api_host=$3
    for h in "$dex_host" "$api_host"; do
      case $h in
        *[!A-Za-z0-9.-]*) echo "invalid hostname: $h" >&2; exit 2 ;;
        *[!0-9.]*) ;;
        *) echo "invalid hostname: $h (looks like an IP; --cloud-lb takes hostnames, not addresses)" >&2; exit 2 ;;
      esac
    done
    exposure=$dir/cloud-lb.yaml.tmpl
    ;;
  ''|-*) usage ;;
  *)
    [ $# -eq 1 ] || usage
    case $1 in
      *[!A-Za-z0-9.-]*) echo "invalid domain: $1" >&2; exit 2 ;;
    esac
    dex_host=dex.$1 api_host=forge.$1
    exposure=$dir/ingress.yaml.tmpl
    ;;
esac

case $image in
  *[!A-Za-z0-9./:@_-]*) echo "invalid image: $image" >&2; exit 2 ;;
esac

app_id=${FORGE_GITHUB_APP_ID:-}
org=${FORGE_GITHUB_ORG:?FORGE_GITHUB_ORG is required}
templates_repo=${FORGE_TEMPLATES_REPO:-$org/forge-templates}
forge_repo=${FORGE_REPO:-$org/forge}
deploy_repo=${FORGE_DEPLOY_REPO:-$org/forge-deploys}
chart_repo=${FORGE_CHART_REPO:-$org/forge-chart}
# GHCR (and OCI references generally) require a lower-case path; release.yml
# lowercases its own push target the same way, so push and pull always agree
# even when FORGE_GITHUB_ORG has upper case in it.
chart_repo=$(printf '%s' "$chart_repo" | tr '[:upper:]' '[:lower:]')
case $app_id in
  *[!0-9]*) echo "invalid FORGE_GITHUB_APP_ID: $app_id" >&2; exit 2 ;;
esac
case $org$templates_repo$forge_repo$deploy_repo$chart_repo in
  *[!A-Za-z0-9./_-]*) echo "invalid FORGE_GITHUB_ORG, FORGE_TEMPLATES_REPO, FORGE_REPO, FORGE_DEPLOY_REPO or FORGE_CHART_REPO" >&2; exit 2 ;;
esac
onepassword_item=${FORGE_ONEPASSWORD_ITEM:-}
case $onepassword_item in
  *[!A-Za-z0-9./_-]*) echo "invalid FORGE_ONEPASSWORD_ITEM: $onepassword_item" >&2; exit 2 ;;
esac
if [ -n "$onepassword_item" ] && [ -z "$app_id" ]; then
  echo "FORGE_ONEPASSWORD_ITEM needs FORGE_GITHUB_APP_ID" >&2; exit 2
fi
dex_org=${FORGE_DEX_GITHUB_ORG:-$org}
case $dex_org in
  ''|*[!A-Za-z0-9-]*) echo "invalid FORGE_DEX_GITHUB_ORG: $dex_org" >&2; exit 2 ;;
esac
# No colon: an explicitly empty FORGE_WRITER_GROUPS is refused, not defaulted.
writer_groups=${FORGE_WRITER_GROUPS-$dex_org:platform-engineering}
case $writer_groups in
  '') echo "FORGE_WRITER_GROUPS must not be empty: every login could deploy to production" >&2; exit 2 ;;
  *[!A-Za-z0-9:,_.-]*) echo "invalid FORGE_WRITER_GROUPS: $writer_groups" >&2; exit 2 ;;
esac
# A value without a group (",", "a,", "a,,b") would reach forge-api as an empty list.
case $writer_groups in
  ,*|*,|*,,*) echo "FORGE_WRITER_GROUPS must not be empty: every login could deploy to production" >&2; exit 2 ;;
esac
case $writer_groups in
  *[A-Za-z0-9]*) ;;
  *) echo "FORGE_WRITER_GROUPS must not be empty: every login could deploy to production" >&2; exit 2 ;;
esac
dex_onepassword_item=${FORGE_DEX_ONEPASSWORD_ITEM:-}
case $dex_onepassword_item in
  *[!A-Za-z0-9./_-]*) echo "invalid FORGE_DEX_ONEPASSWORD_ITEM: $dex_onepassword_item" >&2; exit 2 ;;
esac
preview_ip=${FORGE_PREVIEW_IP:-}
case $preview_ip in
  *[!0-9.]*) echo "invalid FORGE_PREVIEW_IP: $preview_ip" >&2; exit 2 ;;
esac
gateway_class=${FORGE_GATEWAY_CLASS:-istio}
case $gateway_class in
  *[!A-Za-z0-9.-]*) echo "invalid FORGE_GATEWAY_CLASS: $gateway_class" >&2; exit 2 ;;
esac
gateway_domain=${FORGE_GATEWAY_DOMAIN:-}
case $gateway_domain in
  *[!A-Za-z0-9.-]*) echo "invalid FORGE_GATEWAY_DOMAIN: $gateway_domain" >&2; exit 2 ;;
esac
if [ -n "$preview_ip" ] && [ -n "$gateway_domain" ]; then
  echo "set only one of FORGE_PREVIEW_IP and FORGE_GATEWAY_DOMAIN" >&2; exit 2
fi
# previews_on is true in either address mode; preview_ip itself still gates
# the MetalLB-specific annotation and the nip.io IP-derived hostname.
previews_on=
[ -n "$preview_ip" ] && previews_on=1
[ -n "$gateway_domain" ] && previews_on=1
if [ -n "$preview_ip" ]; then
  preview_base=$preview_ip.nip.io
elif [ -n "$gateway_domain" ]; then
  preview_base=$gateway_domain
else
  preview_base= # unvalidated without previews; keep it out of the sed
fi
if [ -n "$previews_on" ]; then
  pull_secret=${FORGE_PREVIEW_PULL_SECRET:?FORGE_PREVIEW_PULL_SECRET is required when previews are configured}
  case $pull_secret in
    *[!a-z0-9.-]*) echo "invalid FORGE_PREVIEW_PULL_SECRET: $pull_secret" >&2; exit 2 ;;
  esac
else
  pull_secret= # unvalidated without previews; keep it out of the sed
fi
if [ -n "$previews_on" ] && [ -z "$app_id" ]; then
  echo "previews need FORGE_GITHUB_APP_ID" >&2; exit 2
fi
if [ -n "$previews_on" ]; then
  previews='s/ *#previews$//' # keep the preview lines, drop their marker
else
  previews='/#previews$/d' # no previews: drop the preview lines
fi
if [ -n "$preview_ip" ]; then
  previewip_marks='s/ *#previewip$//' # IP mode: keep the MetalLB annotation
else
  previewip_marks='/#previewip$/d' # domain mode, or no previews: drop it
fi
deploys=${FORGE_DEPLOYS:-}
chart_rev=${FORGE_DEPLOY_CHART_REVISION:-}
case $deploys in
  ''|1) ;;
  *) echo "invalid FORGE_DEPLOYS: $deploys (use 1)" >&2; exit 2 ;;
esac
if [ -n "$deploys" ]; then
  [ -n "$previews_on" ] || { echo "FORGE_DEPLOYS needs previews (FORGE_PREVIEW_IP or FORGE_GATEWAY_DOMAIN)" >&2; exit 2; }
  # The character-class guard first: grep's ^/$ anchor per line, not per
  # string, so a value with an embedded newline could otherwise slip a
  # second "line" past the SemVer check and into the sed chain below.
  case $chart_rev in
    *[!0-9A-Za-z.-]*) echo "FORGE_DEPLOYS needs FORGE_DEPLOY_CHART_REVISION, a chart version, such as 0.20.0" >&2; exit 2 ;;
  esac
  printf '%s' "$chart_rev" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$' ||
    { echo "FORGE_DEPLOYS needs FORGE_DEPLOY_CHART_REVISION, a chart version, such as 0.20.0" >&2; exit 2; }
  deploys_marks='s/ *#deploys$//'
else
  deploys_marks='/#deploys$/d'
  chart_rev= # unvalidated without deploys; keep it out of the sed
fi
resources=${FORGE_RESOURCES:-}
runner_image=${FORGE_RUNNER_IMAGE:-}
case $resources in
  ''|1) ;;
  *) echo "invalid FORGE_RESOURCES: $resources (use 1)" >&2; exit 2 ;;
esac
if [ -n "$resources" ]; then
  [ -n "$deploys" ] || { echo "FORGE_RESOURCES needs FORGE_DEPLOYS" >&2; exit 2; }
  case $runner_image in
    ''|*[!A-Za-z0-9./:@_-]*) echo "FORGE_RESOURCES needs FORGE_RUNNER_IMAGE, the runner image" >&2; exit 2 ;;
  esac
  resources_marks='s/ *#resources$//'
else
  resources_marks='/#resources$/d'
  runner_image= # unvalidated without resources; keep it out of the sed
fi
clusters=${FORGE_CLUSTERS:-}
cluster_name=${FORGE_CLUSTER_NAME:-default}
grafana_url=${FORGE_GRAFANA_URL:-}
prometheus_url=${FORGE_PROMETHEUS_URL:-}
case $clusters in
  ''|1) ;;
  *) echo "invalid FORGE_CLUSTERS: $clusters (use 1)" >&2; exit 2 ;;
esac
if [ -n "$clusters" ]; then
  [ -n "$previews_on" ] || { echo "FORGE_CLUSTERS needs previews (FORGE_PREVIEW_IP or FORGE_GATEWAY_DOMAIN): it uses forge-api's ServiceAccount" >&2; exit 2; }
  # The character-class guards first: grep anchors per line, not per string.
  case $cluster_name in
    *[!a-z0-9-]*) echo "invalid FORGE_CLUSTER_NAME: $cluster_name (a lowercase DNS label, like default)" >&2; exit 2 ;;
  esac
  printf '%s' "$cluster_name" | grep -Eq '^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$' ||
    { echo "invalid FORGE_CLUSTER_NAME: $cluster_name (a lowercase DNS label, like default)" >&2; exit 2; }
  clusters_marks='s/ *#clusters$//'
  if [ -n "$grafana_url" ]; then
    case $grafana_url in
      *[!a-z0-9.:/_~-]*) echo "invalid FORGE_GRAFANA_URL: $grafana_url (lower case, like https://grafana.example.com)" >&2; exit 2 ;;
    esac
    printf '%s' "$grafana_url" | grep -Eq '^https?://[a-z0-9]([a-z0-9.-]*[a-z0-9])?(:[0-9]{1,5})?(/[a-z0-9._~/-]*)?$' ||
      { echo "invalid FORGE_GRAFANA_URL: $grafana_url (an http or https address, like https://grafana.example.com)" >&2; exit 2; }
    grafana_marks='s/ *#grafana$//'
  else
    grafana_marks='/#grafana$/d'
  fi
  if [ -n "$prometheus_url" ]; then
    case $prometheus_url in
      *[!a-z0-9.:/_~-]*) echo "invalid FORGE_PROMETHEUS_URL: $prometheus_url (lower case, like http://prometheus.monitoring.svc:9090)" >&2; exit 2 ;;
    esac
    printf '%s' "$prometheus_url" | grep -Eq '^https?://[a-z0-9]([a-z0-9.-]*[a-z0-9])?(:[0-9]{1,5})?(/[a-z0-9._~/-]*)?$' ||
      { echo "invalid FORGE_PROMETHEUS_URL: $prometheus_url (an http or https address, like http://prometheus.monitoring.svc:9090)" >&2; exit 2; }
    prometheus_marks='s/ *#prometheus$//'
  else
    prometheus_marks='/#prometheus$/d'
  fi
else
  [ -z "$grafana_url" ] || { echo "FORGE_GRAFANA_URL needs FORGE_CLUSTERS" >&2; exit 2; }
  [ -z "$prometheus_url" ] || { echo "FORGE_PROMETHEUS_URL needs FORGE_CLUSTERS" >&2; exit 2; }
  clusters_marks='/#clusters$/d'
  grafana_marks='/#grafana$/d'
  prometheus_marks='/#prometheus$/d'
  cluster_name= # unvalidated without clusters; keep it out of the sed
fi
# The web dashboard: its addresses become Dex's forge-web client (redirect
# URI <origin>/auth/callback), Dex's allowed origins and forge-api's CORS list.
web_origins=${FORGE_WEB_ORIGINS:-}
web_list=
web_redirects=
if [ -n "$web_origins" ]; then
  # Lower case only: a browser sends its origin in lower case, and both Dex
  # and forge-api compare it as text.
  case $web_origins in
    *[!a-z0-9.:/,-]*) echo "invalid FORGE_WEB_ORIGINS: $web_origins (lower case; letters, digits, dots and hyphens)" >&2; exit 2 ;;
  esac
  old_ifs=$IFS
  IFS=,
  for origin in $web_origins; do
    case $origin in
      http://?*|https://?*) ;;
      *) echo "invalid FORGE_WEB_ORIGINS: \"$origin\" is not http://host or https://host" >&2; exit 2 ;;
    esac
    case ${origin#*://} in
      */*) echo "invalid FORGE_WEB_ORIGINS: \"$origin\" must not have a path or a trailing slash" >&2; exit 2 ;;
      *:*:*|*:|:*|*:*[!0-9]*) echo "invalid FORGE_WEB_ORIGINS: \"$origin\" has a bad port" >&2; exit 2 ;;
    esac
    case $origin in
      http://*:80|https://*:443) echo "invalid FORGE_WEB_ORIGINS: \"$origin\": leave out the scheme's own port, as a browser does" >&2; exit 2 ;;
    esac
    web_list="$web_list${web_list:+, }\"$origin\""
    web_redirects="$web_redirects${web_redirects:+, }\"$origin/auth/callback\""
  done
  IFS=$old_ifs
  web_marks='s/ *#web$//'
else
  web_marks='/#web$/d'
fi
# FORGE_DEX_THEME=1 dresses Dex's pages (sign-in, the CLI's device code,
# errors) in Forge's look: the files of deploy/platform/chart/files/dex-theme
# (shared with the platform chart) become the ConfigMap dex-theme. Lines with
# a trailing "theme" marker exist only then.
theme_dir="$dir/../platform/chart/files/dex-theme"
theme_text="styles.css header.html logo.svg"
theme_binary="favicon.png archivo.woff2 plex-sans-400.woff2 plex-sans-600.woff2"
case ${FORGE_DEX_THEME:-} in
  1) theme=1 ;;
  '') theme= ;;
  *) echo "invalid FORGE_DEX_THEME: \"$FORGE_DEX_THEME\": set it to 1, or leave it empty for Dex's own look" >&2; exit 2 ;;
esac
if [ -n "$theme" ]; then
  for f in $theme_text $theme_binary; do
    [ -f "$theme_dir/$f" ] || { echo "FORGE_DEX_THEME: $theme_dir/$f is missing" >&2; exit 2; }
  done
  # shellcheck disable=SC2086 # the lists are words on purpose
  theme_sum=$(cd "$theme_dir" && cat $theme_text $theme_binary | { sha256sum 2>/dev/null || shasum -a 256; } | cut -c1-16)
  theme_marks='s/ *#theme$//'
else
  theme_sum=
  theme_marks='/#theme$/d'
fi
# theme_configmap prints the ConfigMap dex-theme: text files as they are,
# fonts and the icon as base64.
theme_configmap() {
  printf 'apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: dex-theme\n  namespace: forge\ndata:\n'
  for f in $theme_text; do
    printf '  %s: |\n' "$f"
    sed 's/^/    /' "$theme_dir/$f"
  done
  printf 'binaryData:\n'
  for f in $theme_binary; do
    printf '  %s: %s\n' "$f" "$(base64 < "$theme_dir/$f" | tr -d '\n')"
  done
}
# The login's own address. Dex has one, its issuer: what you, the CLI, the
# dashboard and forge-api's pods all use. By default it is the LAN address
# http://<dex host>; FORGE_LOGIN_URL gives it a public HTTPS name, which a
# dashboard served over HTTPS needs (a tunnel or proxy must send that name to
# Dex, and the GitHub OAuth app's callback must be <FORGE_LOGIN_URL>/dex/callback).
login_url=${FORGE_LOGIN_URL:-}
dex_url=http://$dex_host
if [ -n "$login_url" ]; then
  printf '%s' "$login_url" | grep -Eq '^https?://([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}(:[0-9]{1,5})?$' ||
    { echo "invalid FORGE_LOGIN_URL: $login_url (a scheme and a host, lower case, no path, like https://login.example.com)" >&2; exit 2; }
  dex_url=$login_url
fi
# Argo CD's web address: deployments link to their Application there.
argocd_url=${FORGE_ARGOCD_URL:-}
if [ -n "$argocd_url" ]; then
  [ -n "$deploys" ] || { echo "FORGE_ARGOCD_URL needs FORGE_DEPLOYS" >&2; exit 2; }
  printf '%s' "$argocd_url" | grep -Eq '^https?://([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}(:[0-9]{1,5})?$' ||
    { echo "invalid FORGE_ARGOCD_URL: $argocd_url (a scheme and a host, lower case, no path, like https://argocd.example.com)" >&2; exit 2; }
  argocd_marks='s/ *#argocd$//'
else
  argocd_marks='/#argocd$/d'
fi
# The 1Password vaults a service's run.secrets may read. Never one with the
# platform's own secrets (Kube): any writer could put them into a service.
secret_vaults=${FORGE_SECRET_VAULTS:-}
if [ -n "$secret_vaults" ]; then
  [ -n "$deploys" ] || { echo "FORGE_SECRET_VAULTS needs FORGE_DEPLOYS" >&2; exit 2; }
  printf '%s' "$secret_vaults" | grep -Eq '^[A-Za-z0-9._-]+(,[A-Za-z0-9._-]+)*$' ||
    { echo "invalid FORGE_SECRET_VAULTS: $secret_vaults (vault names, comma separated, like Apps,Shared)" >&2; exit 2; }
  vaults_marks='s/ *#vaults$//'
else
  vaults_marks='/#vaults$/d'
fi
# Items in those vaults no service may read: the platform's own credentials.
secret_denied=${FORGE_SECRET_DENIED_ITEMS:-}
if [ -n "$secret_denied" ]; then
  [ -n "$secret_vaults" ] || { echo "FORGE_SECRET_DENIED_ITEMS needs FORGE_SECRET_VAULTS" >&2; exit 2; }
  printf '%s' "$secret_denied" | grep -Eq '^[A-Za-z0-9._-]+(,[A-Za-z0-9._-]+)*$' ||
    { echo "invalid FORGE_SECRET_DENIED_ITEMS: $secret_denied (item names, comma separated)" >&2; exit 2; }
fi
# Which operator populates a run.secrets entry's Secret: forge-api's own
# governance (vaults/<vault>/items/<item> only, or also <group>/<item>) and
# the chart's secretBackend value must agree, so render.sh sets both from
# the same input. Unset (the default) leaves both at "onepassword".
secret_backend=${FORGE_SECRET_BACKEND:-}
if [ -n "$secret_backend" ]; then
  [ -n "$deploys" ] || { echo "FORGE_SECRET_BACKEND needs FORGE_DEPLOYS" >&2; exit 2; }
  case $secret_backend in
    *[!A-Za-z0-9._-]*) echo "invalid FORGE_SECRET_BACKEND: $secret_backend" >&2; exit 2 ;;
  esac
  secretbackend_marks='s/ *#secretbackend$//'
else
  secretbackend_marks='/#secretbackend$/d'
fi
# Public addresses: services reached under real domains, through the
# platform's DNS records and tunnel. Each item is <domain>=<DNS zone name>.
public_domains=${FORGE_PUBLIC_DOMAINS:-}
public_tunnel=${FORGE_PUBLIC_DNS_TUNNEL:-production}
public_listeners=
if [ -n "$public_domains" ]; then
  [ -n "$deploys" ] || { echo "FORGE_PUBLIC_DOMAINS needs FORGE_DEPLOYS" >&2; exit 2; }
  # The same rules as forge-api (ParsePublicDomains), so that what renders also starts.
  case $public_domains in
    *[!a-z0-9.=,-]*) echo "invalid FORGE_PUBLIC_DOMAINS: $public_domains (lower case, like example.com=example-com,other.io=other-io)" >&2; exit 2 ;;
    ,*|*,|*,,*) echo "invalid FORGE_PUBLIC_DOMAINS: $public_domains has an empty item" >&2; exit 2 ;;
  esac
  printf '%s' "$public_tunnel" | grep -Eq '^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$' ||
    { echo "invalid FORGE_PUBLIC_DNS_TUNNEL: $public_tunnel (a name such as production)" >&2; exit 2; }
  seen=,
  old_ifs=$IFS
  IFS=,
  for item in $public_domains; do
    domain=${item%%=*}
    zone=${item#*=}
    case $item in
      *=*=*|*=|=*) echo "invalid FORGE_PUBLIC_DOMAINS: \"$item\" is not <domain>=<zone name>" >&2; exit 2 ;;
      *=*) ;;
      *) echo "invalid FORGE_PUBLIC_DOMAINS: \"$item\" is not <domain>=<zone name>" >&2; exit 2 ;;
    esac
    printf '%s' "$domain" | grep -Eq '^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$' ||
      { echo "invalid FORGE_PUBLIC_DOMAINS: \"$domain\" is not a domain such as example.com" >&2; exit 2; }
    printf '%s' "$zone" | grep -Eq '^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$' ||
      { echo "invalid FORGE_PUBLIC_DOMAINS: the zone name \"$zone\" is a name such as example-com" >&2; exit 2; }
    case $zone in
      *-root) echo "invalid FORGE_PUBLIC_DOMAINS: the zone name \"$zone\" cannot end in -root (public-<zone>-root is a domain's root listener)" >&2; exit 2 ;;
    esac
    case $seen in
      *,$domain,*) echo "invalid FORGE_PUBLIC_DOMAINS: $domain is listed twice" >&2; exit 2 ;;
    esac
    seen=$seen$domain,
    # One listener per domain: it takes only hosts under that domain, and
    # only routes from the namespace of a Forge deployment, never a preview.
    # *.<domain> does not match the domain itself: its root has a listener
    # of its own, for a service whose address is set to the root.
    for listener in "public-$zone *.$domain" "public-$zone-root $domain"; do
      public_listeners="$public_listeners    - name: ${listener% *}
      protocol: HTTP
      port: 80
      hostname: \"${listener#* }\"
      allowedRoutes:
        namespaces:
          from: Selector
          selector:
            matchLabels:
              app.kubernetes.io/managed-by: forge
            matchExpressions:
              - {key: forge.dev/deploy-env, operator: Exists}
"
    done
  done
  IFS=$old_ifs
  # A chart from before public addresses ignores them: the deployment would
  # report success and be reachable nowhere. Checked against the published
  # chart itself, not local git history: helm (and network access to
  # ghcr.io, or prior `helm registry login` if the package is private) are
  # both required once FORGE_PUBLIC_DOMAINS is set.
  command -v helm >/dev/null 2>&1 ||
    { echo "FORGE_PUBLIC_DOMAINS needs helm on PATH, to check the published chart for templates/dns.yaml" >&2; exit 2; }
  chart_check_dir=$(mktemp -d)
  if ! helm pull "oci://ghcr.io/$chart_repo" --version "$chart_rev" -d "$chart_check_dir" \
      >"$chart_check_dir/pull.log" 2>&1; then
    echo "FORGE_PUBLIC_DOMAINS: could not fetch oci://ghcr.io/$chart_repo version $chart_rev to check for templates/dns.yaml. This needs network access to ghcr.io (or, if the package is private, a prior 'helm registry login ghcr.io'); it is not necessarily that FORGE_DEPLOY_CHART_REVISION itself is wrong:" >&2
    cat "$chart_check_dir/pull.log" >&2
    rm -rf "$chart_check_dir"
    exit 2
  fi
  if ! tar tzf "$chart_check_dir"/*.tgz | grep -Eq '^[^/]+/templates/dns\.yaml$'; then
    rm -rf "$chart_check_dir"
    echo "FORGE_PUBLIC_DOMAINS needs a FORGE_DEPLOY_CHART_REVISION whose chart makes DNS records: $chart_rev has no templates/dns.yaml" >&2
    exit 2
  fi
  rm -rf "$chart_check_dir"
  public_marks='s/ *#public$//'
else
  public_marks='/#public$/d'
  public_tunnel= # unvalidated without public domains; keep it out of the sed
fi
if [ -n "$app_id" ]; then
  app='s/ *#app$//' # keep the App lines, drop their marker
else
  app='/#app$/d' # no App: drop the App lines
fi

{
  cat "$dir/forge.yaml.tmpl"
  echo "---"
  cat "$exposure"
  echo "---"
  cat "$dir/database.yaml.tmpl"
  if [ -n "$db_backup_dest" ]; then
    echo "---"
    cat "$dir/backups.yaml.tmpl"
  fi
  if [ -n "$onepassword_item" ]; then
    echo "---"
    cat "$dir/onepassword.yaml.tmpl"
  fi
  if [ -n "$dex_onepassword_item" ]; then
    echo "---"
    cat "$dir/dex-onepassword.yaml.tmpl"
  fi
  if [ -n "$previews_on" ]; then
    echo "---"
    cat "$dir/previews.yaml.tmpl"
  fi
  if [ -n "$deploys" ]; then
    echo "---"
    cat "$dir/deploys.yaml.tmpl"
  fi
  if [ -n "$resources" ]; then
    echo "---"
    cat "$dir/resources.yaml.tmpl"
  fi
  if [ -n "$clusters" ]; then
    echo "---"
    cat "$dir/clusters.yaml.tmpl"
  fi
} | sed -e "$app" -e "$web_marks" -e "$theme_marks" -e "s|\${DEX_THEME_SUM}|$theme_sum|g" -e "$public_marks" -e "$argocd_marks" -e "s|\${FORGE_ARGOCD_URL}|$argocd_url|g" -e "$vaults_marks" -e "s|\${FORGE_SECRET_VAULTS}|$secret_vaults|g" -e "s|\${FORGE_SECRET_DENIED_ITEMS}|$secret_denied|g" -e "$secretbackend_marks" -e "s|\${FORGE_SECRET_BACKEND}|$secret_backend|g" -e "s|\${FORGE_PUBLIC_DOMAINS}|$public_domains|g" -e "s|\${FORGE_PUBLIC_DNS_TUNNEL}|$public_tunnel|g" -e "s|\${WEB_ORIGINS_YAML}|$web_list|g" -e "s|\${WEB_REDIRECTS_YAML}|$web_redirects|g" -e "s|\${FORGE_WEB_ORIGINS}|$web_origins|g" -e "$previews" -e "$previewip_marks" -e "$deploys_marks" -e "$resources_marks" -e "s|\${FORGE_RUNNER_IMAGE}|$runner_image|g" -e "$clusters_marks" -e "$grafana_marks" -e "$prometheus_marks" -e "s|\${FORGE_CLUSTER_NAME}|$cluster_name|g" -e "s|\${FORGE_GRAFANA_URL}|$grafana_url|g" -e "s|\${FORGE_PROMETHEUS_URL}|$prometheus_url|g" -e "s|\${DEPLOY_CHART_REVISION}|$chart_rev|g" -e "s|\${PREVIEW_IP}|$preview_ip|g" -e "s|\${PREVIEW_BASE}|$preview_base|g" -e "s|\${GATEWAY_CLASS}|$gateway_class|g" -e "s|\${PULL_SECRET}|$pull_secret|g" -e "s|\${FORGE_GITHUB_APP_ID}|$app_id|g" -e "s|\${FORGE_GITHUB_ORG}|$org|g" \
        -e "s|\${FORGE_TEMPLATES_REPO}|$templates_repo|g" -e "s|\${FORGE_REPO}|$forge_repo|g" -e "s|\${FORGE_DEPLOY_REPO}|$deploy_repo|g" -e "s|\${FORGE_CHART_REPO}|$chart_repo|g" -e "s|\${FORGE_ONEPASSWORD_ITEM}|$onepassword_item|g" -e "s|\${FORGE_DEX_GITHUB_ORG}|$dex_org|g" -e "s|\${FORGE_WRITER_GROUPS}|$writer_groups|g" -e "s|\${FORGE_DEX_ONEPASSWORD_ITEM}|$dex_onepassword_item|g" -e "s|\${DEX_URL}|$dex_url|g" -e "s|\${DEX_HOST}|$dex_host|g" -e "s|\${API_HOST}|$api_host|g" \
        -e "s|\${DEX_IP}|$dex_ip|g" -e "s|\${API_IP}|$api_ip|g" -e "s|\${INGRESS_CLASS}|$ingress_class|g" -e "$dbstorage_marks" -e "$backups_marks" -e "s|\${FORGE_DB_BACKUP_DEST}|$db_backup_dest|g" -e "s|\${FORGE_DB_BACKUP_ITEM}|$db_backup_item|g" -e "s|\${DB_STORAGE_CLASS}|$db_storage_class|g" \
        -e "s|\${FORGE_API_IMAGE}|$image|g" |
  PUBLIC_LISTENERS=$public_listeners awk '/#public-listeners$/ { printf "%s", ENVIRON["PUBLIC_LISTENERS"]; next } { print }'
# The theme comes last and outside the sed above: its CSS and HTML must
# reach Dex byte for byte, whatever they end a line with.
if [ -n "$theme" ]; then
  echo "---"
  theme_configmap
fi
