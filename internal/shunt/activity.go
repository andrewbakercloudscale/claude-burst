package shunt

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// A shunt is TWO log events written by two different processes: the guard
// hook logs a deny when it blocks a direct Read, and `shunt read` logs the
// read when Claude follows the redirect a few seconds later. In the raw log
// that is a red-looking REFUSED line sitting on top of a READ line, which
// reads as something failing when in fact it is the feature working.
//
// Activity folds the pair back into the one thing that happened. It is built
// at read time from the append-only log rather than written into it, because
// the two halves genuinely are separate events and the log must stay a plain
// record of them; what changes is only how they are presented.

// PendingWindow is how long a blocked read is given to be followed by a
// delegated one before it stops being "waiting" and becomes "no shunt". The
// worker itself takes 4-25 s; the rest is Claude deciding to run the command.
const PendingWindow = 2 * time.Minute

// Activity is one row of the activity list: a shunt, or the reason there
// wasn't one.
type Activity struct {
	// Event is the headline event: the delegated read or write, the guard
	// error, or -- for a block nothing answered -- the latest deny.
	Event
	// Attempts is how many blocked direct reads this row stands for. On a
	// completed shunt it is the number that redirected Claude here (0 for a
	// read Claude chose to delegate unprompted); on an unanswered block it is
	// how many times in a row the same file was blocked.
	Attempts int
	// Blocked is the first deny that led to this row, when there was one.
	Blocked *Event
	// now is when the row was built; it decides whether an unanswered block is
	// still waiting or has gone stale.
	now time.Time
}

// Fold turns raw events (newest first, as Recent returns them) into activity
// rows (newest first). now decides whether an unanswered block is still
// pending.
func Fold(events []Event, now time.Time) []Activity {
	chron := make([]Event, len(events))
	for i, e := range events {
		chron[len(events)-1-i] = e
	}

	var rows []Activity
	var open []Event // blocks not yet answered by a delegated read

	for _, e := range chron {
		switch e.Kind {
		case KindDeny:
			open = append(open, e)
		case KindRead:
			var claimed, rest []Event
			for _, d := range open {
				if answers(d, e) {
					claimed = append(claimed, d)
				} else {
					rest = append(rest, d)
				}
			}
			open = rest
			a := Activity{Event: e, Attempts: len(claimed), now: now}
			if len(claimed) > 0 {
				first := claimed[0]
				a.Blocked = &first
			}
			rows = append(rows, a)
		default:
			rows = append(rows, Activity{Event: e, now: now})
		}
	}

	// What is still open was blocked and never answered. Consecutive blocks of
	// the same file by the same session are one row, not one per retry: the
	// retrying is the story, and it is what LOOP names.
	type key struct{ actor, path string }
	groups := map[key]*Activity{}
	var order []key
	for _, d := range open {
		k := key{actor: d.Session, path: d.Path}
		if d.Session == "" {
			k.actor = "cwd:" + d.Cwd
		}
		if g, ok := groups[k]; ok {
			g.Event = d // the latest block is the headline
			g.Attempts++
			continue
		}
		first := d
		groups[k] = &Activity{Event: d, Attempts: 1, Blocked: &first, now: now}
		order = append(order, k)
	}
	for _, k := range order {
		rows = append(rows, *groups[k])
	}

	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Time.Before(rows[j].Time) })
	out := make([]Activity, len(rows))
	for i, a := range rows {
		out[len(rows)-1-i] = a
	}
	return out
}

// answers reports whether read r is the follow-up to block d: same session (or
// same project when a side has none), close enough in time to be one attempt,
// and asking about the file that was blocked.
func answers(d, r Event) bool {
	if r.Time.Before(d.Time) || r.Time.Sub(d.Time) > RepeatWindow {
		return false
	}
	if d.Session != "" && r.Session != "" {
		if d.Session != r.Session {
			return false
		}
	} else if d.Cwd != r.Cwd {
		return false
	}
	dp := filepath.Clean(d.Path)
	for _, p := range r.Paths {
		if !filepath.IsAbs(p) && r.Cwd != "" {
			p = filepath.Join(r.Cwd, p)
		}
		p = filepath.Clean(p)
		// The base name is accepted alongside the full path: the guard records
		// the resolved absolute path but `shunt read` records what was typed,
		// and a symlinked checkout makes the two differ without meaning a
		// different file. Scoped to one session inside ten minutes, a base-name
		// collision is far less likely than a spelling difference.
		if p == dp || filepath.Base(p) == filepath.Base(dp) {
			return true
		}
	}
	return false
}

