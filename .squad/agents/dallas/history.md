# Project Context

- **Owner:** Ryan Graham
- **Project:** Containerized Go application that uses AKS workload identity to authenticate to a Microsoft Entra multi-tenant application, then writes timestamps to an Azure Blob Storage account in a different Azure tenant.
- **Stack:** Go, Docker (distroless/alpine), Kubernetes / AKS, Azure SDK for Go (azidentity, azblob), Microsoft Entra ID (multi-tenant app registration), Azure Blob Storage
- **Created:** 2026-04-29

## Learnings

<!-- Append new learnings below. Each entry is something lasting about the project. -->

### 2026-04-29 — SQLite persistence added to setup wizard

- Added `modernc.org/sqlite` (pure Go, no CGO) for SQLite — required for distroless containers where CGO-based drivers fail at runtime.
- DB file path read from `SETUP_DB_PATH` env (default `/data/setup.db`). This aligns with Hudson's emptyDir/PVC mount pattern.
- `initDB` opens the connection and creates the `consents` table with `CREATE TABLE IF NOT EXISTS` — safe to call on every startup.
- `handleCallback` now renders `configure.html` (a form) instead of `success.html` directly. `success.html` is now the post-save confirmation page.
- `handleConfigure` (`POST /configure`) inserts the row, then renders `success.html` with fully-substituted CLI command and ConfigMap values — no more placeholder strings for the admin to fill in.
- `savedData` struct holds all fields for the confirmation template; `configureData` is the minimal struct for the form page (just TenantID pre-filled as hidden input).
- DB errors on insert → log + render `error.html` gracefully rather than crashing the server.
- `run()` now takes `*sql.DB` as a parameter; `main()` calls `initDB`, defers `db.Close()`, and passes db to `run()`.

### 2026-04-29 — Simplified setup wizard (admin consent link)

- Previous PKCE + two-hop OAuth approach replaced with admin consent link: `https://login.microsoftonline.com/common/adminconsent?client_id=…&redirect_uri=…&state=…`
- No token exchange, no Graph API calls, no ARM calls, no in-memory session map.
- State CSRF protection via a short-lived HttpOnly cookie (`setup_state`, `MaxAge: 600`) — no session map needed.
- Callback receives `admin_consent=True` + `tenant={targetTenantID}` on success; `error` + `error_description` on failure.
- RBAC assignment is shown as a copy-paste `az role assignment create` command — admin runs it manually with their own credentials. Placeholders `{SUBSCRIPTION_ID}`, `{RESOURCE_GROUP}`, `{STORAGE_ACCOUNT_NAME}` are literal text for the user to fill in.
- ConfigMap and ServiceAccount annotation values displayed on success page; `azure.workload.identity/tenant-id` note clarifies it's the SOURCE tenant (app registration), not the target tenant returned in the callback.
- `configure.html` deleted — no form needed.
- `main.go` reduced from 645 lines to ~160 lines. stdlib only; no new imports beyond `encoding/hex`.

- `cmd/setup/main.go` is purely stdlib — no new go.mod entries needed. All OAuth and REST calls use `net/http` directly.
- PKCE implementation: `generateCodeVerifier` produces 32 random bytes base64url-encoded (no padding). `generateCodeChallenge` applies `sha256.Sum256` then base64url-encodes with no padding. Both are stdlib-only.
- Go 1.22 `http.ServeMux` enhanced routing (`GET /path`, `POST /path`, `GET /{$}` for exact root) keeps handler registration concise and type-safe — no external router needed.
- JWT `tid` claim extraction without signature verification: split on `.`, base64url-decode the middle segment (handle padding manually), unmarshal JSON claims. This is safe here because the token was just retrieved directly from the Microsoft token endpoint over TLS.
- In-memory sessions: global `sync.Mutex`-protected map. Lock only for map reads/writes; lock-free reads on session struct fields inside a single request goroutine are safe because the OAuth flow is linear (one active request per session at a time). Session cleanup runs every 15 minutes via a ticker goroutine.
- ARM RBAC assignment: HTTP 409 Conflict is idempotent — treat as success. The role assignment PUT is scoped to the storage account resource path, not the container, which is the correct granularity for `ba92f5b4-2d11-453d-a403-e96b0029c9fe` (Storage Blob Data Contributor).
- `html/template.ParseFS` + `ExecuteTemplate` with the `{{define}}`/`{{template}}` pattern: layout.html defines `"header"` and `"footer"` named templates; each page file defines its own named template (e.g. `"index.html"`) and calls `{{template "header" .}}` / `{{template "footer" .}}`. All files parsed together into one template set.
- CSRF protection via `state` parameter stored in session and validated on callback — same pattern for both Step 1 and Step 2.
- HTTP server timeouts (`ReadTimeout`, `WriteTimeout`, `IdleTimeout`) set explicitly — important even for internal tooling.
- `go.sum` is pre-existing missing for Azure SDK packages (timestampwriter) — this is expected and requires `go mod tidy` at build time. `cmd/setup` is unaffected (stdlib only).

