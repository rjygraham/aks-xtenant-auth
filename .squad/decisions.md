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

## Security Audit 2026-04-30

**Status:** Complete + Hardening Fixes Committed  
**Agents:** Bishop (Azure/Identity), Ripley (Code/Architecture), Vasquez (Attack Scenarios)

### Key Decisions

1. **Container-Scoped RBAC** (Bishop + Team consensus): Target Storage RBAC narrowed from account scope to container scope. This limits blast radius of potential cross-tenant compromise (Vasquez Scenario 3a) to the specific container, not the entire storage account. Applied to `docs/azure-setup.md` Step 9 and `cmd/setup/templates/success.html`.

2. **One-Time Write Guard for Setup Wizard** (Vasquez P1 + Team consensus): Setup `POST /configure` now rejects re-runs unless `setup.db` is empty. Prevents silent production config override if setup is re-invoked (Vasquez Scenario 5a). Implemented in `cmd/setup/main.go`.

3. **NetworkPolicy for Identity Isolation** (Ripley + Team consensus): New `deploy/networkpolicy.yaml` enforces:
   - Deny all ingress (setup wizard accessible only via kubectl port-forward).
   - Allow DNS + HTTPS egress (legitimate workload needs).
   - Block `169.254.169.254` IMDS egress (mitigates Vasquez Scenario 6b: UAMI token theft by other pods).

4. **Input Validation on Storage Names** (Ripley + Team consensus): Added regex validation for storage account name and container name before Azure SDK calls in `cmd/timestampwriter/main.go`. Eliminates ambiguity if SQLite data is compromised (Ripley Gap #1–2). Also added in `cmd/setup/main.go` for form POST.

5. **Per-Upload Timeout** (Vasquez Scenario 1c + Team consensus): Wrapped blob write in `context.WithTimeout(15s)` to prevent indefinite stalls. Prevents a hung upload from blocking all subsequent ticks.

6. **FIC Issuer / Audience Correctness Verified** (Bishop audit + Team rejection): Bishop repeated prior error that FIC issuer should be cluster OIDC URL. Team confirmed existing production FIC (`09c0d7e3`) is correct: uses cluster standard OIDC issuer + audience `api://AKSIdentityBinding`. Decision documented in `decisions/bishop-identity-chain.md`.

7. **Documentation Cleanup** (Bishop + Ripley): Removed stale PKCE/Graph/ARM guidance from `docs/azure-setup.md`. Option B section now matches actual simplified admin-consent implementation. Fixed stale comment in `Dockerfile.setup` ("PKCE callbacks" → "admin consent callbacks").

8. **Namespace RBAC Security Warning** (Vasquez Scenario 3a + Team consensus): Documented in `docs/azure-setup.md` that pod creation RBAC in `aks-xtenant-auth` namespace should be restricted to deployment pipeline only. Kubernetes has no pod-level RBAC primitive; namespace discipline is the control. This is a high-priority operational constraint — not a code fix, but process discipline.

### Priority Fixes (Completed)

| Priority | Finding | Status |
|----------|---------|--------|
| P0 | Namespace write → SA impersonation → cross-tenant blob write (Vasquez 3a) | MITIGATED: container-scoped RBAC + namespace warning documented |
| P1 | Setup wizard silent config override (Vasquez 5a) | FIXED: one-time write guard |
| P2 | IMDS accessible from all pods (Vasquez 6b) | FIXED: NetworkPolicy blocks 169.254.169.254 |
| P2 | Storage account underspec'd (Bishop) | FIXED: hardening flags in docs/azure-setup.md |
| P2 | Input validation gaps (Ripley) | FIXED: regex validation added |
| P3 | Upload timeout missing (Vasquez 1c) | FIXED: 15s context.WithTimeout |
| Low | Docs drift (Bishop/Ripley) | FIXED: Option B rewrite, PKCE comment fix |

### Overall Posture
- **Azure/Identity:** Tight (FIC issuer/audience correct; ServiceAccount annotation fixed)
- **Kubernetes-Internal:** Medium → Low (NetworkPolicy + namespace discipline)
- **Data Validation:** Low → Mitigated (regex validation + one-time write guard)
- **Risk Classification:** LOW (after fixes)

## Governance

- All meaningful changes require team consensus
- Document architectural decisions here
- Keep history focused on work, decisions focused on direction
