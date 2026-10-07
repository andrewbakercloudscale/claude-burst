package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// A Go process that dies of a panic or a runtime fault writes its last
// words to stderr, which launchd appends to launchd.err.log: a file with no
// dates on those lines, no rotation, and until 7 Oct 2026 no mention in the
// log everyone reads. So a crash was a stack trace nobody could date in a
// file nobody opened. Each start now does three things with that file:
// keeps it to a size, says in the gateway's log (and the audit) when the
// run before it ended in a crash, and writes a dated line so what follows
// can be told from what came before.

const (
	// stderrLogMax is where launchd.err.log is cut back, and stderrLogKeep
	// what is kept of its end.
	stderrLogMax  = 4 << 20
	stderrLogKeep = 1 << 20
	// startMarker begins the line each start writes to stderr.
	startMarker = "=== claude-burst "
)

// crashLines are how the Go runtime begins its last words.
var crashLines = []string{"panic: ", "fatal error: ", "runtime: out of memory", "SIGSEGV", "SIGBUS", "signal SIGABRT"}

// previousCrash reads the end of the stderr log at path and returns the
// first line of the crash the last run ended in, "" when it ended otherwise.
// Only what follows the last start line counts, so one crash is reported
// once however many times the gateway starts after it.
func previousCrash(path string) string {
	b := tailBytes(path, 256<<10)
	if i := bytes.LastIndex(b, []byte(startMarker)); i >= 0 {
		b = b[i:]
		if nl := bytes.IndexByte(b, '\n'); nl >= 0 {
			b = b[nl+1:]
		}
	}
	for _, line := range strings.Split(string(b), "\n") {
		for _, c := range crashLines {
			if strings.HasPrefix(line, c) || (strings.HasPrefix(c, "SIG") && strings.Contains(line, c)) {
				if len(line) > 300 {
					line = line[:300] + "..."
				}
				return line
			}
		}
	}
	return ""
}

// tailBytes is up to n bytes from the end of path, nil when unreadable.
func tailBytes(path string, n int64) []byte {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil
	}
	off := st.Size() - n
	if off < 0 {
		off = 0
	}
	b, err := io.ReadAll(io.NewSectionReader(f, off, st.Size()-off))
	if err != nil {
		return nil
	}
	return b
}

// trimStderrLog cuts path back to its last keep bytes once it is over max.
// In place, never by renaming: launchd holds the file open for appending,
// so a renamed file would go on receiving the output under its new name.
func trimStderrLog(path string, max, keep int64) (trimmed bool, err error) {
	st, err := os.Stat(path)
	if err != nil || st.Size() <= max {
		return false, nil
	}
	b := tailBytes(path, keep)
	if nl := bytes.IndexByte(b, '\n'); nl >= 0 {
		b = b[nl+1:] // the first line may be cut
	}
	head := fmt.Sprintf("(cut back from %d bytes on %s: the oldest lines were dropped)\n", st.Size(), time.Now().Format("2006-01-02 15:04:05 -07:00"))
	return true, os.WriteFile(path, append([]byte(head), b...), st.Mode().Perm())
}

// startLine is the dated line a start writes to stderr.
func startLine(version string, now time.Time) string {
	return fmt.Sprintf("%s%s started %s pid %d ===", startMarker, version, now.Format("2006-01-02 15:04:05 -07:00"), os.Getpid())
}
