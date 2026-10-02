// Package atomicfile replaces a file's contents so that a reader, or the next
// start after a crash, sees either the old bytes or the new ones, never a
// truncated mix.
//
// os.WriteFile truncates first and writes second. The 2026-10-02 review
// found it on config.json (a truncated one makes the gateway fatal-loop at
// launch, under launchd, with nothing to say why), on Claude Code's own
// ~/.claude/settings.json (a truncated one is Claude Code's problem, and the
// user's), on router state, handover files and backups. A crash, a full
// disk or a kill -9 in between left an empty or half-written file.
package atomicfile

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// Write replaces path with data: a temp file in the same directory (so the
// rename cannot cross a filesystem), fsync, rename over the target, then a
// best-effort fsync of the directory so the rename itself survives a power
// cut.
//
// perm applies only when path does not exist yet. An existing file keeps its
// mode, which is what os.WriteFile did and what a user who chmod'ed their own
// settings.json expects.
//
// A symlink at path is followed and its TARGET replaced. Renaming over the
// link itself would silently turn a dotfiles-managed ~/.claude/settings.json
// into a plain file, cutting it off from the repo it lives in.
func Write(path string, data []byte, perm os.FileMode) error {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	mode := perm
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			tmp.Close()
			os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	// CreateTemp makes the file 0600; set the real mode before the rename so
	// the target is never visible with the wrong one.
	if err := tmp.Chmod(mode); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	ok = true
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}
