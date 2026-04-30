# Project Context

- **Owner:** Ryan Graham
- **Project:** Containerized Go application that uses AKS workload identity to authenticate to a Microsoft Entra multi-tenant application, then writes timestamps to an Azure Blob Storage account in a different Azure tenant.
- **Stack:** Go, Docker (distroless/alpine), Kubernetes / AKS, Azure SDK for Go (azidentity, azblob), Microsoft Entra ID (multi-tenant app registration), Azure Blob Storage
- **Created:** 2026-04-29

## Learnings

<!-- Append new learnings below. Each entry is something lasting about the project. -->

### 2026-04-29 — SQLite persistent storage for setup wizard

- **PVC over emptyDir for setup wizard DB.** The setup wizard writes `/data/setup.db` via `modernc.org/sqlite`. A PVC (`setup-db`, 100Mi, ReadWriteOnce) is used so the database survives pod restarts. emptyDir would destroy the DB on every restart, breaking in-progress admin flows.
- **100Mi is sufficient for a setup wizard SQLite DB.** The wizard stores at most dozens of rows (consent state, admin records). SQLite at this scale stays well under 1Mi; 100Mi is comfortably safe without over-provisioning.
- **readOnlyRootFilesystem removed due to modernc.org/sqlite temp file writes.** The pure-Go SQLite driver writes temp files to `/tmp` during query execution. Even with `/data` volume-mounted and writable, a read-only root filesystem blocks `/tmp` writes at the kernel level, causing driver failures. Removed the flag; all other hardening controls remain (`runAsNonRoot`, `allowPrivilegeEscalation: false`, `capabilities.drop: ALL`, `seccompProfile: RuntimeDefault`).
- **SETUP_DB_PATH added to both ConfigMap and container env.** ConfigMap is the source of truth; a redundant hardcoded env entry in the Deployment is commented as matching the volumeMount path (`/data/setup.db`) for clarity at apply time.

### 2026-04-29 — Setup wizard Dockerfile and deploy manifests

