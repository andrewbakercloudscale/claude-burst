package shunt

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStageErrorsKeepTheFirstStage(t *testing.T) {
	base := errors.New("boom")
	inner := stage(StageWorkerCall, base)
	if StageOf(inner) != StageWorkerCall {
		t.Fatalf("stage lost")
	}
	// the place a failure began is the useful one, not where it was rethrown
	if StageOf(stage(StageArgs, inner)) != StageWorkerCall {
		t.Errorf("an inner stage must win over an outer one")
	}
	if StageOf(fmt.Errorf("wrapped: %w", inner)) != StageWorkerCall {
		t.Errorf("a stage must survive %%w wrapping")
	}
	if StageOf(base) != "error" || stage(StageArgs, nil) != nil {
		t.Errorf("untagged errors report \"error\"; nil stays nil")
	}
	if !errors.Is(inner, base) {
		t.Errorf("a stage error must still unwrap to its cause")
	}
}

func TestDecideNamesTheToolThatWasRefused(t *testing.T) {
	dir := t.TempDir()
	big := writeLines(t, dir, "big.go", 500)
	if d := Decide(readIn(big), opt); d.Tool != "Read" {
		t.Errorf("tool = %q", d.Tool)
	}
	for cmd, want := range map[string]string{"cat big.go": "Bash cat", "head -n 400 big.go": "Bash head", "less big.go": "Bash less", "tail -n +1 big.go": "Bash tail"} {
		if d := Decide(bashIn(cmd, dir), opt); !d.Deny || d.Tool != want {
			t.Errorf("%q: deny=%v tool=%q, want %q", cmd, d.Deny, d.Tool, want)
		}
	}
}

func TestTagsAndProblems(t *testing.T) {
	cases := []struct {
		e       Event
		tag     string
		problem bool
	}{
		{Event{Kind: KindDeny, OK: true}, "REFUSED", false},
		{Event{Kind: KindDeny, OK: true, Repeat: 1}, "REFUSED", false}, // a second refusal is not yet a loop
		{Event{Kind: KindDeny, OK: true, Repeat: 2}, "LOOP", true},
		{Event{Kind: KindRead, OK: true}, "READ", false},
		{Event{Kind: KindRead, OK: false}, "READ-FAIL", true},
		{Event{Kind: KindWrite, OK: true}, "WRITE", false},
		{Event{Kind: KindWrite, OK: false}, "WRITE-FAIL", true},
		{Event{Kind: KindGuardError}, "GUARD-ERR", true},
	}
	for _, c := range cases {
		if c.e.Tag() != c.tag || c.e.IsProblem() != c.problem {
			t.Errorf("%+v: tag=%q problem=%v, want %q %v", c.e, c.e.Tag(), c.e.IsProblem(), c.tag, c.problem)
		}
	}
}

func TestDescribeSaysWhatHappenedAndWhere(t *testing.T) {
	deny := Event{Kind: KindDeny, OK: true, Tool: "Bash cat", Path: "/p/wporg-ready/shared/php-parse.php", BytesIn: 26719, Lines: 931, Threshold: 350}
	got := deny.Describe()
	for _, w := range []string{"refused a direct Bash cat of php-parse.php", "26.1 KB", "931+ lines", "threshold 350"} {
		if !strings.Contains(got, w) {
			t.Errorf("deny description missing %q: %s", w, got)
		}
	}
	deny.Repeat = 3
	if d := deny.Describe(); !strings.Contains(d, "refusal #4") || !strings.Contains(d, "retrying instead of running shunt read") {
		t.Errorf("a loop must be named as one: %s", d)
	}
	fail := Event{Kind: KindRead, OK: false, Stage: StageWorkerInit, Note: "no API key"}
	if d := fail.Describe(); !strings.Contains(d, "FAILED at worker_init") || !strings.Contains(d, "no API key") {
		t.Errorf("a failure must say where and why: %s", d)
	}
	if d := (Event{Kind: KindGuardError, Stage: StageInput, Note: "bad json"}).Describe(); !strings.Contains(d, "ALLOWED through") {
		t.Errorf("a guard error must say the call was let through: %s", d)
	}
	if (Event{Cwd: "/Users/x/proj/wporg-ready"}).Project() != "wporg-ready" || (Event{}).Project() != "" {
		t.Errorf("project is the last element of cwd")
	}
	if ShortSession("aaaa1111-2222") != "aaaa1111" || ShortSession("abc") != "abc" {
		t.Errorf("short session is the first 8 characters")
	}
}

func TestRepeatCount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shunt.jsonl")
	now := time.Now()
	file := "/p/big.go"
	deny := func(ago time.Duration, session string) Event {
		return Event{Time: now.Add(-ago), Kind: KindDeny, OK: true, Session: session, Cwd: "/p", Path: file}
	}
	probe := Event{Kind: KindDeny, Session: "S1", Cwd: "/p", Path: file}

	if n := RepeatCount(path, probe, now); n != 0 {
		t.Errorf("no log: %d", n)
	}
	for _, e := range []Event{deny(4*time.Minute, "S1"), deny(3*time.Minute, "S1"), deny(2*time.Minute, "S2"), deny(time.Minute, "S1")} {
		Append(path, e)
	}
	if n := RepeatCount(path, probe, now); n != 3 {
		t.Errorf("S1 was refused 3 times already, got %d (another session's refusal must not count)", n)
	}
	other := probe
	other.Path = "/p/other.go"
	if n := RepeatCount(path, other, now); n != 0 {
		t.Errorf("a different file starts its own count: %d", n)
	}
	// an answered read resolves everything before it
	Append(path, Event{Time: now.Add(-30 * time.Second), Kind: KindRead, OK: true, Session: "S1", Cwd: "/p"})
	if n := RepeatCount(path, probe, now); n != 0 {
		t.Errorf("following the redirect must reset the count, got %d", n)
	}
	Append(path, Event{Time: now.Add(-10 * time.Second), Kind: KindDeny, OK: true, Session: "S1", Cwd: "/p", Path: file})
	if n := RepeatCount(path, probe, now); n != 1 {
		t.Errorf("counting restarts after the answer: %d", n)
	}
	// a FAILED read is not an answer
	Append(path, Event{Time: now.Add(-5 * time.Second), Kind: KindRead, OK: false, Session: "S1", Cwd: "/p"})
	if n := RepeatCount(path, probe, now); n != 1 {
		t.Errorf("a failed read must not reset the count: %d", n)
	}
	// old refusals age out
	if n := RepeatCount(path, probe, now.Add(2*RepeatWindow)); n != 0 {
		t.Errorf("refusals older than the window must not count: %d", n)
	}
}

func TestSummaryCountsGuardErrorsSeparately(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shunt.jsonl")
	Append(path, Event{Kind: KindGuardError, Stage: StageInput})
	Append(path, Event{Kind: KindRead, OK: false})
	s, _ := SummarizeLog(path, time.Time{})
	if s.GuardErrors != 1 || s.Failures != 1 || s.Reads != 1 {
		t.Errorf("%+v", s)
	}
	if !strings.Contains(s.String(), "guard_errors=1") {
		t.Errorf("the summary must show guard errors: %s", s)
	}
}
