# Azure Setup Guide: Cross-Tenant AKS Workload Identity to Blob Storage

This guide walks you through setting up a containerized Go application running in an Azure Kubernetes Service (AKS) cluster (source tenant) that uses workload identity to write timestamps to Azure Blob Storage in a different Azure tenant (target tenant).

**No secrets are stored anywhere.** Authentication flows through:
1. AKS pod → Identity Binding RBAC check (UAMI authorization)
2. AKS Identity Binding OIDC issuer → Microsoft Entra federated credential on multi-tenant app
   (one FIC per UAMI — shared across all AKS clusters using the same UAMI)
3. Cross-tenant service principal in the target tenant
4. Azure RBAC (Storage Blob Data Contributor) on the target storage account

**Identity Bindings (PREVIEW):** This solution uses AKS Identity Bindings to overcome the 20-FIC limit on the Entra app registration. Instead of one federated credential per cluster, a single FIC on the multi-tenant app trusts the Identity Binding OIDC issuer — which is stable across all AKS clusters that bind to the same UAMI.

---

## Prerequisites

Before starting, verify you have:

- **Azure CLI** installed and authenticated to both source and target tenants  
  ```bash
  az --version
  az account show
  ```
- **`aks-preview` Azure CLI extension** (required for Identity Bindings):
  ```bash
  az extension add --name aks-preview
  az extension update --name aks-preview
  ```
- **kubectl** installed and configured to access your AKS cluster  
  ```bash
  kubectl version --client
  ```
- **Access to both tenants:**
  - Source tenant: where the AKS cluster and Entra application registration live
  - Target tenant: where the Azure Storage account and target service principal live
- **An existing AKS cluster with OIDC Issuer and Workload Identity enabled** (or follow Step 1 to create/enable)
- **Workload Identity webhook v1.6.0-alpha.1 or later** must be installed on the AKS cluster (required for Identity Bindings support)
- **Sufficient permissions in both tenants:**
  - Source: Contributor or higher on the subscription/resource group
  - Target: Cloud Application Administrator (for admin consent) and Contributor (for RBAC and storage resources)
- **`IdentityBindingPreview` feature flag** registered on your source subscription (see Step 2b for registration commands)

> ⚠️ **PREVIEW:** AKS Identity Bindings is a preview feature. APIs and behavior may change before general availability.

**Setup Approach:** Choose either **Option A (manual CLI)** or **Option B (setup wizard UI)** below. Option B is recommended for cross-tenant deployments where you want to share the setup with the target tenant admin without exposing CLI commands.

---

## Variables

Edit the block below with your values, then use it for all subsequent commands. **Copy the entire block, set your values, then paste into your shell.**

```bash
# ============================================================
# SOURCE TENANT (where AKS and the Entra app registration live)
# ============================================================
SOURCE_TENANT_ID="<your-aks-tenant-id>"                    # e.g., "12345678-1234-1234-1234-123456789abc"
SOURCE_SUBSCRIPTION_ID="<your-aks-subscription-id>"        # e.g., "87654321-4321-4321-4321-abcdef123456"
RESOURCE_GROUP="aks-xtenant-auth-rg"
CLUSTER_NAME="aks-xtenant-auth"
LOCATION="eastus"
MANAGED_IDENTITY_NAME="timestampwriter-identity"           # User-assigned managed identity (optional if using app registration only)

# ============================================================
# IDENTITY BINDINGS (new — required for AKS Identity Bindings)
# ============================================================
IDENTITY_BINDING_NAME="timestampwriter-binding"            # Name for the identity binding resource
IB_OIDC_ISSUER_URL=""                                      # Set in Step 2b after identity binding is created

APP_DISPLAY_NAME="aks-xtenant-auth-app"                    # Multi-tenant Entra app registration
K8S_NAMESPACE="aks-xtenant-auth"
K8S_SERVICE_ACCOUNT="timestampwriter"

# ============================================================
# TARGET TENANT (where Azure Storage lives)
# ============================================================
TARGET_TENANT_ID="<target-tenant-id>"                      # e.g., "99999999-9999-9999-9999-999999999999"
TARGET_SUBSCRIPTION_ID="<target-subscription-id>"          # e.g., "11111111-1111-1111-1111-111111111111"
TARGET_RESOURCE_GROUP="aks-xtenant-auth-target-rg"
STORAGE_ACCOUNT_NAME="<globally-unique-storage-name>"     # e.g., "timestampwriter001"
STORAGE_CONTAINER_NAME="timestamps"

# ============================================================
# DERIVED VALUES (DO NOT EDIT THESE)
# ============================================================
# These will be populated as you follow the steps
OIDC_ISSUER_URL=""                                          # Set in Step 1
UAMI_CLIENT_ID=""                                           # Set in Step 2
UAMI_PRINCIPAL_ID=""                                        # Set in Step 2
APP_CLIENT_ID=""                                            # Set in Step 4
APP_OBJECT_ID=""                                            # Set in Step 4
TARGET_SP_OBJECT_ID=""                                      # Set in Step 5

# ============================================================
# NFS STORAGE (shared file share for setup.db)
# Required when subscription enforces allowSharedKeyAccess: false
# ============================================================
NFS_RESOURCE_GROUP="<nfs-resource-group>"                  # Resource group for NFS storage account
NFS_STORAGE_ACCOUNT="<nfs-storage-account>"                # Premium_LRS FileStorage account (NFS)
AKS_VNET_NAME="<aks-vnet-name>"                            # VNet containing AKS node subnet
AKS_NODE_SUBNET_NAME="<aks-node-subnet-name>"              # AKS node pool subnet name
AKS_NODE_NSG_NAME="<aks-node-nsg-name>"                    # NSG on AKS node subnet

# ============================================================
# SETUP WIZARD (Option B only)
# ============================================================
SETUP_REDIRECT_BASE_URI="http://localhost:8081"             # Or your public URL if deploying outside localhost
SETUP_PORT="8081"
```

