package shunt

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// StageError tags an error with where it happened, so the log can say "failed
// at worker_call" instead of just "failed".
type StageError struct {
	Stage string
	Err   error
}

func (e *StageError) Error() string { return e.Err.Error() }
func (e *StageError) Unwrap() error { return e.Err }

// stage wraps err with a stage, keeping an inner stage if one is already set:
// the place a failure began is the useful one, not the place it was rethrown.
func stage(st string, err error) error {
	if err == nil {
		return nil
	}
	var se *StageError
	if errors.As(err, &se) {
		return err
	}
	return &StageError{Stage: st, Err: err}
}

// StageOf reports the stage an error carries, or "error" when it has none.
func StageOf(err error) string {
	var se *StageError
	if errors.As(err, &se) {
		return se.Stage
	}
	return "error"
}

// ShortSession is the first 8 characters of a session id: enough to tell
// sessions apart at a glance, and what every display uses.
func ShortSession(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// Project is the last element of the working directory: the name a person
// recognises a project by.
func (e Event) Project() string {
	if e.Cwd == "" {
		return ""
	}
	return filepath.Base(e.Cwd)
}

// IsProblem reports whether the event is something a person should look at: a
// failure, a guard that could not decide, or a refusal that is repeating.
func (e Event) IsProblem() bool {
	return !e.OK || e.Kind == KindGuardError || (e.Kind == KindDeny && e.Repeat >= 2)
}

// Tag is the short label a log line leads with.
func (e Event) Tag() string {
	switch {
	case e.Kind == KindGuardError:
		return "GUARD-ERR"
	case e.Kind == KindDeny && e.Repeat >= 2:
		return "LOOP"
	case e.Kind == KindDeny:
		return "REFUSED"
	case !e.OK && e.Kind == KindRead:
		return "READ-FAIL"
	case !e.OK && e.Kind == KindWrite:
		return "WRITE-FAIL"
	case e.Kind == KindRead:
		return "READ"
	case e.Kind == KindWrite:
		return "WRITE"
	}
	return strings.ToUpper(e.Kind)
}

// Describe says what happened in one sentence, and is the single wording used
// by `shunt log`, `shunt status` and the dashboard, so they cannot disagree. It
// deliberately leaves out the time, project and session: those are columns.
func (e Event) Describe() string {
	file := ""
	if e.Path != "" {
		file = filepath.Base(e.Path)
	}
	switch e.Kind {
	case KindDeny:
		s := fmt.Sprintf("refused a direct %s of %s (%s, %d+ lines, threshold %d)", orDefault(e.Tool, "read"), orDefault(file, "a file"), humanBytes(e.BytesIn), e.Lines, e.Threshold)
		switch {
		case e.Repeat >= 2:
			s += fmt.Sprintf(" — refusal #%d of this file with no answer in between: the session is retrying instead of running shunt read", e.Repeat+1)
		case e.Repeat == 1:
			s += " — second refusal of this file"
		}
		return s
	case KindGuardError:
		return fmt.Sprintf("guard could not decide (%s): %s — the call was ALLOWED through", orDefault(e.Stage, "error"), orDefault(e.Note, "no detail"))
	case KindRead:
		if !e.OK {
			return fmt.Sprintf("delegated read FAILED at %s: %s", orDefault(e.Stage, "error"), orDefault(e.Note, "no detail"))
		}
		return fmt.Sprintf("delegated read of %d file(s) to %s: %s in, ≈%d tokens kept out of context, %.1fs",
			e.Files, orDefault(e.Model, "the worker"), humanBytes(e.BytesIn), e.KeptOutTokens(), float64(e.DurationMS)/1000)
	case KindWrite:
		target := orDefault(file, "a file")
		if !e.OK {
			return fmt.Sprintf("code write of %s FAILED at %s: %s", target, orDefault(e.Stage, "error"), orDefault(e.Note, "no detail"))
		}
		return fmt.Sprintf("generated %s (%s) straight to disk via %s, %.1fs", target, humanBytes(e.BytesOut), orDefault(e.Model, "the worker"), float64(e.DurationMS)/1000)
	}
	return e.Kind
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// RepeatWindow is how far back a refusal counts as "the same attempt again".
const RepeatWindow = 10 * time.Minute

// RepeatCount reports how many times this session has already been refused this
// file inside RepeatWindow without getting a delegated answer since. A session
// that follows the redirect runs shunt read and the count resets; a session that
// retries the same cat does not, and that is the failure worth naming.
//
// Sessions are compared when both sides have one; otherwise the project
// directory stands in, which is right for a session-less caller and merely
// coarse for two sessions in one project.
func RepeatCount(logPath string, e Event, now time.Time) int {
	prev, err := Recent(logPath, 300, nil)
	if err != nil {
		return 0
	}
	same := func(p Event) bool {
		if e.Session != "" && p.Session != "" {
			return p.Session == e.Session
		}
		return p.Cwd == e.Cwd
	}
	n := 0
	for _, p := range prev { // newest first
		if now.Sub(p.Time) > RepeatWindow {
			break
		}
		if !same(p) {
			continue
		}
		if p.Kind == KindRead && p.OK {
			break // it got an answer: whatever came before is resolved
		}
		if p.Kind == KindDeny && p.Path == e.Path {
			n++
		}
	}
	return n
}
