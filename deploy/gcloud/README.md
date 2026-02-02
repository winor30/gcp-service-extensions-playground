# ext_authz Runbook

1) Prepare keys (PKCS1 RSA PRIVATE KEY and matching PUBLIC KEY)

```bash
mkdir -p ".secrets"
openssl genrsa -out ".secrets/private.pem" 2048
openssl rsa -in ".secrets/private.pem" -pubout -out ".secrets/public.pem"
```

2) Edit `.env`
- Set `PROJECT_ID`, `REGION`, `AR_LOCATION`, `AR_REPO`, `LB_PREFIX`, `CALLOUT_SERVICE`, `ORIGIN_SERVICE`
- Point `PUBLIC_KEY_PEM_FILE` / `PRIVATE_KEY_PEM_FILE` to files under `.secrets/`
- Set `PROXY_SUBNET_RANGE` to an unused CIDR in the VPC (for the proxy-only subnet)
  - The default VPC uses 10.128.0.0/9, so choose a non-overlapping range

3) Run the script

```bash
./deploy/gcloud/deploy-authz.sh
```

> Required APIs: compute / run / cloudbuild / artifactregistry / networksecurity / networkservices

4) Put the printed `LB IP` into `.env` as `TARGET_URL`

5) Run the client

```bash
TARGET_URL="http://<LB_IP>/" \
PRIVATE_KEY_PEM_FILE=".secrets/private.pem" \
JWT_SUB="demo-user" \
go run ./cmd/client
```

> Cloud Run is deployed from a Docker image (multi-stage + distroless).
> Cloud Build runs Docker builds using `deploy/gcloud/cloudbuild.yaml`.

# ext_proc Runbook

1) Run ext_authz at least once (it creates the proxy-only subnet and base services)

2) Run the script

```bash
./deploy/gcloud/deploy-proc.sh
```

3) Use the printed `LB IP` as the target URL (overwrite `TARGET_URL` or export it)

```bash
TARGET_URL="http://<LB_IP>/" \
PRIVATE_KEY_PEM_FILE=".secrets/private.pem" \
JWT_SUB="demo-user" \
go run ./cmd/client
```

Expected: the response JSON from `origin` includes `x-uid: <JWT_SUB>` in headers.

# proxy_wasm Runbook

1) Run ext_authz or ext_proc at least once (it creates the proxy-only subnet and base services)

2) Set `AR_LOCATION=us` in `.env`
   - Proxy-Wasm with a **global** external HTTP(S) LB requires the plugin image in the multi-region `us` Artifact Registry.

3) (Optional) Set `WASM_PLUGIN_VERSION` in `.env` when updating the plugin image
   - Wasm plugins are created in the `global` location (handled by the script)
   - LB3 for proxy_wasm is a **global** external HTTP(S) LB

4) Run the script

```bash
./deploy/gcloud/deploy-wasm.sh
```

5) Use the printed `LB IP` as the target URL (overwrite `TARGET_URL` or export it)

```bash
TARGET_URL="http://<LB_IP>/" \
PRIVATE_KEY_PEM_FILE=".secrets/private.pem" \
JWT_SUB="demo-user" \
go run ./cmd/client
```

Expected: the response JSON from `origin` includes `x-uid: <JWT_SUB>` in headers.