---

## Option A: Manual Setup (CLI)

## Step 1: Ensure AKS has OIDC Issuer + Workload Identity Enabled

The AKS cluster needs:
1. **OIDC Issuer** enabled — AKS publishes its OIDC metadata so Kubernetes ServiceAccounts can issue signed tokens
2. **Workload Identity** enabled — a mutating webhook injects the OIDC token and workload identity environment variables into pods

### Check if already enabled:

```bash
az account set --subscription "$SOURCE_SUBSCRIPTION_ID"

az aks show \
  --resource-group "$RESOURCE_GROUP" \
  --name "$CLUSTER_NAME" \
  --query "oidcIssuerProfile.issuerUrl" -o tsv
```

If you see a URL like `https://eastus.oic.prod-aks.azure.com/...`, the OIDC issuer is already enabled. Skip to **capturing the OIDC issuer URL** below.

### Enable on an existing cluster:

If the query above returned nothing, enable OIDC issuer and workload identity:

```bash
az aks update \
  --resource-group "$RESOURCE_GROUP" \
  --name "$CLUSTER_NAME" \
  --enable-oidc-issuer \
  --enable-workload-identity-for-all-pods \
  --query "oidcIssuerProfile.issuerUrl" -o tsv
```

### Create a new AKS cluster with both enabled:

If you don't have a cluster yet:

```bash
# Create resource group
az group create \
  --name "$RESOURCE_GROUP" \
  --location "$LOCATION"

# Create AKS cluster with OIDC Issuer and Workload Identity
az aks create \
  --resource-group "$RESOURCE_GROUP" \
  --name "$CLUSTER_NAME" \
  --location "$LOCATION" \
  --enable-oidc-issuer \
  --enable-workload-identity-for-all-pods \
  --enable-managed-identity \
  --generate-ssh-keys \
  --node-count 1 \
  --query "oidcIssuerProfile.issuerUrl" -o tsv
```

### Capture the OIDC Issuer URL:

```bash
OIDC_ISSUER_URL=$(az aks show \
  --resource-group "$RESOURCE_GROUP" \
  --name "$CLUSTER_NAME" \
  --query "properties.oidcIssuerProfile.issuerUrl" -o tsv)

echo "OIDC Issuer URL: $OIDC_ISSUER_URL"
```

**Save this URL**. You will use it in Step 4 and Step 3.

---

## Step 2: Create User-Assigned Managed Identity (UAMI) in source tenant

While we will use a **multi-tenant Entra app registration** as the primary identity (Step 4), we first create a UAMI for reference and for the federated credential. The UAMI's federated credential will link to the Entra app.

```bash
az account set --subscription "$SOURCE_SUBSCRIPTION_ID"

# Create the UAMI
az identity create \
  --resource-group "$RESOURCE_GROUP" \
  --name "$MANAGED_IDENTITY_NAME"

# Capture its Client ID and Principal (Object) ID
UAMI_CLIENT_ID=$(az identity show \
  --resource-group "$RESOURCE_GROUP" \
  --name "$MANAGED_IDENTITY_NAME" \
  --query "clientId" -o tsv)

UAMI_PRINCIPAL_ID=$(az identity show \
  --resource-group "$RESOURCE_GROUP" \
  --name "$MANAGED_IDENTITY_NAME" \
  --query "principalId" -o tsv)

echo "UAMI Client ID: $UAMI_CLIENT_ID"
echo "UAMI Principal ID: $UAMI_PRINCIPAL_ID"
```

**Save these values.** The `UAMI_CLIENT_ID` will appear in optional places; the `UAMI_PRINCIPAL_ID` may be useful for RBAC debugging.

---

## Step 2b: Create Identity Binding (NEW — replaces per-cluster FIC)

This is the key change from plain workload identity. Instead of creating a federated identity
credential (FIC) per cluster on the Entra app, we create a single Identity Binding between the
UAMI and the AKS cluster. AKS automatically manages a single FIC on the UAMI, and its OIDC
issuer URL is **stable across all clusters** using the same UAMI.

> ⚠️ **PREVIEW:** Identity Bindings requires the `IdentityBindingPreview` feature flag. See Prerequisites.

First, register the feature flag (one-time per subscription):

```bash
az feature register --namespace Microsoft.ContainerService --name IdentityBindingPreview
# Wait up to 15 minutes for registration
az feature show --namespace Microsoft.ContainerService --name IdentityBindingPreview --query "properties.state"
# Once "Registered":
az provider register --namespace Microsoft.ContainerService
```

Then create the identity binding:

```bash
az aks identity-binding create \
  --resource-group "$RESOURCE_GROUP" \
  --cluster-name "$CLUSTER_NAME" \
  --name "$IDENTITY_BINDING_NAME" \
  --managed-identity-resource-id "$(az identity show \
      --resource-group "$RESOURCE_GROUP" \
      --name "$MANAGED_IDENTITY_NAME" \
      --query id -o tsv)"
```

Capture the Identity Binding OIDC issuer URL (this replaces `OIDC_ISSUER_URL` for the Entra app FIC):

