# Project Context

- **Owner:** Ryan Graham
- **Project:** Containerized Go application that uses AKS workload identity to authenticate to a Microsoft Entra multi-tenant application, then writes timestamps to an Azure Blob Storage account in a different Azure tenant.
- **Stack:** Go, Docker (distroless/alpine), Kubernetes / AKS, Azure SDK for Go (azidentity, azblob), Microsoft Entra ID (multi-tenant app registration), Azure Blob Storage
- **Created:** 2026-04-29

## Key Learnings (Summary)

**Cross-Tenant Architecture:**
- AZURE_TENANT_ID override required to scope token requests to target tenant (not source tenant).
- Target tenant SP and RBAC pre-existed; only manifest changes needed (no code changes).
- Multi-tenant Entra app is mandatory (UAMIs cannot have SPs in target tenants).
- Multi-tenant apps require explicit SP creation in home tenant (`az ad sp create`); they do not auto-create on registration.
- Three-hop identity chain: AKS pod (source) → Entra app FIC (source) → token exchange (target) → RBAC (target).

**Identity Bindings (IB):**
- IB is UAMI-only; cannot support cross-tenant scenarios (no SDK pattern for dual token exchange).
- IB OIDC issuer (`ib.oic.prod-aks.azure.com/<tenant>/<client-id>`) constant across clusters for same UAMI.
- IB tokens use `api://AKSIdentityBinding` audience; standard WI uses `api://AzureADTokenExchange`.
- FIC issuer field depends on whether IB is active: cluster standard OIDC (direct exchange) or IB OIDC (proxy exchange).
- Token presentation to Entra always uses cluster standard OIDC issuer (IB OIDC is internal to proxy).

**Federated Identity Credentials (FIC):**
- Exact subject matching required: `system:serviceaccount:namespace:name`.
- Issuer/audience/subject mismatches reported as AADSTS700211 (issuer) or AADSTS700212 (audience).
- FIC targets Entra app for cross-tenant flows; UAMI FICs are only for direct UAMI-to-tenant exchange.

**Go SDK (azidentity):**
- EnableAzureProxy option required for IB support (v1.14.0-beta.3+).
- Proxy injects env vars: AZURE_KUBERNETES_TOKEN_PROXY, AZURE_KUBERNETES_SNI_NAME, AZURE_KUBERNETES_CA_FILE.

## Work History (2026-04-29 to 2026-04-30)

**Confused Deputy Analysis:** Complete 6-vector threat model analysis. Production FIC (id `09c0d7e3`) correctly configured with cluster OIDC issuer + `api://AKSIdentityBinding` audience. Documentation contained high-severity FIC config errors (now fixed in commit 29d85c1). Architecture assessed by Ripley: NOT susceptible to confused deputy problem.

**Identity Bindings Feasibility:** Determined IB is UAMI-only; cannot support cross-tenant scenarios. Recommendation: request FIC limit increase from Azure Support.

**Docs Updates:** Fixed FIC audience (`api://AzureADTokenExchange` → `api://AKSIdentityBinding`), added missing source-tenant SP creation step, clarified OIDC_ISSUER_URL and UAMI descriptions.

**FIC Configuration Fix:** Created new FIC (id `09c0d7e3`) with correct issuer/audience. Fixed missing home-tenant SP creation. AADSTS700211 resolved.

**Cross-Tenant AZURE_TENANT_ID Override:** Added ConfigMap-sourced `AZURE_TENANT_ID` override to deployment to scope tokens to target tenant. Resolved `InvalidAuthenticationInfo: Issuer validation failed`. Cross-tenant flow now fully functional.

**Recent Session (2026-04-30):** Merged 3 decision files into primary decisions.md. decisions.md grown from 3303 to 7307 bytes. All inbox files deleted.

## Learnings

### 2026-04-29 — Azure & Identity Security Audit

- `docs/azure-setup.md` still mixes the accepted simplified admin-consent setup with a superseded PKCE/Graph/ARM design. That drift would cause operators to add unnecessary delegated permissions, redirect URIs, and broader admin expectations.
- For the current working Identity Bindings sample, the Entra app FIC must use the **cluster standard OIDC issuer** plus audience `api://AKSIdentityBinding` and the exact ServiceAccount subject. The internal `ib.oic.prod-aks.azure.com/...` issuer remains the wrong value for the app FIC.
- The setup wizard success page currently emits two bad operator instructions: it suggests ServiceAccount `azure.workload.identity/client-id` should be the **app** client ID instead of the **UAMI** client ID, and it generates `Storage Blob Data Contributor` at **storage-account scope** instead of container scope.
- The shared SQLite/NFS design is only weakly isolated. Both pods mount the same RWX PVC, `timestampwriter` does not mount it read-only, and any same-namespace pod that can mount the claim could read or tamper with `setup.db`.

## 2026-04-30 — Broad Security Audit: Repeated FIC Issuer Error

**Finding:** During the 2026-04-30 security audit (commit e74fe76), Bishop repeated the prior FIC issuer confusion: advised that the Entra app FIC issuer should be the cluster standard OIDC URL (marked as "correct in the docs"). This contradicted the accepted decision log (`decisions/bishop-identity-chain.md`) and production-verified configuration (FIC `09c0d7e3` uses cluster standard OIDC issuer).

**Team Decision:** Rejected. The existing production FIC configuration is correct. Cluster standard OIDC issuer + `api://AKSIdentityBinding` audience + exact ServiceAccount subject is the verified working path. No code change required. Documented in `decisions.md` Security Audit 2026-04-30 entry.

**Severity:** Low (no action taken; decision log is authoritative). Indicates this topic requires extra clarity in future documentation or training.

