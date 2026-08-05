package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// BackupDefaults holds the defaults applied when the caller does
// not supply a value. The 24h/30-day combo matches the issue
// spec: "daily, keep last 30."
const (
	DefaultBackupEvery = 24 * time.Hour
	DefaultBackupKeep  = 30
)

// BackupResult reports the outcome of a single backup pass.
type BackupResult struct {
	Path        string
	Bytes       int64
	Pruned      int
	DurationMS  int64
}

// Backup writes a consistent snapshot of the SQLite database to
// the named directory and prunes old snapshots, returning the
// path of the file just written and the number of files
// pruned. The snapshot is produced via SQLite's `VACUUM INTO`
// command (single-statement, atomic, no driver-specific code).
//
// The directory is created if it does not exist. Filenames are
// `boop-YYYY-MM-DD.db`; an existing file for the same date is
// overwritten (a re-run of the same day's backup is
// idempotent). `keep` is the number of daily snapshots to
// retain; the rest are deleted from the directory. keep<=0
// means "keep all" (no pruning).
//
// The function is safe to call from a single goroutine. The
// returned error is the first failure encountered; the caller
// should log it and let the next tick retry.
func (s *Store) Backup(ctx context.Context, dir string, keep int) (BackupResult, error) {
	var res BackupResult
	if s == nil || s.db == nil {
		return res, fmt.Errorf("store: nil")
	}
	if dir == "" {
		return res, fmt.Errorf("store: backup: empty dir")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return res, fmt.Errorf("store: backup mkdir: %w", err)
	}
	now := time.Now().UTC()
	filename := fmt.Sprintf("boop-%s.db", now.Format("2006-01-02"))
	dest := filepath.Join(dir, filename)

	start := time.Now()
	// VACUUM INTO requires the destination file to not exist;
	// remove any stale file from a prior failed backup so the
	// statement does not error out. The new file replaces it
	// atomically as far as readers are concerned: a concurrent
	// reader that has already opened dest by inode will keep
	// reading the old content.
	_ = os.Remove(dest)
	if _, err := s.db.ExecContext(ctx, `VACUUM INTO ?`, dest); err != nil {
		return res, fmt.Errorf("store: vacuum into: %w", err)
	}
	res.Path = dest
	res.DurationMS = time.Since(start).Milliseconds()

	if info, err := os.Stat(dest); err == nil {
		res.Bytes = info.Size()
	}

	if keep > 0 {
		n, err := pruneOldBackups(dir, keep, now)
		if err != nil {
			// Prune failure is not fatal — the backup itself
			// landed. Log via the caller's caller.
			slog.Warn("backup: prune failed", "err", err)
		} else {
			res.Pruned = n
		}
	}

	slog.Info("backup ok", "path", res.Path, "bytes", res.Bytes, "pruned", res.Pruned, "duration_ms", res.DurationMS)
	return res, nil
}

// pruneOldBackups lists the daily snapshots in dir, sorts
// newest-first by filename, and deletes everything past the
// `keep`th entry. The filename is `boop-YYYY-MM-DD.db`; the
// date sorts lexically, which is also chronological, so no
// per-file timestamp is needed.
func pruneOldBackups(dir string, keep int, now time.Time) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	var backups []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, "boop-") || !strings.HasSuffix(name, ".db") {
			continue
		}
		backups = append(backups, name)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(backups)))
	if len(backups) <= keep {
		return 0, nil
	}
	pruned := 0
	for _, name := range backups[keep:] {
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			return pruned, err
		}
		pruned++
	}
	return pruned, nil
}

// RunBackup is the periodic backup pass. Designed to be called
// from a goroutine on a ticker. Errors are logged but do not
// stop the loop — a transient failure (disk full, target
// PVC not mounted) is recoverable on the next tick.
func (s *Store) RunBackup(ctx context.Context, dir string, keep int) error {
	if dir == "" {
		return nil
	}
	if keep <= 0 {
		keep = DefaultBackupKeep
	}
	if _, err := s.Backup(ctx, dir, keep); err != nil {
		// ErrEnotExist for /backups means the PVC isn't
		// mounted. The receiver can still serve webhooks; we
		// just log and try again next tick.
		if errors.Is(err, os.ErrNotExist) {
			slog.Warn("backup: destination not mounted", "dir", dir)
			return err
		}
		slog.Warn("backup: failed", "err", err)
		return err
	}
	return nil
}