```bash
IB_OIDC_ISSUER_URL=$(az aks identity-binding show \
  --resource-group "$RESOURCE_GROUP" \
  --cluster-name "$CLUSTER_NAME" \
  --name "$IDENTITY_BINDING_NAME" \
  --query "properties.oidcIssuer.oidcIssuerUrl" -o tsv)

echo "Identity Binding OIDC Issuer URL: $IB_OIDC_ISSUER_URL"
# Expected format: https://ib.oic.prod-aks.azure.com/<UAMI-tenant-id>/<UAMI-client-id>
```

**Save this URL** — it is the issuer for ALL FICs going forward (replaces the cluster OIDC issuer URL).

---

## Step 2c: Set Up NFS File Share for Setup Database

> **Only required when your subscription enforces `allowSharedKeyAccess: false`** (common in enterprise environments with Azure Policy). The default `azurefile-csi` storage class uses CIFS/SMB, which authenticates with the storage account key and is permanently blocked by this policy. Azure Files NFS v4.1 uses network-level access control instead — no key is exchanged at mount time.

Use the `NFS_RESOURCE_GROUP`, `NFS_STORAGE_ACCOUNT`, `AKS_VNET_NAME`, `AKS_NODE_SUBNET_NAME`, and `AKS_NODE_NSG_NAME` variables from the Variables section above.

### 1. Enable Microsoft.Storage service endpoint on the AKS node subnet

NFS traffic must route over the Azure backbone via the `Microsoft.Storage` service endpoint so the NFS storage account can restrict access to the subnet without a public IP.

```bash
az network vnet subnet update \
  --resource-group "$NFS_RESOURCE_GROUP" \
  --vnet-name "$AKS_VNET_NAME" \
  --name "$AKS_NODE_SUBNET_NAME" \
  --service-endpoints Microsoft.Storage
```

### 2. Add NSG rule for NFS outbound (TCP 2049)

The AKS node subnet's NSG must allow outbound TCP port 2049 to the Azure Storage service tag for your region.

```bash
az network nsg rule create \
  --resource-group "$NFS_RESOURCE_GROUP" \
  --nsg-name "$AKS_NODE_NSG_NAME" \
  --name "Allow-NFS-Outbound" \
  --priority 110 \
  --direction Outbound \
  --protocol Tcp \
  --destination-port-ranges 2049 \
  --destination-address-prefixes "Storage.${LOCATION}" \
  --access Allow
```

### 3. Create Premium_LRS FileStorage storage account

NFS file shares require a `Premium_LRS` storage account with `FileStorage` kind and `https-only` disabled (NFS does not use HTTPS).

```bash
az account set --subscription "$SOURCE_SUBSCRIPTION_ID"

az storage account create \
  --resource-group "$NFS_RESOURCE_GROUP" \
  --name "$NFS_STORAGE_ACCOUNT" \
  --location "$LOCATION" \
  --sku Premium_LRS \
  --kind FileStorage \
  --https-only false \
  --allow-shared-key-access false
```

### 4. Add VNet rule to the NFS storage account

Restrict NFS mount access to the AKS node subnet only.

```bash
NFS_SA_ID=$(az storage account show \
  --resource-group "$NFS_RESOURCE_GROUP" \
  --name "$NFS_STORAGE_ACCOUNT" \
  --query "id" -o tsv)

SUBNET_ID=$(az network vnet subnet show \
  --resource-group "$NFS_RESOURCE_GROUP" \
  --vnet-name "$AKS_VNET_NAME" \
  --name "$AKS_NODE_SUBNET_NAME" \
  --query "id" -o tsv)

az storage account network-rule add \
  --resource-group "$NFS_RESOURCE_GROUP" \
  --account-name "$NFS_STORAGE_ACCOUNT" \
  --subnet "$SUBNET_ID"

az storage account update \
  --resource-group "$NFS_RESOURCE_GROUP" \
  --name "$NFS_STORAGE_ACCOUNT" \
  --default-action Deny
```

### 5. Create the NFS file share "setup-db"

NFS file shares must be created via ARM (`az storage share-rm`) — the data plane command does not support NFS.

```bash
az storage share-rm create \
  --resource-group "$NFS_RESOURCE_GROUP" \
  --storage-account "$NFS_STORAGE_ACCOUNT" \
  --name "setup-db" \
  --enabled-protocols NFS \
  --quota 100
```

After completing these prerequisites, update `deploy/setup-pv.yaml` with your `NFS_RESOURCE_GROUP`, `NFS_STORAGE_ACCOUNT`, `SOURCE_SUBSCRIPTION_ID`, and `K8S_NAMESPACE` values, then proceed to Step 3.

---

## Step 3: SKIP — AKS Manages the UAMI Federated Credential Automatically

With Identity Bindings enabled (Step 2b), AKS automatically creates and manages a federated identity credential on the UAMI. **Do NOT manually create a FIC on the UAMI** — AKS owns it.

The AKS-managed FIC (named `aks-identity-binding`) handles the cluster-to-UAMI token exchange internally via the in-cluster proxy.

If you previously ran this step manually, remove the manual FIC:

```bash
az identity federated-credential delete \
  --resource-group "$RESOURCE_GROUP" \
  --identity-name "$MANAGED_IDENTITY_NAME" \
  --name "aks-oidc-credential"
```

---

## Step 4: Register a Multi-Tenant Entra Application in source tenant

To enable cross-tenant access to Azure Storage, you need an Entra app registration. Unlike a UAMI, an Entra app can have a service principal in the target tenant, and that service principal can be assigned RBAC roles on target resources.

### Create the app registration:

