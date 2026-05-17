// Package main provides the entry point for Kiro API Proxy.
//
// Kiro API Proxy is a reverse proxy service that translates Kiro API requests
// into OpenAI and Anthropic (Claude) compatible formats. Key features include:
//   - Multi-account pool with round-robin load balancing
//   - Automatic OAuth token refresh
//   - Streaming response support for real-time AI interactions
//   - Admin panel for account and configuration management
//
// The service exposes the following endpoints:
//   - /v1/messages - Claude API compatible endpoint
//   - /v1/chat/completions - OpenAI API compatible endpoint
//   - /v1/responses - OpenAI Responses API (Codex CLI)
//   - /admin - Web-based administration panel
package main

import (
	"context"
	"errors"
	"fmt"
	"kiro-go/config"
	"kiro-go/logger"
	"kiro-go/pool"
	"kiro-go/proxy"
	"kiro-go/stats"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

// defaultAdminPassword matches the password the bundled config writes on first
// run. We refuse to start unprotected if the user hasn't changed it (and
// hasn't supplied an env override) — this is the single biggest production
// risk for the proxy.
const defaultAdminPassword = "changeme"

func main() {
	configPath := "data/config.json"
	if envPath := os.Getenv("CONFIG_PATH"); envPath != "" {
		configPath = envPath
	}

	if err := os.MkdirAll(filepath.Dir(configPath), 0755); err != nil {
		log.Fatalf("Failed to create data directory: %v", err)
	}

	if err := config.Init(configPath); err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	logger.Init(config.GetLogLevel())

	// Persistent statistics. Stored alongside config.json so the existing
	// data volume / mount path covers it. Stats record best-effort: a failure
	// to open the DB is logged but does not block startup.
	statsPath := filepath.Join(filepath.Dir(configPath), "stats.db")
	if err := stats.Init(statsPath); err != nil {
		logger.Errorf("Failed to open stats DB at %s: %v (statistics will not persist)", statsPath, err)
	} else {
		logger.Infof("Statistics DB: %s", statsPath)
	}

	if envPassword := os.Getenv("ADMIN_PASSWORD"); envPassword != "" {
		config.SetPassword(envPassword)
	}

	// Refuse to bind a public port if the password is still the bundled
	// default. The user must either supply ADMIN_PASSWORD or change the
	// password in data/config.json before the proxy starts. Allow the
	// override env to skip this check for explicit unattended deployments.
	if config.GetPassword() == defaultAdminPassword && os.Getenv("KIRO_ALLOW_DEFAULT_PASSWORD") == "" {
		logger.Errorf("Refusing to start: admin password is still the default '%s'.", defaultAdminPassword)
		logger.Errorf("Set the ADMIN_PASSWORD environment variable, or edit data/config.json,")
		logger.Errorf("or set KIRO_ALLOW_DEFAULT_PASSWORD=1 to bypass this guard (not recommended).")
		os.Exit(2)
	}

	// If this was a fresh install, log the auto-generated starter API key
	// loud-and-clear so the operator can copy it. The same key is also
	// visible on the dashboard's "API Keys" tab on first login.
	if k := config.FirstRunStarterKey(); k != "" {
		logger.Infof("============================================================")
		logger.Infof("First-run setup: created default admin API key")
		logger.Infof("  Key: %s", k)
		logger.Infof("  Save this now. You can rotate / revoke it from /admin -> API Keys")
		logger.Infof("============================================================")
	}

	pool.GetPool()

	handler := proxy.NewHandler()

	addr := fmt.Sprintf("%s:%d", config.GetHost(), config.GetPort())
	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		// Long write timeout because streaming responses can run for minutes.
		// ReadHeaderTimeout above already protects against Slowloris.
		WriteTimeout: 30 * time.Minute,
		IdleTimeout:  120 * time.Second,
	}

	logger.Infof("Kiro-Go starting on http://%s (log level: %s)", addr, logger.LevelName(logger.GetLevel()))
	logger.Infof("Admin panel: http://%s/admin", addr)
	logger.Infof("Claude API: http://%s/v1/messages", addr)
	logger.Infof("OpenAI API: http://%s/v1/chat/completions", addr)
	logger.Infof("Responses API: http://%s/v1/responses (Codex CLI)", addr)

	// Graceful shutdown: catch SIGINT / SIGTERM, drain in-flight requests,
	// and let the handler clean up its background goroutines.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serveErr := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
		close(serveErr)
	}()

	select {
	case err, ok := <-serveErr:
		if ok && err != nil {
			logger.Fatalf("Server failed: %v", err)
		}
	case <-ctx.Done():
		logger.Infof("Shutdown signal received; draining in-flight requests…")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Errorf("Graceful shutdown error: %v", err)
		}
		handler.Stop()
		_ = stats.Close()
		logger.Infof("Bye.")
	}
}
