# Shared UAMI: Using a Single Managed Identity for Azure and AWS Authentication

The same User-Assigned Managed Identity (UAMI) powers both the cross-tenant Azure Blob
write path and the AWS S3 write path. This is architecturally sound because both paths
derive their tokens from the same UAMI credential through **independent downstream token
exchanges** — the UAMI is an identity anchor, not a credential that is directly shared.
The IB mechanism, pod annotation, ServiceAccount configuration, and ClusterRole/Binding
remain identical regardless of which downstream targets are in use; what differs per path
is only the token audience and the target resource.

This document helps operators decide whether to use a shared UAMI or provision separate
UAMIs for the two paths.

---

## How It Works

### The Fork Point

After the AKS Identity Bindings webhook exchanges the cluster OIDC token for a UAMI
access token, the pod holds a single root credential: the projected `azure-identity-token`
at `AZURE_FEDERATED_TOKEN_FILE`. From that single root, two independent token exchanges
branch out.

```
AKS Pod
  │
  │  azure-identity-token (IB-projected)
  │  iss: ib.oic.prod-aks.azure.com/<tenant>/<uami-client-id>
  │  aud: api://AKSIdentityBinding
  │  sub: system:serviceaccount:aks-xtenant-auth:timestampwriter
  │
  ▼
IB Proxy  ──  SubjectAccessReview ──►  Kubernetes RBAC
  │           (use-managed-identity
  │            on UAMI resource)
  │
  ▼
UAMI Credential
  (ManagedIdentityCredential via azidentity)
  │
  ├─────────────────────────────────────────────────────────────────────┐
  │  AZURE PATH                                                         │  AWS PATH
  │                                                                     │
  │  GetToken(resource: cross-tenant Entra app)                         │  GetToken(resource: api://aws-sts-audience)
  │  AZURE_CLIENT_ID = <ENTRA_APP_CLIENT_ID>                            │  (dedicated Entra app in source tenant)
  │  AZURE_TENANT_ID = <TARGET_TENANT_ID>                               │
  │                                                                     │
  ▼                                                                     ▼
Entra ID (target tenant OBO exchange)                         Entra ID (source tenant)
  │                                                                     │
  │  Access token:                                                      │  Azure AD JWT:
  │  iss: login.microsoftonline.com/<TARGET_TENANT>/v2.0                │  iss: login.microsoftonline.com/<SOURCE_TENANT>/v2.0
  │  sub: <target-tenant SP object ID>                                  │  sub: <UAMI_OBJECT_ID>   ← principalId
  │  aud: https://storage.azure.com/                                    │  aud: api://aws-sts-audience
  │                                                                     │
  ▼                                                                     ▼
Azure Blob Storage (target tenant)                           AWS STS AssumeRoleWithWebIdentity
  Storage Blob Data Contributor                                         │
  scoped to: <container>                                                │  Trust policy conditions:
                                                                        │  Federated: oidc-provider/login.microsoftonline.com/<tenantId>/v2.0
                                                                        │  sub: <UAMI_OBJECT_ID>
                                                                        │  aud: api://aws-sts-audience
                                                                        │
                                                                        ▼
                                                               Short-lived AWS credentials
                                                                 → S3 PutObject
```

### What Stays the Same (Both Paths)

| Component | Value |
|---|---|
| UAMI | Single managed identity — same `clientId`, same `objectId` |
| IB webhook trigger | Pod annotation `azure.workload.identity/use-identity-binding: "true"` |
| ServiceAccount annotation | `azure.workload.identity/client-id: <UAMI_CLIENT_ID>` |
| ClusterRole / ClusterRoleBinding | One pair; grants `use-managed-identity` on UAMI resource |
| FIC on Entra app | One FIC; `iss: IB OIDC issuer`, `aud: api://AKSIdentityBinding`, `sub: system:serviceaccount:aks-xtenant-auth:timestampwriter` |

### What Changes Per Path

| Component | Azure Path | AWS Path |
|---|---|---|
| `GetToken` resource | Cross-tenant Entra app (`AZURE_CLIENT_ID` override) | `api://aws-sts-audience` app |
| Target tenant | `AZURE_TENANT_ID` = target tenant | Source tenant only |
| Token audience | `https://storage.azure.com/` | `api://aws-sts-audience` |
| Downstream call | Azure Blob SDK write | `sts:AssumeRoleWithWebIdentity` → S3 SDK write |

