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

### 2026-04-30 — Shared UAMI: Azure + AWS Dual-Path Documentation

**Work completed:** Created `docs/uami-shared-identity.md` documenting whether and how
a single UAMI can serve both the cross-tenant Azure Blob path (IB → Entra OBO → Storage)
and the AWS S3 path (IB → `api://aws-sts-audience` Azure AD JWT → STS).

**Key documented facts:**

- **Both paths are architecturally valid from a single UAMI.** The fork happens after
  the IB proxy exchange — the pod holds a single UAMI credential and calls `GetToken`
  independently for each target resource. No UAMI configuration changes required to add
  the AWS path.
- **What stays the same:** UAMI, IB webhook, pod annotation, ServiceAccount annotation,
  ClusterRole/Binding, FIC on Entra app — identical for both paths.
- **What changes per path:** `GetToken` resource/audience, target tenant (Azure path
  only), downstream call (Blob SDK vs. STS + S3 SDK).
- **Shared UAMI advantages:** Operational simplicity, stable AWS `sub` (UAMI OID,
  not cluster OIDC), consistent IB setup, audit coherence (single identity in both
  Azure Monitor and CloudTrail), no FIC proliferation.
- **Shared UAMI risks:** Wider blast radius (both clouds reachable from single pod
  compromise), coupled revocation (no partial disable), single point of failure for
  identity infrastructure, entangled access reviews.
- **Security nuance on confused deputy:** Three IB enforcement layers protect the UAMI
  token acquisition. But once the IB gate is passed, no per-path gate exists on
  downstream token exchanges — blast radius of a successful IB gate bypass is wider
  with a shared UAMI.
- **Split UAMI triggers:** Different data classification, compliance-required
  independent revocation, AWS role permissions broader than `s3:PutObject` on single
  prefix, different owning teams.
- **Monitoring key:** UAMI Object ID is the cross-cloud correlation key for Azure
  Monitor + CloudTrail. Separate alerts required per audience (Azure-path vs. AWS-path)
  to avoid noise.

### 2026-04-29 — Azure & Identity Security Audit

- `docs/azure-setup.md` still mixes the accepted simplified admin-consent setup with a superseded PKCE/Graph/ARM design. That drift would cause operators to add unnecessary delegated permissions, redirect URIs, and broader admin expectations.
- For the current working Identity Bindings sample, the Entra app FIC must use the **cluster standard OIDC issuer** plus audience `api://AKSIdentityBinding` and the exact ServiceAccount subject. The internal `ib.oic.prod-aks.azure.com/...` issuer remains the wrong value for the app FIC.
- The setup wizard success page currently emits two bad operator instructions: it suggests ServiceAccount `azure.workload.identity/client-id` should be the **app** client ID instead of the **UAMI** client ID, and it generates `Storage Blob Data Contributor` at **storage-account scope** instead of container scope.
- The shared SQLite/NFS design is only weakly isolated. Both pods mount the same RWX PVC, `timestampwriter` does not mount it read-only, and any same-namespace pod that can mount the claim could read or tamper with `setup.db`.

## 2026-04-30 — Broad Security Audit: Repeated FIC Issuer Error

**Finding:** During the 2026-04-30 security audit (commit e74fe76), Bishop repeated the prior FIC issuer confusion: advised that the Entra app FIC issuer should be the cluster standard OIDC URL (marked as "correct in the docs"). This contradicted the accepted decision log (`decisions/bishop-identity-chain.md`) and production-verified configuration (FIC `09c0d7e3` uses cluster standard OIDC issuer).

**Team Decision:** Rejected. The existing production FIC configuration is correct. Cluster standard OIDC issuer + `api://AKSIdentityBinding` audience + exact ServiceAccount subject is the verified working path. No code change required. Documented in `decisions.md` Security Audit 2026-04-30 entry.

**Severity:** Low (no action taken; decision log is authoritative). Indicates this topic requires extra clarity in future documentation or training.

### 2026-04-30 — AWS Option B: Azure AD as Stable OIDC Provider for AWS

**Work completed:** Added Option B section to `docs/aws-setup.md` documenting the stable-identity AWS path using Azure AD as the OIDC provider for AWS IAM.

**Key technical facts documented:**

