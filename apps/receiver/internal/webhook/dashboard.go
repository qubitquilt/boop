// Package webhook: dashboard data-layer primitives (RF-007 split).
//
// After the split, dashboard.go owns the cross-cutting helpers
// (auth gate, FK-error detection) and the four background loops
// (installations poller, retention, backup). The HTTP handlers
// live in dashboard_get.go (ListInstallations, ListRuns,
// GetRun, Stats) and dashboard_post.go (RecordTelemetry,
// RecordStatus, RecordStage, RecordHeartbeat, RecordLensTelemetry).
// The split keeps the file under 500 LOC so a future dashboard
// cross-cutting has a single short file to navigate, instead of
// the 951-LOC god-handler that the audit flagged.

package webhook

import (
	"context"
	"crypto/subtle"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/michaelruelas/boop-receiver/internal/store"
)

// checkRunnerToken compares the request's X-BOOP-Runner-Token
// against h.cfg.RunnerToken using a constant-time compare. An
// empty Config.RunnerToken rejects every request — the
// receiver never accepts a runner POST unless the operator
// opted in by setting the env var.
func (h *Handler) checkRunnerToken(r *http.Request) bool {
	if h.cfg.RunnerToken == "" {
		return false
	}
	got := r.Header.Get("X-BOOP-Runner-Token")
	if got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(h.cfg.RunnerToken)) == 1
}

// isForeignKeyError reports whether err is a SQLite
// "FOREIGN KEY constraint failed" error. The error
// message is the only stable signal (sqlite3 does not
// export typed errors for FK violations); matching the
// substring is the same approach the rest of the store
// uses for parse-error detection. A driver swap would
// need to add a typed wrapper here; today the
// dependency on mattn/go-sqlite3 keeps the message
// stable.
func isForeignKeyError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "FOREIGN KEY constraint failed")
}

// startInstallationsPoller kicks off a background goroutine that
// refreshes the installations table from GitHub on a fixed
// interval. The poller is best-effort: a transient GitHub API
// error is logged and the previous cached data is left in
// place. The handler returns a pointer to a stop function so
// main can shut the poller down on signal.
//
// interval is clamped to a minimum of 1 minute to avoid an
// unbounded-tight loop on a misconfigured 0 or negative value.
func (h *Handler) StartInstallationsPoller(ctx context.Context, interval time.Duration) func() {
	if h.store == nil || h.ghClient == nil {
		return func() {}
	}
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	if interval < time.Minute {
		interval = time.Minute
	}
	pollerCtx, cancel := context.WithCancel(ctx)
	go func() {
		// First tick after a small delay so the receiver has
		// time to bind its port before we start hammering
		// GitHub.
		t := time.NewTimer(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-pollerCtx.Done():
				return
			case <-t.C:
			}
			if err := h.refreshInstallations(pollerCtx); err != nil {
				h.logger.Warn("installations poll", "err", err)
			}
			t.Reset(interval)
		}
	}()
	return cancel
}

func (h *Handler) refreshInstallations(ctx context.Context) error {
	fresh, err := h.ghClient.ListInstallations(ctx)
	if err != nil {
		return fmt.Errorf("fetch: %w", err)
	}
	installs := make([]store.Installation, len(fresh))
	for i, ins := range fresh {
		installs[i] = store.Installation{
			ID:                  ins.ID,
			AccountLogin:        ins.AccountLogin,
			AccountType:         ins.AccountType,
			RepositorySelection: ins.RepositorySelection,
			InstalledAt:         ins.InstalledAt,
		}
	}
	if err := h.store.UpsertInstallations(ctx, installs); err != nil {
		return fmt.Errorf("upsert: %w", err)
	}
	h.logger.Info("installations refreshed", "count", len(installs))
	return nil
}

