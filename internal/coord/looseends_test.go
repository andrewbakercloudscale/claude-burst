package coord

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/notice"
)

// A session that ends with a file uncommitted and nobody to hand it to
// leaves a loose end: it is announced, the next session in the repository
// is told whose it is, and an edit adopts it.
func TestASessionThatEndsUncommittedLeavesALooseEnd(t *testing.T) {
	r := newRig(t)
	r.c.Notices = filepath.Join(t.TempDir(), "notices.json")
	r.start("aaaa1111")
	r.edit("aaaa1111", "a.go", "package x", "package y")
	r.hook("session-end", "aaaa1111", nil)

	ev, _ := notice.Read(r.c.Notices)
	if len(ev) != 1 || ev[0].Severity != notice.Error || ev[0].Kind != "loose-ends" || !strings.Contains(ev[0].Detail, "a.go") {
		t.Fatalf("want one error alert naming a.go, got %+v", ev)
	}
	if o := r.c.Owners()[filepath.Join(r.repo, "a.go")]; o.Session != "aaaa1111" || o.Live {
		t.Fatalf("owner = %+v, want the ended session", o)
	}

	r.advance(3 * time.Hour)
	out := r.start("bbbb2222")
	if !strings.Contains(out, "LOOSE ENDS") || !strings.Contains(out, "a.go (left uncommitted by session") || !strings.Contains(out, "aaaa1111") {
		t.Fatalf("session start does not name the loose end and who left it:\n%s", out)
	}
	if !strings.Contains(out, "3h ago") {
		t.Fatalf("session start does not say how long ago:\n%s", out)
	}

	_, ctx := r.edit("bbbb2222", "a.go", "package y", "package z")
	if !strings.Contains(ctx, "left when it ended") {
		t.Fatalf("adopting edit not told whose changes it holds: %s", ctx)
	}
	if o, ok := r.c.Owners()[filepath.Join(r.repo, "a.go")]; !ok || o.Session != "bbbb2222" || !o.Live {
		t.Fatalf("after adoption owner = %+v, want the live adopter", o)
	}
}

// A loose end that gets committed is forgotten, and a clean session end
// announces nothing.
func TestACommittedLooseEndIsForgotten(t *testing.T) {
	r := newRig(t)
	r.c.Notices = filepath.Join(t.TempDir(), "notices.json")
	r.start("aaaa1111")
	r.edit("aaaa1111", "a.go", "package x", "package y")
	r.hook("session-end", "aaaa1111", nil)
	r.git("commit", "-qam", "by hand")
	r.start("bbbb2222")
	if _, ok := r.c.Owners()[filepath.Join(r.repo, "a.go")]; ok {
		t.Fatal("a committed loose end is still recorded")
	}

	r.start("cccc3333")
	r.edit("cccc3333", "b.go", "package x", "package q")
	r.git("commit", "-qam", "done")
	r.hook("session-end", "cccc3333", nil)
	if ev, _ := notice.Read(r.c.Notices); len(ev) != 1 {
		t.Fatalf("a session that ended clean was announced: %+v", ev)
	}
}

// A file another live session masters is its work in progress, not a loose
// end; a file changed by hand with no session behind it is.
func TestSessionStartLeavesOutLiveWorkAndNamesTheRest(t *testing.T) {
	r := newRig(t)
	r.start("aaaa1111")
	r.edit("aaaa1111", "a.go", "package x", "package y")
	os.WriteFile(filepath.Join(r.repo, "b.go"), []byte("package hand\n"), 0o644)
	out := r.start("bbbb2222")
	if strings.Contains(out, "/a.go (") {
		t.Fatalf("a live session's file is reported as a loose end:\n%s", out)
	}
	if !strings.Contains(out, "b.go (uncommitted, no session recorded") {
		t.Fatalf("an unrecorded change is not reported:\n%s", out)
	}
	if !strings.Contains(out, "Never discard") {
		t.Fatalf("no instruction to leave them alone:\n%s", out)
	}
}

func TestACleanRepositoryHasNoLooseEnds(t *testing.T) {
	r := newRig(t)
	if out := r.start("aaaa1111"); strings.Contains(out, "LOOSE ENDS") {
		t.Fatalf("clean repository reported loose ends:\n%s", out)
	}
}
