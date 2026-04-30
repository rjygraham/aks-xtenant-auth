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

### 2026-04-29 — Confused Deputy Architectural Security Review

- **Verdict: Architecture is NOT susceptible to confused deputy.** The two-client-ID pattern (UAMI for IB RBAC, Entra app for cross-tenant token exchange) is intentional defense-in-depth, not a vulnerability.
- **Key insight: Separation of identities REDUCES attack surface.** UAMI has no cross-tenant capability; Entra app has no source-tenant RBAC. Neither identity alone can complete the full attack chain.
- **Identity Bindings improves security posture vs. standard workload identity.** The ClusterRole/ClusterRoleBinding becomes an explicit Kubernetes-level authorization checkpoint before the webhook injects credentials. Standard workload identity lacks this gate.
- **Blast radius analysis:** Pod-level RCE is limited to blob writes on one container. Namespace admin can't escalate (Entra app SP has no RBAC elsewhere). Cluster operator and subscription owner trust is assumed.
- **No design changes required.** Optional hardening: OPA policy on ConfigMap, Azure Policy on SP RBAC, Defender for Storage. These are belt-and-suspenders, not fixes.
- **Safe to present as a secure pattern** for cross-tenant authentication demonstrations.

### 2026-04-30 — Confused Deputy Analysis Complete

- **Delivered complete architectural security review.** Six attack vectors analyzed (pod impersonation, token exfiltration, ConfigMap modification, FIC cross-SA acceptance, RBAC over-scoping, two-client-ID pattern).
- **Production architecture verdict: LOW-MEDIUM RISK.** Well-defended on identity chain; unmitigated RBAC over-scoping at RG level (should narrow to container scope). Documentation contained errors (fixed by Bishop in commit 29d85c1).
- **Concurrent Bishop analysis confirmed.** 6-vector threat model matches architectural review findings.
- **Decision merged into primary decisions.md entry.**

### 2026-04-29 — Security Mitigations Documentation

- **Created `docs/security.md`** — full security review document for the cross-tenant AKS workload identity solution. Covers five sections: threat model overview (with trust boundary diagram and token exchange flow), confused deputy mitigation (three independent enforcement layers), six additional attack vectors from the security audit, honest residual risks, and a quick-reference operator checklist.
- **Three-layer confused deputy defense documented explicitly:** (1) IB RBAC `SubjectAccessReview` gate, (2) FIC exact-subject constraint (`system:serviceaccount:aks-xtenant-auth:timestampwriter`), (3) audience scoping (`api://AKSIdentityBinding` only). Explained why all three must be bypassed simultaneously.
- **SA impersonation threat documented honestly** — acknowledged as viable attack path if namespace write access is obtained; documented that Kubernetes has no pod-level RBAC primitive and the control boundary is namespace discipline.
- **All cross-references included:** `deploy/clusterrole.yaml`, `deploy/clusterrolebinding.yaml`, `deploy/networkpolicy.yaml`, `cmd/timestampwriter/main.go`, `cmd/setup/main.go`, `docs/azure-setup.md`.
- **Operator checklist** includes 11 controls with concrete verification guidance (exact audience strings, CIDR values, regex patterns).

### 2026-04-30 — Broad Code & Security Audit

- **Overall verdict: LOW risk posture.** Token handling, CSRF, consent flow, RBAC manifests, and image supply chain are all sound. No exploitable vulnerabilities in the primary threat model identified.
- **Token handling is correct.** `NewWorkloadIdentityCredential(nil)` called once at startup is the right pattern. SDK handles token refresh automatically by re-reading `AZURE_FEDERATED_TOKEN_FILE` on each acquisition. No token cache poisoning risk.
- **Consent flow is not PKCE.** The `Dockerfile.setup` has a stale comment saying "PKCE callbacks" — the actual implementation (per Dallas/Simplified Setup Wizard decision) is a plain admin consent redirect with no token exchange. State CSRF protection (128-bit crypto/rand, HttpOnly cookie, SameSite=Lax) is correctly implemented.
- **Two input validation gaps on storage SDK call paths.** `container_name` and `storage_account_name` from SQLite flow into Azure SDK calls without regex validation. Low exploitability (requires NFS write access), but cheap to fix with `^[a-z0-9]{3,24}$` and `^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$` checks.
- **Missing NetworkPolicy.** No `NetworkPolicy` in `deploy/` means any pod in the cluster can reach the setup wizard's HTTP server on 8081 via ClusterIP. A `NetworkPolicy` restricting ingress to kubectl port-forward clients is the highest-priority manifest fix.
- **`readOnlyRootFilesystem: false` on both pods** is an accepted trade-off documented in the manifests (modernc.org/sqlite writes to /tmp). Remaining controls (non-root, drop ALL caps, RuntimeDefault seccomp) maintain meaningful depth.
- **No liveness probe on timestampwriter** — operational gap, not security gap. A hung write loop is invisible to Kubernetes.
- **Priority fixes documented in `.squad/decisions/inbox/ripley-security-audit.md`** (7 items, 4 code/manifest changes).

### 2026-04-30 — AWS + Azure Dual-Cloud Feasibility (DECISION)

- **VERDICT: YES — feasible.** Dual-cloud access from single AKS pod requires **two separately projected service account tokens** with different audiences (not single token).
- **Recommended Architecture: Option B1.** IB token for Azure (existing, proven path); cluster OIDC token for AWS (GA Kubernetes mechanism). Both tokens mount simultaneously in pod without conflict.
- **Why single token fails:** Azure requires `aud: api://AKSIdentityBinding`; AWS requires `aud: sts.amazonaws.com`. Technically possible to couple both clouds to one audience, but violates least privilege and architecturally unsound.
- **Kubernetes v1.21+ supports multiple projected SA token volumes** with different audiences. IB webhook injects only its volume; does not block manually-declared projected volumes. No webhook conflict.
- **Blast radius analysis:** Pod RCE expands from one Azure container to all AWS resources accessible via IAM role. Mitigated by least-privilege AWS IAM policy, strict trust policy conditions on SA subject, existing NetworkPolicy allows HTTPS egress. Optional: separate pods for Azure/AWS (defense-in-depth).
- **Implementation steps:** Register AKS cluster OIDC issuer in AWS IAM, create AWS IAM role with trust policy scoped to SA subject + `sts.amazonaws.com` audience, attach least-privilege policy, add second projected volume, configure env vars, add `aws-sdk-go-v2` dependency.
- **What does NOT change:** IB binding configuration, Azure cross-tenant flow, existing RBAC manifests, existing NetworkPolicy.
- **Decision merged into primary decisions.md.**