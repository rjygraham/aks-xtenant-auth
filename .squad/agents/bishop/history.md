# Project Context

- **Owner:** Ryan Graham
- **Project:** Containerized Go application that uses AKS workload identity to authenticate to a Microsoft Entra multi-tenant application, then writes timestamps to an Azure Blob Storage account in a different Azure tenant.
- **Stack:** Go, Docker (distroless/alpine), Kubernetes / AKS, Azure SDK for Go (azidentity, azblob), Microsoft Entra ID (multi-tenant app registration), Azure Blob Storage
- **Created:** 2026-04-29

## Learnings

- **Cross-tenant AZURE_TENANT_ID override is required for workload identity flows:** The AKS workload identity webhook always injects `AZURE_TENANT_ID` from the ServiceAccount annotation (the source/home tenant). For cross-tenant storage access, the Azure SDK must request a token scoped to the **target tenant** (where the multi-tenant app's SP and the storage RBAC live). The fix is to add `AZURE_TENANT_ID` explicitly in the deployment's `env` section (sourced from the ConfigMap). The webhook skips injection when the env var is already present in the container spec, so the explicit value wins. Without this, the SDK requests a home-tenant token which Entra's storage endpoint rejects with `InvalidAuthenticationInfo: Issuer validation failed`. Target tenant for this project: `c8c22f13-9f4d-41a5-b619-eda5a1d50c39`. Storage subscription: `d40f92cd-a83a-4ef0-a38b-f8eabe80448f`.
- **Target tenant SP and RBAC pre-existed (no remediation needed):** SP `161bf58d-5722-4fa8-a845-fe7664128ece` for app `7c9dd5ee` was already present in the target tenant. `Storage Blob Data Contributor` RBAC was already assigned on `rgxtenantsa` in `CROSS-TENANT-STORAGE`. The `xtenant` blob container also already existed. The only required change was the `AZURE_TENANT_ID` override.
- **Identity Bindings FIC must use the CLUSTER's standard OIDC issuer, not the IB-specific `ib.oic.prod-aks.azure.com` issuer:** Even when Identity Bindings is configured and AZURE_KUBERNETES_TOKEN_PROXY is injected, the token presented to Entra for FIC exchange carries the cluster's standard OIDC issuer (`https://eastus2.oic.prod-aks.azure.com/<tenant-id>/<cluster-oidc-id>/`), not the Identity Binding's own OIDC URL. The `oidcIssuerUrl` returned by `az aks identity-binding show` (`ib.oic.prod-aks.azure.com`) is used internally by the AKS IB proxy, not as the claim in the token presented to Entra.
- **Identity Bindings tokens use audience `api://AKSIdentityBinding`, not `api://AzureADTokenExchange`:** The FIC on the multi-tenant Entra app must list `api://AKSIdentityBinding` in the `audiences` array. Standard workload identity uses `api://AzureADTokenExchange`; Identity Bindings changes this audience.
- **Multi-tenant app needs a service principal in its own home tenant for federated credential exchange:** The home tenant doesn't auto-create the SP for multi-tenant apps on registration. `az ad sp create --id <APP_CLIENT_ID>` must be run in the source (home) tenant as well as the target tenant. Without it, FIC exchange fails with AADSTS7000229.
- **FIC issuer mismatch (AADSTS700211) ≠ FIC subject/audience mismatch (AADSTS700212):** Entra reports the specific mismatch; 700211 is issuer, 700212 is audience. Both indicate the presented assertion doesn't match the registered FIC. Check each field independently when debugging.

- **Multi-tenant Entra app approach is mandatory for cross-tenant storage RBAC:** User-assigned managed identities (UAMIs) cannot have service principals in target tenants; only Entra app registrations with `signInAudience: AzureADMultipleOrgs` can. This enables the RBAC role assignment on target storage resources.
- **Federated identity credential targets the Entra app, not the UAMI:** For cross-tenant flows, the federated credential (OIDC issuer + ServiceAccount subject + audience) should be on the Entra app registration. The UAMI is optional for this architecture.
- **Service principal creation in target tenant auto-grants admin consent:** `az ad sp create --id <APP_CLIENT_ID>` both creates the service principal and automatically grants tenant-wide admin consent. No separate consent workflow required.
- **Every federated credential requires exact subject matching:** The Kubernetes ServiceAccount subject must be exact (`system:serviceaccount:namespace:serviceaccount-name`). The AKS workload identity webhook uses this to bind the OIDC token to pod identity.
- **Cross-tenant identity chain is three-hop:** AKS pod (source tenant) → Entra app federated credential (source tenant) → service principal token exchange (target tenant) → RBAC assignment (target tenant). Each hop must be explicitly configured.
- **PKCE OAuth2 flow enables setup wizard without secrets:** Setup wizard uses Authorization Code flow with PKCE (Proof Key for Code Exchange) instead of client credentials. This allows delegated permissions without pre-sharing secrets, making it safe to send the wizard URL to target tenant admins. Admin grants consent interactively in their tenant.
- **Delegated permissions are essential for cross-tenant admin consent:** The wizard requests `Application.Read.All` (Microsoft Graph) and `user_impersonation` (Azure Service Management) as delegated permissions. This signals to Entra that the admin is granting consent on behalf of the organization, not just for themselves. The target tenant admin clicks "Accept" during the PKCE flow.
- **Simplified admin consent link approach:** Session state stored only in HttpOnly cookie (10-min MaxAge), no token exchange, no ARM API calls. RBAC assignment shown as CLI command for admin to run manually. Replaced previous PKCE + two-hop OAuth due to coordinator directive (app registration already exists in source tenant).
- **AKS Identity Bindings is UAMI-only (2026-04-29):** Identity Bindings (preview) addresses the 20 FIC limit but ONLY for User-Assigned Managed Identities. It cannot help cross-tenant scenarios because UAMIs cannot have service principals in other tenants. The Identity Binding proxy routes token exchange for the UAMI; there's no path to get a multi-tenant app token through it. For cross-tenant, the FIC must remain on the multi-tenant Entra app.
- **Identity Bindings OIDC issuer is constant per UAMI:** The issuer URL `https://ib.oic.prod-aks.azure.com/<tenant-id>/<client-id>` is the same across ALL clusters with identity bindings for that UAMI. This is what allows a single FIC. However, this issuer is only valid when identity bindings exist; Entra ID won't recognize it for direct token exchange.
- **azidentity EnableAzureProxy option:** Go SDK v1.14.0-beta.3+ adds `WorkloadIdentityCredentialOptions.EnableAzureProxy` which routes token exchange through the in-cluster proxy instead of directly to `login.microsoftonline.com`. Required for Identity Bindings. The proxy injects additional env vars: `AZURE_KUBERNETES_TOKEN_PROXY`, `AZURE_KUBERNETES_SNI_NAME`, `AZURE_KUBERNETES_CA_FILE`.
- **FIC limit workarounds for cross-tenant:** (1) Request limit increase from Azure Support (soft limit), (2) Use client secret approach (violates zero-trust), (3) Wait for Identity Bindings to support app registrations in future release.

### 2026-04-29 — Identity Bindings Feasibility Assessment (COMPLETE)

- **DECISION MADE: Do NOT adopt Identity Bindings for cross-tenant scenarios.** Identity Bindings is fundamentally UAMI-only. The proxy routes to UAMI token endpoint; no SDK pattern exists for the double token exchange (SA → UAMI → multi-tenant app) needed for cross-tenant access.
- **Recommendation: Request FIC limit increase from Azure Support.** The 20-FIC limit is soft; Azure Support can increase it. Simplest path forward, no architecture changes.
- **Hybrid approach explored and rejected:** Trusting Identity Binding OIDC issuer from FIC on multi-tenant app doesn't work because the issuer is only valid with the proxy; direct Entra ID rejects it.
- **Fallback strategies documented:** Client secrets (viable but violates zero-trust), custom OIDC provider (complex), wait for IB GA with multi-tenant support (no timeline).

### 2026-04-29 — Identity Bindings Docs Update (COMPLETE)

- **Documented hybrid architecture in azure-setup.md per team request.** Despite previous feasibility finding, the team requested documentation of the Identity Bindings approach with appropriate PREVIEW warnings.
- **Key doc changes:** Updated intro flow, prerequisites (aks-preview extension, IdentityBindingPreview flag, webhook v1.6.0-alpha.1+ requirement), variables (IB_OIDC_ISSUER_URL, IDENTITY_BINDING_NAME), Step 2b (identity binding creation), Step 3 (skipped — AKS manages UAMI FIC), Step 4 FIC updated to use IB_OIDC_ISSUER_URL, Step 8 ServiceAccount/Deployment/ConfigMap updated for UAMI client-id + AZURE_CLIENT_ID override, new ClusterRole/ClusterRoleBinding apply steps.
- **PREVIEW warnings added throughout** to signal this is not production-ready.
- **Decision written** to `.squad/decisions/inbox/bishop-identity-bindings-docs.md`.

### 2026-04-30 — Identity Bindings FIC Fix (COMPLETE)

- **Root cause:** The FIC on the multi-tenant app `aks-xtenant-auth-app` (`7c9dd5ee-3444-4559-9edc-917b520b3081`) was configured with the Identity Binding `ib.oic.prod-aks.azure.com` issuer. Tokens actually presented to Entra use the cluster's standard OIDC issuer `https://eastus2.oic.prod-aks.azure.com/89dfbdeb-9c84-4dd4-9ce7-9932e30998de/f190254e-a0d2-4627-adb2-e101c3a32e34/`.
- **Fix 1:** Deleted old FIC (id `aad7ca5d`, issuer `ib.oic.prod-aks.azure.com/...`, audience `api://AzureADTokenExchange`). Created new FIC (id `09c0d7e3`) with issuer = cluster OIDC URL, audience = `api://AKSIdentityBinding`.
- **Fix 2:** Created service principal in home tenant for the multi-tenant app — SP was missing from the source tenant (SP object ID `cf15697a`). AADSTS7000229 resolved.
- **Outcome:** AADSTS700211 resolved. Token exchange now succeeds. Pod is obtaining an access token. Remaining error (`InvalidAuthenticationInfo: Issuer validation failed` from Azure Storage) is a cross-tenant RBAC/AZURE_TENANT_ID scoping issue, not a FIC issue.
- **Decision written** to `.squad/decisions/inbox/bishop-ib-fic-created.md`.

### 2026-04-30 — Cross-Tenant AZURE_TENANT_ID Override (COMPLETE)

- **Root cause:** Azure Storage endpoint was rejecting the token with `InvalidAuthenticationInfo: Issuer validation failed`. The token was scoped to the source tenant (home tenant `89dfbdeb`), but the storage account is in the target tenant (`c8c22f13`). Azure Storage validates that the token issuer and token tenant match the resource tenant.
- **Fix:** Override `AZURE_TENANT_ID` environment variable in the deployment to the **target tenant ID** (`c8c22f13-9f4d-41a5-b619-eda5a1d50c39`). The AKS workload identity webhook always injects `AZURE_TENANT_ID` from the ServiceAccount annotation (the source tenant). By adding an explicit env entry in the deployment spec that sources from a ConfigMap, the webhook sees the var is already set and skips injection. The ConfigMap value wins, so the SDK requests tokens scoped to the target tenant.
- **Implementation:**
  - Added `AZURE_TENANT_ID: "c8c22f13-9f4d-41a5-b619-eda5a1d50c39"` to `deploy/configmap.yaml`
  - Added `AZURE_TENANT_ID` env entry (valueFrom.configMapKeyRef) to `deploy/deployment.yaml` spec.containers.env before webhook injection point
  - ConfigMap patched live in cluster
  - Pod restarted
- **Pre-existing infrastructure verified:** SP `161bf58d` for app `7c9dd5ee` already existed in target tenant. `Storage Blob Data Contributor` RBAC already assigned on `rgxtenantsa`. Blob container `xtenant` already existed. Only the `AZURE_TENANT_ID` override was required.
- **Verification:** Pod logs show successful blob writes with no auth errors: `{"level":"INFO","msg":"blob written","timestamp":"2026-04-30T00:18:52Z"}`. Cross-tenant workload identity flow fully functional.
- **Key insight:** SP/RBAC pre-existed; the only missing piece was tenant scoping at the SDK level. The fix is manifests-only, zero code changes.
- **Decision written** to `.squad/decisions/inbox/bishop-cross-tenant-tenant-id.md`.

