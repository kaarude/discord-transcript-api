package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/kaarude/discord-transcript-api/internal/auth"
	"github.com/kaarude/discord-transcript-api/internal/config"
	"github.com/kaarude/discord-transcript-api/internal/server"
	"github.com/kaarude/discord-transcript-api/internal/store"
)

func main() {
	config.LoadDotEnv(".env")
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		client := &http.Client{Timeout: 4 * time.Second}
		response, err := client.Get("http://127.0.0.1:3010/access/status")
		if err != nil || response.StatusCode != http.StatusOK {
			os.Exit(1)
		}
		_ = response.Body.Close()
		return
	}
	level := slog.LevelInfo
	if os.Getenv("LOG_LEVEL") == "debug" {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	if os.Getenv("PUBLIC_BASE_URL") == "" {
		log.Error("PUBLIC_BASE_URL is required, for example https://transcripts.example.com")
		os.Exit(1)
	}
	authOptions, err := auth.OptionsFromEnvironment(
		os.Getenv("PUBLIC_BASE_URL"),
		os.Getenv("PASSKEY_RP_ID"),
		os.Getenv("PASSKEY_ORIGINS"),
	)
	if err != nil {
		log.Error("configure passkeys", "error", err)
		os.Exit(1)
	}
	authPath := config.AuthPath()
	authStore, setupCode, err := auth.Open(authPath, authOptions.RelyingPartyID)
	if err != nil {
		log.Error("load passkey authentication", "error", err)
		os.Exit(1)
	}
	authenticator, err := auth.NewManager(authStore, authOptions)
	if err != nil {
		log.Error("initialize passkey authentication", "error", err)
		os.Exit(1)
	}
	if setupCode != "" {
		log.Warn("first-time passkey setup required", "setupCodeFile", filepath.Join(filepath.Dir(authPath), "setup-code"))
	}

	settings, err := config.Open(config.SettingsPath())
	if err != nil {
		log.Error("load settings", "error", err)
		os.Exit(1)
	}
	registry, err := store.Open(config.RegistryPath(), config.TranscriptsDir())
	if err != nil {
		log.Error("load transcript registry", "error", err)
		os.Exit(1)
	}
	if err := registry.CleanupInterrupted(); err != nil {
		log.Warn("startup transcript cleanup failed", "error", err)
	}
	if compacted, err := registry.CompactIfNeeded(); err != nil {
		log.Warn("startup registry compaction failed", "error", err)
	} else if compacted {
		log.Info("compacted transcript registry")
	}
	if refreshed, failures := server.RefreshStoredHTML(registry); refreshed > 0 || len(failures) > 0 {
		log.Info("startup transcript renderer refresh", "refreshed", refreshed, "failures", len(failures))
		for _, failure := range failures {
			log.Warn("transcript renderer refresh failed", "error", failure)
		}
	}

	maintenanceCtx, stopMaintenance := context.WithCancel(context.Background())
	defer stopMaintenance()
	go func() {
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-maintenanceCtx.Done():
				return
			case <-ticker.C:
				if compacted, err := registry.CompactIfNeeded(); err != nil {
					log.Warn("registry compaction failed", "error", err)
				} else if compacted {
					log.Info("compacted transcript registry")
				}
			}
		}
	}()

	app := server.New(settings, registry, log, authenticator)
	httpServer := &http.Server{
		Addr:              ":3010",
		Handler:           app,
		ReadTimeout:       30 * time.Second,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    32 << 10,
	}

	go func() {
		log.Info("server listening", "address", httpServer.Addr, "admin", "http://localhost"+httpServer.Addr+"/admin")
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http server stopped", "error", err)
			os.Exit(1)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(ctx); err != nil {
		log.Error("graceful shutdown failed", "error", err)
	}
}
