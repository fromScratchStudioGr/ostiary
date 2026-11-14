# KMS: local, GCP, or OpenBao

Provider API keys are envelope-encrypted: a per-key DEK encrypts the actual
provider key, and a KMS master key wraps that DEK (see
`internal/keystore/kms.go`). Three `KMSClient` implementations exist:

- **`local`** (default) — `LocalKMS`, an AES-256-GCM key read from
  `LOCAL_KMS_MASTER_KEY`. **Dev only**: no separation of duties, no
  audit trail, the "master key" lives in the same process as the app.
- **`gcp`** — `GCPKMSClient`, backed by a real GCP Cloud KMS key.
- **`openbao`** — `VaultKMSClient`, backed by an OpenBao (or any
  Vault-API-compatible) Transit secrets engine key. Unlike `local`, the raw
  key never lives in this app's process; unlike `gcp`, it's self-hosted —
  useful where running your own KMS is preferred over a cloud one.

Switch between them with `KMS_PROVIDER` in `.env` (`local`, `gcp`, or
`openbao`, defaults to `local`). `gcp` additionally needs `GCP_KMS_KEY_NAME`
set to the key's full resource name; `openbao` needs `OPENBAO_VAULT_TOKEN`
and `OPENBAO_VAULT_KEYNAME` (see `.env.example` for both).

Running via Docker Compose with `gcp` needs one more file layered on:

```
docker compose -f docker-compose.yml -f docker-compose.gcp.yml up --build -d
```

`docker-compose.gcp.yml` adds `KMS_PROVIDER=gcp`, `GCP_KMS_KEY_NAME`, and
mounts a service account key (path from `GCP_KMS_CREDENTIALS_FILE` in
`.env`) into the container — plain `docker compose up` with no `-f` flags
keeps using `local` and needs none of this.

**On a Linux host, fix the key file's permissions first.** A bind-mounted
file keeps its host owner and mode, and the containers run as the
unprivileged user `10001:10001`, so a typical `0600` file owned by your own
user is unreadable inside the container and the gateway exits at startup.
Give group `10001` read access; the file stays unreadable to everyone else:

```
sudo chown $USER:10001 gcp-kms-key.json && chmod 640 gcp-kms-key.json
```

Docker Desktop on Windows and macOS doesn't enforce this, so it only shows
up on Linux. (Alternatively `sudo chown 10001:10001 gcp-kms-key.json &&
chmod 400 gcp-kms-key.json`, at the cost of needing `sudo` to read it
yourself. `chmod 644` also works but makes the key readable by every local
user.) On a host with SELinux enforcing (Fedora/RHEL), the bind mount also
needs the `:z` suffix.

`openbao` is similar, with its own override file:

```
docker compose -f docker-compose.yml -f docker-compose.openbao.yml up -d
```

Unlike `gcp`, nothing needs to pre-exist outside this repo: `openbao.yml`
also defines the `openbao` server itself (dev-mode — in-memory, auto-unsealed,
no persistent storage) and an `openbao-init` one-shot container that enables
the Transit engine and creates the `ostiary-dek-wrap` key before
`gateway`/`admin` start. `KMS_PROVIDER=openbao` and `OPENBAO_VAULT_ADDR` are
hardcoded in that file (pointing at the `openbao` service on the compose
network) — only `OPENBAO_VAULT_TOKEN`/`OPENBAO_VAULT_KEYNAME` come from
`.env`, and both have working defaults if left unset.

**Important caveat**: `docker-compose.openbao.yml` runs `openbao` in `-dev`
mode — in-memory, auto-unsealed, root token pinned to a fixed value. This is
**only suitable for local dev and stress/non-prod testing**, never for
production: its Transit key is generated fresh in memory on every container
start (it is never the same key twice, and it cannot be exported), and
there's no real unseal process or access control behind the pinned root
token. A production deployment needs its own properly-provisioned OpenBao
(or Vault) instance with real persistent storage, a genuine init/unseal
flow, and scoped non-root tokens — not this compose file.

