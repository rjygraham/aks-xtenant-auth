# Setup UI Architecture

## Overview

The **setup UI** is a one-time wizard web application that external tenant admins use to:
1. Grant admin consent for the multi-tenant Entra app in their tenant
2. Enter their storage account details
3. Have the app programmatically assign the necessary RBAC role

This document specifies the complete architecture, security model, and deployment strategy for the setup application.

---

## Two-Binary Architecture

### Rationale

The project splits into two independently deployable binaries:

| Binary | Purpose | Lifecycle | Deployment |
|--------|---------|-----------|-----------|
| `cmd/timestampwriter` | Production workload: writes timestamps to cross-tenant Blob Storage every 30s | Long-running service | Continuous, auto-scaling |
| `cmd/setup` | Setup wizard: one-time admin consent + role assignment | One-time operation | Temporary, accessed via port-forward or LoadBalancer |

**Why separate?**
- **Separation of concerns**: Setup is an admin UI; timestampwriter is headless business logic.
- **Different lifecycle**: timestampwriter runs indefinitely; setup is deployed on-demand, torn down after configuration.
- **Security posture**: timestampwriter uses workload identity (pod token); setup delegates to admin (OAuth2 user token).
- **Scalability**: timestampwriter may auto-scale; setup remains a single instance (stateful session state).
- **Blast radius**: timestampwriter failure doesn't expose setup; setup compromise doesn't affect running workload.

### Container Strategy: Recommendation

**Use a single multi-stage Docker image with both binaries.**

```dockerfile
# Build stage
FROM golang:1.23-alpine AS builder
WORKDIR /app
COPY . .
RUN go build -o timestampwriter ./cmd/timestampwriter
RUN go build -o setup ./cmd/setup

# Runtime stage
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /app/timestampwriter /timestampwriter
COPY --from=builder /app/setup /setup
ENTRYPOINT ["/timestampwriter"]  # Default
```

**Why one image?**
- Reduces build complexity and artifact surface.
- Both binaries share the same Go runtime and dependencies.
- Override `ENTRYPOINT` or use a shell wrapper for setup deployment.
- Cost: negligible binary bloat (~10–20 MB combined).

**Alternative (if binary hygiene is critical):** Separate images. Complexity trade-off: two Dockerfiles, two ECR repos, two image pulls.
**Recommendation:** Single image for this project scale.

---

## Setup UI Flow: Complete User Journey

### Step 1: Landing Page (`GET /`)

**What the user sees:**
- App name ("AKS Cross-Tenant Blob Setup")
- What the wizard does (1–2 sentence summary: admin consent, storage details, auto RBAC)
- "Start Setup" button

**Backend:** Static HTML, served by the setup server. No dependencies.

---

### Step 2: Admin Consent + Graph Auth (`GET /auth`)

**Flow:**
User clicks "Start Setup" → redirected to Microsoft OAuth2 to grant admin consent and retrieve Graph token.

**URL:**
```
https://login.microsoftonline.com/common/oauth2/v2.0/authorize?
  client_id={AZURE_CLIENT_ID}
  &redirect_uri={SETUP_REDIRECT_BASE_URI}/callback
  &scope=https://graph.microsoft.com/Application.Read.All openid profile
  &response_type=code
  &prompt=consent
  &code_challenge={PKCE_CHALLENGE}
  &code_challenge_method=S256
```

**Why this URL?**
- `/common` endpoint allows multi-tenant auth (any Entra tenant).
- `scope=Application.Read.All` allows querying service principals in the target tenant.
- `prompt=consent` forces the admin consent screen (non-admin consent flows will fail).
- PKCE (`code_challenge`, `code_challenge_method=S256`) eliminates the need for a client secret—aligns with the project's no-secrets philosophy.

**Backend logic:**
```go
// Generate PKCE code_challenge and code_verifier
codeVerifier := generateRandomString(128)
codeChallenge := base64URLEncode(sha256(codeVerifier))

// Store in session (keyed by a secure UUID cookie)
sessions[sessionID] = &SessionState{
  CodeVerifier: codeVerifier,
  Timestamp:    time.Now(),
}

// Redirect to /auth with code_challenge
```

---

### Step 3: Graph Callback (`GET /callback?code=...&session_state=...`)

**What happens:**
User completes OAuth2 flow → Microsoft redirects back with `code`. Exchange code for Graph access token.

