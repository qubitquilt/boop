package main

import (
	"context"
	"crypto/rsa"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/golang-jwt/jwt/v5"
	boopgithub "github.com/michaelruelas/boop-receiver/internal/github"
	"github.com/michaelruelas/boop-receiver/internal/webhook"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: parseLevel(os.Getenv("LOG_LEVEL")),
	}))
	slog.SetDefault(logger)

	appID, err := parseInt64(getenv("GITHUB_APP_ID", "0"))
	if err != nil || appID == 0 {
		logger.Error("GITHUB_APP_ID is required and must be a non-zero integer", "err", err)
		os.Exit(1)
	}
	privKeyPEM := os.Getenv("GITHUB_APP_PRIVATE_KEY")
	if privKeyPEM == "" {
		logger.Error("GITHUB_APP_PRIVATE_KEY is required")
		os.Exit(1)
	}
	privKey, err := parseRSAPrivateKey(privKeyPEM)
	if err != nil {
		logger.Error("parse GITHUB_APP_PRIVATE_KEY", "err", err)
		os.Exit(1)
	}

	cfg := webhook.Config{
		Port:            getenv("PORT", "8080"),
		WebhookSecret:   os.Getenv("WEBHOOK_SECRET"),
		JobImage:        getenv("JOB_IMAGE", "ghcr.io/michaelruelas/boop-runner:latest"),
		TargetNamespace: getenv("TARGET_NAMESPACE", "dev-tools"),
		BotLogin:        os.Getenv("BOT_LOGIN"), // optional; if empty, the receiver ignores all issue_comment events with sender == self check
	}

	if cfg.WebhookSecret == "" {
		logger.Error("WEBHOOK_SECRET is required")
		os.Exit(1)
	}

	mgr := boopgithub.NewManager(boopgithub.AppConfig{
		AppID:      appID,
		PrivateKey: privKey,
	})

	h, err := webhook.NewHandler(cfg, mgr, logger)
	if err != nil {
		logger.Error("init handler", "err", err)
		os.Exit(1)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /webhook", h.HandleWebhook)
	mux.HandleFunc("GET /health", h.Health)

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		logger.Info("receiver starting", "port", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server failed", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func parseInt64(s string) (int64, error) {
	if s == "" {
		return 0, fmt.Errorf("empty")
	}
	return strconv.ParseInt(s, 10, 64)
}

func parseLevel(s string) slog.Level {
	switch s {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func parseRSAPrivateKey(pem string) (*rsa.PrivateKey, error) {
	// GitHub App private keys are PEM-encoded PKCS#1 RSA keys.
	return jwt.ParseRSAPrivateKeyFromPEM([]byte(pem))
}