One consequence of the dev-mode setup worth knowing before it surprises you:
a Postgres dump taken from one environment is **not** portable to another,
since each has unrelated key material under the same key *name* — the
`encrypted_dek` column was wrapped under a key that's now gone. This is a
deliberate tradeoff for local/non-prod convenience (simplicity over
cross-environment portability), not a bug — if that portability is ever
needed non-prod, the fix is either a shared persistent OpenBao instance
across environments, real (non-dev) storage with paired backup/restore of
OpenBao's own state alongside each Postgres dump, or making the key
exportable and importing identical bytes everywhere.

## Rotating the KMS key

Rotating the key in OpenBao or GCP changes nothing for the provider keys
already stored: their wrapped DEKs still decrypt, but stay on the old key
version. To move them onto the current version (and so be able to retire the
old one), an admin calls

```
POST /provider-keys/rewrap
```

The admin panel exposes it as a "Rewrap keys" action. It re-wraps the DEK of
every provider key, soft-deleted ones included, under the current version of
the key that wrapped it. Only `encrypted_dek` changes: the provider key's
ciphertext and nonce, and its `kms_key_id`, stay as they are. It also works
without a rotation (it just wraps each DEK again with the same key version),
and running it twice is harmless.

- **`openbao`**: done server-side by `transit/rewrap`, so no DEK leaves
  OpenBao. The token needs `update` on `transit/rewrap/<key>`. To rotate the
  key (here against the Compose setup, with its default token and key name):
  ```
  docker exec -e BAO_ADDR=http://127.0.0.1:8200 -e BAO_TOKEN=dev-root-token ostiary-openbao bao write -f transit/keys/ostiary-dek-wrap/rotate
  ```
  It prints the new `latest_version`. A wrapped DEK shows the version it's on
  as its prefix: `vault:v1:...` before the rewrap, `vault:v2:...` after.
  Once the rewrap has run, the old version can be retired with
  ```
  docker exec -e BAO_ADDR=http://127.0.0.1:8200 -e BAO_TOKEN=dev-root-token ostiary-openbao bao write transit/keys/ostiary-dek-wrap/config min_decryption_version=2
  ```
  (see the note on when that is safe, below). Outside Compose, use your own
  container name or `bao` directly, with the key name from
  `OPENBAO_VAULT_KEYNAME`.
- **`gcp`**: Cloud KMS has no rewrap call, so the admin process decrypts each
  DEK and encrypts it again, and the plaintext DEK sits in the admin
  process's memory for that moment (it is zeroed right after). The service
  account needs the same encrypt/decrypt role it already has. To rotate the
  key by hand, create a new version and make it the primary one (replace
  `LOCATION` and `PROJECT_ID` as in the setup below):
  ```
  gcloud kms keys versions create --key=dek-wrapper --keyring=ostiary --location=LOCATION --project=PROJECT_ID --primary
  ```
  Unlike OpenBao, a wrapped DEK doesn't show which version it's on, so
  there's no prefix to check after the rewrap. Once the rewrap has run, the
  old version can be disabled (reversible) before it is ever destroyed:
  ```
  gcloud kms keys versions disable OLD_VERSION --key=dek-wrapper --keyring=ostiary --location=LOCATION --project=PROJECT_ID
  ```
  A key can also be set to rotate on a schedule
  (`--rotation-period` and `--next-rotation-time` on `gcloud kms keys
  update`), which is the usual way to run it in production.
- **`local`**: has no rotation, and the endpoint answers 400.

The whole run happens in one request and finishes even if the client
disconnects. A key that can't be rewrapped is skipped and listed, and the
response is a 500 with the same body as a success:

```json
{"rewrapped": 41, "failed": 1, "failedIds": [7]}
```

