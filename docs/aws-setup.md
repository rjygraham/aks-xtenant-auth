# AWS Setup Guide: Dual-Cloud Write Path from AKS

This guide extends the cross-tenant Azure write path with an AWS write path. The same AKS pod that writes timestamps to Azure Blob Storage (source → target tenant) also writes to an AWS S3 bucket — both from a single pod, with no secrets stored anywhere.

**Authentication flows through:**
1. AKS pod → projected service account token (`aud: sts.amazonaws.com`, cluster OIDC issuer)
2. AWS STS `AssumeRoleWithWebIdentity` → short-lived AWS credentials
3. IAM Role with least-privilege S3 policy → S3 bucket write

The Azure path is unaffected. Both paths run concurrently inside the same pod. The two sets of credentials are completely independent.

**Prerequisite:** Complete `docs/azure-setup.md` first. The Azure path must already be working before adding the AWS path.

---

## Choosing an Approach

This guide documents two independent approaches for configuring AWS access from AKS pods. Both approaches write to the same S3 bucket and use the same IAM Role — they differ in how AWS validates the pod's identity.

| | Option A (Per-Cluster OIDC) | Option B (Azure AD as OIDC Provider) |
|--|--|--|
| IAM IdP registrations | 1 per cluster | 1 for all clusters |
| `sub` claim in trust policy | `system:serviceaccount:...` | UAMI Object ID |
| Pod projected volume | Requires `aws-identity-token` manual volume | No AWS volume needed |
| Token hops to AWS STS | 1 (cluster OIDC → AWS STS) | 2 (cluster OIDC → IB proxy → UAMI → Azure AD → AWS STS) |
| Multi-cluster overhead | New IAM IdP registration per cluster | Zero — new clusters inherit automatically |
| Azure resource required | None beyond AKS cluster | Entra app registration for AWS audience |
| Go application complexity | Minimal (AWS SDK auto-handles) | Moderate (explicit STS call required) |

**Option A** is simpler to set up and appropriate when you have one cluster or a small, stable cluster fleet.

**Option B** eliminates the N-IAM-IdP scaling problem by using `https://login.microsoftonline.com/<ENTRA_SOURCE_TENANT_ID>/v2.0` as the single registered OIDC provider. The UAMI access token (issued by that stable endpoint, with `sub` = UAMI Object ID) is used as the web identity token for AWS STS. This is the AWS analog of how Identity Bindings make the Azure path cluster-independent.

---

## How the Two Token Paths Coexist

The AKS Workload Identity webhook injects one projected service account token for Azure:

- **Azure token** — volume name `azure-identity-token`, `aud: api://AKSIdentityBinding`, issued by the cluster OIDC issuer. The Identity Binding proxy re-signs this internally for the UAMI → Entra app → target tenant chain.

You add a **second** projected token volume manually:

- **AWS token** — volume name `aws-identity-token`, `aud: sts.amazonaws.com`, issued by the cluster OIDC issuer. AWS STS accepts this directly via `AssumeRoleWithWebIdentity`.

The IB webhook does not interfere with the second volume as long as you do not name it `azure-identity-token`. Kubernetes v1.21+ supports multiple projected service account token volumes with different audiences on the same pod.

The `AWS_WEB_IDENTITY_TOKEN_FILE` and `AWS_ROLE_ARN` env vars are completely separate from `AZURE_*` env vars. The two SDKs do not interact.

---

## Prerequisites

Before starting:

- **The Azure path is already working** — complete `docs/azure-setup.md` and confirm the pod is writing to Azure Blob Storage.
- **AKS cluster OIDC issuer URL** — retrieve it:
  ```bash
  az aks show \
    --resource-group "$RESOURCE_GROUP" \
    --name "$CLUSTER_NAME" \
    --query "oidcIssuerProfile.issuerUrl" -o tsv
  ```
  Save this value. You will use it in Step 1.
- **AWS account access** — you need permissions to create:
  - IAM Identity Providers
  - IAM Roles and policies
- **Target S3 bucket** — already provisioned in the target AWS region. Note the bucket name and region.
- **AWS CLI** installed and authenticated:
  ```bash
  aws --version
  aws sts get-caller-identity
  ```

---

## Variables

Set these values before running the commands below.

