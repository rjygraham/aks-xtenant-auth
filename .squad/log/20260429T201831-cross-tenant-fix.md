# Session Log: Cross-Tenant Fix — 2026-04-29T20:18:31

## Summary

Cross-tenant blob write issue resolved. Pod now successfully writing blobs to target tenant storage account.

## What Changed

### ConfigMap (`deploy/configmap.yaml`)
- Added `AZURE_TENANT_ID: "c8c22f13-9f4d-41a5-b619-eda5a1d50c39"` (target tenant ID)

### Deployment (`deploy/deployment.yaml`)
- Added explicit `AZURE_TENANT_ID` env entry sourcing from ConfigMap
- Placed before webhook injection point so ConfigMap value wins

## Why This Works

AKS workload identity webhook:
1. Always injects `AZURE_TENANT_ID` as source tenant
2. Respects pre-existing env vars (skips injection if already set)
3. ConfigMap env entry is set first, so webhook sees it's already set

Azure SDK (azidentity) uses `AZURE_TENANT_ID` to scope the token to the correct tenant. By overriding to the target tenant ID, the SDK now requests tokens valid in the target tenant.

## Verification

- ✓ SP/RBAC assignments existed in target tenant (no additional setup needed)
- ✓ ConfigMap patched live in cluster
- ✓ Pod restarted
- ✓ Pod logs show successful blob writes:
  - `{"level":"INFO","msg":"storage account configured","account":"https://rgxtenantsa.blob.core.windows.net/","container":"xtenant"}`
  - `{"level":"INFO","msg":"blob written","timestamp":"2026-04-30T00:18:52Z"}`
- ✓ No `InvalidAuthenticationInfo` or `AADSTS` errors

## Outcome

Cross-tenant workload identity flow fully functional. Pod writing blobs to target tenant storage account successfully. No code changes required — manifests-only solution.