Running it again retries just those. Only one rewrap runs at a time (the
second request gets a 409). When any key was rewrapped, every cached session
is dropped from Redis, so the next requests reload the new wrapped DEKs from
Postgres.

**Before retiring the old key version** (raising `min_decryption_version` in
OpenBao, or disabling/destroying the old version in GCP), check that the run
reported no failures, and wait for the gateways' in-memory session cache to
expire (`LOCAL_CACHE_TTL`). The admin can only clear Redis; a gateway's local
entries still hold the old wrapped DEK until they expire. If a request does
hit one after the old version is gone, decryption fails, the cached session
is dropped, and the next request loads the rewrapped DEK.

## Setting up GCP KMS from scratch

Replace `PROJECT_ID` and `LOCATION` (e.g. a region close to wherever the app
runs, such as `europe-west8`) throughout.

1. **Log in and select the project:**
   ```
   gcloud auth login
   gcloud config set project PROJECT_ID
   ```

2. **Enable the Cloud KMS API:**
   ```
   gcloud services enable cloudkms.googleapis.com
   ```

3. **Create a key ring and a symmetric key:**
   ```
   gcloud kms keyrings create ostiary --location=LOCATION
   gcloud kms keys create dek-wrapper --keyring=ostiary --location=LOCATION --purpose=encryption
   ```
   Note the key's full resource name — this is `GCP_KMS_KEY_NAME`:
   ```
   projects/PROJECT_ID/locations/LOCATION/keyRings/ostiary/cryptoKeys/dek-wrapper
   ```
   Decrypting only ever needs this top-level key name, never a specific
   version — Cloud KMS resolves the correct version from the ciphertext
   itself, so key rotation later needs no code or config change.

4. **Create a service account for the app, scoped to just this key:**
   ```
   gcloud iam service-accounts create ostiary-kms --display-name="Ostiary KMS wrapper"
   gcloud kms keys add-iam-policy-binding dek-wrapper \
     --keyring=ostiary --location=LOCATION \
     --member="serviceAccount:ostiary-kms@PROJECT_ID.iam.gserviceaccount.com" \
     --role="roles/cloudkms.cryptoKeyEncrypterDecrypter"
   ```

5. **Local dev credentials** — impersonate the service account instead of
   creating a key file (needs `roles/iam.serviceAccountTokenCreator` on
   yourself for that service account, which the project owner already has):
   ```
   gcloud auth application-default login --impersonate-service-account=ostiary-kms@PROJECT_ID.iam.gserviceaccount.com
   ```
   This is enough for `go run ./cmd/gateway` or `go run ./cmd/admin` locally
   with `KMS_PROVIDER=gcp` — no code change needed, the Go client picks up
   Application Default Credentials automatically.