**Backend logic:**
```
1. Retrieve PKCE code_verifier from session (via UUID cookie).
2. POST to https://login.microsoftonline.com/common/oauth2/v2.0/token:
   - grant_type=authorization_code
   - code={auth_code}
   - code_verifier={PKCE_verifier}
   - redirect_uri={SETUP_REDIRECT_BASE_URI}/callback
   - client_id={AZURE_CLIENT_ID}
   (NO client_secret)

3. Receive access_token (Graph token) and id_token.
   - Extract `tid` (tenant ID) from id_token.
   - Store in session:
     sessions[sessionID].GraphToken = graphToken
     sessions[sessionID].TargetTenantID = tid

4. Call Microsoft Graph to find the service principal in the target tenant:
   GET https://graph.microsoft.com/v1.0/servicePrincipals?$filter=appId eq '{AZURE_CLIENT_ID}'
   Authorization: Bearer {graphToken}

5. Extract service principal object ID from response:
   sessions[sessionID].ServicePrincipalObjectID = objectID

6. Redirect to /mgmt-auth (management auth for RBAC assignment).
```

**Key validation:**
- The `tid` claim in `id_token` confirms the admin is authenticated in the target tenant (cross-tenant safety).
- If Graph query returns zero results, the app must be registered in this tenant before role assignment can work.

---

### Step 4: Management Auth Redirect (`GET /mgmt-auth`)

**Flow:**
Redirect to Microsoft OAuth2 for management plane access token.

**URL:**
```
https://login.microsoftonline.com/{target_tenant_id}/oauth2/v2.0/authorize?
  client_id={AZURE_CLIENT_ID}
  &redirect_uri={SETUP_REDIRECT_BASE_URI}/mgmt-callback
  &scope=https://management.azure.com/user_impersonation
  &response_type=code
  &code_challenge={PKCE_CHALLENGE_2}
  &code_challenge_method=S256
```

**Why separate from Graph auth?**
- Different resource (`management.azure.com` vs. `graph.microsoft.com`).
- Different scope requirement (`user_impersonation` for ARM).
- Allows the admin to revoke setup permissions independently from Graph permissions.

**Backend logic:**
```go
// Generate a new PKCE challenge for management token
codeVerifier2 := generateRandomString(128)
codeChallenge2 := base64URLEncode(sha256(codeVerifier2))

sessions[sessionID].ManagementCodeVerifier = codeVerifier2

// Redirect to the URL above
```

---

### Step 5: Management Callback (`GET /mgmt-callback?code=...`)

**What happens:**
Exchange code for management (ARM) access token.

**Backend logic:**
```
1. Retrieve PKCE code_verifier from session.
2. POST to https://login.microsoftonline.com/{target_tenant_id}/oauth2/v2.0/token:
   - grant_type=authorization_code
   - code={auth_code}
   - code_verifier={PKCE_verifier_2}
   - redirect_uri={SETUP_REDIRECT_BASE_URI}/mgmt-callback
   - client_id={AZURE_CLIENT_ID}
   (NO client_secret)

3. Receive access_token (management token).
   - Extract `tid` claim — MUST match sessions[sessionID].TargetTenantID.
   - Store in session:
     sessions[sessionID].ManagementToken = mgmtToken

4. Redirect to /configure (configuration form).
```

**Token validation:**
- Verify `tid` matches the target tenant ID from Graph flow. If mismatch, abort — admin's browser was tampered with or reused across tenants.

---

### Step 6: Configuration Form (`GET /configure`)

**What the user sees:**
A form with fields:
- **Subscription ID** (input field, required)
- **Resource Group** (input field, required)
- **Storage Account Name** (input field, required)
- **Container Name** (input field, required)
- **Pre-filled info (display only):**
  - Target Tenant ID
  - Service Principal Object ID

**Backend logic:**
- Verify session is still valid and contains GraphToken + ManagementToken.
- If either token is missing, redirect to landing page (session expired).
- Render form with pre-filled tenant/SP info.

---

### Step 7: Role Assignment (`POST /setup`)

**Form submission:**
User submits configuration → App assigns Storage Blob Data Contributor role.

