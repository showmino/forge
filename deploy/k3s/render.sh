#!/usr/bin/env sh
# Render the k3s manifests.
#
# Stock k3s, behind the bundled Traefik ingress (dex.<domain>, forge.<domain>):
#   deploy/k3s/render.sh 192.168.1.50.nip.io | kubectl apply -f -
#
# No Traefik: Dex and forge-api on their own LoadBalancer IPs, such as from MetalLB
# (dex.<dex-ip>.nip.io, forge.<api-ip>.nip.io):
#   deploy/k3s/render.sh --lb 10.150.1.220 10.150.1.221 | kubectl apply -f -
#
# FORGE_API_IMAGE picks the image (default forge-api:dev):
#   FORGE_API_IMAGE=ghcr.io/showmino/forge-api:v0.1.0 deploy/k3s/render.sh forge.lan | kubectl apply -f -
#
# With the Forge GitHub App (templates and 'forge create'), create the Secret
# first, then render with the App's ID (FORGE_GITHUB_ORG defaults to showmino,
# FORGE_TEMPLATES_REPO to showmino/forge-templates):
#   kubectl -n forge create secret generic forge-github-app --from-file=github_app_private_key=<key.pem>
#   FORGE_GITHUB_APP_ID=5239131 FORGE_API_IMAGE=... deploy/k3s/render.sh --lb <dex-ip> <api-ip> | kubectl apply -f -
# or let the 1Password operator create that Secret from an item with a
# github_app_private_key field:
#   FORGE_ONEPASSWORD_ITEM=vaults/Kube/items/forge-github-app FORGE_GITHUB_APP_ID=5239131 ... | kubectl apply -f -
#
# Login is through GitHub (Dex's GitHub connector): only members of
# FORGE_DEX_GITHUB_ORG (default FORGE_GITHUB_ORG) log in, and only the teams
# in FORGE_WRITER_GROUPS (default <org>:platform-engineering; never empty) may
# change anything. Dex reads the OAuth App's client_id and client_secret from
# the Secret dex-github, which the 1Password operator can create:
#   FORGE_DEX_ONEPASSWORD_ITEM=vaults/Kube/items/forge-dex-github ... | kubectl apply -f -
#
# Preview environments (forge env) need the App too, plus the IP of their
# Gateway and the pull secret in namespace forge (default mino-ghcr); lines in
# forge.yaml.tmpl with a trailing "previews" marker exist only then:
#   FORGE_PREVIEW_IP=10.150.1.222 FORGE_GITHUB_APP_ID=5239131 ... | kubectl apply -f -
#
# The web dashboard (Phase 6) needs its addresses, comma-separated, each a
# scheme and a host; lines with a trailing "web" marker exist only then:
#   FORGE_WEB_ORIGINS=http://forge-web.staging.<domain>,http://forge-web.production.<domain> ... | kubectl apply -f -
# It adds Dex's public client forge-web and lets those pages call forge-api
# from a browser. Dex reads its config at start: restart it after a change.
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
# Deploys (forge deploy) need previews, plus a commit on origin/main that Argo
# CD takes the chart from (an unpushed commit makes every Application fail);
# lines with a trailing "deploys" marker exist only then:
#   FORGE_DEPLOYS=1 FORGE_DEPLOY_CHART_REVISION=$(git rev-parse origin/main) FORGE_PREVIEW_IP=... | kubectl apply -f -
# Once deploys are on, every re-render must include FORGE_DEPLOYS=1 and the
# current FORGE_DEPLOY_CHART_REVISION (read it with `kubectl -n argocd get
# applicationset forge-services -o jsonpath='{.spec.template.spec.sources[0].targetRevision}'`);
# a render without them removes the staging and production routes. Changing the
# revision re-renders every deployed service.
#
# Resources (forge resource) need deploys, plus the image of the runner Job;
# lines with a trailing "resources" marker exist only then:
#   FORGE_RESOURCES=1 FORGE_RUNNER_IMAGE=ghcr.io/showmino/forge-runner:<tag> FORGE_DEPLOYS=1 ... | kubectl apply -f -
# The chart revision must be one whose chart knows database.secret. Once
# resources are on, every re-render must include both settings: a render
# without them removes the runner's namespace and permissions.
set -eu

usage() {
  echo "usage: $0 <domain>                 (for example 192.168.1.50.nip.io)" >&2
  echo "       $0 --lb <dex-ip> <api-ip>   (for example --lb 10.150.1.220 10.150.1.221)" >&2
  exit 2
}

dir=$(dirname "$0")
image=${FORGE_API_IMAGE:-forge-api:dev}
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
org=${FORGE_GITHUB_ORG:-showmino}
templates_repo=${FORGE_TEMPLATES_REPO:-showmino/forge-templates}
case $app_id in
  *[!0-9]*) echo "invalid FORGE_GITHUB_APP_ID: $app_id" >&2; exit 2 ;;
