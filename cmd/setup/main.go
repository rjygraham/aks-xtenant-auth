// Package main implements the setup wizard binary.
//
// # What this binary does
//
// The setup wizard is a small web application that a *source-tenant admin*
// runs once to onboard a new *target tenant* admin. It walks the target tenant
// admin through two steps:
//
//  1. Grant admin consent for the multi-tenant Entra application (so it has a
//     service principal in the target tenant).
//  2. Record which Azure Blob Storage account and container the timestampwriter
//     should write to.
//
// After the wizard completes, the timestampwriter pod reads the saved
// configuration from the shared SQLite database and begins writing blobs.
//
// # Admin consent link approach
//
// The wizard uses the simplest possible OAuth2 flow: a browser redirect to
// Microsoft's /adminconsent endpoint. This requires no token exchange, no
// Graph API calls, and no ARM calls. The target tenant admin uses their own
// browser session and credentials — the setup wizard never handles any tokens.
//
// CSRF is prevented by a short-lived HttpOnly cookie ("setup_state", 10-min
// MaxAge) that holds a random hex state value. Microsoft echoes the state back
// on the callback; the handler validates it before trusting the response.
//
// # SQLite on NFS
//
// Configuration is stored in a SQLite database at SETUP_DB_PATH (default
// /data/setup.db). The file lives on an Azure Files NFS mount shared with the
// timestampwriter pod. NFS is required because the subscription policy blocks
// key-based Azure Files authentication (which SMB mounts depend on).
//
// The timestampwriter reads the most recent row from this DB on each tick,
// allowing it to pick up new configuration without restarting.
//
// # Routes
//
//   GET /             – landing page (index.html)
//   GET /start-consent – generates state, sets cookie, redirects to Microsoft
//   GET /callback      – validates consent response, shows configure form
//   POST /configure    – saves storage account details to SQLite, shows success
package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"embed"
	"encoding/hex"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	_ "modernc.org/sqlite"
)

// templateFS embeds all HTML templates at compile time so the binary is
// self-contained (no external files needed in the container image).
//go:embed templates/*.html
var templateFS embed.FS

// config holds runtime configuration sourced entirely from environment variables.
// All values are resolved once at startup by loadConfig.
type config struct {
	// clientID is the multi-tenant Entra application's client ID. This is the
	// app that target-tenant admins will grant consent to. Sourced from
	// AZURE_CLIENT_ID (set via the setup-config ConfigMap).
	clientID     string
	// redirectBase is the base URL that Microsoft will redirect back to after
	// consent (e.g. http://localhost:8081 when using kubectl port-forward).
	redirectBase string
	// port is the TCP port the HTTP server listens on.
	port         string
	// dbPath is the path to the SQLite database file shared with the
	// timestampwriter pod. Defaults to /data/setup.db.
	dbPath       string
}

// configureData is the template data passed to configure.html — the form
// shown after a successful admin consent. It pre-populates the target tenant
// ID (returned by Microsoft in the callback) as a hidden field.
type configureData struct {
	TenantID string
}

// savedData is the template data passed to success.html after the admin
// submits the storage configuration form. It carries all values needed to
// render the pre-filled az CLI command and ConfigMap YAML snippets.
type savedData struct {
	// ClientID is the multi-tenant Entra app client ID — shown in the ConfigMap
	// snippet as the AZURE_CLIENT_ID override that the timestampwriter uses.
	ClientID           string
	// TenantID is the *target* tenant ID returned by Microsoft in the consent
	// callback. Note: AZURE_TENANT_ID in the ConfigMap should be the *source*
	// tenant (where the Entra app is registered), not this value.
	TenantID           string
	ResourceID         string
	ContainerName      string
	// StorageAccountName is derived from ResourceID for display convenience.
	StorageAccountName string
}

// errorData is the template data passed to error.html.
type errorData struct {
	Error            string
	ErrorDescription string
}

// randomHex returns n random bytes as a hex string.
func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// renderTemplate writes the named template to the response writer. Errors are
// logged but not returned — by the time Execute fails the HTTP headers are
// already sent, so there's nothing useful to do except log.
func renderTemplate(w http.ResponseWriter, tmpl *template.Template, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.ExecuteTemplate(w, name, data); err != nil {
		slog.Error("template render error", "template", name, "error", err)
	}
}

// handleIndex renders the landing page (index.html) with no dynamic data.
func handleIndex(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		renderTemplate(w, tmpl, "index.html", nil)
	}
}