```bash
az account set --subscription "$SOURCE_SUBSCRIPTION_ID"

# Create the multi-tenant app registration
APP_ID=$(az ad app create \
  --display-name "$APP_DISPLAY_NAME" \
  --sign-in-audience "AzureADMultipleOrgs" \
  --query "appId" -o tsv)

echo "App Registration created with Client ID (App ID): $APP_ID"

# Store it in our variable
APP_CLIENT_ID="$APP_ID"

# Get the app's Object ID (needed later for federated credentials)
APP_OBJECT_ID=$(az ad app show \
  --id "$APP_CLIENT_ID" \
  --query "id" -o tsv)

echo "App Object ID: $APP_OBJECT_ID"
```

### Add a federated credential to the Entra app:

The federated credential tells Entra ID: "Trust OIDC tokens from this Identity Binding's issuer and exchange them for access tokens."

By using the Identity Binding OIDC issuer URL (not the cluster-specific OIDC issuer), this single FIC works across **all AKS clusters** that have an Identity Binding for the same UAMI. This eliminates the need to create a new FIC per cluster — the 20-FIC limit on the Entra app is no longer a scaling constraint.

```bash
az account set --subscription "$SOURCE_SUBSCRIPTION_ID"

# Delete the old cluster-OIDC-issuer-based FIC (if it exists from a previous setup)
az ad app federated-credential delete \
  --id "$APP_OBJECT_ID" \
  --federated-credential-id "aks-oidc-credential"

# Add federated credential to the Entra app registration using the Identity Binding OIDC issuer
az ad app federated-credential create \
  --id "$APP_OBJECT_ID" \
  --parameters '{
    "name": "aks-identity-binding-credential",
    "issuer": "'"$IB_OIDC_ISSUER_URL"'",
    "subject": "system:serviceaccount:'"${K8S_NAMESPACE}"':'"${K8S_SERVICE_ACCOUNT}"'",
    "audiences": ["api://AzureADTokenExchange"]
  }'

echo "Federated credential created on Entra app registration"
```

---

## Step 5: Grant Admin Consent and Create Service Principal in target tenant

For the app to access resources in the target tenant, a service principal of the source tenant's app must exist in the target tenant, and the target tenant admin must have granted consent.

### Switch to target tenant and grant consent:

```bash
# Switch to target tenant
az account set --subscription "$TARGET_SUBSCRIPTION_ID"

# Create the service principal in the target tenant
# This automatically grants admin consent for the target tenant
TARGET_SP_OBJECT_ID=$(az ad sp create \
  --id "$APP_CLIENT_ID" \
  --query "id" -o tsv)

echo "Service Principal created in target tenant with Object ID: $TARGET_SP_OBJECT_ID"
```

**What this does:**
- Creates a service principal of the source app in the target tenant
- Automatically grants tenant-wide admin consent for the multi-tenant app
- No further consent prompts are needed

---

## Step 6: Create Storage Account and Container in target tenant

Now create the Azure Storage account and blob container in the target tenant where the Go application will write timestamps.

```bash
az account set --subscription "$TARGET_SUBSCRIPTION_ID"

# Create resource group in target tenant (if it doesn't exist)
az group create \
  --name "$TARGET_RESOURCE_GROUP" \
  --location "$LOCATION"

# Create the storage account
az storage account create \
  --resource-group "$TARGET_RESOURCE_GROUP" \
  --name "$STORAGE_ACCOUNT_NAME" \
  --location "$LOCATION" \
  --sku "Standard_LRS" \
  --https-only true

# Get the storage account ID (needed for RBAC role assignment)
STORAGE_ACCOUNT_ID=$(az storage account show \
  --resource-group "$TARGET_RESOURCE_GROUP" \
  --name "$STORAGE_ACCOUNT_NAME" \
  --query "id" -o tsv)

echo "Storage Account ID: $STORAGE_ACCOUNT_ID"

# Create the blob container
az storage container create \
  --account-name "$STORAGE_ACCOUNT_NAME" \
  --name "$STORAGE_CONTAINER_NAME" \
  --public-access "off"

echo "Container '$STORAGE_CONTAINER_NAME' created"
```

---

## Step 7: Assign RBAC in target tenant

Assign the `Storage Blob Data Contributor` role to the target tenant's service principal on the storage account. This allows the app to read, write, and delete blobs.

```bash
az account set --subscription "$TARGET_SUBSCRIPTION_ID"

# Assign Storage Blob Data Contributor to the service principal
az role assignment create \
  --role "Storage Blob Data Contributor" \
  --assignee-object-id "$TARGET_SP_OBJECT_ID" \
  --assignee-principal-type "ServicePrincipal" \
  --scope "$STORAGE_ACCOUNT_ID"

echo "RBAC role 'Storage Blob Data Contributor' assigned to service principal"
```

**What this does:**
- The service principal (of the source app in the target tenant) can now read, write, and delete blobs
- The role is scoped to the entire storage account; the app can access any container
- If you want to restrict to a single container, add `--scope "$STORAGE_ACCOUNT_ID/blobServices/default/containers/$STORAGE_CONTAINER_NAME"`

---

## Step 8: Update Kubernetes Manifests

Edit your Kubernetes manifests to use the correct values. Below are the key pieces.

### Apply Identity Bindings RBAC first:

ClusterRole and ClusterRoleBinding must be applied before the ServiceAccount so the Identity Binding webhook can authorize the pod at creation time.

Before applying, edit `deploy/clusterrole.yaml` to replace `<UAMI_CLIENT_ID>` with the actual UAMI client ID (`$UAMI_CLIENT_ID` from Step 2):