The two token requests are issued sequentially by application code; they are entirely
independent at the protocol level.

---

## Advantages of a Shared UAMI

### 1. Operational Simplicity

A single managed identity means a single object to provision, monitor, alert on, and
decommission. There is one UAMI resource ID to put in runbooks, one identity to grant
app-role assignments, one principal to include in PIM reviews. Adding a new write target
(another Azure tenant, another AWS account) requires configuring the downstream resource
and app-role — not a new UAMI.

### 2. Stable AWS Identity

The UAMI's Object ID (`principalId` from `az identity show`) is the `sub` claim in the
AWS IAM trust policy. Unlike a cluster OIDC issuer, the UAMI Object ID is a directory
object in the source Entra tenant — it is stable across every AKS cluster that uses this
UAMI. Adding clusters, replacing node pools, or scaling to new regions never changes the
`sub` value. The result: **one AWS IAM trust policy entry covers all clusters**, and
changing clusters never requires AWS reconfiguration.

```bash
# Retrieve the stable sub value once
az identity show --name <uami-name> --resource-group <rg> --query principalId -o tsv
```

### 3. Consistent IB Setup

Because all clusters share the same UAMI, the Identity Binding configuration is
identical everywhere:

- One `ClusterRole` + `ClusterRoleBinding` template (parameterized by UAMI client ID)
- One FIC on the Entra app (IB OIDC issuer is `ib.oic.prod-aks.azure.com/<tenant>/<uami-client-id>/` — this is constant for a given UAMI regardless of which cluster originates the request)
- One app-role assignment on the `api://aws-sts-audience` app

Adding a new AKS cluster requires only creating a new Identity Binding resource pointing
to the same UAMI. No new FICs, no new IAM trust policy entries, no new UAMI.

### 4. Audit Coherence

Every blob write and every S3 write is traceable to a single Entra identity. In Azure
Monitor / Entra sign-in logs, all token exchanges appear under the same UAMI Object ID.
In AWS CloudTrail, all `AssumeRoleWithWebIdentity` calls carry the same `sub`. Using the
UAMI Object ID as a join key lets operators build a unified audit trail across both
clouds from a single query:

```
Azure Monitor (sign-in logs): principalId = <UAMI_OBJECT_ID>
AWS CloudTrail: userIdentity.principalId contains <UAMI_OBJECT_ID>
```

### 5. No FIC Proliferation

The AKS Identity Bindings mechanism guarantees a single FIC per UAMI regardless of how
many clusters use it — the IB OIDC issuer is UAMI-scoped, not cluster-scoped. This
property applies equally to both the Azure cross-tenant path and the AWS path. A shared
UAMI means neither path contributes additional FICs as clusters scale. Separate UAMIs
would each require their own FIC on the Entra app (consuming from the per-app FIC quota
of 20).

---

## Disadvantages / Trade-offs

### 1. Wider Blast Radius on Compromise

If a vulnerability in the `timestampwriter` pod allows token exfiltration (e.g.,
arbitrary code execution, SSRF to the token file path, or a compromised container
image), the attacker can use the pod's network identity to call both the Azure token
exchange endpoint and the `api://aws-sts-audience` token endpoint. A shared UAMI means
**both clouds are reachable from a single pod compromise**. With separate UAMIs, a
pod-level compromise exposes only one downstream path.

### 2. Coupled Rotation and Revocation

Disabling or soft-deleting the UAMI interrupts both paths simultaneously. There is no
way to revoke only the AWS path (e.g., in response to an AWS-side incident) while keeping
the Azure path live without first splitting to separate UAMIs. Similarly, if the UAMI
must be rotated for compliance reasons, both the Azure and AWS configurations require
coordinated updates during the same maintenance window.

### 3. Single Point of Failure for Identity Infrastructure

If the UAMI becomes unavailable — soft-deleted, its resource group deleted, the source
tenant undergoes a disruptive change — both write paths fail simultaneously. The blast
radius of an infrastructure accident (accidental deletion, resource group lock removal)
spans both clouds. Separate UAMIs allow one path to remain operational while the other
is recovered.