- **UAMI access token issuer is stable.** After the IB proxy exchanges the cluster OIDC token, the resulting UAMI access token has `iss: https://login.microsoftonline.com/<tenantId>/v2.0` — this is cluster-independent. Registering that endpoint as the AWS IAM IdP gives a single registration for all clusters.
- **`sub` in UAMI access token is the Object ID, not the client ID.** The `principalId` from `az identity show` is the correct value for the IAM trust policy `sub` condition. Using the `clientId` (application ID) here is a silent misconfiguration that causes every STS call to fail.
- **Dedicated Entra app for audience isolation.** The token presented to AWS STS must have a stable, purpose-scoped `aud` claim. Creating a dedicated app registration (`api://aws-sts-audience`) avoids coupling AWS auth to production Azure resource audiences. The UAMI must have an app role assignment on this app before Azure AD will issue tokens for it.
- **2-hop exchange chain (vs 1 hop in Option A).** Cluster OIDC → IB proxy → UAMI access token → dedicated-audience Azure AD JWT → AWS STS. More hops, but the `iss` and `sub` are stable across all clusters.
- **No `aws-identity-token` projected volume needed.** Option B removes the manual volume entirely. The `azure-identity-token` from the IB webhook is the starting point for both Azure and AWS auth.
- **Application code must call STS explicitly.** `AWS_WEB_IDENTITY_TOKEN_FILE` is not set; the AWS SDK's auto-detection flow is not used. Application acquires an Azure AD JWT for the audience app, calls `sts.AssumeRoleWithWebIdentity` directly, builds a custom credentials provider.
- **Token refresh is application responsibility.** `ManagedIdentityCredential.GetToken` caches and refreshes the UAMI token; calling it before each STS exchange is the simplest correct approach for a long-running pod.


- **Critical Finding: Pod never holds token with IB OIDC issuer.** IB proxy **re-signs** cluster's standard OIDC token when exchanging with Entra. Pod's token file always carries cluster OIDC issuer (`https://oidc.prod-aks.azure.com/<cluster-guid>/`). IB OIDC issuer (`ib.oic.prod-aks.azure.com`) is internal proxy infrastructure.
- **Implication for AWS:** Registering IB OIDC issuer in AWS IAM is mechanically possible (JWKS endpoint is public) but irrelevant because pod never holds token with that issuer.
- **Audience mismatch blocks single-token approach.** Azure FIC requires `aud: api://AKSIdentityBinding`; AWS IAM requires `aud: sts.amazonaws.com`. IB webhook projects single audience. Separate tokens required.
- **Two-volume architecture confirmed viable.** IB webhook is mutating admission controller that appends volume; does not block other volumes. Kubernetes projects both tokens independently, renewed independently, no conflict.
- **Per-cluster AWS OIDC registration required.** Each cluster has unique OIDC issuer; AWS IAM needs one registered OIDC IdP per issuer. For N clusters: N registrations in AWS. AWS IAM role trust policies support multiple conditions (4096-char limit, expandable).
- **Azure FIC count problem NOT reintroduced.** Azure side unchanged; IB handles FIC scaling. Separate per-cluster burden exists on AWS side (different infrastructure, no per-role FIC equivalent).
- **Security boundary expanded.** Dual-cloud pod blast radius includes all AWS resources accessible via IAM role (vs. single Azure container). Token files co-located in pod. Lateral movement risk: exfiltrated AWS token callable from outside pod. Containment: least-privilege AWS IAM role, consider pod separation (separate SA, RBAC, NetworkPolicy).
- **IB preview open questions:** JWKS format conformance to AWS IdP requirements (unverified); webhook idempotency on volume name conflicts (test required); cross-cluster Entra app FIC issuer does not benefit from single-FIC-per-UAMI promise (pre-existing constraint).
- **Recommendation:** Use cluster OIDC token for AWS (second projected volume), register each cluster issuer in AWS IAM, scope AWS IAM role tightly, consider pod separation if containment critical, document combined credential model.
- **Decision merged into primary decisions.md.**

### 2026-04-30 — UAMI Shared Identity Documentation

**Work completed:** Created `docs/uami-shared-identity.md` — comprehensive analysis of using a single UAMI for both cross-tenant Azure and AWS authentication paths.

**Content:**
- ASCII token flow diagram showing fork point after IB proxy
- Comparison table: shared UAMI vs. split UAMI
- 5 architectural advantages
- 5 accepted trade-offs with blast radius analysis
- 6 security sections: confused deputy implications, token co-location risks, IAM sub pinning, separation of duty, monitoring/alerting patterns, split UAMI decision criteria
- Decision table for when to split
- Cross-references to decision log

**Key insight:** Both paths are independent downstream branches from the same UAMI credential. Zero UAMI configuration changes required to add AWS path alongside Azure path. Trade-off is wider blast radius from pod compromise.
