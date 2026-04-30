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