### 4. Entanglement in Access Reviews

Periodic access reviews (e.g., PIM or custom reviews) for the UAMI must evaluate both
the Azure RBAC assignments (cross-tenant Storage Blob Data Contributor) and the AWS IAM
role permissions simultaneously. Reviewers unfamiliar with either system may incorrectly
mark the UAMI as compliant because they understand one context but not the other. This
creates a practical gap in access review quality.

### 5. Privilege Scope Is Harder to Reason About

An identity that simultaneously holds cross-tenant Azure blob write access and AWS S3
write access requires reviewers to understand two separate IAM systems and their
respective blast radii. It is harder to answer "is this identity over-privileged?"
when the answer spans Azure RBAC and AWS IAM. The cognitive burden increases further
if downstream permissions expand over time (additional Azure containers, additional S3
prefixes) — both contexts must be evaluated at each review cycle.

---

## Security Implications

### 1. Confused Deputy Risk — Mitigated But Not Eliminated

The three IB enforcement layers protect the initial UAMI token exchange:

- **Layer 1:** Kubernetes `SubjectAccessReview` — only the exact ServiceAccount in the
  exact namespace receives the IB-projected token
- **Layer 2:** FIC subject constraint — Entra ID rejects assertions not carrying
  `sub: system:serviceaccount:aks-xtenant-auth:timestampwriter`
- **Layer 3:** Audience scoping — `api://AKSIdentityBinding` is required; IMDS tokens
  and standard WI tokens cannot be substituted

These three layers apply to the IB proxy exchange only. Once the pod has a UAMI
credential, there is **no per-path gate** on the downstream token exchanges. A pod
that has legitimately passed the IB gate can call both the Azure token endpoint and
the `api://aws-sts-audience` token endpoint. The blast radius of a confused deputy
attack that successfully passes the IB gate is therefore wider with a shared UAMI
than with separate UAMIs — one gate protects two downstream resources.

### 2. Token Co-Location in the Pod

The `azure-identity-token` projected by the IB webhook is the root of both downstream
exchanges. An attacker who achieves arbitrary code execution inside the
`timestampwriter` pod can call both `login.microsoftonline.com` token endpoints from
the pod's network identity. Both short-lived AWS credentials and the cross-tenant Azure
token can be obtained in a single pod compromise.

This is inherent to the single-pod design (both write paths run in the same process)
and is not unique to the shared UAMI — but the shared UAMI ensures that both clouds
are reachable from the same root token. Separate UAMIs would require separate pods
with separate IB configurations to achieve the same isolation.

The `deploy/networkpolicy.yaml` IMDS egress block mitigates raw UAMI token theft via
the node metadata service, but does not prevent in-process token requests from the
legitimately projected `azure-identity-token`.

### 3. AWS IAM Trust Policy `sub` Pinning

The UAMI Object ID is pinned as the `sub` condition in the AWS IAM trust policy. This
is a strong control: only the exact UAMI OID (a stable directory object in the source
Entra tenant) can satisfy the condition.

However, the UAMI OID **does not rotate**. If an attacker acquires a valid Azure AD JWT
for `api://aws-sts-audience` with `sub: <UAMI_OID>` (which requires either
compromising the UAMI's Entra identity or obtaining a token while the legitimate
exchange is live), they can call `sts:AssumeRoleWithWebIdentity` until the IAM role
trust policy is explicitly modified to revoke the OID.

Compare to Option A (cluster OIDC): compromising one cluster's OIDC issuer does not
compromise other clusters, and the per-cluster OIDC issuer can be rotated. With Option
B + shared UAMI, the attack surface is the **Entra tenant + UAMI** — which is shared
across all clusters. A UAMI compromise is wider in scope than a single-cluster OIDC
compromise.

### 4. Separation of Duty Consideration

If the Azure write path and the AWS write path have different data sensitivity levels
(e.g., Azure writes to production financial data; AWS writes to diagnostic logs), then
sharing a UAMI creates a privilege escalation path: a compromise of the lower-sensitivity
pod context can immediately reach the higher-sensitivity downstream target, because
both token exchanges are accessible from the same `azure-identity-token`.

