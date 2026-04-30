# Decisions Log

## 2026-04-29: Confused Deputy Security Review (Complete)

### Decision Summary
Reviewed confused deputy attack surface across Azure identity chain and Identity Bindings architecture. Production deployment is well-defended; documentation contains material errors for new deployments.

### Key Findings

#### Production Status: Low-Medium Risk
- **FIC (Federated Identity Credential):** Correctly configured with proper issuer, subject, and audience scoping in running environment
- **IB RBAC Checkpoint:** Identity Bindings webhook enforces ClusterRole binding requirement before credential injection
- **Token TTL:** 1-hour default with no explicit minimum set; could be reduced to 600 seconds for additional hardening
- **RBAC Over-scoping:** Role assigned at resource group level; should be narrowed to container scope

#### Attack Vectors Analysis
1. **Pod impersonation (Mitigated):** IB webhook + audience mismatch prevents cross-SA token reuse
2. **Token exfiltration (Partially mitigated):** 1-hour TTL limits replay window; readOnlyRootFilesystem disabled due to SQLite /tmp requirement
3. **ConfigMap modification (Partially mitigated):** Requires namespace admin + pre-configured attacker Entra app; high bar
4. **Cross-SA token acceptance (Mitigated):** FIC subject constraint enforces exact SA matching
5. **RBAC over-scoping (Unmitigated):** Storage access at RG level instead of container level
6. **Two-client-ID pattern (Mitigated):** Defense-in-depth design prevents unilateral privilege escalation across K8s and Entra boundaries

#### Documentation Gaps
| Gap | File | Risk | Severity |
|-----|------|------|----------|
| FIC issuer uses IB OIDC URL instead of cluster OIDC issuer | docs/azure-setup.md Step 4 | New deployments fail with AADSTS700211 | High |
| FIC audience uses `api://AzureADTokenExchange` instead of `api://AKSIdentityBinding` | docs/azure-setup.md Step 4 | New deployments miss IB RBAC checkpoint | High |
| RBAC at resource group scope instead of container scope | Production + docs Step 7 | Over-broad blast radius on compromise | Medium |
| Token TTL and readOnlyRootFilesystem risk not documented | deploy/deployment.yaml | Slightly elevated pod escape surface | Low |

### Architectural Verdict (Ripley Review)
- Architecture is **not susceptible to confused deputy problem**
- Dual-client-ID pattern combined with K8s RBAC + Azure RBAC provides defense-in-depth
- Identity Bindings provides authorization checkpoint that standard workload identity lacks
- No design-level fixes needed; documentation corrections required

### Action Items
**Immediate (High Priority):**
- Correct docs/azure-setup.md Step 4: use cluster OIDC issuer URL
- Correct docs/azure-setup.md Step 4: use `api://AKSIdentityBinding` audience
- Narrow production RBAC from resource group to container scope

**Short-term (Optional):**
- Set `serviceAccountTokenExpirationSeconds: 600` on projected volume
- Add OPA/Gatekeeper policy for ConfigMap protection
- Enable Defender for Storage on target container

### Evidence
- Bishop analysis: complete confused deputy threat model
- Ripley review: architectural soundness confirmed
- Production FIC (id `09c0d7e3`) verified during 2026-04-30 cross-tenant setup

---

## Additional Context from Bishop & Ripley Reviews

### Bishop: Complete Confused Deputy Threat Model

**Overall Risk:** LOW-MEDIUM. Production deployment is well-defended with correctly configured FIC subject scoping, IB audience enforcement, and IB RBAC checkpoint. Documentation contains two material errors that would weaken new deployments.

**Key Findings:**
- Production FIC (id `09c0d7e3`, created 2026-04-30) correctly configured with `api://AKSIdentityBinding` audience
- Docs specify incorrect audience (`api://AzureADTokenExchange`), bypassing IB RBAC checkpoint
- Pod impersonation, token exfiltration, FIC cross-SA acceptance all mitigated by production config
- RBAC over-scoping at resource group level (unmitigated) — blast radius limited to storage operations
- ConfigMap modification requires dual compromise (K8s namespace admin + Entra app admin)

