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

//go:embed templates/*.html
var templateFS embed.FS

type config struct {
	clientID     string
	redirectBase string
	port         string
	dbPath       string
}

type configureData struct {
	TenantID string
}

type savedData struct {
	ClientID           string
	TenantID           string
	ResourceID         string
	ContainerName      string
	StorageAccountName string // derived from ResourceID
}

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

func renderTemplate(w http.ResponseWriter, tmpl *template.Template, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.ExecuteTemplate(w, name, data); err != nil {
		slog.Error("template render error", "template", name, "error", err)
	}
}

func handleIndex(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		renderTemplate(w, tmpl, "index.html", nil)
	}
}

// handleStartConsent generates a state token, stores it in a short-lived cookie,
// and redirects the browser to the Microsoft admin consent URL.
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
			MaxAge:   600,
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
// Success: admin_consent=True + tenant query params present.
// Failure: error + error_description query params present.
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

const createTableSQL = `
CREATE TABLE IF NOT EXISTS consents (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  tenant_id TEXT NOT NULL,
  resource_id TEXT NOT NULL,
  container_name TEXT NOT NULL,
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);`

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
