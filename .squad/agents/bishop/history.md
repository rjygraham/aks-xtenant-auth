# Project Context

- **Owner:** Ryan Graham
- **Project:** Containerized Go application that uses AKS workload identity to authenticate to a Microsoft Entra multi-tenant application, then writes timestamps to an Azure Blob Storage account in a different Azure tenant.
- **Stack:** Go, Docker (distroless/alpine), Kubernetes / AKS, Azure SDK for Go (azidentity, azblob), Microsoft Entra ID (multi-tenant app registration), Azure Blob Storage
- **Created:** 2026-04-29

## Learnings Summary (Summarized 2026-04-30)

See full detailed learnings in `decisions.md` for all entries. Key milestones preserved below.

**Cross-Tenant Architecture:**
- AZURE_TENANT_ID override required to scope token requests to target tenant (not source tenant).
- Target tenant SP and RBAC pre-existed; only manifest changes needed (no code changes).
- Multi-tenant Entra app is mandatory (UAMIs cannot have SPs in target tenants).

**Identity Bindings (IB):**
- IB is UAMI-only; cannot support cross-tenant scenarios without dual IB instances.
- IB OIDC issuer is UAMI-scoped, constant across clusters for same UAMI.
- IB tokens use `api://AKSIdentityBinding` audience; standard WI uses `api://AzureADTokenExchange`.

**AWS Option B (2026-04-30):**
- Azure AD is stable cross-cluster OIDC provider for AWS IAM (zero per-cluster AWS changes).
- UAMI Object ID is stable `sub` across all clusters; single AWS registration covers all.
- Application acquires Azure AD JWT for dedicated Entra app, calls AWS STS directly (no projected volume).

## 2026-04-30 — AWS Option B: Azure AD as Stable OIDC Provider for AWS

See `decisions.md` 2026-04-30 entries for full technical analysis. Key implementation decisions:

**Documentation Clarity Work:**
- Updated `docs/aws-setup.md` intro to state Option B's key benefit upfront: "One Entra app registration in your source tenant is all that's needed for every cluster, forever."
- Added "Single Entra App = Stable AWS Identity" subsection explaining stability properties (stable issuer, stable `sub`, zero AWS changes per cluster, single identity correlation).
- Added one-line Option B opening summary: solution framing vs. problem statement.

**Technical Decisions Captured:**
- UAMI access token issuer is stable Microsoft endpoint (`https://login.microsoftonline.com/<tenant-id>/v2.0`).
- UAMI Object ID (not client ID) is the correct AWS trust policy `sub` value.
- Dedicated Entra app registration (`api://aws-sts-audience`) for audience isolation.
- No `aws-identity-token` projected volume (Go app calls STS directly with Azure AD JWT).
- Token refresh is app responsibility; STS credentials re-acquired on every tick.

### 2026-04-30 — AWS Option B orchestration & documentation complete

- Session orchestration via Scribe: merged 3 decision inbox files (Dallas Go implementation, Hudson deployment env vars, Bishop docs clarity) into primary decisions.md.
- Deleted decision inbox files after merge.
- Orchestration logs created per agent; session log written.
- Coordinated across teams for full AWS Option B lifecycle (code → deployment → docs).

## Work Summary (Condensed)

**Recent Focus:**
- 2026-04-30: AWS Option B stable identity implementation (Azure AD as cross-cluster OIDC provider for AWS).
- 2026-04-30: Merged inbox decision files into primary decisions.md; created orchestration/session logs.
- 2026-04-30: Updated `docs/aws-setup.md` with clarity on single Entra app scaling benefit.

**Key Architectural Properties (Preserved for Reference):**
- Cross-tenant auth requires AZURE_TENANT_ID override to scope tokens correctly.
- Identity Bindings is UAMI-only with cluster-independent stable OIDC issuer.
- AWS Option B uses Azure AD (not per-cluster OIDC) for stable AWS identity across all clusters.
- See `decisions.md` for detailed analysis of confused deputy, dual-cloud feasibility, and shared UAMI trade-offs.



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

### 2026-04-30 — Documentation Clarity: Single Entra App = Stable AWS Identity

**Work completed:** Enhanced `docs/aws-setup.md` to make the stable-identity story explicit and prominent at three levels: intro, approach table callout, and Option B opening.

**Key clarifications added:**