// StartRetentionLoop kicks off the periodic cleanup pass that
// prunes old runs, runs a WAL checkpoint, and (weekly) runs
// incremental_vacuum. The loop is best-effort: each tick
// runs independently and a transient error is logged and
// swallowed. The returned cancel func stops the goroutine
// (safe to call multiple times).
//
// retention is the time window before "now" used as the
// PruneRuns cutoff (0 = store.DefaultRetention, 365 days).
// cleanupEvery is the tick period (0 = store.DefaultCleanupEvery,
// 5 min). vacuumInterval is the minimum time between
// incremental_vacuum calls (0 = store.DefaultVacuumInterval, 7
// days). The receiver logs the resolved values on startup so
// an operator can see the effective schedule.
func (h *Handler) StartRetentionLoop(ctx context.Context, retention, cleanupEvery, vacuumInterval time.Duration) func() {
	if h.store == nil {
		return func() {}
	}
	if retention <= 0 {
		retention = store.DefaultRetention
	}
	if cleanupEvery <= 0 {
		cleanupEvery = store.DefaultCleanupEvery
	}
	if vacuumInterval <= 0 {
		vacuumInterval = store.DefaultVacuumInterval
	}
	// Floor the cleanup tick at 30s to keep a misconfigured
	// zero/negative value from busy-looping the receiver.
	if cleanupEvery < 30*time.Second {
		cleanupEvery = 30 * time.Second
	}
	h.logger.Info("retention loop starting",
		"retention", retention,
		"cleanup_every", cleanupEvery,
		"vacuum_interval", vacuumInterval,
	)
	pollerCtx, cancel := context.WithCancel(ctx)
	go func() {
		// First tick after a small delay so the receiver has
		// time to bind its port and serve the readiness
		// probe before we start hammering SQLite.
		t := time.NewTimer(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-pollerCtx.Done():
				return
			case <-t.C:
			}
			tickCtx, tickCancel := context.WithTimeout(pollerCtx, 2*time.Minute)
			if _, err := h.store.RunRetention(tickCtx, retention, vacuumInterval); err != nil {
				h.logger.Warn("retention tick failed", "err", err)
			}
			tickCancel()
			t.Reset(cleanupEvery)
		}
	}()
	return cancel
}

// StartBackupLoop kicks off the periodic snapshot pass. Each
// tick writes a daily VACUUM-INTO snapshot to dir and prunes
// older entries. The receiver is one replica and the data
// PVC is RWO, so the backup has to happen in-process — a
// separate CronJob would not be able to mount the same PVC
// while the receiver holds it. The trade-off is that the
// backup is only as fresh as the receiver is alive; for
// point-in-time restore on a dead receiver, restore from the
// most recent snapshot and accept the gap.
//
// dir is the destination directory (typically /backups,
// backed by the boop-receiver-backups PVC). Empty dir
// disables the loop. every is the period (0 = 24h). keep is
// the number of daily snapshots to retain (0 = 30). All
// defaults live in the store package; this method just
// forwards them.
//
// The returned cancel func is safe to call multiple times.
// First tick is delayed by 15s so the receiver has time to
// serve the readiness probe and bind /backups before the
// first VACUUM-INTO call.
func (h *Handler) StartBackupLoop(ctx context.Context, dir string, every time.Duration, keep int) func() {
	if h.store == nil || dir == "" {
		return func() {}
	}
	if every <= 0 {
		every = store.DefaultBackupEvery
	}
	if keep <= 0 {
		keep = store.DefaultBackupKeep
	}
	h.logger.Info("backup loop starting", "dir", dir, "every", every, "keep", keep)
	pollerCtx, cancel := context.WithCancel(ctx)
	go func() {
		t := time.NewTimer(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-pollerCtx.Done():
				return
			case <-t.C:
			}
			tickCtx, tickCancel := context.WithTimeout(pollerCtx, 10*time.Minute)
			if err := h.store.RunBackup(tickCtx, dir, keep); err != nil {
				h.logger.Warn("backup tick failed", "err", err)
			}
			tickCancel()
			t.Reset(every)
		}
	}()
	return cancel
}
