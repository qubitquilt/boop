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
	"strings"
	"syscall"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/michaelruelas/boop-receiver/internal/dashboard"
	boopgithub "github.com/michaelruelas/boop-receiver/internal/github"
	"github.com/michaelruelas/boop-receiver/internal/webhook"
)

// acceptsJSON reports whether the Accept header is
// missing, is `*/*`, or contains `application/json` or
// `application/*`. The two permissive cases (missing
// header, `*/*`) match curl's default behavior: a client
// that does not pick a content type is implicitly
// consenting to anything, and JSON is the only thing on
// offer. A browser that hits the URL with `Accept:
// text/html,...` is rejected with 406, which is the
// right answer for an API that has no HTML surface to
// render. The CLI and any tailnet agent that sets
// `Accept: application/json` pass.
func acceptsJSON(accept string) bool {
	if accept == "" || accept == "*/*" {
		return true
	}
	for _, part := range strings.Split(accept, ",") {
		mediaType := strings.TrimSpace(strings.SplitN(part, ";", 2)[0])
		if mediaType == "application/json" || mediaType == "application/*" {
			return true
		}
	}
	return false
}

// enforceJSONAccept wraps a handler so a GET request
// without an Accept header that includes application/json
// is rejected with 406. POSTs pass through unchanged
// (the body shape is a request contract, not a response
// shape). The /webhook and /health routes are not gated
// — the former because GitHub does not send Accept, the
// latter because cluster probers usually omit it.
func enforceJSONAccept(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && !acceptsJSON(r.Header.Get("Accept")) {
			w.Header().Set("Accept", "application/json")
			http.Error(w, "Accept: application/json required", http.StatusNotAcceptable)
			return
		}
		next(w, r)
	}
}

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
	// QUB-115: log the public surface so an operator can
	// confirm the Ingress is wired correctly without
	// `curl /api/runs` from a tailnet node. The URL comes
	// from PUBLIC_API_URL on the receiver Deployment; if
	// unset we fall back to the in-cluster address (no
	// public exposure). The receiver does not validate the
	// URL — the operator is responsible for the value.
	if publicURL := os.Getenv("PUBLIC_API_URL"); publicURL != "" {
		logger.Info("public api", "url", publicURL)
	} else {
		logger.Info("public api", "url", "(unset — /api/* and /dashboard/* are in-cluster only; reach them via port-forward)")
	}

	// QUB-115: wrap the public API surface in a small
	// middleware that enforces `Accept: application/json`
	// on GETs. The CLI and a tailnet agent both send the
	// header by default; a browser that hits the URL
	// directly does not, and a 406 keeps the contract
	// explicit. The /webhook path is GitHub's HMAC; we do
	// not gate the webhook on Accept (GitHub does not send
	// it). The /health path is internal liveness; it also
	// does not require Accept (cluster probers usually omit
	// it).
	mux := http.NewServeMux()
	mux.HandleFunc("POST /webhook", h.HandleWebhook)
	mux.HandleFunc("GET /health", h.Health)
	mux.HandleFunc("GET /api/reviews", enforceJSONAccept(h.ListReviews))
	mux.HandleFunc("GET /api/installations", enforceJSONAccept(h.ListInstallations))
	mux.HandleFunc("GET /api/runs", enforceJSONAccept(h.ListRuns))
	mux.HandleFunc("GET /api/runs/{id}", enforceJSONAccept(h.GetRun))
	mux.HandleFunc("GET /api/stats", enforceJSONAccept(h.Stats))
	mux.HandleFunc("POST /api/runs/{id}/telemetry", h.RecordTelemetry)
	mux.HandleFunc("POST /api/runs/{id}/status", h.RecordStatus)
	// QUB-109: runner instrumentation. Stage POSTs (the
	// waterfall) and 30s heartbeats (the stuck-runs panel)
	// are receiver-stamped; the runner's clock is never the
	// source of truth.
	mux.HandleFunc("POST /api/runs/{id}/stages", h.RecordStage)
	mux.HandleFunc("POST /api/runs/{id}/heartbeat", h.RecordHeartbeat)
	// Per-lens telemetry (Phase 4's "lens is the row grain"
	// rule). Batch replace — re-runs land on the same shape.
	mux.HandleFunc("POST /api/runs/{id}/lens_telemetry", h.RecordLensTelemetry)
	// QUB-110 / QUB-113: re-run lineage. The preview
	// endpoint is a GET (the operator-facing diff);
	// the rerun endpoint is a POST that mints a new
	// run + K8s Job. The dashboard's form-based
	// requeue posts to /dashboard/runs/{id}/rerun
	// (which lands here via the cross-package
	// callback) so the audit row has the same shape
	// regardless of which path the operator uses.
	mux.HandleFunc("GET /api/runs/{id}/rerun-preview", h.RerunPreview)
	mux.HandleFunc("POST /api/runs/{id}/rerun", h.Rerun)

	// QUB-111: dashboard. The BOOP_DASHBOARD_TOKEN env
	// var gates the /dashboard/* routes; an empty value
	// keeps the dashboard disabled (every request 401)
	// so a fresh install does not accidentally expose
	// the operator UI.
	dashboardToken := os.Getenv("BOOP_DASHBOARD_TOKEN")
	if dashboardToken == "" {
		logger.Warn("BOOP_DASHBOARD_TOKEN is unset: /dashboard/* will return 401 (operator UI is opt-in)")
	}
	// QUB-113: wire the dashboard's cross-package
	// actions. The form-based re-run button on the
	// exceptions view needs to mint a new K8s Job;
	// that requires the K8s client + cfg which only
	// the webhook package holds. The callback keeps
	// the dashboard package from importing webhook
	// (which would be a cycle once webhook starts
	// wanting to call back into dashboard).
	//
	// FetchPodLogs is the run-detail page's on-demand
	// log fetcher: the dashboard wants the raw
	// runner stdout for the run, and only the
	// webhook package holds the kube client.
	dash, err := dashboard.NewHandler(h.Store(), logger, dashboardToken, dashboard.Actions{
		CreateRerunJob: h.CreateRerunJob,
		FetchPodLogs:   h.FetchPodLogs,
	})
	if err != nil {
		logger.Error("init dashboard", "err", err)
		os.Exit(1)
	}
	dash.RegisterRoutes(mux)
	mux.HandleFunc("GET /dashboard/health", dash.Health)

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
