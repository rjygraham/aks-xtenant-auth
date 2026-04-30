# Security Mitigations

This document explains how this solution mitigates the confused deputy problem and
other attack vectors relevant to cross-tenant AKS workload identity. It is intended
for security reviewers evaluating the design.

For step-by-step setup of each control referenced here, see
[`docs/azure-setup.md`](azure-setup.md).

---

## 1. Threat Model Overview

This solution spans two Azure tenants and three trust boundaries.

```
SOURCE TENANT
  ┌─────────────────────────────────────────────────┐
  │  AKS Cluster                                    │
  │  ├─ UAMI  (Identity Bindings principal)         │
  │  │   └─ IB OIDC issuer: https://ib.oic.prod-   │
  │  │        aks.azure.com/<tenant>/<uami-client-id>│
  │  │                                              │
  │  └─ Entra App Registration (multi-tenant)       │
  │      client_id: <APP_CLIENT_ID>                 │
  │      FIC issuer:  IB OIDC issuer (above)        │
  │      FIC subject: system:serviceaccount:        │
  │                   aks-xtenant-auth:timestampwriter│
  │      FIC audience: api://AKSIdentityBinding     │
  └─────────────────────────────────────────────────┘
            │  OBO token exchange
            ▼  (Entra ID)
TARGET TENANT
  ┌─────────────────────────────────────────────────┐
  │  Service Principal (for Entra app above)        │
  │  RBAC: Storage Blob Data Contributor            │
  │         scoped to: <storage-account>/containers/│
  │                    <container-name>  ONLY       │
  │                                                 │
  │  Storage Account                                │
  │   allowSharedKeyAccess:  false                  │
  │   allowBlobPublicAccess: false                  │
  │   minTlsVersion:         TLS1_2                 │
  └─────────────────────────────────────────────────┘
```

**Token exchange path:**

1. The AKS Identity Bindings webhook performs a Kubernetes `SubjectAccessReview` to
   confirm the pod's ServiceAccount (`system:serviceaccount:aks-xtenant-auth:timestampwriter`)
   holds the `use-managed-identity` verb on the UAMI resource. If the check passes,
   the webhook projects a service account token with audience `api://AKSIdentityBinding`
   into the pod at `AZURE_FEDERATED_TOKEN_FILE`.

