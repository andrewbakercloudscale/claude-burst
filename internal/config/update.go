package config

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

// Update is the ONE way to change config.json: load, modify, save under a
// lock, so two writers cannot each load the same old file and have the
// second save silently undo the first.
//
// Found by the 2026-10-02 review: every writer (the dashboard's settings,
// secondary, pruning, compaction and keep-awake handlers, the shunt and
// coordination toggles, `claude-burst configure`) did Load, change one
// field, Save, with nothing between them. Two dashboard saves a moment apart,
// or a dashboard save racing a `claude-burst shunt on` in a terminal, lost
// whichever change landed first, and nothing said so.
//
// Two locks because there are two kinds of writer: a process mutex for
// goroutines in this process (flock is per open file description, so two
// goroutines would each get their own and both "hold" it), and an flock on a
// lock file beside config.json for the CLI and the gateway, which are
// separate processes. The file is re-read INSIDE the lock, which is the
// point: fn always sees the latest saved config, never a copy loaded before
// someone else's save.
//
// fn returning an error aborts the update and nothing is written. A config
// that does not parse is returned as *UnreadableError and is never
// overwritten: saving defaults over a hand-edited file with one typo would
// destroy everything else in it.
func Update(fn func(*Config) error) error {
	updateMu.Lock()
	defer updateMu.Unlock()

	if err := EnsureDir(); err != nil {
		return err
	}
	unlock, err := lockConfigFile()
	if err != nil {
		return err
	}
	defer unlock()

	cfg, err := Load()
	if err != nil {
		return &UnreadableError{Err: err}
	}
	if err := fn(&cfg); err != nil {
		return err
	}
	return Save(cfg)
}

// UnreadableError is Update refusing to modify a config.json it cannot read
// or parse.
type UnreadableError struct{ Err error }

func (e *UnreadableError) Error() string { return e.Err.Error() }
func (e *UnreadableError) Unwrap() error { return e.Err }

// IsUnreadable reports whether err is Update refusing an unparseable config.
func IsUnreadable(err error) bool {
	var u *UnreadableError
	return errors.As(err, &u)
}

var updateMu sync.Mutex

// lockConfigFile takes an exclusive flock on config.json.lock. A separate
// file rather than config.json itself, because Save replaces config.json by
// rename: a lock on the old inode would not exclude a writer that opened the
// new one. The lock is released when the descriptor closes, so a writer that
// crashes cannot leave it held.
func lockConfigFile() (func(), error) {
	d, err := ConfigDir()
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(d, "config.json.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if err != syscall.EINTR {
			break
		}
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