1. **Intro paragraph:** Now explicitly names both approaches. Option B's key benefit stated upfront: "One Entra app registration in your source tenant is all that's needed for every cluster, forever. New clusters inherit AWS access automatically with zero AWS changes."

2. **"Choosing an Approach" section:** Added new subsection **"The Single Entra App = Stable AWS Identity"** immediately after the comparison table. Explains four concrete stability properties:
   - **Stable OIDC issuer:** `https://login.microsoftonline.com/<source-tenant-id>/v2.0` is Microsoft-owned, never changes, covers all clusters forever on one registration.
   - **Stable `sub`:** UAMI Object ID is identical across all clusters; AWS trust policies pin this stable value.
   - **Zero AWS changes per cluster:** Scaling to 10, 20, or 100 clusters requires zero AWS changes — only new Kubernetes Identity Binding resources.
   - **Single identity correlation:** UAMI Object ID correlates across Azure Monitor and CloudTrail, enabling coherent audit.
   - **Contrast with Option A:** Each cluster needs its own IAM IdP registration; scaling is O(N) work in AWS.

3. **Option B opening:** Added one-line summary before "The N-IdP Scaling Problem": _"Register one Entra app in the source tenant. That's the only Azure resource needed to give every AKS cluster using your UAMI stable, automatic AWS access."_ Shifts focus from problem statement to solution.

**Rationale:** Documentation previously described *how* to set up Option B but buried the *why* (stable identity) in body text. Ryan flagged that operators needed explicit clarity on WHERE/WHAT/WHY/HOW. The doc now answers all four before Step B1, making the architectural property discoverable at a glance.

### 2026-04-30 — AWS Option B orchestration & documentation complete

- Session orchestration via Scribe: merged 3 decision inbox files (Dallas, Hudson, Bishop implementation notes) into primary decisions.md as detailed decision entries.
- Deleted decision inbox files after merge; decisions.md now documented with implementation-level details from all three agents.
- Orchestration logs created; session log written documenting coordinated AWS Option B full lifecycle (code → deployment → docs).

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

### 2026-04-30 — Documentation: "Single Entra App = Stable AWS Identity for All Clusters"

**Work completed:** Enhanced `docs/aws-setup.md` to make the stable-identity story explicit and prominent at three levels: intro, approach table callout, and Option B opening.

**Key clarifications added:**

1. **Intro paragraph:** Now explicitly names both approaches. Option B's key benefit stated upfront: "One Entra app registration in your source tenant is all that's needed for every cluster, forever. New clusters inherit AWS access automatically with zero AWS changes."

2. **"Choosing an Approach" section:** Added new subsection **"The Single Entra App = Stable AWS Identity"** immediately after the comparison table. Explains four concrete stability properties:
   - **Stable OIDC issuer:** `https://login.microsoftonline.com/<source-tenant-id>/v2.0` is Microsoft-owned, never changes, covers all clusters forever on one registration.
   - **Stable `sub`:** UAMI Object ID is identical across all clusters; AWS trust policies pin this stable value.
   - **Zero AWS changes per cluster:** Scaling to 10, 20, or 100 clusters requires zero AWS changes — only new Kubernetes Identity Binding resources.
   - **Single identity correlation:** UAMI Object ID correlates across Azure Monitor and CloudTrail, enabling coherent audit.
   - **Contrast with Option A:** Each cluster needs its own IAM IdP registration; scaling is O(N) work in AWS.

3. **Option B opening:** Added one-line summary before "The N-IdP Scaling Problem": _"Register one Entra app in the source tenant. That's the only Azure resource needed to give every AKS cluster using your UAMI stable, automatic AWS access."_ Shifts focus from problem statement to solution.

**Rationale:** Documentation previously described *how* to set up Option B (Step B1–B5) but buried the *why* (stable identity) in body text. Ryan flagged that operators needed explicit clarity on:
- WHERE the single Entra app enables stability (Entra source tenant, not cluster)
- WHAT makes the identity stable across clusters (Microsoft-owned OIDC endpoint + UAMI Object ID)
- WHY scaling is frictionless (zero AWS/Azure changes per cluster, only Kubernetes)
- HOW it differs from per-cluster OIDC (N registrations vs. 1 forever)

The doc now answers all four before Step B1, making the architectural property discoverable at a glance.

**Accuracy verified:** Matches production architecture and team decisions in `decisions.md` (Option B analysis, 2026-04-30).

