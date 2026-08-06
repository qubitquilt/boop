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
		JobImage:        getenv("JOB_IMAGE", "ghcr.io/qubitquilt/boop-runner:stable"),
		TargetNamespace: getenv("TARGET_NAMESPACE", "dev-tools"),
		BotLogin:        os.Getenv("BOT_LOGIN"), // optional; if empty, the receiver ignores all issue_comment events with sender == self check
		DBPath:          getenv("DB_PATH", "/data/boop.db"),
		RunnerToken:     os.Getenv("RUNNER_TOKEN"),
		// QUB-101: retention knobs. Env-driven so the same
		// image can run with a 7-day retention in staging and
		// 365-day in prod. Empty / unparseable values fall
		// through to the store package's defaults inside
		// StartRetentionLoop.
		Retention:      parseDurationEnv("DB_RETENTION", 0),
		CleanupEvery:   parseDurationEnv("DB_CLEANUP_INTERVAL", 0),
		VacuumInterval: parseDurationEnv("DB_VACUUM_INTERVAL", 0),
		// QUB-101: backup knobs. Dir is /backups by default;
		// the boop-receiver-backups PVC mounts there. Empty
		// Dir disables the loop (useful for tests and for
		// clusters that run their own snapshot tooling).
		BackupDir:   getenv("BACKUP_DIR", "/backups"),
		BackupEvery: parseDurationEnv("BACKUP_EVERY", 0),
		BackupKeep:  parseIntEnv("BACKUP_KEEP", 0),
		// QUB-94 / QUB-98: cluster-wide default for the OpenRouter
		// SDK feature flag. The runner's only invocation path is
		// the in-process OpenRouter SDK; the flag is preserved
		// for the QUB-N rollout. Per-PR labels (boop:openrouter-sdk)
		// override this for a single review.
		OpenRouterSDKDefault: getenv("BOOP_USE_OPENROUTER_SDK", "0"),
		// QUB-106: model id forwarded to every runner Job as
		// OPENROUTER_MODEL. Sourced from the receiver's own
		// env (typically set in the receiver Deployment
		// manifest, see apps/k8s/base/deployment.yaml). An
		// empty value is preserved so the runner's existing
		// "unset or empty" throw (apps/runner/src/lib/openrouter.mjs)
		// fires loudly on the first review instead of
		// silently landing a Job that always fails.
		OpenRouterModel: os.Getenv("OPENROUTER_MODEL"),
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
	if h.Store() == nil {
		logger.Warn("data layer disabled: DB_PATH is empty or could not be opened; /api/runs, /api/stats, /api/installations, and the runner POST endpoints are 503")
	} else {
		logger.Info("data layer enabled", "path", cfg.DBPath)
	}
	if cfg.RunnerToken == "" {
		logger.Warn("RUNNER_TOKEN is unset: runner telemetry/status posts will be rejected with 401")
	}
	// QUB-106: log the OpenRouter model the receiver is
	// forwarding so an operator can confirm the wire-up from
	// receiver startup logs alone. An empty value is the
	// pre-fix bug shape: every review Job's runner will throw
	// "OPENROUTER_MODEL is unset or empty" at the sniff gate
	// and never post a summary. Warn at startup so the
	// misconfiguration is visible before the first webhook
	// arrives, instead of only after a stuck run shows up in
	// the dashboard.
	if cfg.OpenRouterModel == "" {
		logger.Warn("OPENROUTER_MODEL is unset: every review Job will fail with 'OPENROUTER_MODEL is unset or empty' at the sniff gate")
	} else {
		logger.Info("OPENROUTER_MODEL", "value", cfg.OpenRouterModel)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /webhook", h.HandleWebhook)
	mux.HandleFunc("GET /health", h.Health)
	mux.HandleFunc("GET /api/reviews", h.ListReviews)
	mux.HandleFunc("GET /api/installations", h.ListInstallations)
	mux.HandleFunc("GET /api/runs", h.ListRuns)
	mux.HandleFunc("GET /api/stats", h.Stats)
	mux.HandleFunc("POST /api/runs/{id}/telemetry", h.RecordTelemetry)
	mux.HandleFunc("POST /api/runs/{id}/status", h.RecordStatus)

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	stopPoller := h.StartInstallationsPoller(ctx, cfg.InstallPollInterval)
	defer stopPoller()

	stopRetention := h.StartRetentionLoop(ctx, cfg.Retention, cfg.CleanupEvery, cfg.VacuumInterval)
	defer stopRetention()

	stopBackup := h.StartBackupLoop(ctx, cfg.BackupDir, cfg.BackupEvery, cfg.BackupKeep)
	defer stopBackup()

	// QUB-108: K8s Job reconciler. Polls Jobs in the
	// receiver's namespace and backfills the failure_class
	// column on terminal Jobs from the pod's last container
	// state. The interval is env-driven so a cluster that
	// wants fresher OOMKilled visibility can opt into a
	// tighter poll without rebuilding the image.
	stopReconciler := h.StartJobReconciler(ctx, parseDurationEnv("RECONCILER_INTERVAL", 0))
	defer stopReconciler()

	go func() {
		logger.Info("receiver starting", "port", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server failed", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("server shutdown", "err", err)
	}
	if h.Store() != nil {
		if err := h.Store().Close(); err != nil {
			logger.Error("close store", "err", err)
		}
	}
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// parseDurationEnv reads a Go duration string ("24h", "30m",
// "168h") from the named env var. Empty input or a parse error
// returns the supplied default; a default of 0 means "let the
// store package pick". Used for QUB-101 retention knobs.
func parseDurationEnv(name string, def time.Duration) time.Duration {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		slog.Warn("invalid duration env, using default", "name", name, "value", v, "default", def, "err", err)
		return def
	}
	return d
}

// parseIntEnv reads a base-10 integer from the named env var.
// Empty input or a parse error returns the supplied default; a
// default of 0 means "let the store package pick". Used for
// QUB-101 backup knobs (BACKUP_KEEP).
func parseIntEnv(name string, def int) int {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		slog.Warn("invalid int env, using default", "name", name, "value", v, "default", def, "err", err)
		return def
	}
	return n
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
