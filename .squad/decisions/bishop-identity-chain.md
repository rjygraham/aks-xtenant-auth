# Decision: Cross-Tenant Identity Architecture

**Date:** 2026-04-29  
**Owner:** Bishop (Azure Engineer)  
**Status:** Active  

---

## Decision

Implement cross-tenant AKS workload identity to Azure Blob Storage using:

1. **Multi-tenant Entra application** (not UAMI) with federated identity credential
2. **Service principal creation** in target tenant via admin consent
3. **Azure RBAC** (Storage Blob Data Contributor) on target storage account

---

## Why Not Alternatives?

### UAMI alone (User-Assigned Managed Identity)

**Rejected because:**
- UAMIs cannot have service principals in target tenants
- RBAC role assignment requires a service principal in the target tenant
- Azure Lighthouse (alternative UAMI cross-tenant path) requires extra subscription-level setup and is not suitable for SaaS patterns

### User-Managed Secret Rotation

**Rejected because:**
- Violates zero-trust principle (secrets stored in pod)
- Adds operational burden (rotation, audit logging)
- Workload identity is simpler and more secure

---

## Identity Chain (Executable)

```
AKS Pod (source tenant)
  ↓ [Projected OIDC Token]
Kubernetes ServiceAccount (timestampwriter)
  ↓ [azidentity.WorkloadIdentityCredential]
Entra App Registration (multi-tenant, source tenant)
  ↓ [Federated Credential: OIDC issuer + ServiceAccount subject]
Entra STS [Token Exchange]
  ↓ [Access Token for target tenant]
Service Principal (of source app in target tenant)
  ↓ [Admin Consent + RBAC Role]
Azure Storage Account (target tenant)
  ↓ [Storage Blob Data Contributor]
✓ Read/Write Blobs
```

---

## Key Identity Values

All values are captured in `docs/azure-setup.md` Step 1–7. The Go application reads from environment variables injected by the workload identity webhook:

- `AZURE_CLIENT_ID` = Entra app client ID (from Step 4)
- `AZURE_TENANT_ID` = Target tenant ID
- `AZURE_FEDERATED_TOKEN_FILE` = Path to projected OIDC token
- `AZURE_AUTHORITY_HOST` = https://login.microsoftonline.com/

---

## RBAC Scope

- **Role:** `Storage Blob Data Contributor`
- **Assigned to:** Service principal of source app in target tenant
- **Scope:** Storage account (all containers)
- **Can be narrowed to:** Single container if needed (update Step 7)

---

## No Implicit Magic

Every relationship is explicit:
- Federated credential issuer = AKS OIDC issuer URL (no discovery)
- Federated credential subject = exact Kubernetes ServiceAccount (no wildcards)
- Service principal in target = manual creation via `az ad sp create`
- RBAC assignment = manual role creation via `az role assignment create`

---

## Implications

- **Setup is one-time:** Federated credentials do not expire
- **Tokens are short-lived:** Entra issues 1-hour access tokens; rotation is automatic
- **No secrets in cluster:** All identity material is cryptographically signed
- **Multi-tenant app is required:** Single-tenant app registrations cannot have service principals in target tenant
- **Admin consent is automatic:** Creating the service principal in the target tenant grants consent automatically

---

## Validation

After setup, verify:
1. Pod logs show no 401/403 errors from Azure SDK
2. Blobs appear in target storage account container
3. `kubectl exec` into pod and check `env | grep AZURE_*`
4. Pod can reach AKS OIDC metadata: `curl <OIDC_ISSUER_URL>/.well-known/openid-configuration`