```bash
kubectl apply -f deploy/clusterrole.yaml
kubectl apply -f deploy/clusterrolebinding.yaml
```

### Create the Kubernetes Namespace:

```bash
kubectl create namespace "$K8S_NAMESPACE"
```

### Create or update the ServiceAccount with the workload identity annotation:

**File:** `deploy/serviceaccount.yaml`

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: timestampwriter
  namespace: aks-xtenant-auth
  annotations:
    azure.workload.identity/client-id: "<UAMI_CLIENT_ID>"
    azure.workload.identity/tenant-id: "<SOURCE_TENANT_ID>"
```

**Replace `<UAMI_CLIENT_ID>` with the UAMI's client ID from Step 2** (`$UAMI_CLIENT_ID`) and **`<SOURCE_TENANT_ID>` with your source tenant ID** (`$SOURCE_TENANT_ID`). The Identity Binding RBAC check uses the UAMI client ID. The actual token exchange with Entra uses the multi-tenant app client ID, which is supplied via the `AZURE_CLIENT_ID` env override in the Deployment (see below).

### Create or update the Deployment:

**File:** `deploy/deployment.yaml`

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: timestampwriter
  namespace: aks-xtenant-auth
spec:
  replicas: 1
  selector:
    matchLabels:
      app: timestampwriter
  template:
    metadata:
      labels:
        app: timestampwriter
        azure.workload.identity/use: "true"  # Enable workload identity for this pod
      annotations:
        azure.workload.identity/use-identity-binding: "true"  # Enable Identity Bindings for this pod
    spec:
      serviceAccountName: timestampwriter
      containers:
      - name: timestampwriter
        image: "<ACR_NAME>.azurecr.io/timestampwriter:latest"
        imagePullPolicy: Always
        envFrom:
          - configMapRef:
              name: timestampwriter-config
        env:
          - name: AZURE_CLIENT_ID
            valueFrom:
              configMapKeyRef:
                name: timestampwriter-config
                key: AZURE_CLIENT_ID
          - name: AZURE_TENANT_ID
            valueFrom:
              configMapKeyRef:
                name: timestampwriter-config
                key: AZURE_TENANT_ID
          - name: SETUP_DB_PATH
            value: "/data/setup.db"
        volumeMounts:
          - name: setup-db
            mountPath: /data
      volumes:
        - name: setup-db
          persistentVolumeClaim:
            claimName: setup-db
```

**Key:** All non-secret config is loaded via `envFrom` from the `timestampwriter-config` ConfigMap. The `AZURE_CLIENT_ID` and `AZURE_TENANT_ID` entries are additionally referenced via explicit `env` entries to ensure the workload identity webhook does not override them (the webhook skips injection when env vars are already present in the container spec). `SETUP_DB_PATH` is injected directly as a literal value.

### Create or update ConfigMap for app config:

**File:** `deploy/configmap.yaml`

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: timestampwriter-config
  namespace: aks-xtenant-auth
data:
  STORAGE_ACCOUNT_URL: "https://<STORAGE_ACCOUNT_NAME>.blob.core.windows.net/"
  STORAGE_CONTAINER_NAME: "<STORAGE_CONTAINER_NAME>"
  AZURE_CLIENT_ID: "<APP_CLIENT_ID>"
  AZURE_TENANT_ID: "<TARGET_TENANT_ID>"
```

**Replace `<STORAGE_ACCOUNT_NAME>`** with your target storage account name, **`<STORAGE_CONTAINER_NAME>`** with your blob container name, **`<APP_CLIENT_ID>`** with the multi-tenant app client ID from Step 4, and **`<TARGET_TENANT_ID>`** with the tenant ID where the storage account lives. The `AZURE_CLIENT_ID` and `AZURE_TENANT_ID` keys are explicitly referenced in the Deployment env overrides to prevent the workload identity webhook from overwriting them.

---

## Step 9: Build and Deploy

### Connect to the AKS cluster:

```bash
az account set --subscription "$SOURCE_SUBSCRIPTION_ID"

az aks get-credentials \
  --resource-group "$RESOURCE_GROUP" \
  --name "$CLUSTER_NAME"
```

### Build the Docker image:

If you have a container registry (e.g., Azure Container Registry):

```bash
# Set your ACR variables
ACR_NAME="<your-acr-name>"                  # e.g., "myacregistry"
IMAGE_NAME="aks-xtenant-auth"
IMAGE_TAG="latest"

az acr build \
  --registry "$ACR_NAME" \
  --image "${IMAGE_NAME}:${IMAGE_TAG}" \
  --file "Dockerfile" \
  .
```

Or build locally and push:

```bash
docker build -t "${ACR_NAME}.azurecr.io/${IMAGE_NAME}:${IMAGE_TAG}" \
  --file "Dockerfile" \
  .

docker push "${ACR_NAME}.azurecr.io/${IMAGE_NAME}:${IMAGE_TAG}"
```

### Apply Kubernetes manifests:

```bash
# Apply manifests in order
kubectl apply -f deploy/clusterrole.yaml
kubectl apply -f deploy/clusterrolebinding.yaml
kubectl apply -f deploy/namespace.yaml
# Static PV must be applied before the PVC — the PVC binds by name to this PV.
# If your subscription enforces allowSharedKeyAccess=false, see setup-pv.yaml
# for the required networking prerequisites before this step.
kubectl apply -f deploy/setup-pv.yaml
kubectl apply -f deploy/setup-pvc.yaml -n "$K8S_NAMESPACE"
kubectl apply -f deploy/serviceaccount.yaml
kubectl apply -f deploy/configmap.yaml
kubectl apply -f deploy/deployment.yaml

