# Decisions Log

## 2026-04-29

### Decision: Use modernc.org/sqlite for Setup Wizard Persistence

**Date:** 2026-04-29  
**Author:** Dallas  
**Status:** Active

#### Decision

Use `modernc.org/sqlite` (pure Go SQLite driver) for persisting admin consent + storage account configuration in the setup wizard.

#### Rationale

- **Pure Go, no CGO** — the final container image is distroless (`gcr.io/distroless/static-debian12:nonroot`), which contains no C runtime. CGO-based SQLite drivers (e.g., `mattn/go-sqlite3`) will fail to load at runtime in this environment. `modernc.org/sqlite` compiles the SQLite amalgamation to Go, requiring no C compiler or libc.
- **Single binary** — no external database process to manage; DB file lives on a mounted volume (`/data/setup.db` via `SETUP_DB_PATH` env).
- **Appropriate scale** — setup wizard is a low-traffic, single-use tool. SQLite is more than sufficient; Postgres/MySQL would be over-engineered.
- **Lightweight dependency** — transitive dep tree is self-contained; no network service, no auth, no connection pooling needed.

#### Alternatives Considered

- `mattn/go-sqlite3` — requires CGO; incompatible with distroless.
- `zombiezen.com/go/sqlite` — also pure Go but newer and less battle-tested than modernc.
- Postgres/MySQL — overkill; requires a separate service in the deployment.

#### Schema

```sql
CREATE TABLE IF NOT EXISTS consents (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  tenant_id TEXT NOT NULL,
  subscription_id TEXT NOT NULL,
  resource_group TEXT NOT NULL,
  storage_account_name TEXT NOT NULL,
  container_name TEXT NOT NULL,
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);
```

### Decision: Setup Wizard SQLite Persistent Volume

**Date:** 2026-04-29  
**Author:** Hudson (DevOps)  
**Status:** Accepted

#### Context

The setup wizard now writes a SQLite database to disk (`/data/setup.db`) using the `modernc.org/sqlite` pure-Go driver. The K8s manifests needed persistent storage and adjusted security context to support this.

#### Decisions

##### PVC over emptyDir
A `PersistentVolumeClaim` (`setup-db`, 100Mi, `ReadWriteOnce`) is used instead of `emptyDir`. An `emptyDir` volume is destroyed with the pod; a PVC survives pod restarts. Since the setup wizard records OAuth consent state and admin actions in the database, losing it on restart would break the flow and require the admin to start over.

##### 100Mi storage request
The setup wizard's SQLite database holds at most dozens of rows across a handful of tables (consent state, admin records). SQLite databases at this scale remain well under 1Mi. 100Mi provides ample headroom without over-provisioning.

##### readOnlyRootFilesystem removed
`modernc.org/sqlite` (pure-Go, CGO-free SQLite driver) writes temp files to the OS temp directory (`/tmp`) during query execution. With `readOnlyRootFilesystem: true`, writes to `/tmp` are blocked even when `/data` is writable via a volume mount, causing the SQLite driver to fail at runtime. The flag has been removed. The remaining security controls — `runAsNonRoot: true`, `allowPrivilegeEscalation: false`, `capabilities.drop: [ALL]`, `seccompProfile: RuntimeDefault` — continue to provide strong defence-in-depth.

## 2026-04-30

### Decision: AKS Identity Bindings FIC Configuration

**Date:** 2026-04-30  
**Author:** Bishop (Azure Engineer)  
**Status:** Accepted  

#### Context

The timestampwriter pod had `AZURE_KUBERNETES_TOKEN_PROXY` injected (Identity Bindings active), but token exchange was failing with AADSTS700211. The existing FIC on the multi-tenant Entra app was configured with the Identity Binding's internal OIDC issuer (`ib.oic.prod-aks.azure.com`), which does not match the issuer claim in the token Entra actually receives.

#### Root Cause

Three distinct problems were discovered and fixed in sequence:

| # | Error Code | Issue | Fix |
|---|-----------|-------|-----|
| 1 | AADSTS700211 | FIC issuer pointed to `ib.oic.prod-aks.azure.com/<tenant>/<uami-client-id>` — the Identity Binding's internal issuer URL — but token issuer is the cluster's standard OIDC endpoint | Replace FIC issuer with cluster OIDC URL |
| 2 | AADSTS700212 | FIC audience was `api://AzureADTokenExchange` (standard workload identity); IB tokens use `api://AKSIdentityBinding` | Set FIC audience to `api://AKSIdentityBinding` |
| 3 | AADSTS7000229 | Multi-tenant app had no service principal in its home tenant (source tenant `89dfbdeb`) | `az ad sp create --id 7c9dd5ee-3444-4559-9edc-917b520b3081` |

#### Final FIC Configuration

**App registration:** `aks-xtenant-auth-app`  
**App client ID:** `7c9dd5ee-3444-4559-9edc-917b520b3081`  
**App object ID:** `8886e31a-32fe-4e13-b55c-bbb6156d80e8`  
**FIC ID:** `09c0d7e3-23ae-40a0-9480-d856be8a98ab`  
**FIC name:** `aks-identity-binding-credential`  

```json
{
  "name": "aks-identity-binding-credential",
  "issuer": "https://eastus2.oic.prod-aks.azure.com/89dfbdeb-9c84-4dd4-9ce7-9932e30998de/f190254e-a0d2-4627-adb2-e101c3a32e34/",
  "subject": "system:serviceaccount:aks-xtenant-auth:timestampwriter",
  "audiences": ["api://AKSIdentityBinding"],
  "description": "AKS Identity Bindings credential for timestampwriter in aks-dev-eus2"
}
```