**Recommendation:** If the two paths operate on data of asymmetric sensitivity, use
separate UAMIs with separate IB configurations and separate ClusterRole/Binding pairs.
The operational overhead of a second UAMI is justified by the reduction in blast radius.

### 5. Monitoring and Alerting

With a shared UAMI, both AWS STS calls and Azure token exchanges appear under the same
identity in Entra sign-in logs. This has two implications:

- **Correlation benefit:** Azure Monitor + AWS CloudTrail events can be joined on the
  UAMI Object ID to build a unified timeline of all write activity across both clouds.
  A suspicious burst of writes in either cloud is visible in a single pane.

- **Alert granularity challenge:** Setting an alert on "any `api://aws-sts-audience`
  token exchange without a corresponding Azure write in the same window" requires
  awareness of both signal streams. Per-cloud alerts need audience-scoped filters to
  avoid false positives from the other path.

**Recommendations:**

```
# Alert: Azure path anomaly
Entra sign-in logs | where ResourceId contains "<ENTRA_APP_CLIENT_ID>"
  and UserPrincipalName == "<UAMI_OBJECT_ID>"

# Alert: AWS path anomaly
Entra sign-in logs | where ResourceId contains "<AWS_AUDIENCE_APP_CLIENT_ID>"
  and UserPrincipalName == "<UAMI_OBJECT_ID>"

# AWS CloudTrail: unexpected STS caller
eventName = "AssumeRoleWithWebIdentity"
  AND requestParameters.roleSessionName = <UAMI_OBJECT_ID>
```

Use the UAMI Object ID as the primary correlation key. Set up **separate** alerts for
Azure-path tokens (audience: cross-tenant Entra app) and AWS-path tokens (audience:
`api://aws-sts-audience`) — do not alert only on aggregate UAMI activity.

### 6. When to Split the UAMI

Operators **should** use separate UAMIs in the following circumstances:

| Trigger | Why a Split Is Warranted |
|---|---|
| Different data classification levels on the two paths | Prevents compromise of lower-sensitivity path from escalating to higher-sensitivity path |
| Independent revocation is a compliance requirement | A shared UAMI cannot be partially revoked; only a full disable/delete affects both paths simultaneously |
| AWS IAM role has broader permissions than `s3:PutObject` on a single prefix | Wider AWS blast radius raises the cost of a shared-UAMI compromise |
| Azure and AWS paths are operated by different teams | Separate on-call ownership and incident response require separate identities for clear responsibility boundaries |
| AWS role grants cross-account access or `s3:GetObject` on sensitive buckets | Read access across accounts is a significantly wider blast radius than write-only on a single prefix |

---

## Decision Table

| Factor | Shared UAMI | Separate UAMIs |
|---|---|---|
| Operational complexity | Lower — one identity to manage | Higher — two identities, two IB configs |
| Blast radius on compromise | Both paths (Azure + AWS) | One path per UAMI |
| Independent revocation | Not possible without downtime to both paths | Yes — each path can be revoked independently |
| AWS identity stability | Single OID across all clusters | Same OID per UAMI (also stable, but per-UAMI) |
| FIC count on Entra app | 1 FIC covers both paths for all clusters | 1 FIC per UAMI (2 total for 2 UAMIs) |
| Audit / correlation | Single identity in both Azure Monitor and CloudTrail | Separate identities per cloud; cross-cloud correlation requires explicit join |
| Access review complexity | Higher — reviewers must understand both IAM systems | Lower — each UAMI reviewed in its own cloud context |
| Recommended when | Same data sensitivity, same team, same on-call ownership | Different sensitivity, different teams, or compliance requires independent revocation |

---

## See Also

- [`docs/azure-setup.md`](azure-setup.md) — Full setup for the cross-tenant Azure path (UAMI, IB configuration, Entra app FIC, RBAC)
- [`docs/aws-setup.md`](aws-setup.md) — Full setup for the AWS path, including Option B (Azure AD as stable OIDC provider for AWS STS)
- [`docs/security.md`](security.md) — Threat model, confused deputy mitigations, and security checklist for the full architecture