# Verify deployment is running
kubectl get deployment -n "$K8S_NAMESPACE"
kubectl get pods -n "$K8S_NAMESPACE"
```

---

## Verification

### Check pod status:

```bash
kubectl get pods -n "$K8S_NAMESPACE" -o wide

# Get pod name
POD_NAME=$(kubectl get pods -n "$K8S_NAMESPACE" -l app=timestampwriter -o jsonpath='{.items[0].metadata.name}')

# Check pod logs
kubectl logs -n "$K8S_NAMESPACE" "$POD_NAME"
```

### Check that the pod has the workload identity environment variables:

```bash
kubectl exec -it -n "$K8S_NAMESPACE" "$POD_NAME" -- env | grep -i azure
```

You should see variables like:
- `AZURE_CLIENT_ID=<APP_CLIENT_ID>`
- `AZURE_TENANT_ID=<TARGET_TENANT_ID>`
- `AZURE_FEDERATED_TOKEN_FILE=/var/run/secrets/tokens/azure/tokens.json`
- `AZURE_AUTHORITY_HOST=https://login.microsoftonline.com/`

### Check that blobs appear in storage:

Switch to the target tenant and list blobs:

```bash
az account set --subscription "$TARGET_SUBSCRIPTION_ID"

az storage blob list \
  --account-name "$STORAGE_ACCOUNT_NAME" \
  --container-name "$STORAGE_CONTAINER_NAME" \
  --query "[].name" -o table
```

You should see one or more blob files with timestamps.

### Test a direct token exchange (diagnostic):

```bash
# From inside the pod
POD_NAME=$(kubectl get pods -n "$K8S_NAMESPACE" -l app=timestampwriter -o jsonpath='{.items[0].metadata.name}')

kubectl exec -it -n "$K8S_NAMESPACE" "$POD_NAME" -- bash

# Inside the pod:
cat $AZURE_FEDERATED_TOKEN_FILE | head -c 50
# Should be a JWT (three dot-separated Base64 strings)
```

---

## Option B: Setup Wizard UI

This option uses a web-based setup wizard (deployed as a Kubernetes pod) that handles the cross-tenant authentication flow programmatically. Instead of running CLI commands in both tenants, a target tenant admin can run the wizard via a web browser.

### Additional App Registration Requirements for the Setup Wizard

The multi-tenant Entra app registration needs these additional configurations before the setup wizard can be used. Run these commands in the source tenant:

#### 1. Add Redirect URIs

```bash
# Add redirect URIs for the setup wizard callbacks
az ad app update --id $APP_CLIENT_ID \
  --web-redirect-uris \
    "${SETUP_REDIRECT_BASE_URI}/callback" \
    "${SETUP_REDIRECT_BASE_URI}/mgmt-callback"

echo "Redirect URIs added to app registration"
```

#### 2. Add Delegated API Permissions

The wizard requests delegated permissions (not app-only) so the target tenant admin grants consent interactively:

```bash
# Add Microsoft Graph - Application.Read.All (delegated)
az ad app permission add \
  --id $APP_CLIENT_ID \
  --api 00000003-0000-0000-c000-000000000000 \
  --api-permissions 9a5d68dd-52b0-4cc2-bd40-abcf44ac3a30=Scope

# Add Azure Service Management - user_impersonation (delegated)
az ad app permission add \
  --id $APP_CLIENT_ID \
  --api 797f4846-ba00-4fd7-ba43-dac1f8f63013 \
  --api-permissions 41094075-9dad-400e-a0bd-54e686782033=Scope

echo "Delegated permissions added to app registration"
```

**Note:** Admin consent is granted during the wizard flow (Step 3 below), not through `az ad app permission grant`.

#### 3. Admin Requirements

The person who runs the setup wizard must have:
- **Global Administrator** or **Application Administrator** in the target tenant (to grant admin consent for the app)
- **Owner** or **User Access Administrator** on the target storage account (to assign RBAC roles)

### Deploying the Setup Wizard

#### Prerequisites in the source tenant:

- Azure Container Registry (ACR) where you can push the setup wizard image
- `kubectl` access to the AKS cluster with permission to create deployments

#### Build and deploy the setup wizard pod:

```bash
# Set variables
ACR_NAME="<your-acr-name>"                     # e.g., "myacregistry"
SETUP_IMAGE_NAME="aks-xtenant-setup"
SETUP_IMAGE_TAG="latest"

# Build the setup wizard image
az acr build \
  --registry "$ACR_NAME" \
  --image "${SETUP_IMAGE_NAME}:${SETUP_IMAGE_TAG}" \
  --file "Dockerfile.setup" \
  .

# Alternatively, build locally and push:
# docker build -f Dockerfile.setup -t "${ACR_NAME}.azurecr.io/${SETUP_IMAGE_NAME}:${SETUP_IMAGE_TAG}" .
# docker push "${ACR_NAME}.azurecr.io/${SETUP_IMAGE_NAME}:${SETUP_IMAGE_TAG}"

# Deploy the setup wizard to the AKS cluster
# setup-pv.yaml must be applied BEFORE setup-pvc.yaml — the PVC binds to the static PV.
# NOTE: If your subscription enforces allowSharedKeyAccess=false on storage accounts
# (common in enterprise environments), the default azurefile-csi storage class uses
# CIFS/SMB which requires key auth and will fail with "Permission denied".
# setup-pv.yaml uses Azure Files NFS (static provisioning) which avoids key auth entirely.
# See setup-pv.yaml for the full networking prerequisites before applying.
kubectl apply -f deploy/setup-pv.yaml
kubectl apply -f deploy/setup-pvc.yaml -n aks-xtenant-auth
kubectl apply -f deploy/setup-configmap.yaml
kubectl apply -f deploy/setup-deployment.yaml
kubectl apply -f deploy/setup-service.yaml

echo "Setup wizard deployed"
```

