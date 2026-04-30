# Project Context

- **Owner:** Ryan Graham
- **Project:** Containerized Go application that uses AKS workload identity to authenticate to a Microsoft Entra multi-tenant application, then writes timestamps to an Azure Blob Storage account in a different Azure tenant.
- **Stack:** Go, Docker (distroless/alpine), Kubernetes / AKS, Azure SDK for Go (azidentity, azblob), Microsoft Entra ID (multi-tenant app registration), Azure Blob Storage
- **Created:** 2026-04-29

## Learnings

### 2026-04-29 — Setup UI architecture documentation

- **Setup wizard is a one-time admin consent + RBAC role assignment flow.** Two-binary design (timestampwriter + setup) recommended for separation of concerns, different lifecycles. Single multi-stage Docker image reduces build complexity. Setup uses simplified admin consent link (no complex PKCE flow). No workload identity on setup pod. Sessions stored in HttpOnly cookie only (10-min MaxAge). RBAC assignment shown as CLI command for manual execution.
- **Simplified admin consent link (`https://login.microsoftonline.com/common/adminconsent?...`) replaces PKCE flow.** Transparent, admin-controlled, no token exchange needed. On callback, admin gets copy-paste CLI command and ConfigMap snippets. State validation via HttpOnly cookie.
- **Cross-tenant safety enforced by Microsoft Entra redirect validation.** The callback includes `admin_consent=True&tenant={tenantID}` only if the admin completed the flow in their tenant. No need for `tid` claim extraction.
- **Key risks: admin must have Owner or User Access Admin role on storage account (for RBAC assignment), pod restart loses session (rare during 30-min window).** Mitigations: clear documentation, admin controls RBAC execution (no automated API call), short setup window.

### 2026-04-29 — Identity Bindings Migration Scope Analysis

- **AKS Identity Bindings is a PREVIEW feature** requiring `IdentityBindingPreview` feature flag, webhook v1.6.0-alpha.1+, and `azidentity` v1.14.0-beta.3+. Triple-preview stack = high risk for production.
- **Go changes are minimal** — single line change from `NewWorkloadIdentityCredential(nil)` to `NewWorkloadIdentityCredential(&azidentity.WorkloadIdentityCredentialOptions{EnableAzureProxy: true})` plus SDK version bump.
- **K8s changes require new RBAC** — ClusterRole + ClusterRoleBinding authorizing ServiceAccount to use IdentityBinding resources. Also need pod annotation `azure.workload.identity/use-identity-binding: "true"`.
- **Critical architecture question for Bishop:** Does the SA annotation `client-id` stay as the multi-tenant Entra app's client ID, or must it change to the UAMI's client ID? This determines whether the current cross-tenant identity chain survives or needs redesign.
- **UAMI alone cannot do cross-tenant** (per existing `bishop-identity-chain.md`). If Identity Bindings requires UAMI-based identity, the cross-tenant pattern may need a hybrid approach or alternative solution.
- **Documentation impact is significant** — 600+ line azure-setup.md needs new sections for feature flag, Identity Binding creation, RBAC manifests, and revised identity chain explanation.

### 2026-04-29 — Identity Bindings Scope Analysis (COMPLETE)

- **Scope is technically achievable but NOT RECOMMENDED.** 8 affected files total: `cmd/timestampwriter/main.go`, `go.mod`, 3 K8s manifests, `docs/azure-setup.md`, `README.md`, and decision archive files.
- **Critical blocker identified: Identity chain viability.** If SA annotation must change from multi-tenant app client-id to UAMI client-id, the entire cross-tenant pattern needs redesign. Bishop must validate this is supported.
- **Key risks: BETA SDK (azidentity v1.14.0-beta.3), PREVIEW FEATURE flag (IdentityBindingPreview), ALPHA webhook version (v1.6.0-alpha.1).** Triple-preview stack creates production risk.
- **Recommendation: Hold on migration until Bishop confirms feasibility AND FIC limit increase is denied by Azure Support.** Current architecture is proven and stable. Only migrate if FIC limit becomes urgent blocker.
- **Prioritized change list created (10 items):** All contingent on Bishop's identity chain redesign. Scope serves as blueprint if migration becomes necessary.