**Backend logic:**
```
1. Validate form inputs (not empty, valid GUIDs where required).

2. Call Azure Resource Manager to assign the role:
   PUT https://management.azure.com/subscriptions/{sub}/resourceGroups/{rg}/providers/Microsoft.Storage/storageAccounts/{name}/providers/Microsoft.Authorization/roleAssignments/{uuid}?api-version=2022-04-01
   
   Headers:
   - Authorization: Bearer {ManagementToken}
   - Content-Type: application/json
   
   Body:
   {
     "properties": {
       "roleDefinitionId": "/subscriptions/{sub}/providers/Microsoft.Authorization/roleDefinitions/ba92f5b4-2d11-453d-a403-e96b0029c9fe",  // Storage Blob Data Contributor
       "principalId": "{service_principal_object_id}"
     }
   }

3. If success (201 Created or 204 No Content):
   - Redirect to /success
   
4. If failure:
   - Log error details (do NOT expose token or tenant ID to user).
   - Render error page with troubleshooting advice (see Risks section).
   - Do NOT retry automatically (requires manual intervention).
```

**UUID generation:**
The `{uuid}` in the role assignment URL should be a stable, deterministic UUID (e.g., hash of subscription + RG + storage account + service principal ID). This ensures idempotency: re-running the same role assignment is a no-op (204 No Content).

---

### Step 8: Success Page (`GET /success`)

**What the user sees:**
- Confirmation message ("Role assigned successfully").
- **ConfigMap values to copy:**
  ```
  AZURE_TARGET_SUBSCRIPTION_ID={sub}
  AZURE_TARGET_RESOURCE_GROUP={rg}
  AZURE_TARGET_STORAGE_ACCOUNT={storage_account_name}
  AZURE_TARGET_CONTAINER={container_name}
  ```
- Link to `docs/azure-setup.md` for remaining manual steps (if any).
- "Close Setup" button (or message: "You can now close this window").

**Backend logic:**
- Display session state. Do NOT clear session immediately (user may need to screenshot values).
- Session expires after 10 minutes of inactivity.

---

## Security Considerations

### 1. PKCE (Proof Key for Code Exchange)

**What:** Two-step OAuth2 code exchange without a client secret.

**How:**
- Browser → Setup app generates `code_verifier` (random 128-character string).
- Setup app → Browser redirects to OAuth2 with `code_challenge = SHA256(code_verifier)` (base64url-encoded).
- Browser → User grants consent; Microsoft redirects back with `code`.
- Setup app → Exchanges `code` for token, providing `code_verifier` (proves app owns the challenge).
- Microsoft validates: SHA256(code_verifier) == code_challenge. Only token if match.

**Why safe without client secret:**
- Client secret is not needed; the `code_verifier` proves the setup app (and only that instance) can exchange the code.
- Mitigates authorization code interception attacks.

**Aligns with project philosophy:** No secrets, no key vaults, no rotations.

---

### 2. Session State Storage

**Location:** Server-side, in-memory map.

```go
type SessionState struct {
  CodeVerifier                string
  ManagementCodeVerifier      string
  GraphToken                  string
  ManagementToken             string
  TargetTenantID              string
  ServicePrincipalObjectID    string
  CreatedAt                   time.Time
  LastAccessedAt              time.Time
}

var sessions = make(map[string]*SessionState)  // UUID cookie -> state
```

**UUID Cookie:**
- Cryptographically random UUID (e.g., `uuid.New().String()`).
- Set as secure, HttpOnly, SameSite=Strict cookie.
- Short TTL: 30 minutes.

**Why acceptable for a setup wizard:**
- Setup is a one-time operation (not user-session data).
- In-memory state is lost on pod restart, but **this is expected**: admin must restart the setup flow. Pod crashes are rare during setup.
- Tokens are short-lived (~1 hour); the form must be completed within that window.
- Only the admin's browser (via cookie) can access the session.

**Risk:** See Risks section ("In-memory session state is lost on pod restart").

---

### 3. Token Validation

**Every token received must be validated:**