esac
case $org$templates_repo in
  *[!A-Za-z0-9./_-]*) echo "invalid FORGE_GITHUB_ORG or FORGE_TEMPLATES_REPO" >&2; exit 2 ;;
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
pull_secret=${FORGE_PREVIEW_PULL_SECRET:-mino-ghcr}
case $preview_ip in
  *[!0-9.]*) echo "invalid FORGE_PREVIEW_IP: $preview_ip" >&2; exit 2 ;;
esac
case $pull_secret in
  ''|*[!a-z0-9.-]*) echo "invalid FORGE_PREVIEW_PULL_SECRET: $pull_secret" >&2; exit 2 ;;
esac
if [ -n "$preview_ip" ] && [ -z "$app_id" ]; then
  echo "FORGE_PREVIEW_IP needs FORGE_GITHUB_APP_ID" >&2; exit 2
fi
if [ -n "$preview_ip" ]; then
  previews='s/ *#previews$//' # keep the preview lines, drop their marker
else
  previews='/#previews$/d' # no previews: drop the preview lines
fi
deploys=${FORGE_DEPLOYS:-}
chart_rev=${FORGE_DEPLOY_CHART_REVISION:-}
case $deploys in
  ''|1) ;;
  *) echo "invalid FORGE_DEPLOYS: $deploys (use 1)" >&2; exit 2 ;;
esac
if [ -n "$deploys" ]; then
  [ -n "$preview_ip" ] || { echo "FORGE_DEPLOYS needs FORGE_PREVIEW_IP" >&2; exit 2; }
  case $chart_rev in
    *[!0-9a-f]*|'') echo "FORGE_DEPLOYS needs FORGE_DEPLOY_CHART_REVISION, a 40-character commit SHA" >&2; exit 2 ;;
  esac
  [ ${#chart_rev} -eq 40 ] || { echo "FORGE_DEPLOY_CHART_REVISION must be a full commit SHA" >&2; exit 2; }
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
    case $seen in
      *,$domain,*) echo "invalid FORGE_PUBLIC_DOMAINS: $domain is listed twice" >&2; exit 2 ;;
    esac
    seen=$seen$domain,
    # One listener per domain: it takes only hosts under that domain, and
    # only routes from the namespace of a Forge deployment, never a preview.
    public_listeners="$public_listeners    - name: public-$zone
      protocol: HTTP
      port: 80
      hostname: \"*.$domain\"
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
  IFS=$old_ifs
  # A chart from before public addresses ignores them: the deployment would
  # report success and be reachable nowhere.
  if git -C "$dir" rev-parse --git-dir >/dev/null 2>&1; then
    git -C "$dir" cat-file -e "$chart_rev:deploy/argocd/chart/templates/dns.yaml" 2>/dev/null ||
      { echo "FORGE_PUBLIC_DOMAINS needs a FORGE_DEPLOY_CHART_REVISION whose chart makes DNS records: $chart_rev has no deploy/argocd/chart/templates/dns.yaml (or is not fetched here)" >&2; exit 2; }
  fi
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
  if [ -n "$onepassword_item" ]; then
    echo "---"
    cat "$dir/onepassword.yaml.tmpl"
  fi
  if [ -n "$dex_onepassword_item" ]; then
    echo "---"
    cat "$dir/dex-onepassword.yaml.tmpl"
  fi
  if [ -n "$preview_ip" ]; then
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
} | sed -e "$app" -e "$web_marks" -e "$public_marks" -e "s|\${FORGE_PUBLIC_DOMAINS}|$public_domains|g" -e "s|\${FORGE_PUBLIC_DNS_TUNNEL}|$public_tunnel|g" -e "s|\${WEB_ORIGINS_YAML}|$web_list|g" -e "s|\${WEB_REDIRECTS_YAML}|$web_redirects|g" -e "s|\${FORGE_WEB_ORIGINS}|$web_origins|g" -e "$previews" -e "$deploys_marks" -e "$resources_marks" -e "s|\${FORGE_RUNNER_IMAGE}|$runner_image|g" -e "s|\${DEPLOY_CHART_REVISION}|$chart_rev|g" -e "s|\${PREVIEW_IP}|$preview_ip|g" -e "s|\${PULL_SECRET}|$pull_secret|g" -e "s|\${FORGE_GITHUB_APP_ID}|$app_id|g" -e "s|\${FORGE_GITHUB_ORG}|$org|g" \
        -e "s|\${FORGE_TEMPLATES_REPO}|$templates_repo|g" -e "s|\${FORGE_ONEPASSWORD_ITEM}|$onepassword_item|g" -e "s|\${FORGE_DEX_GITHUB_ORG}|$dex_org|g" -e "s|\${FORGE_WRITER_GROUPS}|$writer_groups|g" -e "s|\${FORGE_DEX_ONEPASSWORD_ITEM}|$dex_onepassword_item|g" -e "s|\${DEX_HOST}|$dex_host|g" -e "s|\${API_HOST}|$api_host|g" \
        -e "s|\${DEX_IP}|$dex_ip|g" -e "s|\${API_IP}|$api_ip|g" \
        -e "s|\${FORGE_API_IMAGE}|$image|g" |
  PUBLIC_LISTENERS=$public_listeners awk '/#public-listeners$/ { printf "%s", ENVIRON["PUBLIC_LISTENERS"]; next } { print }'