// stale reports whether an unanswered block has waited past PendingWindow.
func (a Activity) stale() bool {
	return a.Event.Kind == KindDeny && !a.now.IsZero() && a.now.Sub(a.Time) > PendingWindow
}

// IsProblem is what deserves a person's attention. An unanswered block is
// deliberately not one until it is a loop: Claude legitimately reads a window
// or moves on, and a red row for that is the false alarm this type exists to
// remove.
func (a Activity) IsProblem() bool {
	if a.Event.Kind == KindDeny {
		return a.Attempts >= 3 || a.Repeat >= 2
	}
	return a.Event.IsProblem()
}

// Tag is the short label the row leads with. SHUNTED is the success; the
// failures and the not-shunted cases each get their own word.
func (a Activity) Tag() string {
	if a.Event.Kind == KindDeny {
		switch {
		case a.Attempts >= 3 || a.Repeat >= 2:
			return "LOOP"
		case a.stale():
			return "NO-SHUNT"
		}
		return "REDIRECTED"
	}
	return a.Event.Tag()
}

// Describe says what happened in one sentence.
func (a Activity) Describe() string {
	switch {
	case a.Event.Kind == KindRead && a.Event.OK:
		return a.describeShunted()
	case a.Event.Kind == KindRead:
		s := a.Event.Describe()
		if a.Attempts > 0 {
			s += " — the direct read had been blocked, so Claude has no other way to read the file"
		}
		return s
	case a.Event.Kind == KindDeny:
		return a.describeBlock()
	}
	return a.Event.Describe()
}

func (a Activity) describeShunted() string {
	e := a.Event
	s := fmt.Sprintf("read %s via %s instead of Claude: %s in, ≈%d tokens kept out of context, %.1fs",
		a.fileList(), orDefault(e.Model, "the worker"), humanBytes(e.BytesIn), e.KeptOutTokens(), float64(e.DurationMS)/1000)
	if a.Attempts > 1 {
		s += fmt.Sprintf(" (Claude tried a direct read %d times before following the redirect)", a.Attempts)
	}
	return s
}

// fileList names what was read: the files themselves when the log has them,
// else a count.
func (a Activity) fileList() string {
	var names []string
	for _, p := range a.Paths {
		names = append(names, filepath.Base(p))
	}
	switch {
	case len(names) == 0:
		return fmt.Sprintf("%d file(s)", a.Files)
	case len(names) <= 2:
		s := strings.Join(names, ", ")
		if a.Blocked != nil && len(names) == 1 && a.Blocked.Lines > 0 {
			s += fmt.Sprintf(" (%d+ lines)", a.Blocked.Lines)
		}
		return s
	}
	return fmt.Sprintf("%s, %s +%d more", names[0], names[1], len(names)-2)
}

func (a Activity) describeBlock() string {
	e := a.Event
	file := orDefault(filepath.Base(e.Path), "a file")
	what := fmt.Sprintf("a direct %s of %s (%s, %d+ lines, threshold %d)", orDefault(e.Tool, "read"), file, humanBytes(e.BytesIn), e.Lines, e.Threshold)
	switch {
	case a.Attempts >= 3 || a.Repeat >= 2:
		n := a.Attempts
		if e.Repeat+1 > n {
			n = e.Repeat + 1
		}
		return fmt.Sprintf("blocked %s — attempt #%d with no answer in between: the session is retrying instead of running shunt read", what, n)
	case a.stale():
		return fmt.Sprintf("not shunted: blocked %s and pointed Claude at shunt read, but no delegated read has followed — most likely Claude read a window of the file or moved on", what)
	}
	return fmt.Sprintf("blocked %s and pointed Claude at shunt read — waiting for it to run", what)
}