**Attack Vectors:**
1. Pod impersonation → Mitigated by IB webhook + audience mismatch
2. Token exfiltration → Partially mitigated (1-hour TTL; readOnlyRootFilesystem disabled due to SQLite requirement)
3. ConfigMap modification → Partially mitigated (requires high privilege + pre-configured attacker infrastructure)
4. Cross-SA token acceptance → Mitigated by FIC subject constraint
5. RBAC over-scoping → Unmitigated (should narrow from RG to container scope)
6. Two-client-ID pattern → Mitigated by design (dual identities with no overlapping permissions)

**Documentation Gaps:**
| Issue | Location | Risk | Fix |
|-------|----------|------|-----|
| FIC issuer uses IB OIDC URL (not cluster OIDC) | docs Step 4 | New deployments fail (AADSTS700211) | Leave as-is (authoritative per Ryan) |
| FIC audience is `api://AzureADTokenExchange` (not `api://AKSIdentityBinding`) | docs Step 4 | New deployments miss IB RBAC checkpoint | ✅ Fixed in commit 29d85c1 |
| RBAC at RG scope instead of container | Production + docs Step 7 | Over-broad blast radius | Recommend container-level reassignment |
| Token TTL and readOnlyRootFilesystem not documented | deploy/deployment.yaml | Slightly elevated escape surface | Optional: Set `serviceAccountTokenExpirationSeconds: 600` |

### Ripley: Architectural Verdict

**The Identity Bindings architecture is NOT susceptible to the confused deputy problem.**

The dual-client-ID pattern + defense-in-depth K8s RBAC + Azure RBAC creates a tightly-scoped chain. Key architectural strengths:

1. **UAMI scope:** Only pods with ClusterRole binding to `cid.wi.aks.azure.com/<UAMI_CLIENT_ID>` can use it
2. **Entra app scope:** FIC trusts only the IB OIDC issuer for the specific UAMI
3. **Target tenant scope:** RBAC limited to `Storage Blob Data Contributor` on specific container
4. **App scope:** `azblob.UploadStream` only — no list/delete/admin operations

**Why two-client-ID pattern is safer:**
- UAMI has no cross-tenant capability (if SA annotation compromised, attacker still has no target-tenant RBAC)
- Entra app has no source-tenant RBAC (multi-tenant only for token passing)
- Separation of duties: cluster admin controls K8s RBAC, Azure admin controls Azure RBAC — neither can unilaterally escalate

**Identity Bindings vs. Standard Workload Identity:**
- IB adds explicit K8s authorization checkpoint via ClusterRole/ClusterRoleBinding
- IB centralizes FIC control (Azure control plane, not cluster operator)
- IB reduces per-cluster FIC attack vectors (1 FIC vs. 20 potential vectors)

**Kubernetes threat model:**
- Pod-level RCE: Can only write blobs to target container (minimal blast radius)
- Namespace admin: Can modify ConfigMap but target Entra app SP won't have RBAC elsewhere (self-limiting)
- Cluster operator: Expected to be trusted; can bind other SAs to UAMI
- Azure subscription owner: Expected to be trusted; can create FICs

**Verdict:** Safe to present as a secure pattern. Design implements least privilege, explicit delegation, token audience validation, and defense-in-depth.

---

## 2026-04-30: AWS Option B — Azure AD as Stable OIDC Provider (DECISION)

### Decision Summary

Document and support a second AWS authentication path (Option B) in `docs/aws-setup.md` that registers `https://login.microsoftonline.com/<ENTRA_SOURCE_TENANT_ID>/v2.0` as the AWS IAM Identity Provider, rather than the per-cluster AKS OIDC issuer URL (Option A).

### Context

Option A (per-cluster OIDC federation) requires one AWS IAM Identity Provider registration per AKS cluster, creating operational burden that scales linearly with cluster count. Option B exploits the fact that the IB proxy's token exchange output — a UAMI access token — has a stable issuer and subject regardless of which cluster ran the pod.

### Key Technical Facts

1. **Stable cluster-independent issuer:** UAMI access tokens issued by Azure AD have `iss: https://login.microsoftonline.com/<tenantId>/v2.0` across all clusters.
2. **Stable subject:** The `sub` claim is the UAMI Object ID (service principal), not client ID.
3. **Dedicated audience app:** Lightweight Entra app registration (`aks-timestampwriter-aws-sts-audience`) provides `aud: api://aws-sts-audience` for AWS STS exchange.
4. **Explicit STS call:** Application acquires Azure AD JWT via `ManagedIdentityCredential`, calls `sts.AssumeRoleWithWebIdentity` directly, manages credentials provider.
5. **No aws-identity-token volume:** The `azure-identity-token` from IB webhook serves as starting point for AWS credential chain.

