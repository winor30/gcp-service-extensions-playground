#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
ENV_FILE="$ROOT_DIR/.env"

if [[ ! -f "$ENV_FILE" ]]; then
  echo "missing .env at $ENV_FILE" >&2
  exit 1
fi

set -a
# shellcheck disable=SC1090
source "$ENV_FILE"
set +a

: "${PROJECT_ID?}"
: "${REGION?}"
: "${LB_PREFIX?}"

WARMUP_RUNS="${WARMUP_RUNS:-5}"
MEASURE_RUNS="${MEASURE_RUNS:-10}"

get_ip() {
  local scope="$1"
  local name="$2"
  if [[ "$scope" == "global" ]]; then
    gcloud compute addresses describe "$name" --global --format='get(address)'
  else
    gcloud compute addresses describe "$name" --region "$REGION" --format='get(address)'
  fi
}

LB1_IP="$(get_ip regional "${LB_PREFIX}-ip")"
LB2_IP="$(get_ip regional "${LB_PREFIX}-proc-ip")"
LB3_IP="$(get_ip global "${LB_PREFIX}-wasm-ip")"

run_client() {
  local label="$1"
  local target="$2"
  echo "=== ${label} (${target}) ==="
  WARMUP_RUNS="$WARMUP_RUNS" MEASURE_RUNS="$MEASURE_RUNS" TARGET_URL="http://${target}" \
    go run "$ROOT_DIR/cmd/client"
  echo
}

run_client "LB1 ext_authz" "$LB1_IP"
run_client "LB2 ext_proc" "$LB2_IP"
run_client "LB3 proxy_wasm" "$LB3_IP"