- **Setup wizard deliberately excluded from workload identity.** The setup pod uses delegated auth (admin's PKCE browser flow), not pod identity. The `azure.workload.identity/use: "true"` label and the annotated ServiceAccount are intentionally absent. `AZURE_CLIENT_ID` is a public OAuth client identifier sourced from a ConfigMap, not injected by the webhook. See `.squad/decisions/inbox/hudson-setup-deployment.md`.
- **Separate Dockerfile for a separate lifecycle.** `Dockerfile.setup` builds `cmd/setup`; the root `Dockerfile` builds `cmd/timestampwriter`. They share the same `go.mod` but are separate images, separate ACR tags, and can be built/versioned independently. The setup image is ephemeral (run-once admin tool); the timestampwriter image is a long-running workload. Coupling them in a single Dockerfile would force a rebuild of both on every change to either.
- **Service is ClusterIP + port-forward only.** The setup wizard's OAuth redirect URI is `http://localhost:8081` (port-forwarded). Exposing it via LoadBalancer would create a publicly reachable OAuth endpoint with no auth guard before the PKCE flow completes — unnecessary attack surface for an ephemeral admin tool.
- **No liveness/readiness probes on the setup pod.** It is a short-lived admin tool, not a production service. Probe infrastructure adds complexity with no operational benefit for a pod that is expected to be deleted after first use.

### 2026-04-29 — Setup wizard simplified deployment

- **Separate `Dockerfile.setup` builds `cmd/setup` independently.** Multi-stage build with distroless final image. Shares `go.mod` with timestampwriter but is versioned and deployed separately. Setup wizard is an ephemeral admin tool; timestampwriter is the long-running workload.
- **Setup manifests follow timestampwriter security patterns:** distroless image, read-only root filesystem, non-root user (UID 65532), no liveness/readiness probes.
- **ConfigMap `setup-config` provides `AZURE_CLIENT_ID` and `SETUP_REDIRECT_BASE_URI`.** These are public values sourced from ConfigMap, not injected by workload identity webhook.
- **Service is ClusterIP only; port-forward for access.** Setup wizard's OAuth redirect is `http://localhost:8081` (port-forwarded). No LoadBalancer needed for ephemeral admin tool.

### 2026-04-29 — AKS Identity Bindings manifest migration

- **Dual-identity split in manifests.** ServiceAccount annotation `azure.workload.identity/client-id` must use the UAMI client ID — the Identity Bindings webhook verifies the SA has `use-managed-identity` RBAC permission for that exact UAMI before injecting proxy env vars. The multi-tenant Entra app client ID goes in the ConfigMap (`AZURE_CLIENT_ID`) and is overridden in the Deployment `env` section as a `configMapKeyRef`, which takes precedence over the webhook's injection.
- **`env` explicit entry beats webhook injection.** The workload identity webhook skips `AZURE_CLIENT_ID` injection if the key already exists in the pod's env. A top-level `env` entry (via `configMapKeyRef`) is evaluated before `envFrom`, so placing `AZURE_CLIENT_ID` in `env` is the correct override mechanism — `envFrom` alone would not win.
- **Identity Binding OIDC issuer is UAMI-scoped, not cluster-scoped.** The IB issuer URL is stable for a given UAMI regardless of cluster. This means one FIC on the multi-tenant Entra app covers all clusters that use the same UAMI — reducing FIC count from N-per-cluster to 1-per-UAMI.
- **Apply order now includes RBAC resources first.** ClusterRole and ClusterRoleBinding must exist before the pod starts; the webhook checks RBAC at admission time. New apply order: `clusterrole → clusterrolebinding → namespace → serviceaccount → configmap → deployment`.
- **`azure.workload.identity/use-identity-binding: "true"` is a pod annotation, not a label.** It goes under `template.metadata.annotations`; the existing `azure.workload.identity/use: "true"` stays as a label under `template.metadata.labels`.



- **Distroless chosen over Alpine for final image.** `gcr.io/distroless/static-debian12:nonroot` removes shell, package manager, and libc entirely, reducing attack surface. The `:nonroot` tag provides UID 65532 out of the box, satisfying `runAsNonRoot: true` in Kubernetes without a custom `RUN adduser` step. This only works because the binary is built with `CGO_ENABLED=0`.
- **Workload Identity webhook contract.** Two things must be in place for the webhook to inject identity: (1) pod label `azure.workload.identity/use: "true"` and (2) the pod's ServiceAccount must be annotated with `azure.workload.identity/client-id`. The identity env vars (`AZURE_CLIENT_ID`, `AZURE_TENANT_ID`, `AZURE_FEDERATED_TOKEN_FILE`) are never set manually in the Deployment — the webhook owns them.
- **Cross-tenant wiring is purely application-level.** From a DevOps perspective, the ServiceAccount annotation `azure.workload.identity/tenant-id` points at the source tenant (where the managed identity lives). The Go app's SDK call to the target tenant's storage account is handled in code; the K8s manifests have no awareness of Tenant B.
- **All placeholders reference `docs/azure-setup.md`.** Three values must be filled before deploying: `MANAGED_IDENTITY_CLIENT_ID`, `SOURCE_TENANT_ID`, and `TARGET_STORAGE_ACCOUNT`. The ACR hostname is also a placeholder. Each is tagged with a `# TODO:` comment pointing to Bishop's azure-setup doc.
- **`readOnlyRootFilesystem: true` is safe here** because the app writes to Azure Blob (remote), not to local disk. The distroless image has no writable directories the app needs.

### 2026-04-29 — Repository history squashed to single commit

- **Git history rewritten using orphan branch technique.** All ~10 commits were squashed into a single initial commit (a40d61e). The orphan branch `initial-state` was created, all files committed, the old `main` branch deleted, and `initial-state` renamed to `main`. This ensures clean project initialization from the curated state.
- **Commit message includes all essential context.** The single commit message documents the project's core purpose: AKS workload identity via Identity Bindings, cross-tenant auth to Azure Blob Storage, and the key innovation (bypassing the 20 FIC limit via UAMI-bound bindings).

## 2026-04-29

### Setup Wizard Database Volume (hudson-2)

Created persistent storage infrastructure and updated deployment manifests for SQLite database.

**Changes:**
- deploy/setup-pvc.yaml: 100Mi PVC (ReadWriteOnce)
- deploy/setup-deployment.yaml: volumeMount /data, SETUP_DB_PATH env, removed readOnlyRootFilesystem
- deploy/setup-configmap.yaml: Added SETUP_DB_PATH config

**Rationale:** 
- PVC survives pod restarts (vs emptyDir)
- readOnlyRootFilesystem removed because modernc.org/sqlite writes temp files to /tmp
- Security context remains strong (non-root, no escalation, capabilities dropped)