```bash
# ============================================================
# AKS cluster (already set if you followed docs/azure-setup.md)
# ============================================================
RESOURCE_GROUP="aks-xtenant-auth-rg"
CLUSTER_NAME="aks-xtenant-auth"

# ============================================================
# AWS configuration
# ============================================================
AWS_REGION="us-east-1"                                      # Region for your S3 bucket
AWS_ACCOUNT_ID="<your-aws-account-id>"                      # 12-digit AWS account ID
IAM_ROLE_NAME="aks-timestampwriter"                          # Name for the new IAM role
S3_BUCKET_NAME="<your-s3-bucket-name>"                      # Target S3 bucket (must already exist)
S3_PREFIX="timestamps/"                                      # Key prefix the pod is allowed to write

# ============================================================
# Derived values (DO NOT EDIT)
# ============================================================
OIDC_ISSUER_URL=""    # Set from az aks show command above
IAM_ROLE_ARN=""       # Set after Step 2
# Strip the https:// prefix for IAM trust policy references
OIDC_PROVIDER=""      # e.g., eastus.oic.prod-aks.azure.com/<tenant-id>/<cluster-id>

# ============================================================
# Option B: Azure AD as OIDC provider (stable cross-cluster identity)
# ============================================================
ENTRA_SOURCE_TENANT_ID="<source-tenant-id>"              # Entra tenant where UAMI lives
UAMI_NAME="<uami-name>"                                   # UAMI resource name
UAMI_RESOURCE_GROUP="<rg>"                                # RG containing the UAMI
AWS_STS_AUDIENCE_APP_CLIENT_ID="<app-client-id>"          # Client ID of the dedicated Entra app
AWS_STS_AUDIENCE_URI="api://aws-sts-audience"             # App ID URI of the dedicated Entra app

# Derived (Option B)
UAMI_OBJECT_ID=""   # Set from: az identity show --query principalId
```

---

## Option A: Per-Cluster OIDC Federation (Simpler Setup)

## Step 1 — Register the AKS OIDC Issuer as an AWS IAM Identity Provider

AWS needs to trust the AKS cluster's OIDC issuer before it will accept tokens from it. This is a one-time registration per cluster OIDC issuer.

### Retrieve the OIDC provider thumbprint

AWS requires the TLS thumbprint of the OIDC issuer's JWKS endpoint. Retrieve it:

```bash
OIDC_ISSUER_URL=$(az aks show \
  --resource-group "$RESOURCE_GROUP" \
  --name "$CLUSTER_NAME" \
  --query "oidcIssuerProfile.issuerUrl" -o tsv)

# Strip trailing slash if present
OIDC_ISSUER_URL="${OIDC_ISSUER_URL%/}"

# Fetch the JWKS URI from the OIDC discovery document
JWKS_URI=$(curl -s "${OIDC_ISSUER_URL}/.well-known/openid-configuration" | jq -r '.jwks_uri')

# Extract the TLS thumbprint from the JWKS endpoint certificate
THUMBPRINT=$(openssl s_client -connect "$(echo $JWKS_URI | sed 's|https://||' | cut -d/ -f1):443" \
  -servername "$(echo $JWKS_URI | sed 's|https://||' | cut -d/ -f1)" \
  </dev/null 2>/dev/null \
  | openssl x509 -fingerprint -noout -sha1 \
  | sed 's/SHA1 Fingerprint=//' \
  | tr -d ':' \
  | tr '[:upper:]' '[:lower:]')

echo "Thumbprint: $THUMBPRINT"
```

### Option A: AWS Console

1. Navigate to **IAM → Identity providers → Add provider**
2. Provider type: **OpenID Connect**
3. Provider URL: paste `$OIDC_ISSUER_URL` (e.g., `https://eastus.oic.prod-aks.azure.com/<tenant-id>/<cluster-id>`)
4. Click **Get thumbprint** — AWS will retrieve it automatically, or paste the `$THUMBPRINT` value from above
5. Audience: `sts.amazonaws.com`
6. Click **Add provider**

### Option B: AWS CLI

```bash
# Strip the https:// prefix for IAM references
OIDC_PROVIDER=$(echo "$OIDC_ISSUER_URL" | sed 's|https://||')

aws iam create-open-id-connect-provider \
  --url "$OIDC_ISSUER_URL" \
  --client-id-list "sts.amazonaws.com" \
  --thumbprint-list "$THUMBPRINT"
```

Save the resulting ARN. It will be in the form:
`arn:aws:iam::<AWS_ACCOUNT_ID>:oidc-provider/<OIDC_PROVIDER>`

