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
#   FORGE_API_IMAGE=ghcr.io/showmino/forge-api:0.1.0 deploy/k3s/render.sh forge.lan | kubectl apply -f -
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
# Preview environments (forge env) need the App too, plus the IP of their
# Gateway and the pull secret in namespace forge (default mino-ghcr); lines in
# forge.yaml.tmpl with a trailing "previews" marker exist only then:
#   FORGE_PREVIEW_IP=10.150.1.222 FORGE_GITHUB_APP_ID=5239131 ... | kubectl apply -f -
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
if [ -n "$app_id" ]; then
  app='s/ *#app$//' # keep the App lines, drop their marker
else
  app='/#app$/d' # no App: drop the App lines
fi

{
  cat "$dir/forge.yaml.tmpl"
  echo "---"
  cat "$exposure"
  if [ -n "$onepassword_item" ]; then
    echo "---"
    cat "$dir/onepassword.yaml.tmpl"
  fi
  if [ -n "$preview_ip" ]; then
    echo "---"
    cat "$dir/previews.yaml.tmpl"
  fi
} | sed -e "$app" -e "$previews" -e "s|\${PREVIEW_IP}|$preview_ip|g" -e "s|\${PULL_SECRET}|$pull_secret|g" -e "s|\${FORGE_GITHUB_APP_ID}|$app_id|g" -e "s|\${FORGE_GITHUB_ORG}|$org|g" \
        -e "s|\${FORGE_TEMPLATES_REPO}|$templates_repo|g" -e "s|\${FORGE_ONEPASSWORD_ITEM}|$onepassword_item|g" -e "s|\${DEX_HOST}|$dex_host|g" -e "s|\${API_HOST}|$api_host|g" \
        -e "s|\${DEX_IP}|$dex_ip|g" -e "s|\${API_IP}|$api_ip|g" \
        -e "s|\${FORGE_API_IMAGE}|$image|g"