6. **Docker credentials** — the container can't use your personal
   impersonated login, so it needs its own service account key:
   ```
   gcloud iam service-accounts keys create gcp-kms-key.json --iam-account=ostiary-kms@PROJECT_ID.iam.gserviceaccount.com
   ```
   If this fails with `Key creation is not allowed on this service account`,
   your org has the `constraints/iam.disableServiceAccountKeyCreation`
   policy enabled by default (Google's current default for new orgs). To
   allow it for just this project:
   ```
   gcloud organizations add-iam-policy-binding ORG_ID --member="user:YOUR_EMAIL" --role="roles/orgpolicy.policyAdmin"
   ```
   then apply a policy override (`orgpolicy.policyAdmin` is org-level only,
   so this role has to be granted at the organization, not the project):
   ```yaml
   # policy.yaml
   name: projects/PROJECT_ID/policies/iam.disableServiceAccountKeyCreation
   spec:
     rules:
     - enforce: false
   ```
   ```
   gcloud org-policies set-policy policy.yaml
   ```
   Policy changes can take a few minutes to propagate before the key
   creation command above succeeds. **Never commit the resulting
   `gcp-kms-key.json`** — it's already covered by `.gitignore` and
   `.dockerignore`, but treat it as a real secret regardless (rotate/delete
   it if it ever leaks).

7. **Fill in `.env`:**
   ```
   KMS_PROVIDER=gcp
   GCP_KMS_KEY_NAME=projects/PROJECT_ID/locations/LOCATION/keyRings/ostiary/cryptoKeys/dek-wrapper
   GCP_KMS_CREDENTIALS_FILE=./gcp-kms-key.json
   ```
   `GCP_KMS_CREDENTIALS_FILE` is only read by `docker-compose.gcp.yml`, not
   by the Go app itself — locally, ADC from step 5 is used instead.

8. **Verify the wiring** without going through the full app:
   ```
   GCP_KMS_KEY_NAME=projects/PROJECT_ID/locations/LOCATION/keyRings/ostiary/cryptoKeys/dek-wrapper go test -tags=integration ./internal/keystore/...
   ```
   This round-trips a real DEK through Encrypt/Decrypt against the live key
   — it's excluded from a normal `go test ./...` since it needs network
   access and real credentials.

## Running OpenBao manually (outside Compose)

Normally `docker-compose.openbao.yml` handles all of this automatically (see
above). This is for running/inspecting an OpenBao instance by hand — e.g. to
poke at the Transit API directly, or to debug `VaultKMSClient` against a real
server outside the compose network.

1. **Start a dev-mode server:**
   ```
   docker run -d --cap-add=IPC_LOCK --name openbao -p 8200:8200 \
     -e BAO_DEV_ROOT_TOKEN_ID=dev-root-token \
     -e BAO_DEV_LISTEN_ADDRESS=0.0.0.0:8200 \
     openbao/openbao:latest server -dev
   ```
   `-dev` means in-memory storage, auto-unsealed, with the root token pinned
   to a known value instead of generated — nothing here survives a restart.

2. **Enable Transit and create the key** `openbao-init` would otherwise
   create for you:
   ```
   docker exec -e BAO_ADDR=http://127.0.0.1:8200 -e BAO_TOKEN=dev-root-token openbao sh -c "bao secrets enable transit; bao write -f transit/keys/ostiary-dek-wrap"
   ```

3. **Smoke-test it** before pointing the app at it:
   ```
   docker exec -e BAO_ADDR=http://127.0.0.1:8200 -e BAO_TOKEN=dev-root-token openbao bao write transit/encrypt/ostiary-dek-wrap plaintext=aGVsbG8=
   ```
   Take the returned `vault:v1:...` ciphertext and feed it back in — both
   `encrypt` and `decrypt` are POST-only endpoints, so both use `bao write`,
   never `bao read`:
   ```
   docker exec -e BAO_ADDR=http://127.0.0.1:8200 -e BAO_TOKEN=dev-root-token openbao bao write transit/decrypt/ostiary-dek-wrap ciphertext=PASTE_CIPHERTEXT_HERE
   ```
   This should return `plaintext: aGVsbG8=` (base64 for `hello`).

4. **Point the app at it:**
   ```
   KMS_PROVIDER=openbao
   OPENBAO_VAULT_ADDR=http://localhost:8200
   OPENBAO_VAULT_TOKEN=dev-root-token
   OPENBAO_VAULT_KEYNAME=ostiary-dek-wrap
   ```
   `OPENBAO_VAULT_ADDR` has no built-in default — it has to be set explicitly
   whenever running outside Compose (`go run ./cmd/gateway` on the host, as
   here). `docker-compose.openbao.yml` is the only place that hardcodes it,
   to `http://openbao:8200`, since that's the compose service name reachable
   from inside `gateway`'s/`admin`'s own containers.

   All three settings are required: with `KMS_PROVIDER=openbao`, `gateway` and
   `admin` refuse to start if any of them is empty, and say which. They also
   ignore the OpenBao client's own `BAO_AGENT_ADDR` environment variable,
   which would otherwise take precedence over `OPENBAO_VAULT_ADDR`.
