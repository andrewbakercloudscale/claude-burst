package shunt

import (
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 9, 21, 10, 51, 0, 0, time.UTC)

func at(sec int) time.Time { return t0.Add(time.Duration(sec) * time.Second) }

// newestFirst is how Recent hands events over.
func newestFirst(evs ...Event) []Event {
	out := make([]Event, len(evs))
	for i, e := range evs {
		out[len(evs)-1-i] = e
	}
	return out
}

func block(sec int, session, path string) Event {
	return Event{Time: at(sec), Kind: KindDeny, OK: true, Session: session, Cwd: "/p/proj", Tool: "Read",
		Path: path, Lines: 602, Threshold: 350, BytesIn: 21400}
}

func read(sec int, session string, ok bool, paths ...string) Event {
	return Event{Time: at(sec), Kind: KindRead, OK: ok, Session: session, Cwd: "/p/proj", Paths: paths, Files: len(paths),
		BytesIn: 25600, BytesOut: 200, Model: "zai-org/GLM-5.3", DurationMS: 3900}
}

// The screenshot that prompted this: a REFUSED row directly above a READ row,
// which looks like something failed when it is the feature working.
func TestABlockAndItsAnswerAreOneShuntedRow(t *testing.T) {
	rows := Fold(newestFirst(
		block(0, "s1", "/p/proj/big.go"),
		read(7, "s1", true, "big.go"),
	), at(30))

	if len(rows) != 1 {
		t.Fatalf("a block and the read that answered it must be ONE row, got %d: %+v", len(rows), rows)
	}
	r := rows[0]
	if r.Tag() != "SHUNTED" || r.IsProblem() {
		t.Fatalf("want a quiet SHUNTED row, got %q problem=%v", r.Tag(), r.IsProblem())
	}
	if r.Attempts != 1 || r.Blocked == nil {
		t.Fatalf("the row should remember it was a redirect: attempts=%d blocked=%v", r.Attempts, r.Blocked)
	}
	d := r.Describe()
	for _, w := range []string{"big.go", "602+ lines", "zai-org/GLM-5.3", "kept out of context", "3.9s"} {
		if !strings.Contains(d, w) {
			t.Errorf("description missing %q: %s", w, d)
		}
	}
	if strings.Contains(strings.ToLower(d), "refus") {
		t.Errorf("nothing about a working shunt may sound like a refusal: %s", d)
	}
}

// Claude ignored the redirect once, then followed it. That is still one shunt,
// and it says so rather than showing the wasted attempt as a separate row.
func TestARetryBeforeFollowingTheRedirectIsStillOneRow(t *testing.T) {
	rows := Fold(newestFirst(
		block(0, "s1", "/p/proj/big.go"),
		block(4, "s1", "/p/proj/big.go"),
		read(11, "s1", true, "big.go"),
	), at(30))

	if len(rows) != 1 || rows[0].Tag() != "SHUNTED" {
		t.Fatalf("got %d rows, first tag %q", len(rows), rows[0].Tag())
	}
	if rows[0].Attempts != 2 || !strings.Contains(rows[0].Describe(), "tried a direct read 2 times") {
		t.Fatalf("the retry should be named on the row: %s", rows[0].Describe())
	}
}

// Claude delegated a read nobody blocked (it followed the skill). No redirect
// happened, and the row must not pretend one did.
func TestAnUnpromptedReadIsShuntedWithNoRedirect(t *testing.T) {
	rows := Fold(newestFirst(read(0, "s1", true, "a.go", "b.go", "c.go")), at(5))
	if len(rows) != 1 || rows[0].Tag() != "SHUNTED" || rows[0].Attempts != 0 || rows[0].Blocked != nil {
		t.Fatalf("%+v", rows)
	}
	if !strings.Contains(rows[0].Describe(), "a.go, b.go +1 more") {
		t.Fatalf("multi-file reads should be listed compactly: %s", rows[0].Describe())
	}
}

// The different event type the user asked for: it was blocked, and no shunt
// followed. Fresh, it is still in flight; stale, it is NO-SHUNT -- and neither
// is red, because reading a window or moving on is legitimate.
func TestAnUnansweredBlockIsPendingThenNoShuntAndNeverAProblem(t *testing.T) {
	ev := newestFirst(block(0, "s1", "/p/proj/big.go"))

	fresh := Fold(ev, at(20))[0]
	if fresh.Tag() != "REDIRECTED" || fresh.IsProblem() {
		t.Fatalf("a block that may still be answered: %q problem=%v", fresh.Tag(), fresh.IsProblem())
	}

	stale := Fold(ev, at(int(PendingWindow.Seconds())+30))[0]
	if stale.Tag() != "NO-SHUNT" || stale.IsProblem() {
		t.Fatalf("a block nothing answered: %q problem=%v", stale.Tag(), stale.IsProblem())
	}
	if d := stale.Describe(); !strings.Contains(d, "not shunted") || !strings.Contains(d, "big.go") {
		t.Fatalf("it must say no shunt happened and for which file: %s", d)
	}
}

