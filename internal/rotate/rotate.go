// Package rotate implements simple size-based log rotation, shared by the
// gateway's text log (a long-lived io.Writer) and its metrics.jsonl (opened
// fresh on every write). One rotation algorithm, not two: before this
// package existed, neither file had any rotation at all -- both grew
// forever for as long as the gateway ran, which for a LaunchAgent means
// indefinitely. RotateIfOversized is the operation itself; Writer wraps it
// for callers (like a *log.Logger) that hold one io.Writer open for the
// process's entire lifetime rather than reopening per write.
package rotate

import (
	"fmt"
	"os"
	"sync"
)

// RotateIfOversized checks path's current size and, if it exceeds maxBytes,
// shifts path -> path.1 -> path.2 -> ... up to maxBackups, discarding
// whatever was at path.maxBackups. A missing file is not an error -- there
// is nothing to rotate, which is the common case on first run. maxBackups
// <= 0 means no backups are kept: the oversized file is simply removed, so
// the next write starts a fresh one.
func RotateIfOversized(path string, maxBytes int64, maxBackups int) error {
	st, err := os.Stat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if st.Size() < maxBytes {
		return nil
	}
	return shiftBackups(path, maxBackups)
}

// shiftBackups is the actual rotation, unconditional: path -> path.1 ->
// path.2 -> ... up to maxBackups, discarding whatever was at
// path.maxBackups. Split out from RotateIfOversized so Writer.rotate can
// reuse it directly once it has already decided to rotate, rather than
// re-deriving a size threshold that would force one.
func shiftBackups(path string, maxBackups int) error {
	if maxBackups <= 0 {
		return os.Remove(path)
	}
	// Oldest first, shifted up: path.(N-1) -> path.N overwrites whatever
	// was already at path.N (os.Rename replaces an existing destination on
	// POSIX), which is exactly how the oldest backup gets discarded.
	for i := maxBackups - 1; i >= 1; i-- {
		old := fmt.Sprintf("%s.%d", path, i)
		if _, err := os.Stat(old); err == nil {
			if err := os.Rename(old, fmt.Sprintf("%s.%d", path, i+1)); err != nil {
				return err
			}
		}
	}
	return os.Rename(path, fmt.Sprintf("%s.1", path))
}

// Writer is an io.Writer over path that rotates via RotateIfOversized
// whenever the next write would push it past maxBytes, and is otherwise a
// plain append-only file. Safe for concurrent use, since *log.Logger calls
// its Writer from whichever goroutine is logging.
type Writer struct {
	mu         sync.Mutex
	path       string
	maxBytes   int64
	maxBackups int
	f          *os.File
	size       int64
}

func NewWriter(path string, maxBytes int64, maxBackups int) *Writer {
	return &Writer{path: path, maxBytes: maxBytes, maxBackups: maxBackups}
}

func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		if err := w.open(); err != nil {
			return 0, err
		}
	}
	if w.size+int64(len(p)) > w.maxBytes {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	return n, err
}

func (w *Writer) open() error {
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	w.f = f
	w.size = st.Size()
	return nil
}

func (w *Writer) rotate() error {
	if w.f != nil {
		if err := w.f.Close(); err != nil {
			return err
		}
		w.f = nil
	}
	if err := shiftBackups(w.path, w.maxBackups); err != nil {
		return err
	}
	w.size = 0
	return w.open()
}

// Close closes the underlying file, if open. Safe to call even if Write was
// never called.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}
