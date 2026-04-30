// Package main implements the timestampwriter binary.
//
// # What this binary does
//
// timestampwriter runs as a long-lived Kubernetes pod and writes a small
// timestamp blob to an Azure Blob Storage container every 30 seconds. The
// storage account lives in a *different* Azure tenant than the AKS cluster
// running this pod. Cross-tenant access is achieved without any secrets or
// managed identity shared across tenants.
//
// # AKS Identity Bindings (preview)
//
// AKS Identity Bindings (IB) bind a User-Assigned Managed Identity (UAMI) to
// the cluster at the control-plane level. When a pod carries the annotation
// "use-identity-binding: true", the workload identity webhook injects three
// environment variables automatically:
//
//   - AZURE_FEDERATED_TOKEN_FILE – path to the pod's projected service account token
//   - AZURE_TENANT_ID            – the source tenant (where the AKS cluster lives)
//   - AZURE_CLIENT_ID            – defaults to the UAMI client ID
//
// # The two-client-ID pattern
//
// There are *two* distinct client IDs in play:
//
//  1. UAMI client ID — annotated on the ServiceAccount so the IB webhook can
//     authorize the pod to use the identity binding.
//  2. Multi-tenant Entra app client ID — injected via the ConfigMap and used
//     to request access tokens. The Deployment overrides AZURE_CLIENT_ID with
//     this value, replacing the UAMI client ID that the webhook injected.
//
// The override is the key: azidentity reads AZURE_CLIENT_ID at runtime, so
// pointing it at the Entra app causes the SDK to request a token *for that
// app*, not for the UAMI directly. This is what makes cross-tenant writes
// possible.
//
// # Cross-tenant token exchange flow
//
//  1. Pod's projected OIDC token is issued by the IB OIDC issuer
//     (https://ib.oic.prod-aks.azure.com/<tenant>/<uami-client-id>).
//  2. azidentity presents that token to Entra ID as a federated credential
//     assertion for the multi-tenant Entra app (AZURE_CLIENT_ID from ConfigMap).
//  3. Because the Entra app is multi-tenant and has a service principal +
//     Storage Blob Data Contributor RBAC in the target tenant, Entra ID issues
//     an access token valid against the target tenant's storage account.
//
// The Azure SDK handles the exchange transparently — pass the credential to
// azblob.NewClient, target the storage URL in the destination tenant, and
// writes "just work".
//
// # 20-FIC limit bypass via Identity Bindings
//
// Without IB, each AKS cluster needs its own Federated Identity Credential
// (FIC) on the Entra app pointing at that cluster's unique OIDC issuer URL.
// Microsoft enforces a hard limit of 20 FICs per application, which becomes a
// scaling bottleneck at 20+ clusters.
//
// Identity Bindings eliminate this by providing a *UAMI-scoped* OIDC issuer
// that is stable across every cluster using the same UAMI. A single FIC on the
// Entra app points at the IB OIDC issuer, and any cluster bound to that UAMI
// can authenticate — no per-cluster FICs needed, no limit to hit.
//
// # Storage configuration (lazy + fallback)
//
// Storage config is resolved lazily (each tick) to allow the pod to start
// before the admin has finished the setup wizard. The resolution order is:
//
//  1. STORAGE_ACCOUNT_URL + STORAGE_CONTAINER_NAME environment variables.
//  2. Fallback: most recent row in the SQLite DB written by the setup wizard
//     (path from SETUP_DB_PATH). The DB lives on an Azure Files NFS mount.
//     NFS is required because the subscription blocks key-based storage auth.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	_ "modernc.org/sqlite"
)

// storageConfig holds the resolved target storage account details.
// accountURL is the full blob endpoint (e.g. https://<account>.blob.core.windows.net).
type storageConfig struct {
	accountURL    string
	containerName string
}

// storageAccountNameRE validates Azure storage account names: 3–24 lowercase letters and digits.
var storageAccountNameRE = regexp.MustCompile(`^[a-z0-9]{3,24}$`)