// handleStartConsent generates a CSRF state token, stores it in a short-lived
// HttpOnly cookie, and redirects the browser to the Microsoft admin consent URL.
//
// The /common/adminconsent endpoint is used (not a specific tenant) so that
// admins from *any* target tenant can complete consent in a single flow.
// Microsoft will redirect back to /callback with the consented tenant ID.
//
// The state cookie (MaxAge: 600s) is the sole CSRF mechanism — no server-side
// session map is needed, which avoids state loss if the pod restarts mid-flow.
func handleStartConsent(cfg config, tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		state, err := randomHex(16)
		if err != nil {
			http.Error(w, "failed to generate state", http.StatusInternalServerError)
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name:     "setup_state",
			Value:    state,
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
			MaxAge:   600, // 10 minutes — enough time for an admin to complete consent
		})
		params := url.Values{
			"client_id":    {cfg.clientID},
			"redirect_uri": {cfg.redirectBase + "/callback"},
			"state":        {state},
		}
		http.Redirect(w, r, "https://login.microsoftonline.com/common/adminconsent?"+params.Encode(), http.StatusFound)
	}
}

// handleCallback handles the Microsoft admin consent redirect.
//
// Microsoft sends one of two responses:
//   - Success: admin_consent=True + tenant={targetTenantID} + state={…}
//   - Failure: error={code} + error_description={…}
//
// On success the handler validates the CSRF state cookie, clears it, and
// renders the storage configuration form (configure.html) pre-populated with
// the target tenant ID. The admin then enters their storage account details
// and submits to POST /configure.
func handleCallback(cfg config, tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()

		if errParam := q.Get("error"); errParam != "" {
			renderTemplate(w, tmpl, "error.html", errorData{
				Error:            errParam,
				ErrorDescription: q.Get("error_description"),
			})
			return
		}

		// Validate the state cookie before trusting anything in the callback.
		// A mismatch means either a CSRF attempt or the cookie expired
		// (admin took longer than 10 minutes to complete consent).
		cookie, err := r.Cookie("setup_state")
		if err != nil || cookie.Value != q.Get("state") {
			renderTemplate(w, tmpl, "error.html", errorData{
				Error:            "state_mismatch",
				ErrorDescription: "State parameter mismatch — possible CSRF or the session expired. Please try again.",
			})
			return
		}

		tenantID := q.Get("tenant")
		if q.Get("admin_consent") != "True" || tenantID == "" {
			renderTemplate(w, tmpl, "error.html", errorData{
				Error:            "consent_not_granted",
				ErrorDescription: "Admin consent was not granted or the tenant ID is missing from the response.",
			})
			return
		}

		// Clear the state cookie.
		http.SetCookie(w, &http.Cookie{Name: "setup_state", Value: "", Path: "/", MaxAge: -1})

		renderTemplate(w, tmpl, "configure.html", configureData{TenantID: tenantID})
	}
}

// loadConfig reads all configuration from environment variables and returns a
// populated config struct. AZURE_CLIENT_ID is the only required value — it
// must match the client ID of the multi-tenant Entra app registration.
// All other values have sensible defaults for local kubectl port-forward usage.
func loadConfig() (config, error) {
	clientID := os.Getenv("AZURE_CLIENT_ID")
	if clientID == "" {
		return config{}, fmt.Errorf("AZURE_CLIENT_ID is not set")
	}
	redirectBase := os.Getenv("SETUP_REDIRECT_BASE_URI")
	if redirectBase == "" {
		redirectBase = "http://localhost:8081"
	}
	port := os.Getenv("SETUP_PORT")
	if port == "" {
		port = "8081"
	}
	dbPath := os.Getenv("SETUP_DB_PATH")
	if dbPath == "" {
		dbPath = "/data/setup.db"
	}
	return config{clientID: clientID, redirectBase: redirectBase, port: port, dbPath: dbPath}, nil
}

// createTableSQL creates the consents table if it doesn't already exist.
// Each row represents a completed setup: one target tenant admin granting
// consent and recording their storage account. The timestampwriter reads
// the most recent row to determine where to write blobs.
// Using CREATE TABLE IF NOT EXISTS makes initDB safe to call on every startup.
const createTableSQL = `
CREATE TABLE IF NOT EXISTS consents (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  tenant_id TEXT NOT NULL,
  resource_id TEXT NOT NULL,
  container_name TEXT NOT NULL,
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);`