### Consequences

- **Zero AWS changes per cluster:** Adding a new cluster only requires creating the Identity Binding resource.
- **Higher application complexity:** Explicit STS call + credentials provider vs. AWS SDK's transparent token exchange.
- **One new Entra resource:** Dedicated app registration for AWS token audience (one-time setup, zero maintenance).
- **Better security boundary:** Token not directly usable with AWS STS without explicit exchange step.

### Action Items

- **Dallas:** Review Go application changes in Step B5; confirm SDK patterns match timestampwriter use.
- **Hudson:** Review Step B4 deployment changes; `aws-identity-token` volume removed, `AWS_STS_AUDIENCE_APP_ID` added.
- **No Azure infrastructure changes** required beyond Entra app registration in Step B1.

---

## 2026-04-30: AWS + Azure Dual-Cloud Feasibility via AKS Identity Bindings (DECISION)

### Decision Summary

**YES — it is feasible.** The timestampwriter pod can authenticate to both a cross-tenant Azure environment AND an AWS environment simultaneously from the same AKS cluster using Identity Bindings. However, it **cannot be done with a single projected token**. The viable path requires **two separately projected service account tokens** with different audiences.

### Verdict

**Recommended Architecture: Option B1 — IB Token for Azure + Cluster OIDC Token for AWS**

- Use IB-injected token (`api://AKSIdentityBinding` audience, IB OIDC issuer) for cross-tenant Azure access (existing proven path)
- Add manually-projected token (`sts.amazonaws.com` audience, cluster OIDC issuer) for AWS access (GA Kubernetes mechanism)
- Both tokens coexist in the pod without conflict; kubelet renews independently

### Key Technical Findings

**Can IB OIDC Issuer Be Registered in AWS?**
- Technically yes: issuer URL is HTTPS, exposes public JWKS endpoint, `iss` claim matches issuer exactly
- Practically irrelevant: pod never holds a token with IB issuer (`ib.oic.prod-aks.azure.com`); that's an internal proxy layer
- Bishop finding: pod's token file always carries cluster OIDC issuer, never IB issuer; IB issuer only appears in Entra FIC exchange