func TestARepeatedBlockWithNoAnswerIsOneLoopRow(t *testing.T) {
	rows := Fold(newestFirst(
		block(0, "s1", "/p/proj/big.go"),
		block(3, "s1", "/p/proj/big.go"),
		block(6, "s1", "/p/proj/big.go"),
	), at(20))
	if len(rows) != 1 {
		t.Fatalf("three blocks of one file are one loop row, got %d", len(rows))
	}
	if rows[0].Tag() != "LOOP" || !rows[0].IsProblem() || !strings.Contains(rows[0].Describe(), "attempt #3") {
		t.Fatalf("%q problem=%v %s", rows[0].Tag(), rows[0].IsProblem(), rows[0].Describe())
	}
}

// A shunt that fails is a genuine failure, and having been redirected makes it
// worse, not better: Claude was told not to read the file itself.
func TestAFailedShuntIsAProblemThatSaysWhere(t *testing.T) {
	failed := read(7, "s1", false, "big.go")
	failed.Stage, failed.Note = StageWorkerCall, "HTTP 500"
	rows := Fold(newestFirst(block(0, "s1", "/p/proj/big.go"), failed), at(30))

	if len(rows) != 1 || rows[0].Tag() != "SHUNT-FAIL" || !rows[0].IsProblem() {
		t.Fatalf("%+v", rows)
	}
	d := rows[0].Describe()
	if !strings.Contains(d, "FAILED at worker_call") || !strings.Contains(d, "no other way to read the file") {
		t.Fatalf("%s", d)
	}
}

// Two sessions in one project, or two files in one session, must not answer
// each other's blocks.
func TestABlockIsOnlyAnsweredByTheSameSessionAndFile(t *testing.T) {
	rows := Fold(newestFirst(
		block(0, "s1", "/p/proj/big.go"),
		read(5, "s2", true, "big.go"),   // another session
		read(6, "s1", true, "other.go"), // another file
	), at(int(PendingWindow.Seconds())+60))

	tags := map[string]int{}
	for _, r := range rows {
		tags[r.Tag()]++
	}
	if len(rows) != 3 || tags["SHUNTED"] != 2 || tags["NO-SHUNT"] != 1 {
		t.Fatalf("the block must stay unanswered: %v", tags)
	}
}

func TestABlockOlderThanTheRepeatWindowIsNotAnswered(t *testing.T) {
	late := int(RepeatWindow.Seconds()) + 60
	rows := Fold(newestFirst(block(0, "s1", "/p/proj/big.go"), read(late, "s1", true, "big.go")), at(late+10))
	if len(rows) != 2 {
		t.Fatalf("a read 11 minutes later is a different attempt, got %d rows", len(rows))
	}
}

// The guard records the resolved absolute path; `shunt read` records what was
// typed. Both spellings of the same file must pair.
func TestRelativeAndAbsolutePathsPair(t *testing.T) {
	for _, typed := range []string{"big.go", "./big.go", "/p/proj/big.go"} {
		rows := Fold(newestFirst(block(0, "s1", "/p/proj/big.go"), read(5, "s1", true, typed)), at(20))
		if len(rows) != 1 {
			t.Errorf("%q did not pair with the block: %d rows", typed, len(rows))
		}
	}
}

// Writes and guard errors are their own events and pass through untouched.
func TestWritesAndGuardErrorsPassThrough(t *testing.T) {
	rows := Fold(newestFirst(
		Event{Time: at(0), Kind: KindWrite, OK: true, Path: "/p/proj/gen.go", BytesOut: 900},
		Event{Time: at(1), Kind: KindGuardError, Stage: StageConfig, Note: "config unreadable"},
	), at(5))
	if len(rows) != 2 || rows[0].Tag() != "GUARD-ERR" || !rows[0].IsProblem() || rows[1].Tag() != "WRITE" {
		t.Fatalf("%+v", rows)
	}
}

func TestFoldKeepsNewestFirstAcrossMixedRows(t *testing.T) {
	rows := Fold(newestFirst(
		block(0, "s1", "/p/proj/a.go"), read(5, "s1", true, "a.go"),
		Event{Time: at(20), Kind: KindWrite, OK: true, Path: "/p/proj/w.go"},
		block(40, "s1", "/p/proj/b.go"), read(45, "s1", true, "b.go"),
	), at(60))
	if len(rows) != 3 {
		t.Fatalf("%d rows", len(rows))
	}
	for i := 1; i < len(rows); i++ {
		if rows[i].Time.After(rows[i-1].Time) {
			t.Fatalf("rows are not newest-first: %v then %v", rows[i-1].Time, rows[i].Time)
		}
	}
}
