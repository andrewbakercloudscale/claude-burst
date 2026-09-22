// Package backup keeps scripts/rollback.sh's restore point ($BACKUP_DIR/*.latest.bak)
// in sync with what claude-burst has actually written, and keeps a timestamped
// history alongside it.
//
// It exists because of a gap found live on 2026-09-21. `claude-burst shunt
// disable` writes config.json directly and never runs scripts/backup-config.sh,
// so no backup captured the "off" state; config.json.latest.bak still held an
// old snapshot from a deploy hours earlier, in which shunting was on. An
// unrelated rollback.sh run (recovering from a network outage) restored that
// stale snapshot and silently re-enabled shunting -- reinstalling the guard
// hook -- with no warning that anything had changed.
//
// The fix is not "snapshot before every write", which was tried first and does
// NOT close this gap: a before-write snapshot of shunt=true, freshly retaken at
// disable time, is still shunt=true, and a later unrelated rollback still
// restores it (proven by a test that failed against that implementation).
// latest.bak has exactly one consumer, scripts/rollback.sh, and its own header
// comment describes it as a panic button for an unrelated problem (it also
// tears down transparent mode and stops the gateway) -- not a change-specific
// undo stack. For that use, latest.bak must never be older than the config
// actually in force, so it is written from what the caller just wrote to disk,
// AFTER that write succeeds.
package backup

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Dir resolves the backups directory the same way scripts/backup-config.sh
// does: $CLAUDE_BURST_BACKUP_DIR if set, else $HOME/.config/claude-burst/backups.
// Deliberately independent of internal/config's own path helpers -- this
// package is imported BY config, so importing config back would cycle, and
// claudesettings (which has nothing to do with claude-burst's own config)
// would gain an odd dependency for no reason.
func Dir() (string, error) {
	if d := os.Getenv("CLAUDE_BURST_BACKUP_DIR"); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "claude-burst", "backups"), nil
}

// Snapshot archives path's CURRENT on-disk content, if any, as a timestamped
// file -- a historical record only. It does not touch latest.bak; see
// SetLatest for that. Call this BEFORE overwriting path, so the version about
// to be replaced isn't lost to history even though it stops being the restore
// point.
//
// A source file that does not exist yet is not an error: there is nothing to
// archive before the first write ever creates it. A failure to archive
// (permissions, a full disk) is logged to stderr but does not stop the
// caller's write -- this guards routine, frequent, reversible config changes,
// and refusing to let someone run `shunt disable` because the backups
// directory is briefly unwritable would be worse than the gap this package
// closes.
func Snapshot(path string) error {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not read %s to archive it: %v\n", path, err)
		return err
	}
	dir, err := Dir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not archive %s: %v\n", path, err)
		return err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not create backups dir %s: %v\n", dir, err)
		return err
	}
	dated := filepath.Join(dir, filepath.Base(path)+"."+time.Now().Format("20060102-150405")+".bak")
	if err := os.WriteFile(dated, b, 0600); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not write backup %s: %v\n", dated, err)
		return err
	}
	return nil
}

// SetLatest writes content to <base(path)>.latest.bak -- the ONE file
// scripts/rollback.sh actually restores. Call this AFTER writing content to
// path succeeds, passing the exact bytes just written, so latest.bak is never
// older than what claude-burst currently has in force. A later rollback (run
// for a reason unrelated to this write) then restores the current state
// rather than clobbering it with whatever the last snapshot happened to be.
func SetLatest(path string, content []byte) error {
	dir, err := Dir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not update the restore point for %s: %v\n", path, err)
		return err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not create backups dir %s: %v\n", dir, err)
		return err
	}
	latest := filepath.Join(dir, filepath.Base(path)+".latest.bak")
	if err := os.WriteFile(latest, content, 0600); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not write %s: %v\n", latest, err)
		return err
	}
	return nil
}
