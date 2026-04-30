# AKS Cross-Tenant Authentication — Timestamp Writer

This solution demonstrates AKS workload authentication to Azure Blob Storage in a **different Azure tenant** using **AKS Identity Bindings** (preview feature). A containerized Go application writes a timestamp blob every 30 seconds. Authentication flows from the Kubernetes pod through a User-Assigned Managed Identity (UAMI) secured by Identity Binding RBAC, to a multi-tenant Entra app that holds a service principal in the target tenant with Storage Blob Data Contributor RBAC on the target storage account.

**Key differentiator:** AKS Identity Bindings use a stable OIDC issuer URL (keyed to the UAMI, not the cluster), allowing a single federated credential on the Entra app to cover all AKS clusters using that UAMI. This overcomes the 20-FIC limit on the Entra app registration without requiring one FIC per cluster.

## Architecture

The solution has two components:

1. **Timestamp Writer** — long-running service in the source (AKS) tenant that writes timestamp blobs to a storage account in the target tenant every 30 seconds
2. **Setup Wizard** — ephemeral web application running in the source tenant; allows a target tenant admin to grant consent and assign RBAC via PKCE OAuth flow, no secrets required

Both use AKS workload identity via Identity Bindings. The timestampwriter discovers its target storage account from a SQLite database written by the setup wizard, allowing the setup wizard and timestampwriter to be deployed and run independently.

## Components

### Timestamp Writer (`cmd/timestampwriter`)

Long-running Go service that:
- Reads OIDC token injected by the AKS workload identity webhook
- Exchanges it for an access token scoped to the target tenant using the multi-tenant Entra app
- Every 30 seconds, queries a SQLite database (written by the setup wizard) to discover the target storage account
- Writes a timestamp blob (`timestamp-<RFC3339>.txt`) to the configured storage account and container

No configuration is required before deployment — the service waits for the setup wizard to complete and populate the database.

### Setup Wizard (`cmd/setup`)

Ephemeral web application (runs once per target tenant) that:
- Serves a PKCE OAuth flow on port 8081
- Redirects the target tenant admin to Microsoft Entra for consent and RBAC assignment
- Accepts a callback containing the storage account resource ID and container name
- Writes a "consent" row to the SQLite database (mounted as a shared NFS volume)
- Target tenant admin uses this to grant the multi-tenant app permission to access their storage account

Run once per target tenant. After setup completes, scale to 0 or delete.

## Environment Variables

| Variable | Source | Purpose |
|---|---|---|
| `AZURE_CLIENT_ID` | ConfigMap | **Multi-tenant** Entra app client ID — overrides the UAMI client ID injected by the webhook, enabling cross-tenant token exchange |
| `AZURE_TENANT_ID` | ConfigMap | **Target** tenant ID — overrides the source tenant ID injected by the webhook, so the SDK requests tokens scoped to the target tenant |
| `AZURE_FEDERATED_TOKEN_FILE` | Webhook | Path to the projected OIDC token issued by the AKS Identity Binding issuer |
| `AZURE_KUBERNETES_TOKEN_PROXY` | Webhook | Identity Bindings proxy endpoint (injected only when `use-identity-binding: "true"`) |
| `SETUP_DB_PATH` | Deployment env | Path to the SQLite database written by the setup wizard (timestampwriter reads this to discover the storage account) |
| `STORAGE_ACCOUNT_URL` | ConfigMap | *(Optional)* Full blob endpoint; if set, bypasses database query — use for testing without the setup wizard |
| `STORAGE_CONTAINER_NAME` | ConfigMap | *(Optional)* Container name; if set, bypasses database query — use for testing without the setup wizard |

## Deployment

1. **Configure placeholders in `deploy/` and `deploy-setup.yaml`:**
   - `<ACR_NAME>` — Azure Container Registry name (where images are pushed)
   - `<UAMI_CLIENT_ID>` — User-Assigned Managed Identity client ID (3 places: `serviceaccount.yaml`, `clusterrole.yaml`, `clusterrolebinding.yaml`)
   - `<SOURCE_TENANT_ID>` — Source tenant ID where AKS and Entra app live
   - `<APP_CLIENT_ID>` — Multi-tenant Entra app client ID
   - `<TARGET_TENANT_ID>` — Target tenant ID where the storage account lives
   - `<STORAGE_ACCOUNT_NAME>` — Target storage account name *(optional if using setup wizard)*
   - `<SETUP_REDIRECT_BASE_URI>` — Setup wizard redirect URI (e.g., `http://localhost:8081` for local testing)

2. **Apply manifests:**
   ```bash
   kubectl apply -f deploy/
   kubectl apply -f deploy-setup.yaml  # Only when running the setup wizard
   ```

3. **Container images:**
   - `timestampwriter` — build with `Dockerfile`, push to `<ACR_NAME>.azurecr.io/timestampwriter:latest`
   - `setup` — build with `Dockerfile.setup`, push to `<ACR_NAME>.azurecr.io/setup:latest`

## Azure Setup

Complete infrastructure setup (UAMI creation, Entra app registration, federated credentials, RBAC assignments, NFS storage for the database) is documented in [`docs/azure-setup.md`](docs/azure-setup.md).

The guide provides two options:
- **Option A (manual CLI)** — step-by-step Azure CLI commands
- **Option B (setup wizard UI)** — automated PKCE flow (requires `cmd/setup` to be running)

## Prerequisites

- AKS cluster with **OIDC Issuer** and **Workload Identity** enabled
- **`aks-preview` Azure CLI extension** (required for Identity Bindings)
- **Workload Identity webhook v1.6.0-alpha.1 or later** on the AKS cluster (supports Identity Bindings)
- **`IdentityBindingPreview` feature flag** registered on your source Azure subscription
- User-Assigned Managed Identity (UAMI) in the source tenant
- Multi-tenant Entra app registration in the source tenant with federated credential trusting the Identity Binding OIDC issuer
- Service principal of the multi-tenant app in the target tenant with Storage Blob Data Contributor on the target storage account
- NFS-backed persistent volume (required if your subscription enforces `allowSharedKeyAccess: false`)