**Key values:**
- **Issuer:** The AKS cluster's standard OIDC URL (from `az aks show --query "oidcIssuerProfile.issuerUrl"`) — NOT the Identity Binding's `ib.oic.prod-aks.azure.com` URL
- **Subject:** `system:serviceaccount:aks-xtenant-auth:timestampwriter` (namespace:serviceaccount-name)
- **Audience:** `api://AKSIdentityBinding` (Identity Bindings-specific; standard WI uses `api://AzureADTokenExchange`)

#### Service Principal Created in Home Tenant

**SP object ID:** `cf15697a-4111-4bb5-b552-003003434d9f`  
**Tenant:** `89dfbdeb-9c84-4dd4-9ce7-9932e30998de` (source/home tenant)  

The multi-tenant app must have a SP in its home tenant for federated credential exchange to succeed. This is separate from the SP in the target tenant (which must also exist for RBAC).

#### Architecture Note

The Identity Binding `oidcIssuerUrl` (`https://ib.oic.prod-aks.azure.com/...`) is used internally by the AKS proxy for UAMI token exchange. It is NOT the issuer claim in tokens that the multi-tenant app presents to Entra. The token's issuer is always the cluster's standard OIDC endpoint. The `AZURE_KUBERNETES_TOKEN_PROXY` env var routes the request through the proxy, but the resulting token still carries the cluster OIDC issuer.

#### Remaining Issue

After FIC is fixed, the pod now obtains a token but hits `InvalidAuthenticationInfo: Issuer validation failed` from Azure Storage. This is a cross-tenant scoping issue: the token is issued against the source tenant (the pod's `AZURE_TENANT_ID`), but the storage account is in a different tenant. Fix requires setting `AZURE_TENANT_ID` in the ConfigMap to the target tenant's ID so the SDK requests a token scoped to the target tenant.

### Decision: Override AZURE_TENANT_ID for Cross-Tenant Workload Identity Flows

**Date:** 2026-04-30  
**Author:** Bishop (Azure Engineer)  
**Status:** Accepted — implemented and verified

#### Context

The AKS workload identity webhook injects three environment variables into every pod whose ServiceAccount is annotated with `azure.workload.identity/client-id`:

- `AZURE_CLIENT_ID` — sourced from the ServiceAccount annotation
- `AZURE_TENANT_ID` — **always the source (home) tenant** (`89dfbdeb-9c84-4dd4-9ce7-9932e30998de`)
- `AZURE_FEDERATED_TOKEN_FILE` — path to the projected OIDC token

For same-tenant scenarios this is correct. For cross-tenant scenarios, the Azure SDK (azidentity `WorkloadIdentityCredential`) uses `AZURE_TENANT_ID` to determine which Entra tenant to direct the token exchange request to. When this is the source tenant, the SDK acquires a token valid in the source tenant — but the target storage account and its RBAC assignment live in a different tenant (`c8c22f13-9f4d-41a5-b619-eda5a1d50c39`). Azure Storage's endpoint rejects the token with:

```
InvalidAuthenticationInfo: Issuer validation failed
```

#### Decision

Override `AZURE_TENANT_ID` at the container level by:

1. Adding `AZURE_TENANT_ID: "c8c22f13-9f4d-41a5-b619-eda5a1d50c39"` to `deploy/configmap.yaml`.
2. Adding an explicit `env` entry in `deploy/deployment.yaml` that sources `AZURE_TENANT_ID` from the ConfigMap, _before_ the webhook would inject it.

The webhook respects pre-existing env vars: if `AZURE_TENANT_ID` is already set in the container spec, the webhook skips its injection entirely. The ConfigMap value therefore wins.

#### Rationale

- **Zero code change required** — the override lives entirely in Kubernetes manifests.
- **No secrets** — tenant IDs are non-sensitive identifiers.
- **Explicit and auditable** — the target tenant ID is visible in `configmap.yaml` with a clear comment.
- **Consistent with `AZURE_CLIENT_ID` pattern** — the project already overrides `AZURE_CLIENT_ID` this way (to substitute the multi-tenant app client ID for the UAMI client ID). The tenant ID override follows the identical pattern.

#### Facts Confirmed (2026-04-30)

| Item | Status |
|------|--------|
| SP `161bf58d` for app `7c9dd5ee` in target tenant `c8c22f13` | Already existed |
| `Storage Blob Data Contributor` on `rgxtenantsa` (subscription `d40f92cd`) | Already assigned |
| `xtenant` blob container on `rgxtenantsa` | Already existed |
| ConfigMap patched live in cluster | Done |
| `deploy/configmap.yaml` updated | Done |
| `deploy/deployment.yaml` updated | Done |
| Pod restarted and writing blobs successfully | **Verified** |

#### Outcome

After the override, pod logs show:

```json
{"level":"INFO","msg":"storage account configured","account":"https://rgxtenantsa.blob.core.windows.net/","container":"xtenant"}
{"level":"INFO","msg":"blob written","timestamp":"2026-04-30T00:18:52Z"}
```

No `AADSTS` errors. No `InvalidAuthenticationInfo`. Cross-tenant storage write confirmed.

#### Files Changed

- `deploy/configmap.yaml` — added `AZURE_TENANT_ID` key with target tenant value
- `deploy/deployment.yaml` — added `AZURE_TENANT_ID` env entry sourced from ConfigMap

Commit: `e349090`
