# AWS Setup Guide: Dual-Cloud Write Path from AKS

This guide extends the cross-tenant Azure write path with an AWS write path. The same AKS pod that writes timestamps to Azure Blob Storage (source → target tenant) also writes to an AWS S3 bucket — both from a single pod, with no secrets stored anywhere.

**Authentication flows through:**
1. AKS pod → projected service account token (`aud: sts.amazonaws.com`, cluster OIDC issuer)
2. AWS STS `AssumeRoleWithWebIdentity` → short-lived AWS credentials
3. IAM Role with least-privilege S3 policy → S3 bucket write

The Azure path is unaffected. Both paths run concurrently inside the same pod. The two sets of credentials are completely independent.

**Prerequisite:** Complete `docs/azure-setup.md` first. The Azure path must already be working before adding the AWS path.

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
```

---

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

## Multi-Cluster Considerations

### Azure path (no new work)

The Identity Binding OIDC issuer is stable per UAMI across all clusters. One FIC on the Entra app covers all clusters using the same UAMI. Adding a new AKS cluster to the Azure path requires only creating a new Identity Binding resource pointing to the existing UAMI — no FIC changes.

### AWS path (N clusters = N IAM IdP registrations)

Each AKS cluster has its own OIDC issuer URL. AWS requires a separate IAM Identity Provider registration for each unique OIDC issuer. There is no AWS equivalent of Identity Bindings — no single stable issuer URL shared across clusters.

This means:
- 1 cluster → 1 IAM IdP registration
- 5 clusters → 5 IAM IdP registrations (one per cluster)
- The trust policy's `Federated` ARN must reference the correct IdP for each cluster

> **Note:** The IAM Role itself does not need to change per cluster. The same role can be referenced by trust policies from multiple IdPs. However, each trust policy statement must use the correct `${OIDC_PROVIDER}:sub` condition for the cluster it covers.

**Operational recommendation:** Use a Terraform module or AWS CloudFormation to automate IAM IdP registration for each new cluster. Attempting to manage this manually at scale is operationally dangerous — a missing IdP registration silently breaks the AWS write path for that cluster. Consider AWS Organizations SCPs to enforce that all registered clusters follow the same trust policy structure.

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
