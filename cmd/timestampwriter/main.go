package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	_ "modernc.org/sqlite"
)

type storageConfig struct {
	accountURL    string
	containerName string
}

// loadStorageConfig resolves storage configuration. It first checks the
// STORAGE_ACCOUNT_URL and STORAGE_CONTAINER_NAME environment variables.
// If those are absent it falls back to the most recent consent row in the
// SQLite database written by the setup wizard (SETUP_DB_PATH env var).
// Returns (cfg, true, nil)  — config found and valid
// Returns (cfg{}, false, nil) — not yet configured (DB empty or not present)
// Returns (cfg{}, false, err) — unexpected error
func loadStorageConfig(dbPath string) (storageConfig, bool, error) {
	accountURL := os.Getenv("STORAGE_ACCOUNT_URL")
	containerName := os.Getenv("STORAGE_CONTAINER_NAME")
	if accountURL != "" && containerName != "" {
		return storageConfig{accountURL: accountURL, containerName: containerName}, true, nil
	}

	if dbPath == "" {
		return storageConfig{}, false, nil
	}

	db, err := sql.Open("sqlite", dbPath+"?mode=ro")
	if err != nil {
		// DB file may not exist yet — not an error.
		return storageConfig{}, false, nil
	}
	defer db.Close()

	var resourceID, container string
	err = db.QueryRowContext(context.Background(),
		`SELECT resource_id, container_name FROM consents ORDER BY created_at DESC LIMIT 1`,
	).Scan(&resourceID, &container)
	if err == sql.ErrNoRows {
		return storageConfig{}, false, nil
	}
	if err != nil {
		return storageConfig{}, false, fmt.Errorf("query consents: %w", err)
	}

	accountName, err := parseStorageAccountName(resourceID)
	if err != nil {
		return storageConfig{}, false, fmt.Errorf("parse resource ID from DB: %w", err)
	}

	return storageConfig{
		accountURL:    fmt.Sprintf("https://%s.blob.core.windows.net", accountName),
		containerName: container,
	}, true, nil
}

// parseStorageAccountName extracts the storage account name from an Azure resource ID.
func parseStorageAccountName(resourceID string) (string, error) {
	parts := strings.Split(resourceID, "/")
	for i, p := range parts {
		if strings.EqualFold(p, "storageAccounts") && i+1 < len(parts) && parts[i+1] != "" {
			return parts[i+1], nil
		}
	}
	return "", fmt.Errorf("storageAccounts segment not found in resource ID %q", resourceID)
}

func writeTimestampBlob(ctx context.Context, client *azblob.Client, containerName string, now time.Time) error {
	blobName := "timestamp-" + now.UTC().Format(time.RFC3339) + ".txt"
	content := now.UTC().Format(time.RFC3339)

	_, err := client.UploadStream(ctx, containerName, blobName, strings.NewReader(content), nil)
	if err != nil {
		return fmt.Errorf("upload blob %q: %w", blobName, err)
	}
	return nil
}

func run(ctx context.Context, dbPath string, logger *slog.Logger) error {
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
			// the admin has finished the setup wizard.
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

			if err := writeTimestampBlob(ctx, client, cfg.containerName, t); err != nil {
				// A 403 here means the RBAC assignment hasn't propagated yet.
				// Log and keep retrying — do not crash.
				logger.Error("failed to write blob (RBAC may not be assigned yet)", "error", err)
				continue
			}
			logger.Info("blob written", "timestamp", t.UTC().Format(time.RFC3339))
		}
	}
}

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