### 2026-04-29 — Initial app scaffolding

- App lives at `cmd/timestampwriter/main.go`. Module is `github.com/rjygraham/aks-xtenant-auth`.
- `azidentity.NewWorkloadIdentityCredential(nil)` requires no options — the AKS webhook injects `AZURE_CLIENT_ID`, `AZURE_TENANT_ID`, and `AZURE_FEDERATED_TOKEN_FILE` automatically.
- Cross-tenant blob writes work without any special SDK options. The OIDC token exchange is transparent — pass the `azblob.Client` the workload identity credential and target the storage account URL in the destination tenant.
- Blob naming convention: `timestamp-<RFC3339>.txt`. Colons in RFC3339 strings are valid in Azure Blob Storage blob names.
- `azblob.Client.UploadStream` is the right call for small in-memory payloads (uses `strings.NewReader`). No need for block blob staging at this size.
- Graceful shutdown: signal handler calls `context.CancelFunc`; the ticker `select` drains on `ctx.Done()`. Any in-flight `UploadStream` call will respect the cancelled context and return promptly.
- Logging uses `log/slog` (stdlib, Go 1.21+) with JSON output — no third-party logger needed.
- `go.sum` is intentionally excluded from the initial commit; `go mod tidy` with network access will generate it at build time.

### 2026-04-29 — Setup wizard simplified to admin consent link

- **Admin consent link approach (no PKCE):** `https://login.microsoftonline.com/common/adminconsent?client_id=…&redirect_uri=…&state=…` is simpler, requires no token exchange, no OAuth2 library. State CSRF protection via HttpOnly cookie only.
- **Callback validation:** Microsoft sends `admin_consent=True&tenant={targetTenantID}&state=…` on success. Validate state cookie and render success page with CLI command + ConfigMap snippets.
- **RBAC is manual (`az role assignment create`):** Displayed as pre-filled CLI command. Admin retains control; simpler than automating ARM API calls. Requires admin to have Owner or User Access Admin on storage account.
- **No session map needed.** Previous in-memory `sync.Mutex`-protected map replaced by HttpOnly cookie (`setup_state`, 10-min MaxAge). Reduces code complexity and eliminates pod-restart session loss risk.
- **3 routes:** `GET /` (landing), `GET /start-consent` (redirect to Microsoft), `GET /callback` (validate & render success/error). No POST handlers needed.
- **Template structure:** layout.html defines reusable header/footer templates (`{{define "header"}}` / `{{define "footer"}}`). Each page file defines its own named template and calls `{{template "header" .}}` / `{{template "footer" .}}`. All templates parsed together via `html/template.ParseFS`.
- **Idempotent RBAC:** ARM returns 409 Conflict if role assignment already exists (same SP on same account). Treat 409 as success — re-running setup is safe.

## 2026-04-29

### SQLite Persistence Integration (dallas-3)

Implemented SQLite persistence for the setup wizard using modernc.org/sqlite (pure Go driver, distroless-compatible).

**Changes:**
- cmd/setup/main.go: Added initDB, POST /configure handler, configureData/savedData structs, SETUP_DB_PATH config
- cmd/setup/templates/configure.html: Storage account form after OAuth consent
- cmd/setup/templates/success.html: Post-save confirmation with substituted commands
- go.mod/go.sum: Added modernc.org/sqlite v1.34.5

**Decision:** Use modernc.org/sqlite (pure Go, no CGO, distroless-compatible)
**Volume:** Persistent storage via PVC (handled by Hudson)