- **Graph token (`id_token`):**
  - Verify `tid` claim (tenant ID) matches the admin's home tenant.
  - Verify signature (download keys from Microsoft's JWKS endpoint).
  - Verify `exp` (expiration) is in the future.

- **Management token:**
  - Extract and verify `tid` claim matches the Graph `tid`.
  - Verify signature and `exp`.
  - **Safety check:** If `tid` doesn't match, the browser was redirected to a different tenant (possible attack or configuration error). Abort.

**Library recommendation:** Use Go's `github.com/golang-jwt/jwt/v5` + `github.com/mitchellh/mapstructure` to parse and validate.

---

### 4. Delegated Authority

**Key insight:** The setup app does NOT use the AKS pod's workload identity.

Instead:
- The **admin's Graph + Management tokens** are used to assign the role.
- This means the admin must have sufficient permissions (Contributor or Owner on the storage account).
- The setup app **cannot verify this in advance** — role assignment will fail if the admin lacks permissions.

**Why this design:**
- Workload identity tokens cannot be used for admin consent (they represent the pod, not the user).
- The admin must actively approve—delegated authority ensures accountability and reduces blast radius.

---

### 5. Cross-Tenant Safety

**Scenario:** Malicious actor redirects browser to a different tenant mid-setup.

**Mitigation:**
1. Extract `tid` from Graph `id_token` after first OAuth2 flow.
2. Store `TargetTenantID` in server-side session.
3. After Management OAuth2 flow, extract `tid` from management token.
4. Verify: `tid_from_mgmt_token == TargetTenantID`. If not, abort and log.

**Result:** Even if the admin's browser is redirected to a different tenant, the setup app detects the mismatch and refuses to assign roles.

---

### 6. No Client Secret Transmission

**How to stay secret-free:**
- `AZURE_CLIENT_ID` is injected via ConfigMap (non-secret).
- PKCE code_verifier is generated server-side and never leaves the setup pod.
- No client secret needed for OAuth2 token exchange (PKCE eliminates it).

**Container image:**
- Distroless base (no shell, minimal attack surface).
- Read-only root filesystem.
- Non-root user.

---

## Environment Variables (cmd/setup)

The setup app reads:

| Variable | Purpose | Example | Required |
|----------|---------|---------|----------|
| `AZURE_CLIENT_ID` | Multi-tenant Entra app ID | `12345678-1234-...` | Yes |
| `SETUP_REDIRECT_BASE_URI` | Base URL for OAuth2 callbacks | `https://setup.example.com` or `http://localhost:8080` | Yes |
| `SETUP_PORT` | Listening port | `:8081` | No (default `:8081`) |
| `SETUP_LOG_LEVEL` | Logging level (info, debug, error) | `info` | No (default `info`) |

**Example deployment:**
```bash
export AZURE_CLIENT_ID="12345678-1234-1234-1234-123456789abc"
export SETUP_REDIRECT_BASE_URI="http://localhost:8080"  # Local port-forward
export SETUP_PORT=":8081"
./setup
```

---

## Kubernetes Deployment Notes

### Pod Identity: NOT Used

**Key difference from timestampwriter:**
- `timestampwriter` uses `azure.workload.identity/use: "true"` label (workload identity webhook injects AZURE_* env vars).
- `setup` does **NOT** use workload identity—it delegates to the admin's tokens (OAuth2 user auth).

**Therefore:**
- `setup` pod does NOT receive workload identity env vars from the webhook.
- `AZURE_CLIENT_ID` is injected manually via ConfigMap (non-secret).
- `SETUP_REDIRECT_BASE_URI` is set via ConfigMap or Secret (depending on environment).

### Deployment Strategy

**Recommended: ClusterIP + kubectl port-forward**

```yaml
# deploy/setup-deployment.yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: setup
  namespace: timestampwriter
spec:
  replicas: 1  # Single instance (stateful session)
  selector:
    matchLabels:
      app: setup
  template:
    metadata:
      labels:
        app: setup
      spec:
        serviceAccountName: setup  # Non-workload-identity SA
        containers:
        - name: setup
          image: gcr.io/my-project/timestampwriter:latest  # Single multi-stage image
          command: ["/setup"]  # Override entrypoint
          env:
          - name: AZURE_CLIENT_ID
            valueFrom:
              configMapKeyRef:
                name: setup-config
                key: azure_client_id
          - name: SETUP_REDIRECT_BASE_URI
            valueFrom:
              configMapKeyRef:
                name: setup-config
                key: setup_redirect_base_uri
          - name: SETUP_PORT
            value: ":8081"
          ports:
          - containerPort: 8081
          securityContext:
            readOnlyRootFilesystem: true
            allowPrivilegeEscalation: false
            runAsNonRoot: true
            runAsUser: 65532  # nobody
            capabilities:
              drop:
              - ALL
          resources:
            requests:
              memory: "64Mi"
              cpu: "100m"
            limits:
              memory: "128Mi"
              cpu: "200m"
---
apiVersion: v1
kind: Service
metadata:
  name: setup-svc
  namespace: timestampwriter
spec:
  type: ClusterIP
  selector:
    app: setup
  ports:
  - port: 8081
    targetPort: 8081

---
apiVersion: v1
kind: ConfigMap
metadata:
  name: setup-config
  namespace: timestampwriter
data:
  azure_client_id: "<YOUR_AZURE_CLIENT_ID>"
  setup_redirect_base_uri: "http://localhost:8080"  # Updated by admin for port-forward
```

**Access during setup:**
```bash
kubectl port-forward -n timestampwriter svc/setup-svc 8080:8081
# Open http://localhost:8080 in browser
```

**Alternative: LoadBalancer (if public setup endpoint needed)**

```yaml
apiVersion: v1
kind: Service
metadata:
  name: setup-svc
  namespace: timestampwriter
spec:
  type: LoadBalancer
  selector:
    app: setup
  ports:
  - port: 443
    targetPort: 8081
```

Then update `SETUP_REDIRECT_BASE_URI` to the LoadBalancer's public DNS.

**Deployment notes:**
- Setup pod is temporary—delete Deployment after setup is complete.
- ClusterIP + port-forward is simpler and doesn't expose setup to the public internet.
- If your environment requires a public endpoint, use LoadBalancer and ensure firewall rules restrict access (IP whitelist, etc.).

---

### Service Account

**Why a separate ServiceAccount?**

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: setup
  namespace: timestampwriter
```

- Distinct from `timestampwriter` ServiceAccount.
- Never labeled with `azure.workload.identity/use: "true"` (workload identity not used).
- RBAC can restrict setup SA to only read ConfigMaps (deny write access to secrets, etc.).

---

## Multi-Tenant App Registration Prerequisites

The Entra app registration must be configured to support the setup flow. **These steps are typically performed by Bishop (Azure provisioning lead).**

### Prerequisites

1. **Multi-tenant Entra app registration already exists** (created during initial setup per `docs/azure-setup.md`).

2. **Reply URLs (Redirect URIs):**
   Add the following reply URLs to the app registration:
   - `{SETUP_REDIRECT_BASE_URI}/callback`
   - `{SETUP_REDIRECT_BASE_URI}/mgmt-callback`

   Example (for `http://localhost:8080`):
   - `http://localhost:8080/callback`
   - `http://localhost:8080/mgmt-callback`

   Example (for production):
   - `https://setup.myapp.com/callback`
   - `https://setup.myapp.com/mgmt-callback`

   **Azure Portal Path:** App registrations → [App name] → Authentication → Redirect URIs (Web).

3. **API Permissions (Delegated):**

   | API | Permission | Type | Purpose |
   |-----|-----------|------|---------|
   | Microsoft Graph | `Application.Read.All` | Delegated | Query service principals in target tenant |
   | Azure Service Management | `user_impersonation` | Delegated | Call ARM REST API for role assignment |

   **Azure Portal Path:** App registrations → [App name] → API Permissions → Add a permission.

4. **No client secret required** (PKCE-only authentication). Verify:
   - **Certificates & secrets:** No secrets are created or stored.
   - Only the `Client ID` is needed (non-secret).

### Consent Flow

The app uses **incremental consent**:
1. **First consent (Graph):** Admin grants consent to `Application.Read.All` during Graph OAuth2 flow (`prompt=consent`).
2. **Second consent (Management):** Admin grants consent to `user_impersonation` during Management OAuth2 flow.

Each OAuth2 call targets `/common` endpoint, allowing multi-tenant admins to consent.

---

## Risks and Mitigations

### Risk 1: Admin Lacks Required Permissions

**Scenario:** Admin completes setup wizard but lacks Owner or Contributor role on the storage account.

**Impact:** Role assignment fails. Setup appears to succeed but users cannot access storage.

**Mitigation:**
- Setup app displays clear error message with troubleshooting steps.
- Admin should verify they have Contributor or Owner role before starting setup.
- Setup does NOT auto-retry; admin must fix permissions and re-run.
- Document prerequisites in `docs/setup-ui-architecture.md` (this file) and link from success page.

---

### Risk 2: In-Memory Session State Lost on Pod Restart

**Scenario:** Setup pod crashes or is evicted mid-setup. Admin's browser loses session.

**Impact:** Admin must restart the setup flow from the beginning (re-do consent).

**Mitigation:**
- Pod restarts are rare during a 30-minute setup window (normal operation).
- If persistent sessions are required, use Redis or an external session store (out of scope for this design).
- Session TTL is 30 minutes; encourage admins to complete setup in one session.
- Document in troubleshooting guide: "If the setup wizard disconnects, refresh your browser and restart the flow."

---

### Risk 3: Management Token Expires Before Form Submission

**Scenario:** Admin opens the configuration form but waits 1+ hour before submitting (Azure tokens expire in ~1 hour).

**Impact:** Role assignment fails with token expiration error.

**Mitigation:**
- Configuration form displays a countdown timer ("Token expires in X minutes").
- If token is near expiration (< 5 minutes remaining), warn admin.
- If token has expired, redirect to re-run the management OAuth2 flow.
- Clear guidance: "Complete the setup within 1 hour of starting."

---

### Risk 4: Cross-Tenant Redirection Attack

**Scenario:** Attacker tricks admin into OAuth2 flow targeting Tenant B instead of Tenant A.

**Impact:** Setup app assigns roles in the wrong tenant.

**Mitigation:**
- Verify `tid` claim in Graph token matches `tid` in Management token (see Security Considerations).
- Verify both `tid` values match the admin's home tenant ID (pre-configured or user-selected).
- Display tenant ID on the configuration form for manual verification.
- If mismatch, abort and log security event.

---

### Risk 5: Replay Attack (Same Consent / Role Assignment)

**Scenario:** Attacker captures the setup URL and replays it days later with a different storage account.

**Impact:** Role assigned to attacker's storage account using admin's consent.

**Mitigation:**
- Session data (including consent tokens) has a short TTL (30 minutes).
- PKCE code_verifier is single-use (Microsoft's OAuth2 implementation ensures this).
- Each role assignment is idempotent (deterministic UUID prevents duplicate assignments).
- Log all role assignments; monitor for unexpected entries.

---

## Implementation Checklist (for Dallas / Development Team)

- [ ] Create `cmd/setup/main.go` with web server (Gin, Echo, or stdlib http).
- [ ] Implement handlers: `/`, `/auth`, `/callback`, `/mgmt-auth`, `/mgmt-callback`, `/configure`, `/setup`, `/success`.
- [ ] Integrate OAuth2 library (e.g., `golang.org/x/oauth2`).
- [ ] Add Microsoft Graph client (`github.com/Azure/azure-sdk-for-go/sdk/azidentity`, `github.com/microsoftgraph/msgraph-sdk-go`).
- [ ] Add Azure ARM client (`github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/authorization/armauthorization`).
- [ ] Implement PKCE flow (code_verifier, code_challenge generation and validation).
- [ ] Implement session state management (UUID cookie, server-side map with TTL).
- [ ] Add token validation (JWT signature, `tid` claim, expiration).
- [ ] Write unit tests for OAuth2 flows (mock Microsoft endpoints).
- [ ] Write integration tests (mock Azure SDK clients).
- [ ] Update `Dockerfile` with multi-stage build (both binaries).
- [ ] Update `deploy/` with setup Deployment, Service, ConfigMap manifests.
- [ ] Update `docs/azure-setup.md` with setup wizard deployment instructions and prerequisites.

---

## Decision Record

| Aspect | Decision | Rationale |
|--------|----------|-----------|
| Architecture | Two binaries, one container image | Separation of concerns, cost/complexity trade-off |
| Auth method | PKCE (no client secret) | Aligns with no-secrets philosophy |
| Session storage | Server-side in-memory (UUID cookie) | Acceptable for one-time setup wizard |
| Workload identity | NOT used for setup app | Delegates to admin's tokens (no pod identity needed) |
| Deployment | ClusterIP + kubectl port-forward | Simplest, most secure for temporary setup |
| Token scope | Separate Graph + Management tokens | Different resources, independent consent |

---

## References

- [Microsoft Entra OAuth2 v2.0 Implicit Flow](https://learn.microsoft.com/en-us/entra/identity-platform/v2-oauth2-auth-code-flow)
- [PKCE (RFC 7636)](https://tools.ietf.org/html/rfc7636)
- [Microsoft Graph API - Service Principals](https://learn.microsoft.com/en-us/graph/api/resources/serviceprincipal)
- [Azure Resource Manager Role Assignment API](https://learn.microsoft.com/en-us/rest/api/authorization/role-assignments/create)
- [Storage Blob Data Contributor Role ID](https://learn.microsoft.com/en-us/azure/role-based-access-control/built-in-roles/storage#storage-blob-data-contributor)

---

## Next Steps

1. **Bishop:** Update Entra app registration with reply URLs and delegated permissions (see Multi-Tenant App Registration Prerequisites section).
2. **Dallas:** Implement `cmd/setup/` with OAuth2 flows and Azure SDK clients.
3. **Hudson:** Update `deploy/` with setup Deployment/Service/ConfigMap; test with `kubectl port-forward`.
4. **Ripley:** Review implementation against this architecture; sign off on security posture.