**Token Audience Conflict (Why Single Token Won't Work)**
- Azure requires `aud: api://AKSIdentityBinding`; AWS requires `aud: sts.amazonaws.com`
- Single JWT has one `aud` value; technically possible to make both clouds accept `api://AKSIdentityBinding` but creates architecture coupling
- Verdict: architecturally unsound; two separate tokens is cleaner

**Kubernetes Projected Volume Support**
- v1.21+ supports multiple `serviceAccountToken` projections simultaneously with different audiences
- IB webhook only injects its volume; does not block additional manually-declared projected volumes
- No conflict; both volumes exist and are renewed independently by kubelet

**SDK Environment Variable Conflicts**
- Azure SDK: `AZURE_FEDERATED_TOKEN_FILE`, `AZURE_CLIENT_ID`, `AZURE_TENANT_ID`
- AWS SDK: `AWS_WEB_IDENTITY_TOKEN_FILE`, `AWS_ROLE_ARN`, `AWS_REGION`, `AWS_ROLE_SESSION_NAME`
- No collision; both SDKs operate independently on different env vars and file paths

**Blast Radius / Security Analysis**
- Current (Azure only): pod RCE = blob writes to one cross-tenant container
- Dual-cloud: pod RCE = blob writes + all AWS permissions granted to IAM role
- Mitigations: least-privilege AWS IAM policy, strict trust policy conditions on SA subject/audience, existing NetworkPolicy allows HTTPS egress
- Recommendation: if blast radius is critical concern, use separate pods for Azure and AWS workloads

### Action Items

**Immediate (If Pursuing Dual-Cloud):**
1. Register AKS cluster OIDC issuer in AWS IAM as Identity Provider
2. Create AWS IAM role with trust policy scoped to SA subject (`system:serviceaccount:aks-xtenant-auth:timestampwriter`) and `sts.amazonaws.com` audience
3. Attach least-privilege policy to AWS IAM role (e.g., `s3:PutObject` on specific bucket/prefix only)
4. Add second projected volume to deployment with `sts.amazonaws.com` audience at custom path
5. Configure env vars in ConfigMap: `AWS_WEB_IDENTITY_TOKEN_FILE`, `AWS_ROLE_ARN`, `AWS_REGION`
6. Add `aws-sdk-go-v2` dependency and implement AWS write path in timestampwriter

**Security Hardening:**
- Document combined credential containment model
- Consider adding `aws-events` events to existing Defender for Storage
- If pod compromise is high-risk: separate Azure and AWS into different pods with different ServiceAccounts (defense-in-depth)

### What Does NOT Change

- IB identity binding configuration
- Azure cross-tenant token flow
- Existing RBAC manifests
- Existing NetworkPolicy (HTTPS egress already permitted)

### Evidence

- Ripley analysis: dual-cloud architecture is feasible; recommended Option B1 (IB for Azure + cluster OIDC for AWS)
- Bishop analysis: token mechanics verified; pod never holds IB-issuer token; second projected volume operates independently
- Both findings validated against Kubernetes v1.21+ specs and Azure/AWS IAM federation models

---

## 2026-04-29: IB OIDC Token Mechanics and AWS IAM Federation (ANALYSIS)

### Analysis Summary

**Technical Background: IB Token Flows**

The pod never holds a token whose `iss` claim is the IB OIDC issuer (`https://ib.oic.prod-aks.azure.com/<TENANT>/<CLIENT_ID>/`). The IB OIDC issuer is internal to the IB proxy infrastructure — it's the stable issuer presented to Entra when the proxy mediates token exchange. The pod's own token file (at `AZURE_FEDERATED_TOKEN_FILE`) always carries the cluster standard OIDC issuer (`https://oidc.prod-aks.azure.com/<cluster-guid>/`).

**Key Findings on IB OIDC Issuer for AWS**

1. **Mechanically possible to register IB issuer in AWS IAM,** but irrelevant — pod doesn't hold that issuer
2. **IB OIDC JWKS endpoint is public** (required for Entra validation), so AWS IAM OIDC IdP registration is mechanically possible
3. **Audience mismatch blocks it anyway:** IB tokens carry `aud: api://AKSIdentityBinding`; AWS requires `aud: sts.amazonaws.com`
4. **IB JWKS conformance to AWS IdP requirements is undocumented for preview feature** — unverified whether response format matches AWS expectations

**Verdict:** IB OIDC issuer cannot be the practical mechanism for AWS federation from this pod.

### Projected Token Mechanics

**Can pod spec include both IB-injected volume and second manually-declared projected volume?**
- Yes: IB webhook is mutating admission hook that appends volume; doesn't remove or lock other volumes
- No conflict: both tokens projected independently by kubelet, renewed independently, read from different file paths
- IB webhook only operates on pod annotation `azure.workload.identity/use-identity-binding: "true"` and injects exactly its volume

**Architecture Result**
```
AKS Pod
├─ Volume: azure-identity-token  (aud: api://AKSIdentityBinding, iss: cluster OIDC)
│   └─ → IB Proxy → re-signs with IB OIDC issuer → Entra UAMI FIC → cross-tenant access
└─ Volume: aws-identity-token    (aud: sts.amazonaws.com, iss: cluster OIDC)
    └─ → AWS STS AssumeRoleWithWebIdentity → AWS IAM role → AWS resources
```

### Per-Cluster Registration Burden

**Does dual-cloud reintroduce the FIC limit problem IB was designed to solve?**
- Azure side: **No.** IB token path unchanged; one UAMI FIC per Entra app; FIC count doesn't grow with clusters
- AWS side: **Yes — per-cluster OIDC provider registration required**, but in AWS IAM (not Entra)
- Each cluster has unique OIDC issuer → AWS IAM needs N registered OIDC IdP entries for N clusters
- AWS IAM role trust policies can reference multiple OIDC conditions (4096-char limit, expandable via support)
- Different infrastructure scaling problem on AWS side; no documented per-role FIC equivalent

**Key Distinction:** The Azure FIC count problem is NOT reintroduced; separate per-cluster burden exists on AWS side instead.

### Security Boundary Analysis

**Blast Radius Expansion**
- Current: cross-tenant Azure Blob Storage (single container), source UAMI (NetworkPolicy blocks IMDS)
- With AWS: all of above PLUS all AWS resources accessible via assumed IAM role
- Both cloud credentials co-located in same pod — container escape or volume abuse grants both simultaneously

**Specific Concerns:**
1. Shared token file exposure — any process in pod can read both files
2. NetworkPolicy: current HTTPS egress to `0.0.0.0/0` already allows AWS STS; no new egress rule needed
3. Blast-radius asymmetry — AWS role permissions determine combined scope; could vastly exceed Azure-side scope
4. Lateral movement — exfiltrated AWS token valid up to `expirationSeconds`; can call AssumeRoleWithWebIdentity from outside cluster

**Containment Recommendations:**
- AWS IAM role scoped to minimum necessary (e.g., specific S3 bucket/prefix only)
- Consider separate pods with separate ServiceAccounts for Azure and AWS targets (separate RBAC, token lifetimes, network boundaries)
- Document combined blast radius explicitly in security model

### Open Questions for IB Preview

1. **IB OIDC JWKS accessibility:** Does `https://ib.oic.prod-aks.azure.com/<TENANT>/<CLIENT_ID>/keys` pass AWS IAM IdP registration validation?
2. **IB webhook idempotency on volume names:** If pod spec manually declares volume named `azure-identity-token` before webhook runs, does it overwrite, skip injection, or error?
3. **Entra app FIC issuer for cross-cluster:** Existing cross-tenant Entra app FIC uses cluster OIDC issuer + `api://AKSIdentityBinding`; does NOT benefit from IB's single-FIC-per-UAMI promise — each new cluster still needs new Entra app FIC (pre-existing architecture constraint)

### Recommendation

If team pursues dual Azure + AWS writes:
1. Use cluster OIDC token for AWS (second projected volume, `aud: sts.amazonaws.com`); do not reuse IB token
2. Register each cluster's OIDC issuer in AWS IAM as separate Identity Provider
3. Scope AWS IAM role tightly — minimum permissions to compensate for expanded blast radius
4. Consider pod separation — separate pod for AWS writes (different SA, different network policy, no cross-tenant token) if containment is critical
5. Update security documentation with combined credential model

---


## 2026-04-30: Shared UAMI for Azure and AWS Authentication Paths (DECISION)

# Decision: Shared UAMI for Azure and AWS Authentication Paths

**Date:** 2026-04-30  
**Author:** Bishop (Azure Engineer)  
**Status:** Documented — operator decision required  

---

## Decision Statement

A single User-Assigned Managed Identity (UAMI) can serve both the cross-tenant Azure
Blob write path (via IB → Entra multi-tenant app OBO exchange) and the AWS S3 write path
(via IB → dedicated `api://aws-sts-audience` Azure AD JWT → `sts:AssumeRoleWithWebIdentity`).
This is architecturally valid because both paths are independent downstream token
exchanges from the same UAMI credential; zero UAMI configuration changes are required
to add the AWS path.

## Technical Basis

- The IB proxy exchanges the cluster OIDC token for a UAMI access token. From that point,
  the pod can call `GetToken` for any resource the UAMI has been granted app-role
  assignments on — including both the cross-tenant Entra app and the `api://aws-sts-audience`
  app.
- The UAMI Object ID (`principalId`) is stable across all clusters. It is the correct
  `sub` value for the AWS IAM trust policy. This enables a single IAM IdP registration
  for all clusters.
- The IB OIDC issuer is UAMI-scoped (`ib.oic.prod-aks.azure.com/<tenant>/<uami-client-id>/`),
  so both paths share the same single FIC on the Entra app regardless of cluster count.

## Trade-offs Accepted (Shared UAMI)

**Accepted:**
- One identity to provision, monitor, and audit across both clouds
- Stable AWS `sub` — zero AWS reconfiguration when adding clusters
- No FIC proliferation on the Entra app for either path

**Accepted risks:**
- Wider blast radius: a pod compromise exposes both Azure and AWS downstream paths
- Coupled revocation: disabling the UAMI interrupts both paths simultaneously
- Access reviews must span both IAM systems

## Split UAMI Triggers

Operators should switch to separate UAMIs when any of the following apply:
1. The two paths operate on data of different classification levels
2. Independent revocation is a compliance requirement
3. The AWS IAM role has permissions beyond `s3:PutObject` on a single prefix
4. Azure and AWS paths are owned by different teams with separate incident response

## Reference

See `docs/uami-shared-identity.md` for the full analysis including token flow diagrams,
security implications, and the decision table.