#### Access the setup wizard:

**Using kubectl port-forward (recommended for security):**

```bash
# Forward port 8081 from the setup service to localhost
kubectl port-forward -n aks-xtenant-auth service/setup 8081:8081

# Open browser to: http://localhost:8081
```

**Or (if deployed with a public LoadBalancer):**

```bash
# Get the external IP
kubectl get service setup -n aks-xtenant-auth

# Open browser to: http://<EXTERNAL_IP>:8081
```

### Using the Setup Wizard (Target Tenant Admin Perspective)

1. **Open the setup wizard:** `http://localhost:8081` (or your public URL)

2. **Click "Start Setup"** — you'll be taken to Microsoft login

3. **Sign in with your target tenant Azure credentials** — the app will ask for admin consent to:
   - Read app information from your tenant (Microsoft Graph: `Application.Read.All`)
   - This requires **Global Administrator** or **Application Administrator** role

4. **Grant admin consent** — after consent, you'll be redirected to authorize RBAC management access

5. **Sign in again** (if prompted) — the app now requests:
   - Manage Azure resources (`user_impersonation` from Azure Service Management)
   - This requires permissions to assign roles on your storage account

6. **Enter your storage account details:**
   - Subscription ID (target tenant)
   - Resource Group name
   - Storage Account name
   - Container name (e.g., "timestamps")

7. **Click "Configure"** — the wizard:
   - Exchanges your tokens for an access token scoped to your tenant
   - Calls the Azure Resource Manager (ARM) REST API
   - Assigns **Storage Blob Data Contributor** role to the source app's service principal on your storage account

8. **Copy the ConfigMap values** — on the success page, you'll see:
   - `STORAGE_ACCOUNT_URL` — share this with the source tenant team
   - Application ID — confirm this matches your app registration

### Security Notes

- **No client secrets:** The wizard uses PKCE (Proof Key for Code Exchange) — the source tenant's app registration never exposes a secret
- **Session data:** Held in-memory and expires after 1 hour
- **Network isolation:** Deploy on private LoadBalancer or use `kubectl port-forward`; never expose publicly to the internet
- **Admin consent:** Granted by the target tenant admin interactively; no pre-granted permissions

### After the Wizard Completes

The target tenant admin must share the **ConfigMap values** (especially `STORAGE_ACCOUNT_URL`) back to the source tenant team.

The source tenant team then updates `deploy/configmap.yaml`:

```bash
# Update with the values from the wizard
STORAGE_ACCOUNT_URL="https://<STORAGE_ACCOUNT_NAME>.blob.core.windows.net/"  # From wizard success page
STORAGE_CONTAINER_NAME="timestamps"                                            # From wizard setup

# Update configmap.yaml
kubectl set env configmap/timestampwriter-config \
  STORAGE_ACCOUNT_URL="$STORAGE_ACCOUNT_URL" \
  STORAGE_CONTAINER_NAME="$STORAGE_CONTAINER_NAME" \
  -n aks-xtenant-auth

# Restart the app
kubectl rollout restart deployment/timestampwriter -n "$K8S_NAMESPACE"
```

---

## Common Errors and Resolution

### Error: `AADSTS50058: Silent sign-in request failed. The user is not in the scope of the authorization server's policy.`

**Cause:** The Entra app does not have a service principal in the target tenant.

**Fix:** Verify Step 5 completed successfully and the service principal was created.

```bash
az account set --subscription "$TARGET_SUBSCRIPTION_ID"
az ad sp show --id "$APP_CLIENT_ID"
```

### Error: `The user, group or application does not have the appropriate role assignment.`

**Cause:** The service principal does not have `Storage Blob Data Contributor` on the storage account.

**Fix:** Verify Step 7 completed and the role assignment was created:

```bash
az account set --subscription "$TARGET_SUBSCRIPTION_ID"

az role assignment list \
  --assignee-object-id "$TARGET_SP_OBJECT_ID" \
  --scope "$STORAGE_ACCOUNT_ID"
```

### Error: `401 Unauthorized` from Azure SDK

**Cause:** The pod is not using workload identity. Possible reasons:
- The ServiceAccount annotation is missing or has the wrong client ID
- The workload identity webhook is not running or is disabled
- The federated credential on the Entra app is misconfigured

**Fix:**
1. Verify the ServiceAccount annotation:
   ```bash
   kubectl get serviceaccount timestampwriter -n "$K8S_NAMESPACE" -o yaml | grep azure.workload.identity/client-id
   ```
2. Verify the pod has the label:
   ```bash
   kubectl get pods -n "$K8S_NAMESPACE" -o yaml | grep 'azure.workload.identity/use'
   ```
3. Restart the pod to force the webhook to re-inject environment variables:
   ```bash
   kubectl rollout restart deployment/timestampwriter -n "$K8S_NAMESPACE"
   ```

### Error: `Pod cannot pull image from ACR`

**Cause:** The AKS cluster does not have credentials to pull from the container registry.

**Fix:** Grant the AKS cluster's system-assigned managed identity pull permissions on the ACR:

