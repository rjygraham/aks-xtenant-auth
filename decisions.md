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
