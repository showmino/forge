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

{
  cat "$dir/forge.yaml.tmpl"
  echo "---"
  cat "$exposure"
} | sed -e "s|\${DEX_HOST}|$dex_host|g" -e "s|\${API_HOST}|$api_host|g" \
        -e "s|\${DEX_IP}|$dex_ip|g" -e "s|\${API_IP}|$api_ip|g" \
        -e "s|\${FORGE_API_IMAGE}|$image|g"