> **Note:** One IAM Identity Provider registration per AKS cluster OIDC issuer. If you are deploying to multiple clusters, each cluster needs its own IAM IdP registration. This is the AWS equivalent of the pre-IB federated credential scaling problem — see [Multi-Cluster Considerations](#multi-cluster-considerations) below.

---

## Step 2 — Create the IAM Role with a Web Identity Trust Policy

The IAM role grants the pod permission to call AWS services. The trust policy is the security boundary — it restricts which tokens AWS STS will accept to exactly the service account used by the timestampwriter pod.

### Build the trust policy

```bash
# Set this if not already set
OIDC_PROVIDER=$(echo "$OIDC_ISSUER_URL" | sed 's|https://||')

cat > trust-policy.json <<EOF
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Principal": {
        "Federated": "arn:aws:iam::${AWS_ACCOUNT_ID}:oidc-provider/${OIDC_PROVIDER}"
      },
      "Action": "sts:AssumeRoleWithWebIdentity",
      "Condition": {
        "StringEquals": {
          "${OIDC_PROVIDER}:aud": "sts.amazonaws.com",
          "${OIDC_PROVIDER}:sub": "system:serviceaccount:aks-xtenant-auth:timestampwriter"
        }
      }
    }
  ]
}
EOF
```

The `sub` condition is critical. It binds the role to exactly one Kubernetes service account. Any other pod, service account, or namespace is rejected by AWS STS — even if they obtain a valid cluster-OIDC-issued token.

### Create the role

```bash
aws iam create-role \
  --role-name "$IAM_ROLE_NAME" \
  --assume-role-policy-document file://trust-policy.json \
  --description "AKS timestampwriter pod — S3 write access"

IAM_ROLE_ARN=$(aws iam get-role \
  --role-name "$IAM_ROLE_NAME" \
  --query "Role.Arn" --output text)

echo "IAM Role ARN: $IAM_ROLE_ARN"
```

**Save `$IAM_ROLE_ARN`.** You will add it as an env var in Step 4.

### Attach a least-privilege permissions policy

Do not use `AmazonS3FullAccess`. Scope the policy to a specific bucket and prefix.

```bash
cat > s3-write-policy.json <<EOF
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "s3:PutObject"
      ],
      "Resource": "arn:aws:s3:::${S3_BUCKET_NAME}/${S3_PREFIX}*"
    }
  ]
}
EOF

aws iam put-role-policy \
  --role-name "$IAM_ROLE_NAME" \
  --policy-name "s3-write-timestamps" \
  --policy-document file://s3-write-policy.json
```

> **Note:** `s3:PutObject` on a specific prefix is the minimum required. Do not add `s3:GetObject`, `s3:DeleteObject`, or bucket-level actions unless the application explicitly needs them.

Clean up the temporary policy files when done:
```bash
rm trust-policy.json s3-write-policy.json
```

---

## Step 3 — Update the Kubernetes Deployment

Add a second projected service account token volume to `deploy/deployment.yaml`. This volume carries a cluster-OIDC-issued token with `aud: sts.amazonaws.com` — the audience AWS STS expects.

### Add the volume

In the `volumes:` section of `deploy/deployment.yaml`, add alongside the existing `setup-db` volume:

```yaml
volumes:
  - name: setup-db
    persistentVolumeClaim:
      claimName: setup-db
  # AWS web identity token — projected SA token with sts.amazonaws.com audience.
  # The IB webhook only injects azure-identity-token; this volume is independent.
  - name: aws-identity-token
    projected:
      sources:
        - serviceAccountToken:
            audience: sts.amazonaws.com
            expirationSeconds: 3600
            path: aws-identity-token
```

### Add the volumeMount

In the `timestampwriter` container's `volumeMounts:` section, add:

```yaml
volumeMounts:
  - name: setup-db
    mountPath: /data
    readOnly: true
  # AWS web identity token file
  - name: aws-identity-token
    mountPath: /var/run/secrets/aws
    readOnly: true
```

> **Warning:** Do NOT name this volume `azure-identity-token`. That name is reserved by the AKS Identity Binding webhook. The webhook injects its own projected volume under that name — a name collision will cause the pod to fail to start. Any other name is safe.

> **Note:** The IB webhook injects its own volume and environment variables, but it does not remove or lock any other volumes you declare. Your manually-declared `aws-identity-token` volume is injected into the pod spec as-is.

---

## Step 4 — Add AWS Environment Variables to the Pod

Add three environment variables to the `timestampwriter` container in `deploy/deployment.yaml`. These tell the AWS SDK where to find the token and which role to assume.

```yaml
env:
  # ... existing Azure env vars (AZURE_CLIENT_ID, AZURE_TENANT_ID, SETUP_DB_PATH) ...

  # AWS Web Identity configuration — read by aws-sdk-go-v2 config.LoadDefaultConfig
  - name: AWS_WEB_IDENTITY_TOKEN_FILE
    value: /var/run/secrets/aws/aws-identity-token
  - name: AWS_ROLE_ARN
    value: "<IAM_ROLE_ARN>"    # e.g., arn:aws:iam::123456789012:oidc-provider/...
  - name: AWS_REGION
    value: "<AWS_REGION>"      # e.g., us-east-1
```

Replace `<IAM_ROLE_ARN>` with the ARN captured at the end of Step 2. Replace `<AWS_REGION>` with the region your S3 bucket lives in.

Alternatively, move these values into the `timestampwriter-config` ConfigMap alongside the existing `AZURE_CLIENT_ID` and `AZURE_TENANT_ID` entries, and reference them with `configMapKeyRef`. This keeps the Deployment manifest free of environment-specific values.

> **Note:** `AWS_WEB_IDENTITY_TOKEN_FILE`, `AWS_ROLE_ARN`, and `AWS_REGION` do not conflict with `AZURE_CLIENT_ID`, `AZURE_TENANT_ID`, or `AZURE_FEDERATED_TOKEN_FILE`. The Azure SDK reads only `AZURE_*` variables; the AWS SDK reads only `AWS_*` variables.

---

## Step 5 — Update the Go Application

No code changes are required to the Kubernetes manifests or identity plumbing beyond Steps 3 and 4. The application changes are:

1. **Add the `aws-sdk-go-v2` dependency.** Import `github.com/aws/aws-sdk-go-v2/config` and `github.com/aws/aws-sdk-go-v2/service/s3`. Run `go get` to update `go.mod` and `go.sum`.

2. **Load AWS config with `config.LoadDefaultConfig`.** This function automatically reads `AWS_WEB_IDENTITY_TOKEN_FILE` and `AWS_ROLE_ARN` from the environment and calls `AssumeRoleWithWebIdentity` to obtain short-lived credentials. No manual credential handling is needed.

3. **Create an S3 client** from the loaded config and add an upload call alongside the existing Azure blob write on each tick. The S3 key should include a timestamp or UUID to avoid overwriting previous entries.

4. **Extend storage configuration.** The S3 bucket name and prefix are storage-target configuration, not secrets. They should be stored and read the same way as the Azure storage account name and container name — either via the setup wizard (which would need a new "AWS target" step added to the `cmd/setup/` wizard flow) or via additional env vars added to the `timestampwriter-config` ConfigMap.

The existing `azblob` write path does not change. Both writes happen in the same tick loop, independently. Either write can fail without blocking the other — handle errors from each independently.

---

## Option B: Azure AD as OIDC Provider (Stable Cross-Cluster Identity)

### The N-IdP Scaling Problem

Each AKS cluster has a unique OIDC issuer URL (`https://eastus.oic.prod-aks.azure.com/<tenant-id>/<cluster-id>`). AWS requires a separate IAM Identity Provider registration for each unique issuer. With Option A:

- 1 cluster → 1 IAM IdP registration
- N clusters → N IAM IdP registrations (manual work each time, no AWS equivalent of Identity Bindings)

Option B eliminates this by registering `https://login.microsoftonline.com/<ENTRA_SOURCE_TENANT_ID>/v2.0` as the single AWS IAM Identity Provider. That endpoint is stable — it does not change across clusters. Result: **1 IAM IdP registration for all clusters, forever**.

### Why This Works: The Token Exchange Chain

When the AKS pod uses the Identity Binding mechanism, the IB proxy internally exchanges the cluster-OIDC-issued `azure-identity-token` for a UAMI access token. That UAMI access token is issued by `https://login.microsoftonline.com/<ENTRA_SOURCE_TENANT_ID>/v2.0`, with `sub` = the UAMI's Object ID — stable across all clusters because the UAMI is the same identity everywhere.

The application then exchanges that UAMI token for a dedicated-audience Azure AD JWT, which is presented to AWS STS:

```
cluster OIDC token
    ↓  (IB proxy re-signs)
UAMI access token  [iss: login.microsoftonline.com/<tenantId>/v2.0, sub: <UAMI OID>]
    ↓  (application requests token for dedicated Entra app)
Azure AD JWT       [iss: login.microsoftonline.com/<tenantId>/v2.0, sub: <UAMI OID>, aud: api://aws-sts-audience]
    ↓  (AWS STS AssumeRoleWithWebIdentity)
Short-lived AWS credentials
    ↓
S3 write
```

The `iss` and `sub` in the final JWT are cluster-independent. AWS sees the same issuer and the same subject regardless of which cluster ran the pod.

---

### Step B1 — Create an Entra App Registration for the AWS Token Audience

To avoid coupling AWS authentication to a production Azure resource audience (such as `https://management.azure.com/`), create a dedicated Entra app registration in the **source tenant**. This app exists only to define a stable audience value for the token that will be presented to AWS STS.

#### What to create

- **App name:** `aks-timestampwriter-aws-sts-audience` (or any descriptive name)
- **Application ID URI (App ID URI):** `api://aws-sts-audience` (this becomes the `aud` claim in the JWT)
- **No redirect URIs** — this app is not used for delegated auth
- **No API permissions** — no MS Graph or Azure delegated permissions needed
- **No service principal in any target tenant** — source tenant only

#### Azure CLI

```bash
# Create the app registration
APP_ID=$(az ad app create \
  --display-name "aks-timestampwriter-aws-sts-audience" \
  --query "appId" -o tsv)

echo "App Client ID: $APP_ID"
AWS_STS_AUDIENCE_APP_CLIENT_ID="$APP_ID"

# Set the Application ID URI
az ad app update \
  --id "$APP_ID" \
  --identifier-uris "api://aws-sts-audience"
```

#### Grant the UAMI permission to request tokens for this app

The UAMI (a managed identity service principal) must be authorized to request tokens scoped to this app. Assign an app role:

```bash
# Retrieve the UAMI's service principal object ID
UAMI_SP_OBJECT_ID=$(az ad sp list \
  --filter "displayName eq '$UAMI_NAME'" \
  --query "[0].id" -o tsv)

# Expose a default app role on the Entra app (required for app role assignment)
# Add a role definition to the app manifest, then assign it to the UAMI SP.
# The simplest approach: add a role with value ".default" or any string,
# then assign it via the service principal:
az ad app update \
  --id "$APP_ID" \
  --app-roles '[{"id":"00000000-0000-0000-0000-000000000001","allowedMemberTypes":["Application"],"description":"AWS STS token audience","displayName":"AWS STS Audience","isEnabled":true,"value":"aws-sts-audience"}]'

# Get the app's service principal ID
APP_SP_ID=$(az ad sp list \
  --filter "appId eq '$APP_ID'" \
  --query "[0].id" -o tsv)

# Assign the app role to the UAMI service principal
az rest --method POST \
  --uri "https://graph.microsoft.com/v1.0/servicePrincipals/$APP_SP_ID/appRoleAssignedTo" \
  --body "{\"principalId\":\"$UAMI_SP_OBJECT_ID\",\"resourceId\":\"$APP_SP_ID\",\"appRoleId\":\"00000000-0000-0000-0000-000000000001\"}"
```

Save the app client ID and Application ID URI — you will use both in later steps:

```bash
echo "AWS_STS_AUDIENCE_APP_CLIENT_ID: $AWS_STS_AUDIENCE_APP_CLIENT_ID"
echo "AWS_STS_AUDIENCE_URI: api://aws-sts-audience"
```

---

### Step B2 — Register Azure AD OIDC Issuer as AWS IAM Identity Provider

Register `https://login.microsoftonline.com/<ENTRA_SOURCE_TENANT_ID>/v2.0` as an IAM Identity Provider. This is a **one-time operation** that covers all clusters using the same source tenant and UAMI.

#### Retrieve the thumbprint

```bash
AZURE_AD_ISSUER="https://login.microsoftonline.com/${ENTRA_SOURCE_TENANT_ID}/v2.0"

# Fetch JWKS URI from Azure AD OIDC discovery document
JWKS_URI=$(curl -s "${AZURE_AD_ISSUER}/.well-known/openid-configuration" | jq -r '.jwks_uri')

# Extract the TLS thumbprint
THUMBPRINT=$(openssl s_client \
  -connect "login.microsoftonline.com:443" \
  -servername "login.microsoftonline.com" \
  </dev/null 2>/dev/null \
  | openssl x509 -fingerprint -noout -sha1 \
  | sed 's/SHA1 Fingerprint=//' \
  | tr -d ':' \
  | tr '[:upper:]' '[:lower:]')

echo "Thumbprint: $THUMBPRINT"
```

#### Register the IdP (AWS Console)

1. Navigate to **IAM → Identity providers → Add provider**
2. Provider type: **OpenID Connect**
3. Provider URL: `https://login.microsoftonline.com/<ENTRA_SOURCE_TENANT_ID>/v2.0`
4. Click **Get thumbprint** (or paste `$THUMBPRINT`)
5. Audience: `api://aws-sts-audience` ← must exactly match the App ID URI from Step B1
6. Click **Add provider**

#### Register the IdP (AWS CLI)

```bash
# The audience value must exactly match the aud claim the application will present
aws iam create-open-id-connect-provider \
  --url "https://login.microsoftonline.com/${ENTRA_SOURCE_TENANT_ID}/v2.0" \
  --client-id-list "${AWS_STS_AUDIENCE_URI}" \
  --thumbprint-list "$THUMBPRINT"
```

> **Important:** The audience registered here (`api://aws-sts-audience`) must exactly match the `aud` claim in the JWT the application presents to AWS STS. If they do not match, STS will reject the token with an `InvalidIdentityToken` error.

---

### Step B3 — Create an IAM Role with Azure AD Trust Policy

Create or update the IAM role to trust tokens from the Azure AD OIDC provider. The trust policy pins both the issuer (Azure AD) and the subject (UAMI Object ID).

#### Retrieve the UAMI Object ID

The trust policy `sub` condition must be the UAMI's **Object ID** (also called `principalId` in Azure CLI output) — this is the service principal object ID in the directory, **not** the client ID (application ID).

```bash
UAMI_OBJECT_ID=$(az identity show \
  --resource-group "$UAMI_RESOURCE_GROUP" \
  --name "$UAMI_NAME" \
  --query "principalId" -o tsv)

echo "UAMI Object ID (sub): $UAMI_OBJECT_ID"
```

#### Build the trust policy

```bash
cat > trust-policy-optb.json <<EOF
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Principal": {
        "Federated": "arn:aws:iam::${AWS_ACCOUNT_ID}:oidc-provider/login.microsoftonline.com/${ENTRA_SOURCE_TENANT_ID}/v2.0"
      },
      "Action": "sts:AssumeRoleWithWebIdentity",
      "Condition": {
        "StringEquals": {
          "login.microsoftonline.com/${ENTRA_SOURCE_TENANT_ID}/v2.0:aud": "${AWS_STS_AUDIENCE_URI}",
          "login.microsoftonline.com/${ENTRA_SOURCE_TENANT_ID}/v2.0:sub": "${UAMI_OBJECT_ID}"
        }
      }
    }
  ]
}
EOF
```

> **Critical:** The `sub` condition uses the UAMI Object ID (from `principalId`), not the UAMI client ID (from `clientId`). These are different values. Using the client ID here will cause STS to always reject the token — the `sub` claim in a UAMI access token is the Object ID.

#### Create the role

```bash
aws iam create-role \
  --role-name "$IAM_ROLE_NAME" \
  --assume-role-policy-document file://trust-policy-optb.json \
  --description "AKS timestampwriter pod — S3 write via Azure AD OIDC (stable cross-cluster)"

IAM_ROLE_ARN=$(aws iam get-role \
  --role-name "$IAM_ROLE_NAME" \
  --query "Role.Arn" --output text)

echo "IAM Role ARN: $IAM_ROLE_ARN"
```

Attach the same least-privilege S3 permissions policy from Option A (see Step 2 above). Then clean up:

```bash
rm trust-policy-optb.json
```

---

### Step B4 — Update the Kubernetes Deployment

Option B does **not** require a manually-projected `aws-identity-token` volume. The `azure-identity-token` injected by the IB webhook is the starting point for both the Azure Blob write and the AWS credential exchange. Remove or omit the `aws-identity-token` volume and volumeMount entirely.

#### Remove from `deploy/deployment.yaml` (if migrating from Option A)

- The `aws-identity-token` projected volume declaration
- The `aws-identity-token` volumeMount
- The `AWS_WEB_IDENTITY_TOKEN_FILE` env var

#### Keep or add

```yaml
env:
  # ... existing Azure env vars (AZURE_CLIENT_ID, AZURE_TENANT_ID, SETUP_DB_PATH) ...

  # AWS role ARN — still required by the explicit STS call in application code
  - name: AWS_ROLE_ARN
    value: "<IAM_ROLE_ARN>"           # e.g., arn:aws:iam::123456789012:role/aks-timestampwriter

  # AWS region — still required for S3 client construction
  - name: AWS_REGION
    value: "<AWS_REGION>"             # e.g., us-east-1

  # Client ID of the dedicated Entra app — tells application code which audience to request
  - name: AWS_STS_AUDIENCE_APP_ID
    value: "<AWS_STS_AUDIENCE_APP_CLIENT_ID>"  # Client ID GUID of the app from Step B1
```

> **Note:** `AWS_WEB_IDENTITY_TOKEN_FILE` is **not set** in Option B. The AWS SDK's built-in web identity token file flow is not used. The application code calls STS explicitly (see Step B5).

---

### Step B5 — Go Application Changes

Option B requires explicit credential exchange in the application code. Instead of relying on the AWS SDK's automatic `AWS_WEB_IDENTITY_TOKEN_FILE` detection, the application must:

1. **Acquire a UAMI token for the Entra app audience.** Use `azidentity.ManagedIdentityCredential` (or `DefaultAzureCredential`) to request an access token with the resource set to the AWS audience app's client ID (the value of `AWS_STS_AUDIENCE_APP_ID`). The resulting JWT will have `aud: api://aws-sts-audience` (or the App ID URI), `iss: https://login.microsoftonline.com/<tenantId>/v2.0`, and `sub: <UAMI Object ID>`.

2. **Call `sts.AssumeRoleWithWebIdentity` explicitly.** Pass the Azure AD JWT as the `WebIdentityToken` parameter, and `AWS_ROLE_ARN` as the `RoleArn`. The `RoleSessionName` can be any descriptive string (e.g., `"aks-timestampwriter"`).

3. **Build a custom credentials provider** from the STS response. The `AssumeRoleWithWebIdentity` response contains `AccessKeyId`, `SecretAccessKey`, and `SessionToken`. Wrap these in an `aws.CredentialsProvider` and pass it to the AWS config.

4. **Construct the S3 client** using the config loaded with the custom credentials provider. Proceed with the S3 write using the same `s3:PutObject` call pattern as Option A.

The token acquired in step 1 has a finite lifetime. In a long-running pod, refresh logic is required: either re-acquire the Azure AD token before each AWS STS call, or refresh proactively before the STS credentials expire. The `ManagedIdentityCredential` caches and auto-refreshes the underlying UAMI token; calling `GetToken` before each STS exchange is the simplest correct approach.

This is more code than Option A's zero-configuration flow, but it trades that complexity for the stable multi-cluster identity guarantee.

---

### Verification (Option B)

After applying the updated Deployment, confirm both paths are working:

```bash
# Watch timestampwriter logs for Azure and AWS write confirmations
kubectl logs -n aks-xtenant-auth -l app=timestampwriter -f

# Confirm objects appear in S3
aws s3 ls "s3://${S3_BUCKET_NAME}/${S3_PREFIX}" --region "$AWS_REGION"
```

If the AWS write fails under Option B, check:

1. **Entra app client ID is correct:** `AWS_STS_AUDIENCE_APP_ID` must match the app registration created in Step B1.
2. **App ID URI matches the registered audience:** The `aud` in the JWT the application requests must exactly match the audience registered in the IAM IdP (Step B2). Check both with `jwt.io` or `az rest` against the token.
3. **UAMI Object ID matches trust policy `sub`:** The `sub` in the JWT is the UAMI Object ID (`principalId` from `az identity show`). If the trust policy uses the client ID instead, STS will reject every token.
4. **App role assignment exists:** The UAMI service principal must have the app role assignment on the Entra app. Without it, Azure AD will return an error when the application requests the token.
5. **IAM IdP issuer URL is exact:** Must be `https://login.microsoftonline.com/<ENTRA_SOURCE_TENANT_ID>/v2.0` with the correct tenant ID — no trailing slash.

---

## Multi-Cluster Considerations

### Azure path (no new work)

The Identity Binding OIDC issuer is stable per UAMI across all clusters. One FIC on the Entra app covers all clusters using the same UAMI. Adding a new AKS cluster to the Azure path requires only creating a new Identity Binding resource pointing to the existing UAMI — no FIC changes.

### AWS path — Option A (N clusters = N IAM IdP registrations)

Each AKS cluster has its own OIDC issuer URL. AWS requires a separate IAM Identity Provider registration for each unique OIDC issuer. There is no AWS equivalent of Identity Bindings — no single stable issuer URL shared across clusters.

This means:
- 1 cluster → 1 IAM IdP registration
- 5 clusters → 5 IAM IdP registrations (one per cluster)
- The trust policy's `Federated` ARN must reference the correct IdP for each cluster

> **Note:** The IAM Role itself does not need to change per cluster. The same role can be referenced by trust policies from multiple IdPs. However, each trust policy statement must use the correct `${OIDC_PROVIDER}:sub` condition for the cluster it covers.

**Operational recommendation:** Use a Terraform module or AWS CloudFormation to automate IAM IdP registration for each new cluster. Attempting to manage this manually at scale is operationally dangerous — a missing IdP registration silently breaks the AWS write path for that cluster. Consider AWS Organizations SCPs to enforce that all registered clusters follow the same trust policy structure.

### AWS path — Option B (1 IAM IdP registration for all clusters)

Option B eliminates the N-IAM-IdP scaling problem entirely. Because the registered OIDC provider is `https://login.microsoftonline.com/<ENTRA_SOURCE_TENANT_ID>/v2.0` — a stable Azure AD endpoint — and the trust policy `sub` is pinned to the UAMI Object ID (not a cluster-specific service account), adding a new AKS cluster requires:

- Creating the Identity Binding resource for the new cluster (same as the Azure path — see `docs/azure-setup.md`)
- **No AWS changes required** — the IAM IdP, IAM role, and trust policy already cover the UAMI, which is cluster-independent

This is exactly analogous to how Identity Bindings make the Azure path cluster-independent (one FIC on the Entra app regardless of cluster count). The setup cost is paid once (Steps B1–B3 above). Every subsequent cluster is free from an AWS perspective.

**If you are deploying to more than two or three clusters**, Option B is the recommended path. The Entra app registration from Step B1 is a one-time, zero-maintenance Azure resource.

---

## Security Considerations

> **See also:** `docs/security.md` for the full threat model and operator checklist for the Azure path. The considerations below are additive — specific to the expanded blast radius from adding AWS access.

### Expanded blast radius

Before this change, a pod compromise yields write access to one Azure Blob Storage container in one target tenant. After this change, the same pod compromise also yields the IAM role — and whatever that role permits.

The practical impact depends entirely on how tightly scoped the IAM role is. A role restricted to `s3:PutObject` on a specific prefix in one bucket has limited additional damage potential. A role with `s3:*` on `*` is catastrophic. Scope the role to the minimum required (Step 2 shows the correct example).

### Both token files are co-located in the pod

The `azure-identity-token` and `aws-identity-token` files are both mounted inside the same pod. If a process achieves arbitrary read inside the pod's filesystem, it can access both. A co-located read vulnerability is harder to exploit into meaningful credential theft for Azure (token exchange still required), but the AWS token is directly usable with `AssumeRoleWithWebIdentity` — no further exchange needed.

If the Azure role is tightly scoped to a single container and the AWS role is broader, that asymmetry is a real risk. Ensure the AWS role's scope matches or is tighter than the Azure scope.

### Separate pods for maximum isolation

If the Azure-target and AWS-target workloads are genuinely independent, consider splitting them into separate pods:

- Pod A: Azure-only. IB token only. No AWS env vars or volumes.
- Pod B: AWS-only. `aws-identity-token` volume only. No IB annotation or Azure env vars.

This eliminates the co-located token risk entirely. The trade-off is operational complexity: two deployments, two images (or a multi-binary image), two scaling configurations.

The single-pod design in this guide is appropriate for a demonstration or low-sensitivity workload where operational simplicity outweighs isolation benefits.

### Network policy

No changes to `deploy/networkpolicy.yaml` are required. The existing policy already allows HTTPS egress on port 443 to all destinations. AWS STS (`sts.amazonaws.com`) and S3 (`s3.<region>.amazonaws.com`) are both reachable over port 443. The IMDS block (`169.254.169.254`) does not affect AWS STS or S3 — those are public endpoints, not instance metadata.

If you want to tighten egress further, you could add explicit `ipBlock` allow rules for AWS STS and S3 IP ranges (published by AWS in their IP ranges JSON). This is belt-and-suspenders for a workload where the existing policy already restricts all non-443 egress.

---

## Verification

After applying the updated Deployment, confirm the AWS write path is working:

```bash
# Watch timestampwriter logs for AWS write confirmations
kubectl logs -n aks-xtenant-auth -l app=timestampwriter -f

# Confirm objects appear in S3
aws s3 ls "s3://${S3_BUCKET_NAME}/${S3_PREFIX}" --region "$AWS_REGION"
```

If the AWS write fails, check:

1. **Token file is present:** `kubectl exec -n aks-xtenant-auth <pod> -- ls -la /var/run/secrets/aws/`
2. **IAM IdP audience matches:** The projected volume must use `aud: sts.amazonaws.com` (exactly as specified in Step 3).
3. **Trust policy `sub` claim matches:** The IAM trust policy condition must be `system:serviceaccount:aks-xtenant-auth:timestampwriter` (exact namespace and service account name).
4. **Role ARN is correct:** `AWS_ROLE_ARN` env var must exactly match the ARN returned in Step 2.
5. **OIDC issuer URL registered correctly:** The IAM IdP URL must match the cluster OIDC issuer exactly, including any path segments and without a trailing slash.