// containerNameRE validates Azure blob container names: 3–63 characters, lowercase letters,
// digits, and hyphens; must start and end with a letter or digit.
var containerNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`)

// loadStorageConfig resolves the target storage account configuration.
//
// Resolution order:
//  1. STORAGE_ACCOUNT_URL + STORAGE_CONTAINER_NAME environment variables
//     (set directly in the Deployment for simple cases).
//  2. Fallback: the most recent row in the setup wizard's SQLite database
//     (path from SETUP_DB_PATH). This lets the pod start before the admin
//     has finished running the setup wizard — on each tick the pod
//     re-checks the DB until a row appears.
//
// The SQLite DB lives on an Azure Files NFS share. NFS is required because
// the subscription policy blocks key-based authentication for Azure Files,
// which rules out the SMB mount that the sqlite driver would use by default.
//
// Return semantics:
//   - (cfg, true, nil)   – config resolved and valid
//   - (cfg{}, false, nil) – not yet configured (no env vars, DB empty or absent)
//   - (cfg{}, false, err) – unexpected error
func loadStorageConfig(dbPath string) (storageConfig, bool, error) {
	accountURL := os.Getenv("STORAGE_ACCOUNT_URL")
	containerName := os.Getenv("STORAGE_CONTAINER_NAME")
	if accountURL != "" && containerName != "" {
		if !containerNameRE.MatchString(containerName) {
			return storageConfig{}, false, fmt.Errorf("STORAGE_CONTAINER_NAME %q is not a valid Azure container name", containerName)
		}
		return storageConfig{accountURL: accountURL, containerName: containerName}, true, nil
	}

	if dbPath == "" {
		return storageConfig{}, false, nil
	}

	// Open the DB read-only (?mode=ro). If the file doesn't exist yet (the
	// setup wizard hasn't run), sqlite returns an error — treat that as
	// "not configured" rather than a fatal failure.
	db, err := sql.Open("sqlite", dbPath+"?mode=ro")
	if err != nil {
		// DB file may not exist yet — not an error.
		return storageConfig{}, false, nil
	}
	defer db.Close()

	var resourceID, container string
	err = db.QueryRowContext(context.Background(),
		// Take the newest consent row in case the admin ran setup multiple times.
		`SELECT resource_id, container_name FROM consents ORDER BY created_at DESC LIMIT 1`,
	).Scan(&resourceID, &container)
	if err == sql.ErrNoRows {
		// Setup wizard has not yet saved a configuration — keep waiting.
		return storageConfig{}, false, nil
	}
	if err != nil {
		return storageConfig{}, false, fmt.Errorf("query consents: %w", err)
	}

	accountName, err := parseStorageAccountName(resourceID)
	if err != nil {
		return storageConfig{}, false, fmt.Errorf("parse resource ID from DB: %w", err)
	}

	if !containerNameRE.MatchString(container) {
		return storageConfig{}, false, fmt.Errorf("container name %q from DB is not a valid Azure container name", container)
	}

	return storageConfig{
		accountURL:    fmt.Sprintf("https://%s.blob.core.windows.net", accountName),
		containerName: container,
	}, true, nil
}

// parseStorageAccountName extracts the storage account name from an Azure
// resource ID and validates it against Azure's naming rules (3–24 lowercase
// letters and digits). The expected format is:
//
//	/subscriptions/{sub}/resourceGroups/{rg}/providers/Microsoft.Storage/storageAccounts/{name}
//
// The search is case-insensitive on the "storageAccounts" segment so it
// handles any capitalisation variants returned by ARM.
func parseStorageAccountName(resourceID string) (string, error) {
	parts := strings.Split(resourceID, "/")
	for i, p := range parts {
		if strings.EqualFold(p, "storageAccounts") && i+1 < len(parts) && parts[i+1] != "" {
			name := parts[i+1]
			if !storageAccountNameRE.MatchString(name) {
				return "", fmt.Errorf("storage account name %q extracted from resource ID does not match Azure naming rules (3-24 lowercase letters/digits)", name)
			}
			return name, nil
		}
	}
	return "", fmt.Errorf("storageAccounts segment not found in resource ID %q", resourceID)
}

// writeTimestampBlob uploads a tiny text blob whose name and content are both
// the current UTC time in RFC 3339 format. The blob name includes the
// "timestamp-" prefix so blobs sort visually in the storage browser.
//
// UploadStream is appropriate for these small in-memory payloads — no need
// for staged block uploads at this size.
func writeTimestampBlob(ctx context.Context, client *azblob.Client, containerName string, now time.Time) error {
	blobName := "timestamp-" + now.UTC().Format(time.RFC3339) + ".txt"
	content := now.UTC().Format(time.RFC3339)

	_, err := client.UploadStream(ctx, containerName, blobName, strings.NewReader(content), nil)
	if err != nil {
		return fmt.Errorf("upload blob %q: %w", blobName, err)
	}
	return nil
}

// run is the application's main loop. It:
//  1. Creates a WorkloadIdentityCredential, which reads AZURE_CLIENT_ID,
//     AZURE_TENANT_ID, and AZURE_FEDERATED_TOKEN_FILE from the environment.
//     The Deployment overrides AZURE_CLIENT_ID with the multi-tenant Entra
//     app client ID (from the ConfigMap), not the UAMI client ID injected by
//     the IB webhook. This causes the credential to perform the cross-tenant
//     token exchange on every authenticated call.
//  2. On each 30-second tick, lazily resolves storage configuration (env vars
//     or SQLite DB) and writes a timestamp blob to the target tenant's storage.
//
// Lazy config resolution means the pod can reach Running state and begin the
// identity handshake before the admin finishes the setup wizard. Once config
// is found, the azblob client is cached for the lifetime of the process.
func run(ctx context.Context, dbPath string, logger *slog.Logger) error {
	// NewWorkloadIdentityCredential reads the three AKS webhook-injected env
	// vars (AZURE_CLIENT_ID, AZURE_TENANT_ID, AZURE_FEDERATED_TOKEN_FILE).
	// Because the Deployment overrides AZURE_CLIENT_ID with the Entra app's
	// client ID, the credential will exchange the pod's OIDC token for an
	// Entra app access token — enabling writes to the target tenant's storage.
	cred, err := azidentity.NewWorkloadIdentityCredential(nil)
	if err != nil {
		return fmt.Errorf("create workload identity credential: %w", err)
	}

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	logger.Info("timestamp writer started, waiting for storage configuration")

	var client *azblob.Client
	var cfg storageConfig

	for {
		select {
		case <-ctx.Done():
			logger.Info("shutting down")
			return nil

		case t := <-ticker.C:
			// Lazily resolve configuration so the pod starts cleanly before
			// the admin has finished the setup wizard. Once client is non-nil
			// we skip this block and go straight to writing the blob.
			if client == nil {
				c, ok, err := loadStorageConfig(dbPath)
				if err != nil {
					logger.Error("config load error", "error", err)
					continue
				}
				if !ok {
					logger.Info("storage account not yet configured — waiting for setup wizard to complete")
					continue
				}
				cfg = c
				client, err = azblob.NewClient(cfg.accountURL, cred, nil)
				if err != nil {
					logger.Error("failed to create blob client", "account", cfg.accountURL, "error", err)
					client = nil
					continue
				}
				logger.Info("storage account configured", "account", cfg.accountURL, "container", cfg.containerName)
			}

			if err := func() error {
				// 15-second timeout per upload prevents a stalled network call
				// from blocking all subsequent ticks indefinitely.
				uploadCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
				defer cancel()
				return writeTimestampBlob(uploadCtx, client, cfg.containerName, t)
			}(); err != nil {
				// A 403 AuthorizationPermissionMismatch here typically means the
				// RBAC assignment (Storage Blob Data Contributor) hasn't propagated
				// yet — Azure RBAC can take several minutes to become effective.
				// Log and keep retrying — do not crash.
				logger.Error("failed to write blob (RBAC may not be assigned yet)", "error", err)
				continue
			}
			logger.Info("blob written", "timestamp", t.UTC().Format(time.RFC3339))
		}
	}
}

// main is the entry point. It:
//   - Configures structured JSON logging (log/slog, compatible with Azure Monitor).
//   - Reads SETUP_DB_PATH for the optional SQLite fallback path.
//   - Sets up context cancellation wired to SIGTERM/SIGINT for graceful shutdown.
//   - Delegates to run() for the application loop.
func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	dbPath := os.Getenv("SETUP_DB_PATH")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		sig := <-sigCh
		logger.Info("received signal", "signal", sig.String())
		cancel()
	}()

	if err := run(ctx, dbPath, logger); err != nil {
		logger.Error("fatal error", "error", err)
		os.Exit(1)
	}
}