2. `azidentity.NewWorkloadIdentityCredential` (in `cmd/timestampwriter/main.go`) reads
   that token and presents it to Entra ID as a federated credential assertion on behalf
   of the **multi-tenant Entra app** (not the UAMI directly — `AZURE_CLIENT_ID` in the
   Deployment is overridden to the Entra app's client ID).

3. Entra ID validates the assertion against the FIC on the Entra app (issuer, subject,
   audience must all match). On success it issues an access token scoped to the target
   tenant's storage account.

4. The Azure Blob SDK uses that access token to write blobs to the target container.

---

## 2. Confused Deputy Mitigation

### The threat

A confused deputy attack in this context means: a malicious actor tricks the
`timestampwriter` pod into performing storage writes on their behalf by presenting a
token that was intended for a different purpose — e.g. by substituting an IMDS token
(which the pod's UAMI could acquire from `169.254.169.254`) for the IB-projected
service account token, or by causing the pod to present credentials derived from a
different ServiceAccount.

The attack requires breaking the identity chain at one of three independent enforcement
points.

### Layer 1 — Identity Bindings RBAC checkpoint

Before projecting the `api://AKSIdentityBinding` token into any pod, the IB webhook
performs a `SubjectAccessReview` against the Kubernetes API server:

> "Does `system:serviceaccount:aks-xtenant-auth:timestampwriter` hold the
> `use-managed-identity` verb on `cid.wi.aks.azure.com/<UAMI_CLIENT_ID>`?"

This check passes **only** because of the explicit `ClusterRole` + `ClusterRoleBinding`
in [`deploy/clusterrole.yaml`](../deploy/clusterrole.yaml) and
[`deploy/clusterrolebinding.yaml`](../deploy/clusterrolebinding.yaml). No other
ServiceAccount in the cluster has this permission.

Consequence: a pod running under any other ServiceAccount — or the same SA in a
different namespace — never receives the IB-projected token.

### Layer 2 — Federated Identity Credential subject constraint

The FIC on the Entra app is locked to an exact subject:

```
system:serviceaccount:aks-xtenant-auth:timestampwriter
```

Entra ID enforces this during token exchange. A token presented with any other
`sub` claim — even one with the correct issuer and audience — is rejected with
`AADSTS70021` (no matching FIC found).

This means even if an attacker somehow obtained the projected token file
(see §3b below), they cannot exchange it unless they can also make Entra ID believe
the presenting entity is `system:serviceaccount:aks-xtenant-auth:timestampwriter`.
That claim is set by the Kubernetes control plane when the token is minted; it cannot
be forged by the application.

### Layer 3 — Audience scoping

The IB-projected token carries audience `api://AKSIdentityBinding`. The FIC on the
Entra app accepts **only** this audience. Two substitution attacks are therefore closed:

- **IMDS token substitution:** The UAMI's IMDS token has audience
  `https://management.azure.com/` (or the resource-specific audience requested). It
  cannot be used as the `client_assertion` in the FIC exchange because the audience
  value does not match.

- **Standard workload identity token substitution:** A plain AKS workload identity
  token (without Identity Bindings) would carry audience `api://AzureADTokenExchange`
  from the cluster's own OIDC issuer, not the IB OIDC issuer. Both the issuer and
  audience would fail the FIC match.

### Why simultaneous bypass is required

All three layers are independent enforcement mechanisms operated by different systems
(Kubernetes RBAC, Entra ID FIC validation, token audience). An attacker must bypass
all three simultaneously:

1. Obtain a token with the `api://AKSIdentityBinding` audience — requires the IB
   webhook to project it, which requires passing the K8s `SubjectAccessReview`.
2. Have that token carry `sub: system:serviceaccount:aks-xtenant-auth:timestampwriter`
   — requires the pod to actually run under that ServiceAccount in that namespace.
3. Present it to the correct FIC issuer endpoint — the IB OIDC issuer, not the
   cluster's own OIDC issuer.

In practice, an attacker who satisfies all three constraints has achieved the
equivalent of cluster-admin access plus namespace pod-creation rights. That level of
compromise is treated as a residual risk (see §4).

---

## 3. Additional Mitigations

### a. IMDS Token Theft

**Threat:** Any pod running on the same node can query the Azure Instance Metadata
Service (`169.254.169.254`) and retrieve a bearer token for the node's UAMI.

**Mitigation — wrong audience:** An IMDS token has the wrong audience and the wrong
issuer for the IB OIDC exchange path. Even if an attacker obtains the raw token, it
cannot be substituted into the `client_assertion` field used by the FIC exchange
(Layer 3 above).

**Mitigation — network egress block:** [`deploy/networkpolicy.yaml`](../deploy/networkpolicy.yaml)
explicitly excludes `169.254.169.254/32` from the allowed HTTPS egress CIDR for all
pods in the `aks-xtenant-auth` namespace. Both `timestampwriter` and `setup` pods
cannot reach IMDS at the network level.

```yaml
# From deploy/networkpolicy.yaml
- to:
    - ipBlock:
        cidr: 0.0.0.0/0
        except:
          - 169.254.169.254/32
  ports:
    - protocol: TCP
      port: 443
```

### b. Token Theft via Projected Volume

**Threat:** A compromised container or a privileged pod on the same node could read
the projected service account token file at `AZURE_FEDERATED_TOKEN_FILE`. The file
contains a signed JWT with audience `api://AKSIdentityBinding`.

**Mitigation — FIC subject constraint:** The stolen token's `sub` claim is
`system:serviceaccount:aks-xtenant-auth:timestampwriter`. For it to be useful the
attacker must also be able to present it as that subject to Entra ID's FIC exchange
endpoint. Since the claim is minted by the Kubernetes control plane and the FIC
enforces an exact match, the token is only useful to something that **is** the
timestampwriter ServiceAccount — which is precisely the entity the token belongs to.

**Mitigation — short TTL:** The projected token should be configured with a short
expiry using `serviceAccountTokenExpirationSeconds: 600` (5 minutes). A stolen token
becomes invalid before a meaningful attack can be staged. Verify this in the
`ServiceAccount` manifest or the projected volume spec.

### c. Namespace Escape / ServiceAccount Impersonation

**Threat:** An attacker who obtains write access to the `aks-xtenant-auth` namespace
can create a new pod specifying `serviceAccountName: timestampwriter`. That pod will
receive the IB-projected token and satisfy all three identity layers.

**Honest assessment:** This **is** a viable attack path if an attacker gains namespace
write access. There is no Kubernetes primitive to restrict which pods may use a given
ServiceAccount within a namespace — the ServiceAccount is the RBAC boundary.

**Mitigations:**

- Restrict pod creation RBAC in the `aks-xtenant-auth` namespace to the deployment
  pipeline identity only. No developer or operator should have `pods/create` in this
  namespace outside of automated deployment tooling.
- Container-scoped RBAC in the target tenant (see §3f) limits the blast radius to
  writes on a single named container — not the full storage account.
- See [`docs/azure-setup.md`](azure-setup.md) for the namespace RBAC guidance.

### d. SQLite Configuration Injection

**Threat:** An attacker with write access to the NFS share hosting `setup.db` could
insert a malicious `consents` row pointing `resource_id` or `container_name` at an
attacker-controlled storage endpoint, redirecting all blob writes.

**Mitigation — NFS access control:** The Azure Files NFS share is restricted to the
cluster's VNet subnet. There is no key-based authentication path — access requires
network adjacency and valid Azure identity. An attacker without cluster access cannot
write to the share.

**Mitigation — input validation:** Even if a row were injected, the `timestampwriter`
validates both fields before constructing any SDK call:

- Storage account name is extracted from the ARM resource ID and validated against
  `^[a-z0-9]{3,24}$` (see `parseStorageAccountName` in `cmd/timestampwriter/main.go`).
- Container name is validated against `^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`.

A malformed or path-traversal value is rejected before reaching the Azure SDK.

### e. Setup Wizard Abuse

**Threat:** Re-running the setup wizard (e.g. by a rogue operator or by accident)
could silently replace the production `consents` row, redirecting all subsequent blob
writes to a different storage account without warning.

**Mitigation — one-time write guard:** `handleConfigure` in `cmd/setup/main.go`
checks the `consents` table row count before every `INSERT`. If any row already
exists, the handler returns an error and refuses to write:

```go
// From cmd/setup/main.go — handleConfigure
if count > 0 {
    renderTemplate(w, tmpl, "error.html", errorData{
        Error:            "already_configured",
        ErrorDescription: "A storage configuration already exists. To reconfigure,
            delete the existing row from the database (setup.db) and restart the
            setup wizard.",
    })
    return
}
```

Reconfiguration requires explicit manual action (deleting the DB row), making
accidental override impossible.

**Mitigation — access control:** The setup wizard is exposed only via
`kubectl port-forward`, which requires authenticated access to the Kubernetes API
server. There is no ClusterIP or LoadBalancer route to the pod.

### f. Cross-Tenant Blast Radius Control

**Threat:** If the Entra app's service principal in the target tenant is compromised
(e.g. via RBAC misconfiguration or token exfiltration), an attacker could read or
write blobs across the entire target storage account.

**Mitigation — container-scoped RBAC:** The `Storage Blob Data Contributor` role
assignment in the target tenant is scoped to the **specific container**, not to the
storage account or resource group. An attacker with access to the SP's token can only
write to that one container.

```
/subscriptions/{sub}/resourceGroups/{rg}/providers/Microsoft.Storage/
  storageAccounts/{account}/blobServices/default/containers/{container}
```

**Mitigation — storage account hardening:** The target storage account is configured
with:

| Setting | Value |
|---------|-------|
| `allowSharedKeyAccess` | `false` — disables account key auth; Entra ID required |
| `allowBlobPublicAccess` | `false` — no anonymous reads |
| `minTlsVersion` | `TLS1_2` — rejects legacy TLS negotiation |

See [`docs/azure-setup.md`](azure-setup.md) for the exact `az storage account update`
commands.

---

## 4. What This Solution Does NOT Protect Against

Be aware of the following residual risks:

**Cluster-admin compromise.** A cluster administrator can read any projected token
file on any node, bind any pod to any ServiceAccount, or modify the ClusterRole and
ClusterRoleBinding. All three identity layers can be defeated by cluster-admin. If the
cluster itself is compromised, this solution provides no additional protection.

**Source-tenant identity compromise.** If the Entra app registration or the UAMI in
the source tenant is compromised (e.g. credentials leaked, app registration hijacked),
all bets are off. The entire cross-tenant identity chain begins with the integrity of
these objects.

**Target-tenant Global Admin.** A Global Admin or Privileged Role Administrator in
the target tenant can add new RBAC assignments to the storage account, create
additional service principals for the Entra app, or remove existing constraints. This
solution does not prevent actions taken by the target tenant's own privileged
administrators.

**Namespace write access.** As noted in §3c, an attacker with `pods/create` in the
`aks-xtenant-auth` namespace can impersonate the `timestampwriter` ServiceAccount.
Restricting that permission to the deployment pipeline is an operational constraint,
not a technical enforcement.

---

## 5. Security Checklist

Quick-reference for operators deploying this sample. Verify each control before
treating the deployment as production-ready.

| Control | What to verify |
|---------|---------------|
| IB RBAC | `ClusterRole` grants `use-managed-identity` only on the exact UAMI client ID; `ClusterRoleBinding` subjects only `system:serviceaccount:aks-xtenant-auth:timestampwriter` |
| FIC audience | `api://AKSIdentityBinding` (NOT `api://AzureADTokenExchange`) |
| FIC issuer | IB OIDC issuer: `https://ib.oic.prod-aks.azure.com/<tenant-id>/<uami-client-id>/` |
| FIC subject | Exact match: `system:serviceaccount:aks-xtenant-auth:timestampwriter` |
| RBAC scope | Container-level, not storage-account or resource-group level |
| Storage hardening | `allowSharedKeyAccess: false`, `allowBlobPublicAccess: false`, `minTlsVersion: TLS1_2` |
| NetworkPolicy | Applied; `169.254.169.254/32` excluded from HTTPS egress on both pods |
| Namespace RBAC | Pod creation in `aks-xtenant-auth` restricted to deployment pipeline identity only |
| Setup wizard access | No LoadBalancer or ClusterIP ingress; accessible only via `kubectl port-forward` |
| Setup wizard guard | One-time write guard active; re-run rejected if `consents` table is non-empty |
| Token TTL | `serviceAccountTokenExpirationSeconds: 600` (or equivalent short TTL) set on projected volume |

**Cross-references:**
- [`deploy/clusterrole.yaml`](../deploy/clusterrole.yaml) — IB RBAC
- [`deploy/clusterrolebinding.yaml`](../deploy/clusterrolebinding.yaml) — SA binding
- [`deploy/networkpolicy.yaml`](../deploy/networkpolicy.yaml) — IMDS egress block
- [`cmd/timestampwriter/main.go`](../cmd/timestampwriter/main.go) — token credential setup and input validation
- [`cmd/setup/main.go`](../cmd/setup/main.go) — one-time write guard (`handleConfigure`)
- [`docs/azure-setup.md`](azure-setup.md) — step-by-step setup for all controls above