```bash
az account set --subscription "$SOURCE_SUBSCRIPTION_ID"

AKS_IDENTITY=$(az aks show \
  --resource-group "$RESOURCE_GROUP" \
  --name "$CLUSTER_NAME" \
  --query "identity.principalId" -o tsv)

ACR_ID=$(az acr show \
  --resource-group "$RESOURCE_GROUP" \
  --name "$ACR_NAME" \
  --query "id" -o tsv)

az role assignment create \
  --role "AcrPull" \
  --assignee-object-id "$AKS_IDENTITY" \
  --scope "$ACR_ID"
```

---

## Identity Chain Diagram

The complete authentication flow:

```
┌─────────────────────────────────────────────────────────────────────────────┐
│ AKS Cluster (Source Tenant)                                                 │
│                                                                              │
│  ┌──────────────────────────────────────────────────────────────────────┐   │
│  │ Kubernetes Pod                                                       │   │
│  │  • ServiceAccount: timestampwriter                                   │   │
│  │  • Annotation: azure.workload.identity/client-id = <UAMI_CLIENT_ID>   │   │
│  │  • Label: azure.workload.identity/use = "true"                       │   │
│  └──────────────────────┬───────────────────────────────────────────────┘   │
│                         │                                                    │
│                         ▼                                                    │
│  ┌──────────────────────────────────────────────────────────────────────┐   │
│  │ Workload Identity Webhook (mutating webhook)                         │   │
│  │  • Injects OIDC token volume (JWT signed by AKS OIDC issuer)         │   │
│  │  • Injects env vars: AZURE_CLIENT_ID, AZURE_TENANT_ID, etc.         │   │
│  │  • Token path: /var/run/secrets/tokens/azure/tokens.json            │   │
│  └──────────────────────┬───────────────────────────────────────────────┘   │
│                         │                                                    │
│                         ▼                                                    │
│  ┌──────────────────────────────────────────────────────────────────────┐   │
│  │ Go App: azidentity.NewWorkloadIdentityCredential()                   │   │
│  │  • Reads OIDC token from /var/run/secrets/tokens/azure/tokens.json   │   │
│  │  • Calls Entra STS (Security Token Service)                          │   │
│  └──────────────────────┬───────────────────────────────────────────────┘   │
│                         │                                                    │
└─────────────────────────┼────────────────────────────────────────────────────┘
                          │ OIDC Token → Entra STS
                          ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│ Microsoft Entra ID (Source Tenant)                                          │
│                                                                              │
│  ┌──────────────────────────────────────────────────────────────────────┐   │
│  │ App Registration (Multi-Tenant: AzureADMultipleOrgs)                 │   │
│  │  • Display Name: aks-xtenant-auth-app                                │   │
│  │  • Client ID: <APP_CLIENT_ID>                                        │   │
│  │  • Federated Credential:                                             │   │
│  │    - Issuer: <IB_OIDC_ISSUER_URL>                                      │   │
│  │    - Subject: system:serviceaccount:aks-xtenant-auth:timestampwriter │   │
│  │    - Audience: api://AzureADTokenExchange                            │   │
│  └──────────────────────┬───────────────────────────────────────────────┘   │
│                         │ Validates federated credential (OIDC signature)   │
│                         │ Issues app-only access token for target tenant   │
│                         ▼                                                    │
└─────────────────────────┼────────────────────────────────────────────────────┘
                          │ Access Token (scoped to target tenant)
                          ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│ Target Tenant (Different Azure Tenant)                                      │
│                                                                              │
│  ┌──────────────────────────────────────────────────────────────────────┐   │
│  │ Service Principal (of source app in target tenant)                   │   │
│  │  • Created via: az ad sp create --id <APP_CLIENT_ID>                 │   │
│  │  • Represents the source tenant's app in this target tenant          │   │
│  │  • Principal ID: <TARGET_SP_OBJECT_ID>                               │   │
│  │  • Admin consent: granted automatically                              │   │
│  └──────────────────────┬───────────────────────────────────────────────┘   │
│                         │                                                    │
│                         ▼                                                    │
│  ┌──────────────────────────────────────────────────────────────────────┐   │
│  │ Azure Storage Account                                                │   │
│  │  • Account: <STORAGE_ACCOUNT_NAME>                                   │   │
│  │  • Container: timestamps                                             │   │
│  │                                                                       │   │
│  │  ┌──────────────────────────────────────────────────────────────┐    │   │
│  │  │ RBAC Role Assignment                                         │    │   │
│  │  │  • Role: Storage Blob Data Contributor                       │    │   │
│  │  │  • Assigned to: Service Principal (<TARGET_SP_OBJECT_ID>)    │    │   │
│  │  │  • Scope: Storage Account                                    │    │   │
│  │  └──────────────────────────────────────────────────────────────┘    │   │
│  │                                                                       │   │
│  └──────────────────────┬───────────────────────────────────────────────┘   │
│                         │ Go app writes timestamp blobs                     │
│                         ▼                                                    │
│                    ✓ Blobs in container                                     │
└─────────────────────────────────────────────────────────────────────────────┘
```

**Key points:**
- No secrets or credentials stored in the pod
- OIDC token is cryptographically signed by AKS
- Entra app registration is multi-tenant, allowing cross-tenant service principal creation
- Target tenant service principal has only the permissions it needs (Storage Blob Data Contributor)
- All communication is encrypted (HTTPS)

---

## Next Steps

1. **Test the deployment** using the Verification section above
2. **Rotate credentials** (if needed): Federated credentials never expire; to rotate, create a new credential and delete the old one
3. **Scale the deployment** if your workload needs more replicas (workload identity works across all replicas)
4. **Monitor pod logs and Azure Storage** for operational issues
5. **Document your tenant IDs and resource names** for future reference and handoff
