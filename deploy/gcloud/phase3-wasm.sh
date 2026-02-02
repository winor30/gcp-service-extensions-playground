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

case "$PUBLIC_KEY_PEM_FILE" in
  /*) ;;
  *) PUBLIC_KEY_PEM_FILE="$ROOT_DIR/$PUBLIC_KEY_PEM_FILE" ;;
esac

: "${PROJECT_ID?}"
: "${REGION?}"
: "${AR_LOCATION?}"
: "${AR_REPO?}"
: "${LB_PREFIX?}"
: "${LB_NETWORK?}"
: "${ORIGIN_SERVICE?}"
: "${PUBLIC_KEY_PEM_FILE?}"

if [[ ! -f "$PUBLIC_KEY_PEM_FILE" ]]; then
  echo "public key not found: $PUBLIC_KEY_PEM_FILE" >&2
  exit 1
fi

if [[ "$AR_LOCATION" != "us" ]]; then
  echo "AR_LOCATION must be multi-region \"us\" for proxy_wasm with global LB (current: $AR_LOCATION)" >&2
  exit 1
fi

PUBLIC_KEY_PEM=$(awk '{printf "%s\\n", $0}' "$PUBLIC_KEY_PEM_FILE")
LB_WASM_PREFIX="${LB_PREFIX}-wasm"
WASM_PLUGIN_VERSION="${WASM_PLUGIN_VERSION:-v1}"
WASM_LOCATION="global"
LB_WASM_SCOPE="global"
LOG_CONFIG="enable=true,sample-rate=1.0,min-log-level=INFO"

IP_NAME="${LB_WASM_PREFIX}-ip"
NEG_ORIGIN="${LB_WASM_PREFIX}-neg-origin"
BS_ORIGIN="${LB_WASM_PREFIX}-bs-origin"
URL_MAP="${LB_WASM_PREFIX}-urlmap"
TARGET_PROXY="${LB_WASM_PREFIX}-proxy"
FR_NAME="${LB_WASM_PREFIX}-fr"
TRAFFIC_EXT="${LB_WASM_PREFIX}-traffic-ext"
WASM_PLUGIN="${LB_WASM_PREFIX}-plugin"

REPO_HOST="${AR_LOCATION}-docker.pkg.dev"
REPO_URI="${REPO_HOST}/${PROJECT_ID}/${AR_REPO}"
WASM_IMAGE="${REPO_URI}/${WASM_PLUGIN}:${WASM_PLUGIN_VERSION}"
ORIGIN_IMAGE="${REPO_URI}/${ORIGIN_SERVICE}:v1"

CONFIG_FILE="$(mktemp)"
EXT_FILE="$(mktemp)"
trap 'rm -f "$CONFIG_FILE" "$EXT_FILE"' EXIT

cat > "$CONFIG_FILE" <<EOF_CONFIG
{"public_key_pem":"${PUBLIC_KEY_PEM}"}
EOF_CONFIG

cd "$ROOT_DIR"

gcloud config set project "$PROJECT_ID"

gcloud services enable \
  run.googleapis.com \
  compute.googleapis.com \
  cloudbuild.googleapis.com \
  artifactregistry.googleapis.com \
  networkservices.googleapis.com

# Artifact Registry (docker)
if ! gcloud artifacts repositories describe "$AR_REPO" --location "$AR_LOCATION" >/dev/null 2>&1; then
  gcloud artifacts repositories create "$AR_REPO" \
    --repository-format=docker \
    --location="$AR_LOCATION"
fi

# Build plugin image
GOWORK=off gcloud builds submit "$ROOT_DIR" \
  --config "$ROOT_DIR/plugins/wasm-jwt/package/cloudbuild.yaml" \
  --substitutions _IMAGE="$WASM_IMAGE"

# Build origin image (HTTP)
GOWORK=off gcloud builds submit "$ROOT_DIR" \
  --config "$ROOT_DIR/deploy/gcloud/cloudbuild.yaml" \
  --substitutions _IMAGE="$ORIGIN_IMAGE",_TARGET="cmd/origin-server"

# Cloud Run: origin (HTTP)
gcloud run deploy "$ORIGIN_SERVICE" \
  --region "$REGION" \
  --image "$ORIGIN_IMAGE" \
  --allow-unauthenticated

# Serverless NEG
if ! gcloud compute network-endpoint-groups describe "$NEG_ORIGIN" --region "$REGION" >/dev/null 2>&1; then
  gcloud compute network-endpoint-groups create "$NEG_ORIGIN" \
    --region "$REGION" \
    --network-endpoint-type=serverless \
    --cloud-run-service="$ORIGIN_SERVICE"
fi

# Backend service
if ! gcloud compute backend-services describe "$BS_ORIGIN" --global >/dev/null 2>&1; then
  gcloud compute backend-services create "$BS_ORIGIN" \
    --global \
    --load-balancing-scheme=EXTERNAL_MANAGED \
    --protocol=HTTP
fi

ORIGIN_BACKENDS=$(gcloud compute backend-services describe "$BS_ORIGIN" --global --format='value(backends[].group)')
if ! echo "$ORIGIN_BACKENDS" | grep -q "/networkEndpointGroups/${NEG_ORIGIN}$"; then
  gcloud compute backend-services add-backend "$BS_ORIGIN" \
    --global \
    --network-endpoint-group="$NEG_ORIGIN" \
    --network-endpoint-group-region="$REGION"
fi

# Global external HTTP LB
if ! gcloud compute addresses describe "$IP_NAME" --global >/dev/null 2>&1; then
  gcloud compute addresses create "$IP_NAME" \
    --global \
    --network-tier=PREMIUM
fi

if ! gcloud compute url-maps describe "$URL_MAP" --global >/dev/null 2>&1; then
  gcloud compute url-maps create "$URL_MAP" \
    --global \
    --default-service="$BS_ORIGIN"
fi

if ! gcloud compute target-http-proxies describe "$TARGET_PROXY" --global >/dev/null 2>&1; then
  gcloud compute target-http-proxies create "$TARGET_PROXY" \
    --global \
    --url-map="$URL_MAP"
fi

if ! gcloud compute forwarding-rules describe "$FR_NAME" --global >/dev/null 2>&1; then
  gcloud compute forwarding-rules create "$FR_NAME" \
    --global \
    --load-balancing-scheme=EXTERNAL_MANAGED \
    --network-tier=PREMIUM \
    --target-http-proxy="$TARGET_PROXY" \
    --ports=80 \
    --address="$IP_NAME"
fi

# WASM plugin
if ! gcloud service-extensions wasm-plugins describe "$WASM_PLUGIN" --location "$WASM_LOCATION" >/dev/null 2>&1; then
  gcloud service-extensions wasm-plugins create "$WASM_PLUGIN" \
    --location "$WASM_LOCATION" \
    --image "$WASM_IMAGE" \
    --main-version "$WASM_PLUGIN_VERSION" \
    --plugin-config-file "$CONFIG_FILE" \
    --log-config "$LOG_CONFIG"
else
  if ! gcloud service-extensions wasm-plugin-versions describe "$WASM_PLUGIN_VERSION" \
    --location "$WASM_LOCATION" \
    --wasm-plugin "$WASM_PLUGIN" >/dev/null 2>&1; then
    gcloud service-extensions wasm-plugin-versions create "$WASM_PLUGIN_VERSION" \
      --location "$WASM_LOCATION" \
      --wasm-plugin "$WASM_PLUGIN" \
      --image "$WASM_IMAGE" \
      --plugin-config-file "$CONFIG_FILE"
  fi
  gcloud service-extensions wasm-plugins update "$WASM_PLUGIN" \
    --location "$WASM_LOCATION" \
    --main-version "$WASM_PLUGIN_VERSION" \
    --log-config "$LOG_CONFIG"
fi

cat > "$EXT_FILE" <<EOF_EXT
name: ${TRAFFIC_EXT}
loadBalancingScheme: EXTERNAL_MANAGED
forwardingRules:
  - projects/${PROJECT_ID}/${LB_WASM_SCOPE}/forwardingRules/${FR_NAME}
extensionChains:
  - name: ${TRAFFIC_EXT}-chain
    matchCondition:
      celExpression: 'true'
    extensions:
      - name: ${TRAFFIC_EXT}-wasm
        service: projects/${PROJECT_ID}/locations/${WASM_LOCATION}/wasmPlugins/${WASM_PLUGIN}
        supportedEvents:
          - REQUEST_HEADERS
EOF_EXT

# Traffic extension (Proxy-Wasm)
gcloud beta service-extensions lb-traffic-extensions import "$TRAFFIC_EXT" \
  --source="$EXT_FILE" \
  --location="$LB_WASM_SCOPE"

LB_IP=$(gcloud compute addresses describe "$IP_NAME" --global --format='get(address)')

echo "LB IP: $LB_IP"
