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
case "$PRIVATE_KEY_PEM_FILE" in
  /*) ;;
  *) PRIVATE_KEY_PEM_FILE="$ROOT_DIR/$PRIVATE_KEY_PEM_FILE" ;;
esac

: "${PROJECT_ID?}"
: "${REGION?}"
: "${AR_LOCATION?}"
: "${AR_REPO?}"
: "${LB_PREFIX?}"
: "${LB_NETWORK?}"
: "${PROXY_SUBNET_RANGE?}"
: "${CALLOUT_SERVICE?}"
: "${ORIGIN_SERVICE?}"
: "${PUBLIC_KEY_PEM_FILE?}"
: "${PRIVATE_KEY_PEM_FILE?}"
: "${JWT_SUB?}"

if [[ ! -f "$PUBLIC_KEY_PEM_FILE" ]]; then
  echo "public key not found: $PUBLIC_KEY_PEM_FILE" >&2
  exit 1
fi
if [[ ! -f "$PRIVATE_KEY_PEM_FILE" ]]; then
  echo "private key not found: $PRIVATE_KEY_PEM_FILE" >&2
  exit 1
fi

PUBLIC_KEY_PEM=$(awk '{printf "%s\\n", $0}' "$PUBLIC_KEY_PEM_FILE")
LB_PROC_PREFIX="${LB_PREFIX}-proc"
PROXY_SUBNET="${LB_PREFIX}-proxy-subnet"
echo "using PROXY_SUBNET_RANGE=$PROXY_SUBNET_RANGE"
echo "using LB_PROC_PREFIX=$LB_PROC_PREFIX"

IP_NAME="${LB_PROC_PREFIX}-ip"
NEG_ORIGIN="${LB_PROC_PREFIX}-neg-origin"
NEG_CALLOUT="${LB_PROC_PREFIX}-neg-callout"
BS_ORIGIN="${LB_PROC_PREFIX}-bs-origin"
BS_CALLOUT="${LB_PROC_PREFIX}-bs-callout"
URL_MAP="${LB_PROC_PREFIX}-urlmap"
TARGET_PROXY="${LB_PROC_PREFIX}-proxy"
FR_NAME="${LB_PROC_PREFIX}-fr"
TRAFFIC_EXT="${LB_PROC_PREFIX}-traffic-ext"

EXT_FILE="$ROOT_DIR/deploy/gcloud/traffic-extension.yaml"
REPO_HOST="${AR_LOCATION}-docker.pkg.dev"
REPO_URI="${REPO_HOST}/${PROJECT_ID}/${AR_REPO}"
CALLOUT_IMAGE="${REPO_URI}/${CALLOUT_SERVICE}:authz"
ORIGIN_IMAGE="${REPO_URI}/${ORIGIN_SERVICE}:v1"

cd "$ROOT_DIR"

gcloud config set project "$PROJECT_ID"

gcloud services enable \
  run.googleapis.com \
  compute.googleapis.com \
  cloudbuild.googleapis.com \
  artifactregistry.googleapis.com \
  networksecurity.googleapis.com \
  networkservices.googleapis.com

# Artifact Registry (docker)
if ! gcloud artifacts repositories describe "$AR_REPO" --location "$AR_LOCATION" >/dev/null 2>&1; then
  gcloud artifacts repositories create "$AR_REPO" \
    --repository-format=docker \
    --location="$AR_LOCATION"
fi

# Build images (multi-stage Docker)
gcloud builds submit "$ROOT_DIR" \
  --config "$ROOT_DIR/deploy/gcloud/cloudbuild.yaml" \
  --substitutions _IMAGE="$CALLOUT_IMAGE",_TARGET="cmd/callout-server"

gcloud builds submit "$ROOT_DIR" \
  --config "$ROOT_DIR/deploy/gcloud/cloudbuild.yaml" \
  --substitutions _IMAGE="$ORIGIN_IMAGE",_TARGET="cmd/origin-server"

# Cloud Run: callout (gRPC) / origin (HTTP)
gcloud run deploy "$CALLOUT_SERVICE" \
  --region "$REGION" \
  --image "$CALLOUT_IMAGE" \
  --allow-unauthenticated \
  --use-http2 \
  --set-env-vars "PUBLIC_KEY_PEM=$PUBLIC_KEY_PEM"

gcloud run deploy "$ORIGIN_SERVICE" \
  --region "$REGION" \
  --image "$ORIGIN_IMAGE" \
  --allow-unauthenticated

# Proxy-only subnet (required for regional external HTTP(S) LB)
if ! gcloud compute networks subnets describe "$PROXY_SUBNET" --region "$REGION" >/dev/null 2>&1; then
  gcloud compute networks subnets create "$PROXY_SUBNET" \
    --region "$REGION" \
    --network "$LB_NETWORK" \
    --purpose=REGIONAL_MANAGED_PROXY \
    --role=ACTIVE \
    --range "$PROXY_SUBNET_RANGE"
fi

# Serverless NEGs
if ! gcloud compute network-endpoint-groups describe "$NEG_ORIGIN" --region "$REGION" >/dev/null 2>&1; then
  gcloud compute network-endpoint-groups create "$NEG_ORIGIN" \
    --region "$REGION" \
    --network-endpoint-type=serverless \
    --cloud-run-service="$ORIGIN_SERVICE"
fi

if ! gcloud compute network-endpoint-groups describe "$NEG_CALLOUT" --region "$REGION" >/dev/null 2>&1; then
  gcloud compute network-endpoint-groups create "$NEG_CALLOUT" \
    --region "$REGION" \
    --network-endpoint-type=serverless \
    --cloud-run-service="$CALLOUT_SERVICE"
fi

# Backend services
if ! gcloud compute backend-services describe "$BS_ORIGIN" --region "$REGION" >/dev/null 2>&1; then
  gcloud compute backend-services create "$BS_ORIGIN" \
    --region "$REGION" \
    --load-balancing-scheme=EXTERNAL_MANAGED \
    --protocol=HTTP
fi

ORIGIN_BACKENDS=$(gcloud compute backend-services describe "$BS_ORIGIN" --region "$REGION" --format='value(backends[].group)')
if ! echo "$ORIGIN_BACKENDS" | grep -q "/networkEndpointGroups/${NEG_ORIGIN}$"; then
  gcloud compute backend-services add-backend "$BS_ORIGIN" \
    --region "$REGION" \
    --network-endpoint-group="$NEG_ORIGIN" \
    --network-endpoint-group-region="$REGION"
fi

if ! gcloud compute backend-services describe "$BS_CALLOUT" --region "$REGION" >/dev/null 2>&1; then
  gcloud compute backend-services create "$BS_CALLOUT" \
    --region "$REGION" \
    --load-balancing-scheme=EXTERNAL_MANAGED \
    --protocol=HTTP2
fi

CALLOUT_PROTOCOL=$(gcloud compute backend-services describe "$BS_CALLOUT" --region "$REGION" --format='value(protocol)')
if [[ "$CALLOUT_PROTOCOL" != "HTTP2" ]]; then
  gcloud compute backend-services update "$BS_CALLOUT" \
    --region "$REGION" \
    --protocol=HTTP2
fi

CALLOUT_BACKENDS=$(gcloud compute backend-services describe "$BS_CALLOUT" --region "$REGION" --format='value(backends[].group)')
if ! echo "$CALLOUT_BACKENDS" | grep -q "/networkEndpointGroups/${NEG_CALLOUT}$"; then
  gcloud compute backend-services add-backend "$BS_CALLOUT" \
    --region "$REGION" \
    --network-endpoint-group="$NEG_CALLOUT" \
    --network-endpoint-group-region="$REGION"
fi

# Regional external HTTP LB
if ! gcloud compute addresses describe "$IP_NAME" --region "$REGION" >/dev/null 2>&1; then
  gcloud compute addresses create "$IP_NAME" \
    --region "$REGION" \
    --network-tier=STANDARD
fi

if ! gcloud compute url-maps describe "$URL_MAP" --region "$REGION" >/dev/null 2>&1; then
  gcloud compute url-maps create "$URL_MAP" \
    --region "$REGION" \
    --default-service="$BS_ORIGIN"
fi

if ! gcloud compute target-http-proxies describe "$TARGET_PROXY" --region "$REGION" >/dev/null 2>&1; then
  gcloud compute target-http-proxies create "$TARGET_PROXY" \
    --region "$REGION" \
    --url-map="$URL_MAP"
fi

if ! gcloud compute forwarding-rules describe "$FR_NAME" --region "$REGION" >/dev/null 2>&1; then
  gcloud compute forwarding-rules create "$FR_NAME" \
    --region "$REGION" \
    --load-balancing-scheme=EXTERNAL_MANAGED \
    --network-tier=STANDARD \
    --network="$LB_NETWORK" \
    --target-http-proxy="$TARGET_PROXY" \
    --target-http-proxy-region="$REGION" \
    --ports=80 \
    --address="$IP_NAME"
fi

LB_IP=$(gcloud compute addresses describe "$IP_NAME" --region "$REGION" --format='get(address)')

cat > "$EXT_FILE" <<EOF_EXT
name: ${TRAFFIC_EXT}
loadBalancingScheme: EXTERNAL_MANAGED
forwardingRules:
  - projects/${PROJECT_ID}/regions/${REGION}/forwardingRules/${FR_NAME}
extensionChains:
  - name: ${TRAFFIC_EXT}-chain
    matchCondition:
      celExpression: 'true'
    extensions:
      - name: ${TRAFFIC_EXT}-callout
        authority: extproc.example.com
        service: projects/${PROJECT_ID}/regions/${REGION}/backendServices/${BS_CALLOUT}
        timeout: 0.2s
        failOpen: false
        supportedEvents:
          - REQUEST_HEADERS
EOF_EXT

# Traffic extension (ext_proc via callouts)
gcloud beta service-extensions lb-traffic-extensions import "$TRAFFIC_EXT" \
  --source="$EXT_FILE" \
  --location="$REGION"

echo "LB IP: $LB_IP"
