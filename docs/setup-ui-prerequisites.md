# Setup Wizard Prerequisites — Target Tenant Admin

**Who You Are:** You are the target tenant's storage account owner or Azure administrator. The source tenant team has built a containerized application that needs to write to your Azure Blob Storage account.

**What You'll Do:** Use a web-based setup wizard to grant the source application permission to write to your storage account. **No CLI commands required.**

---

## Before You Start

Verify you have access to:

### Azure Permissions (in your tenant)
- **Global Administrator** or **Application Administrator** role in Microsoft Entra ID
  - Required to grant admin consent to the source application
- **Owner** or **User Access Administrator** role on your storage account
  - Required to assign RBAC roles

### Azure Portal Information (gather these)
- **Subscription ID** (where your storage account lives)
- **Resource Group name** (that contains the storage account)
- **Storage Account name**
- **Container name** (e.g., "timestamps" — the blob container where the app will write)

### Network Access
- Access to the setup wizard URL provided by the source tenant team
  - Usually: `http://localhost:8081` (if using `kubectl port-forward`)
  - Or: an external URL if the team deployed it publicly (less secure)

---

## What the Wizard Does (Step-by-Step)

### Step 1: Sign In
Sign in with your Azure credentials (the account with Admin permissions above).

### Step 2: Grant Admin Consent
The wizard will ask you to grant the source application permission to read information about your tenant. This requires **Global Administrator** or **Application Administrator** role.

**You will see a Microsoft consent screen.** Review the permissions and click **Accept**.

### Step 3: Authorize RBAC Management
The wizard will ask you to authorize it to manage roles on your storage account. Sign in again if prompted.

### Step 4: Enter Storage Details
Enter your storage account information:
- Subscription ID
- Resource Group
- Storage Account name
- Container name

### Step 5: Configure
Click **Configure**. The wizard will:
- Call Azure Resource Manager (ARM) REST API
- Find your storage account
- Assign **Storage Blob Data Contributor** role to the source application
- If successful, show a completion screen

---

## What Happens at Completion

On the success screen, you will see:

- **Storage Endpoint URL** — e.g., `https://myaccount.blob.core.windows.net`
- **Application ID** — the source tenant's application

**Share the Storage Endpoint URL** back to the source tenant team. They will use this to configure their application.

---

## Security Notes

- **The wizard uses PKCE (Proof Key for Code Exchange)** — no client secrets are transmitted
- **Your tokens stay local** — they are not stored on the wizard's server
- **Role assignment is minimal** — only Storage Blob Data Contributor, scoped to your storage account
- **Network security** — if the wizard is accessed via `kubectl port-forward`, it is only exposed to your local machine (or the source tenant's internal network)

---

## Troubleshooting

**Q: "Admin consent required" or "You don't have permission"**
- Verify you have **Global Administrator** or **Application Administrator** role in your tenant
- Contact your tenant's identity administrator if you don't have this role

**Q: "Resource not found" after entering storage details**
- Verify your subscription ID, resource group, and storage account name are correct
- Ensure your account has permissions to access that storage account

**Q: The wizard is unreachable**
- Confirm the URL is correct and you have network access to it
- If using `kubectl port-forward`, ask the source tenant team to verify the pod is running

**Q: "Insufficient permissions" when configuring**
- Verify you have **Owner** or **User Access Administrator** role on the storage account (not just Contributor)
- Contact your storage account owner if you need higher permissions

---

## What Comes Next

After the wizard completes:

1. **Copy the Storage Endpoint URL** from the success page
2. **Share it with the source tenant team** — they will update their application configuration
3. **The application will now be able to write to your storage account** using the granted permissions

You do not need to do anything else in your tenant. The setup is complete.

---

## Questions?

Contact your source tenant's application team with:
- Any error messages from the wizard
- Your subscription ID and storage account name
- Your Azure tenant ID (if asked)

They can debug the wizard deployment and permissions from their side.