// initDB opens the SQLite database at path, creating it if necessary, and
// ensures the consents table exists. The database file lives on an Azure Files
// NFS mount shared between the setup pod and the timestampwriter pod.
//
// modernc.org/sqlite is used (pure Go, no CGO) so the binary works in a
// distroless container where a CGO-based driver would fail to find libc.
func initDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	if _, err := db.Exec(createTableSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("create table: %w", err)
	}
	return db, nil
}

// parseStorageAccountName extracts the storage account name from an Azure resource ID.
// Expected format: /subscriptions/{sub}/resourceGroups/{rg}/providers/Microsoft.Storage/storageAccounts/{name}
func parseStorageAccountName(resourceID string) (string, error) {
	parts := strings.Split(resourceID, "/")
	for i, p := range parts {
		if strings.EqualFold(p, "storageAccounts") && i+1 < len(parts) && parts[i+1] != "" {
			return parts[i+1], nil
		}
	}
	return "", fmt.Errorf("storageAccounts segment not found in resource ID %q", resourceID)
}

// handleConfigure processes the storage configuration form submitted after
// admin consent. It:
//  1. Validates the resource ID by extracting the storage account name.
//  2. Persists the consent row to SQLite.
//  3. Renders success.html with pre-filled az CLI and ConfigMap YAML snippets
//     for the admin to copy.
//
// The RBAC assignment (Storage Blob Data Contributor) is *not* automated here.
// success.html displays a pre-filled "az role assignment create" command for
// the admin to run manually. This keeps the setup wizard stateless with
// respect to ARM and avoids requiring Owner/User Access Admin permissions in
// the wizard itself.
func handleConfigure(cfg config, db *sql.DB, tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenantID := r.FormValue("tenant_id")
		resourceID := strings.TrimSpace(r.FormValue("resource_id"))
		containerName := strings.TrimSpace(r.FormValue("container_name"))

		storageAccountName, err := parseStorageAccountName(resourceID)
		if err != nil {
			renderTemplate(w, tmpl, "error.html", errorData{
				Error:            "invalid_resource_id",
				ErrorDescription: "The resource ID you entered is not a valid storage account resource ID. Expected format: /subscriptions/{sub}/resourceGroups/{rg}/providers/Microsoft.Storage/storageAccounts/{name}",
			})
			return
		}

		_, err = db.ExecContext(r.Context(),
			`INSERT INTO consents (tenant_id, resource_id, container_name) VALUES (?, ?, ?)`,
			tenantID, resourceID, containerName,
		)
		if err != nil {
			slog.Error("db insert failed", "error", err)
			renderTemplate(w, tmpl, "error.html", errorData{
				Error:            "db_error",
				ErrorDescription: "Failed to save configuration. Please try again.",
			})
			return
		}

		renderTemplate(w, tmpl, "success.html", savedData{
			ClientID:           cfg.clientID,
			TenantID:           tenantID,
			ResourceID:         resourceID,
			ContainerName:      containerName,
			StorageAccountName: storageAccountName,
		})
	}
}

// run parses templates, registers HTTP routes, starts the server, and waits
// for ctx to be cancelled (SIGTERM/SIGINT) before performing a graceful
// 5-second shutdown. This keeps main() minimal — all meaningful logic lives
// in the handler functions.
func run(ctx context.Context, cfg config, db *sql.DB, logger *slog.Logger) error {
	tmpl, err := template.ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return fmt.Errorf("parse templates: %w", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", handleIndex(tmpl))
	mux.HandleFunc("GET /start-consent", handleStartConsent(cfg, tmpl))
	mux.HandleFunc("GET /callback", handleCallback(cfg, tmpl))
	mux.HandleFunc("POST /configure", handleConfigure(cfg, db, tmpl))

	srv := &http.Server{
		Addr:         ":" + cfg.port,
		Handler:      mux,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutCtx); err != nil {
			logger.Error("server shutdown error", "error", err)
		}
	}()

	logger.Info("setup server started", "port", cfg.port, "redirect_base", cfg.redirectBase)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("server error: %w", err)
	}
	return nil
}

// main is the entry point. It loads config, initialises the SQLite DB,
// wires up graceful shutdown via context cancellation, and delegates to run().
func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	cfg, err := loadConfig()
	if err != nil {
		logger.Error("configuration error", "error", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	db, err := initDB(cfg.dbPath)
	if err != nil {
		logger.Error("database init error", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		sig := <-sigCh
		logger.Info("received signal", "signal", sig.String())
		cancel()
	}()

	if err := run(ctx, cfg, db, logger); err != nil {
		logger.Error("fatal error", "error", err)
		os.Exit(1)
	}
}
