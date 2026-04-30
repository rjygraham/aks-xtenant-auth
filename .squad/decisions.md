# Squad Decisions

## Active Decisions

- **Bishop/Identity Chain** (`decisions/bishop-identity-chain.md`): Multi-tenant Entra app with federated identity credential + target tenant service principal + RBAC for cross-tenant AKS workload identity to Blob Storage. No secrets, no UAMI alone.
- **Dallas/App Structure** (2026-04-29): Go app uses flat two-file structure under `cmd/timestampwriter/`. Single `main.go` with config struct and two helpers. No separate packages—app is too small. If it grows (multiple storage targets, pluggable credential strategies), extract packages then. Uses `azidentity.NewWorkloadIdentityCredential(nil)`, `azblob.Client.UploadStream`, `log/slog` JSON handler, `context.WithCancel` + signal goroutine for lifecycle.
- **Hudson/Deploy Structure** (2026-04-29): Distroless final image (`gcr.io/distroless/static-debian12:nonroot`) for minimal CVE surface. Kubernetes manifests in `deploy/` applied in order (namespace → serviceaccount → configmap → deployment). Workload Identity env vars never set in Deployment—owned by AKS webhook. Placeholders with `# TODO:` comments reference `docs/azure-setup.md`. Container security: `readOnlyRootFilesystem: true`.
- **Ripley/Setup UI Architecture** (2026-04-29): Two-binary architecture (timestampwriter + setup wizard). Single multi-stage Dockerfile for both. Setup wizard uses **simplified admin consent link** (no PKCE, no token exchange, no Graph/ARM API calls). Admin consent redirects to `https://login.microsoftonline.com/common/adminconsent?client_id=…`. State stored in short-lived HttpOnly cookie (10 min). On callback, shows `az role assignment create` CLI command for manual RBAC assignment. No workload identity on setup pod. Deployment: ClusterIP + `kubectl port-forward`. Reference: `docs/setup-ui-architecture.md`.
- **Bishop/Setup Wizard PKCE Flow** (2026-04-29): **SUPERSEDED by Dallas/Simplified Setup Wizard**. Originally proposed delegated PKCE + two-hop OAuth. Preserved as historical reference for OAuth design patterns.
- **Dallas/Simplified Setup Wizard** (2026-04-29): Replaced complex PKCE flow with minimal admin consent link. `cmd/setup/` has 3 routes: `GET /` (landing), `GET /start-consent` (redirects to Microsoft admin consent URL), `GET /callback` (validates state cookie, renders success). No in-memory session map — state is cookie-only (HttpOnly, 10-min MaxAge). RBAC assignment shown as copy-paste CLI command. Rationale: simpler, transparent, admin retains control. No external dependencies (stdlib-only). Reference: `docs/setup-ui-architecture.md`.
- **Hudson/Setup Deployment** (2026-04-29): Setup Deployment uses `default` ServiceAccount (no workload identity). `AZURE_CLIENT_ID` sourced from `setup-config` ConfigMap. Environment: `envFrom: [{configMapRef: {name: setup-config}}]`. No workload identity label needed — setup uses delegated auth (browser flow), not pod identity. Container security: distroless, readOnlyRootFilesystem, non-root. Manifests: `deploy/setup-configmap.yaml`, `deploy/setup-deployment.yaml`, `deploy/setup-service.yaml`.
- **Coordinator/App Registration** (2026-04-29): App registration already exists in source tenant. Setup wizard does NOT create a new registration. It reads `AZURE_CLIENT_ID` from env (ConfigMap) and uses it as-is. Simplifies setup wizard scope to consent + RBAC only.

## Recent Analysis (2026-04-29)

### Bishop: Identity Bindings Architecture Assessment
- **Status:** Analyzed feasibility of Identity Bindings for cross-tenant authentication
- **Finding:** Identity Bindings is UAMI-only and incompatible with multi-tenant Entra app pattern
- **Key Issue:** Identity Bindings proxy routes to UAMI token endpoint; cannot request multi-tenant app tokens
- **Recommendation:** Request FIC limit increase from Azure Support (soft limit, no code changes needed)
- **Details:** See `.squad/decisions/inbox/bishop-identity-bindings-design.md` for full technical analysis

### Ripley: Identity Bindings Migration Scope
- **Status:** Scoped all files affected by hypothetical Identity Bindings migration
- **Finding:** Architecture dependency blocks viable migration path
- **Affected Files:** `cmd/timestampwriter/main.go`, `go.mod`, `deploy/deployment.yaml` (+ new RBAC manifests)
- **Decision Points Identified:** Identity chain redesign needed if migration attempted
- **Details:** See `.squad/decisions/inbox/ripley-identity-bindings-scope.md` for full scope analysis

## 2026-04-29

### Bishop: AKS Identity Bindings Documentation Updates
**Status:** Active | **Owner:** Bishop (Azure Engineer)

Updated `docs/azure-setup.md` to document the AKS Identity Bindings hybrid architecture:
- Added prerequisites: `aks-preview` CLI extension, `IdentityBindingPreview` feature flag, Workload Identity webhook v1.6.0-alpha.1+
- Added Step 2b: register feature flag, create identity binding, capture IB OIDC issuer URL (`https://ib.oic.prod-aks.azure.com/<tenant-id>/<client-id>`)
- Updated Step 3: skip manual UAMI FIC creation; AKS creates `aks-identity-binding` FIC automatically
- Updated Step 4: Entra app FIC now uses IB OIDC issuer URL (eliminates 20-FIC limit by UAMI-scoping)
- Updated Step 8: ServiceAccount annotation uses UAMI client ID (for Identity Binding RBAC); Deployment adds `use-identity-binding` pod annotation; ConfigMap carries `AZURE_CLIENT_ID` override (multi-tenant app client ID)
- Updated Step 9: apply order includes new ClusterRole/ClusterRoleBinding resources
- Added prominent PREVIEW warnings throughout

### Hudson: AKS Identity Bindings Manifest Changes
**Status:** Accepted | **Author:** Hudson (DevOps)

Migrated Kubernetes manifests to support Identity Bindings:
- `deploy/serviceaccount.yaml`: annotation `azure.workload.identity/client-id` uses UAMI client ID (Identity Bindings webhook performs RBAC check with this value)
- `deploy/configmap.yaml`: added `AZURE_CLIENT_ID` key (multi-tenant app client ID)
- `deploy/deployment.yaml`: added `use-identity-binding: "true"` pod annotation; added `AZURE_CLIENT_ID` env override via ConfigMapKeyRef (overrides webhook injection; forces token exchange as multi-tenant app)
- `deploy/clusterrole.yaml` (new): grants `use-managed-identity` verb on `cid.wi.aks.azure.com/<UAMI_CLIENT_ID>`
- `deploy/clusterrolebinding.yaml` (new): binds timestampwriter ServiceAccount to ClusterRole
- Updated apply order: `clusterrole → clusterrolebinding → namespace → serviceaccount → configmap → deployment`

**Rationale:** Identity Bindings provides UAMI-scoped OIDC issuer (stable across clusters). Single FIC per UAMI shared across all clusters eliminates 20-FIC limit scaling constraint.

## Governance

- All meaningful changes require team consensus
- Document architectural decisions here
- Keep history focused on work, decisions focused on direction
