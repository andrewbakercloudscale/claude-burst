package coord

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// rig is a coordinator on a fake clock and a real git repository.
type rig struct {
	t     *testing.T
	c     *Coordinator
	clock time.Time
	repo  string
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{t: t, clock: time.Date(2026, 9, 30, 19, 0, 0, 0, time.Local)}
	r.repo = realPath(t.TempDir())
	r.git("init", "-q")
	r.git("config", "user.email", "t@example.com")
	r.git("config", "user.name", "t")
	for _, f := range []string{"a.go", "b.go"} {
		os.WriteFile(filepath.Join(r.repo, f), []byte("package x\n"), 0o644)
	}
	r.git("add", "a.go", "b.go")
	r.git("commit", "-qm", "init")
	r.c = &Coordinator{Dir: t.TempDir(), Exe: "/x/claude-burst", Settings: Settings{}.Resolved(), Now: func() time.Time { return r.clock }}
	return r
}

func (r *rig) git(args ...string) {
	r.t.Helper()
	cmd := exec.Command("git", append([]string{"-C", r.repo}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		r.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func (r *rig) advance(d time.Duration) { r.clock = r.clock.Add(d) }

// hook runs one hook for session sid and returns its output.
func (r *rig) hook(name, sid string, extra map[string]any) string {
	r.t.Helper()
	in := map[string]any{"session_id": sid, "cwd": r.repo}
	for k, v := range extra {
		in[k] = v
	}
	b, _ := json.Marshal(in)
	var out bytes.Buffer
	if err := r.c.Hook(name, bytes.NewReader(b), &out); err != nil {
		r.t.Fatalf("%s for %s: %v", name, sid, err)
	}
	return out.String()
}

func (r *rig) start(sid string) string { return r.hook("session-start", sid, nil) }

func denied(out string) bool { return strings.Contains(out, `"permissionDecision":"deny"`) }

// edit is Claude Code's Edit: the pre hook, then (unless refused) the
// exact-text replacement, then the post hook. It reports whether the edit
// was applied, and the pre hook's output.
func (r *rig) edit(sid, file, old, new string) (bool, string) {
	r.t.Helper()
	p := filepath.Join(r.repo, file)
	in := map[string]any{"tool_name": "Edit", "tool_input": map[string]any{"file_path": p, "old_string": old, "new_string": new}}
	out := r.hook("pre-tool", sid, in)
	if denied(out) {
		return false, out
	}
	b, _ := os.ReadFile(p)
	if strings.Count(string(b), old) != 1 {
		r.t.Fatalf("%s's Edit: old_string %q not unique in %q", sid, old, b)
	}
	os.WriteFile(p, []byte(strings.Replace(string(b), old, new, 1)), 0o644)
	return true, out + r.hook("post-tool", sid, in)
}

// write is a whole-file Write.
func (r *rig) write(sid, file, body string) (bool, string) {
	p := filepath.Join(r.repo, file)
	in := map[string]any{"tool_name": "Write", "tool_input": map[string]any{"file_path": p, "content": body}}
	out := r.hook("pre-tool", sid, in)
	if denied(out) {
		return false, out
	}
	os.WriteFile(p, []byte(body), 0o644)
	r.hook("post-tool", sid, in)
	return true, out
}

// bash runs the pre hook for a command and, unless refused, the command
// itself in the repo and the post hook.
func (r *rig) bash(sid, command string) (bool, string) {
	r.t.Helper()
	in := map[string]any{"tool_name": "Bash", "tool_input": map[string]any{"command": command}}
	out := r.hook("pre-tool", sid, in)
	if denied(out) {
		return false, out
	}
	c := exec.Command("sh", "-c", command)
	c.Dir = r.repo
	if b, err := c.CombinedOutput(); err != nil {
		r.t.Fatalf("%s: %v\n%s", command, err, b)
	}
	return true, r.hook("post-tool", sid, in)
}

func (r *rig) status() Status {
	st, err := r.c.Status()
	if err != nil {
		r.t.Fatal(err)
	}
	return st
}

func (r *rig) file(name string) *FileStatus {
	p := filepath.Join(r.repo, name)
	for _, f := range r.status().Files {
		if f.Path == p {
			return &f
		}
	}
	return nil
}

// Two sessions build a sentence in one file, one word each in turn. Nobody
// waits, no word is lost, the first to edit is the master and commits the
// file with both sessions' words, and the other session cannot stage it.
func TestTwoSessionsWriteASentenceAWordEachInTurn(t *testing.T) {
	r := newRig(t)
	os.WriteFile(filepath.Join(r.repo, "sentence.txt"), []byte("<end>\n"), 0o644)
	r.git("add", "sentence.txt")
	r.git("commit", "-qm", "empty sentence")
	r.start("alpha-111")
	r.advance(time.Second)
	r.start("beta-2222")

	words := strings.Fields("the quick brown fox jumps over the lazy dog")
	var toAlpha strings.Builder
	for i, w := range words {
		sid := "alpha-111"
		if i%2 == 1 {
			sid = "beta-2222"
		}
		r.advance(10 * time.Second)
		ok, out := r.edit(sid, "sentence.txt", "<end>", w+" <end>")
		if !ok {
			t.Fatalf("word %d (%q) by %s was refused; sharing must never make anyone wait:\n%s", i+1, w, sid, out)
		}
		if sid == "alpha-111" {
			toAlpha.WriteString(out)
		}
		if i == 1 && !strings.Contains(out, "Its master is alpha-111") && !strings.Contains(out, "[alpha-11]") {
			t.Fatalf("beta's first edit must be told the file is shared and who masters it:\n%s", out)
		}
		if i == 3 && out != "" {
			t.Fatalf("beta is told once, not on every edit:\n%s", out)
		}
	}
	b, _ := os.ReadFile(filepath.Join(r.repo, "sentence.txt"))
	if got := strings.TrimSpace(string(b)); got != "the quick brown fox jumps over the lazy dog <end>" {
		t.Fatalf("the sentence came out as %q", got)
	}

	// The dashboard's view: alpha masters it, beta coordinates with it.
	f := r.file("sentence.txt")
	if f == nil || f.Master != "alpha-111" || len(f.Contributors) != 1 || !strings.Contains(f.Contributors[0], "beta-222") {
		t.Fatalf("want master alpha, contributor beta, got %+v", f)
	}
	for _, s := range r.status().Sessions {
		if s.ID == "alpha-111" && (len(s.MasterOf) != 1 || !strings.HasSuffix(s.MasterOf[0], "sentence.txt")) {
			t.Fatalf("alpha should be master of sentence.txt: %+v", s)
		}
	}

	// Alpha was told about each of beta's words.
	msgs := toAlpha.String() + r.hook("prompt", "alpha-111", nil)
	for _, w := range []string{"quick", "fox", "over", "lazy"} {
		if !strings.Contains(msgs, "with \\\""+w+" <end>") {
			t.Fatalf("alpha was not told beta added %q:\n%s", w, msgs)
		}
	}

	// Beta may not stage or commit it; alpha commits it, and it is free.
	if ok, out := r.bash("beta-2222", "git add sentence.txt && git commit -qm beta"); ok || !strings.Contains(out, "is mastered by") {
		t.Fatalf("beta must not commit the file alpha masters, ok=%v:\n%s", ok, out)
	}
	if ok, out := r.bash("alpha-111", "git add sentence.txt && git commit -qm sentence"); !ok {
		t.Fatalf("alpha commits it:\n%s", out)
	}
	if f := r.file("sentence.txt"); f != nil {
		t.Fatalf("committed: no longer shared, got %+v", f)
	}
	log, _ := exec.Command("git", "-C", r.repo, "log", "-1", "--format=%s", "--", "sentence.txt").Output()
	if strings.TrimSpace(string(log)) != "sentence" {
		t.Fatalf("the last commit of sentence.txt should be alpha's, got %q", log)
	}
}

func TestAWholeFileWriteOverAnotherSessionsWorkIsRefused(t *testing.T) {
	r := newRig(t)
	r.start("A")
	r.start("B")
	r.edit("A", "a.go", "package x", "package x // A")
	ok, out := r.write("B", "a.go", "package y\n")
	if ok || !strings.Contains(out, "use Edit") {
		t.Fatalf("B's Write would wipe A's change and must be refused, ok=%v:\n%s", ok, out)
	}
	if ok, _ := r.write("A", "a.go", "package x // rewritten by A\n"); !ok {
		t.Fatal("the master may rewrite its own file")
	}
	if ok, _ := r.write("B", "new.go", "package x\n"); !ok {
		t.Fatal("creating a new file is never refused")
	}
}

func TestSweepingGitIsRefusedWhileAnotherSessionHasWork(t *testing.T) {
	r := newRig(t)
	r.start("A")
	r.start("B")
	r.edit("A", "a.go", "package x", "package x // A")
	for _, cmd := range []string{"git add -A", "git add .", "git commit -am wip", "git add -u && git commit -m x", "git -C . add --all"} {
		if ok, out := r.bash("B", cmd); ok || !strings.Contains(out, "would sweep it into your commit") {
			t.Fatalf("%q must be refused while A has work here, ok=%v:\n%s", cmd, ok, out)
		}
	}
	r.edit("B", "b.go", "package x", "package x // B")
	if ok, out := r.bash("B", "git add b.go && git commit -qm b"); !ok {
		t.Fatalf("staging your own file by name goes through:\n%s", out)
	}
	if ok, _ := r.bash("A", "git add a.go && git commit -qm a"); !ok {
		t.Fatal("A commits its own")
	}
	if ok, _ := r.bash("B", "git add -A"); !ok {
		t.Fatal("with nobody else's work left, git add -A is allowed")
	}
}

func TestAMasterThatLeavesHandsTheFileToItsContributor(t *testing.T) {
	r := newRig(t)
	r.start("A")
	r.start("B")
	r.edit("A", "a.go", "package x", "package x // A")
	r.edit("B", "a.go", "// A", "// A B")
	r.hook("session-end", "A", nil)
	if f := r.file("a.go"); f == nil || f.Master != "B" {
		t.Fatalf("B should now master a.go, got %+v", f)
	}
	if out := r.hook("prompt", "B", nil); !strings.Contains(out, "You are now the master") || !strings.Contains(out, "git diff") {
		t.Fatalf("B must be told it now commits the file:\n%s", out)
	}
}

// An idle master keeps its file until another session needs it. That
// session's edit takes it over: it is told, the old master is told, and the
// old master's uncommitted changes stay in the file for the new one to commit.
func TestASessionTakesOverAFileFromAnIdleMaster(t *testing.T) {
	r := newRig(t)
	r.start("A")
	r.start("B")
	r.edit("A", "a.go", "package x", "package x // A")
	r.hook("prompt", "A", nil)

	// Busy master: B's edit is a contribution, not a take-over.
	r.edit("B", "b.go", "package x", "package x // B")
	r.advance(5 * time.Minute)
	r.hook("prompt", "B", nil)
	if _, out := r.edit("B", "a.go", "// A", "// A B"); strings.Contains(out, "you are now the master") {
		t.Fatalf("A is busy, B must not take a.go:\n%s", out)
	}
	if f := r.file("a.go"); f.Master != "A" {
		t.Fatalf("A stays master while busy, got %+v", f)
	}

	// Idle with nobody asking: A keeps it.
	r.advance(DefaultMasterIdle + time.Minute)
	r.hook("prompt", "B", nil)
	if f := r.file("a.go"); f == nil || f.Master != "A" || !f.TakeOver {
		t.Fatalf("an idle master keeps its file, marked as open to take-over, got %+v", f)
	}

	// B needs it: B's edit takes it over.
	ok, out := r.edit("B", "a.go", "// A B", "// A B again")
	if !ok || !strings.Contains(out, "you are now the master of") || !strings.Contains(out, "idle for") || !strings.Contains(out, "not yours") {
		t.Fatalf("B takes a.go over and is told about A's uncommitted changes:\n%s", out)
	}
	if f := r.file("a.go"); f.Master != "B" || len(f.Contributors) != 1 {
		t.Fatalf("B masters a.go with A as a contributor, got %+v", f)
	}
	if out := r.hook("prompt", "A", nil); !strings.Contains(out, "has taken over") {
		t.Fatalf("A must be told it lost the file:\n%s", out)
	}
	if ok, _ := r.bash("A", "git add a.go"); ok {
		t.Fatal("A may no longer stage a.go: B commits it")
	}
	if out := r.hook("prompt", "B", nil); strings.Contains(out, "waiting for your commit") {
		t.Fatalf("taking over is not a reason to nudge at once:\n%s", out)
	}
}

func TestAWriteThatTakesOverUncommittedWorkIsRefused(t *testing.T) {
	r := newRig(t)
	r.start("A")
	r.start("B")
	r.edit("A", "a.go", "package x", "package x // A")
	r.advance(DefaultMasterIdle + time.Minute)
	r.hook("prompt", "B", nil)
	if ok, out := r.write("B", "a.go", "package y\n"); ok || !strings.Contains(out, "you are now the master") {
		t.Fatalf("B takes over but its Write would wipe A's change, so it is refused:\n%s", out)
	}
	if f := r.file("a.go"); f.Master != "B" {
		t.Fatalf("B is master, got %+v", f)
	}
	if ok, _ := r.edit("B", "a.go", "// A", "// A B"); !ok {
		t.Fatal("B's Edit goes through")
	}
}

func TestEditingAFileWithUnrecordedChangesWarns(t *testing.T) {
	r := newRig(t)
	r.start("A")
	os.WriteFile(filepath.Join(r.repo, "a.go"), []byte("package x // by hand\n"), 0o644)
	if _, out := r.edit("A", "a.go", "// by hand", "// by hand, then A"); !strings.Contains(out, "already had uncommitted changes") {
		t.Fatalf("A must be told the file held changes nobody recorded:\n%s", out)
	}
	if _, out := r.edit("A", "b.go", "package x", "package x // A"); strings.Contains(out, "already had") {
		t.Fatalf("a clean file is taken silently:\n%s", out)
	}
}

func TestTakeAsksABusyMasterAndHandsOnToTheAsker(t *testing.T) {
	r := newRig(t)
	r.start("A")
	r.start("B")
	r.start("C")
	r.edit("A", "a.go", "package x", "package x // A")
	r.edit("C", "a.go", "// A", "// A C")
	p := filepath.Join(r.repo, "a.go")

	res, err := r.c.Take(p, "B")
	if err != nil || !strings.Contains(res, "has been asked") {
		t.Fatalf("A is busy, so B asks: %q %v", res, err)
	}
	if f := r.file("a.go"); f.Master != "A" || f.Wanted == "" {
		t.Fatalf("A stays master with B's request on record, got %+v", f)
	}
	if out := r.hook("prompt", "A", nil); !strings.Contains(out, "I need") || !strings.Contains(out, "coord release") {
		t.Fatalf("A is asked:\n%s", out)
	}
	if err := r.c.Release(p, "A"); err != nil {
		t.Fatal(err)
	}
	if f := r.file("a.go"); f.Master != "B" {
		t.Fatalf("released, a.go goes to B who asked, not C who contributed, got %+v", f)
	}

	// Idle master: take is immediate.
	r.advance(DefaultMasterIdle + time.Minute)
	r.hook("prompt", "C", nil)
	if res, _ := r.c.Take(p, "C"); !strings.Contains(res, "you are now the master") {
		t.Fatalf("B is idle, so C takes it at once: %q", res)
	}
}

func TestTheEndOfATurnLetsGoOfFilesGitDoesNotTrack(t *testing.T) {
	r := newRig(t)
	r.start("A")
	outside := filepath.Join(realPath(t.TempDir()), "MEMORY.md")
	os.WriteFile(outside, []byte("x\n"), 0o644)
	r.hook("pre-tool", "A", map[string]any{"tool_name": "Edit", "tool_input": map[string]any{"file_path": outside}})
	r.edit("A", "a.go", "package x", "package x // A")
	r.hook("stop", "A", nil)
	st := r.status()
	if len(st.Files) != 1 || !strings.HasSuffix(st.Files[0].Path, "a.go") {
		t.Fatalf("uncommitted a.go stays shared, the untracked MEMORY.md is let go: %+v", st.Files)
	}
}

func held(out string) bool { return strings.Contains(out, `"decision":"block"`) }

// A turn that leaves files its session masters uncommitted is held once,
// with the instruction to commit them; a second stop in the same turn is
// let go, and a turn with everything committed is never held.
func TestATurnIsHeldUntilItsFilesAreCommitted(t *testing.T) {
	r := newRig(t)
	r.start("A")
	r.start("B")
	r.edit("A", "a.go", "package x", "package x // A")
	r.edit("B", "a.go", "// A", "// A B")
	r.edit("B", "b.go", "package x", "package x // B")

	out := r.hook("stop", "A", nil)
	if !held(out) || !strings.Contains(out, "a.go") || strings.Contains(out, "b.go") {
		t.Fatalf("A must be held to commit a.go, and only the files it masters:\n%s", out)
	}
	if !strings.Contains(out, "also holds changes from") || !strings.Contains(out, "do not push") || !strings.Contains(out, "WIP:") {
		t.Fatalf("the instruction names the contributor, forbids pushing and allows WIP:\n%s", out)
	}
	// B contributed to a.go (A commits it) and masters b.go.
	if out := r.hook("stop", "B", nil); !held(out) || strings.Contains(out, "a.go") {
		t.Fatalf("B is held for b.go only:\n%s", out)
	}

	// The second stop of the same turn is let go, so a failed commit never loops.
	if out := r.hook("stop", "A", map[string]any{"stop_hook_active": true}); held(out) {
		t.Fatalf("a session already held this turn must be let go:\n%s", out)
	}

	// After committing, the turn ends without being held.
	r.bash("A", "git add a.go && git commit -qm 'A and B'")
	if out := r.hook("stop", "A", nil); held(out) {
		t.Fatalf("nothing uncommitted, nothing to hold:\n%s", out)
	}
	if f := r.file("a.go"); f != nil {
		t.Fatalf("committed a.go stops being shared, got %+v", f)
	}

	// A session that edited nothing is never held.
	r.start("C")
	if out := r.hook("stop", "C", nil); out != "" {
		t.Fatalf("C edited nothing:\n%s", out)
	}
}

func TestSessionStartAsksForCommitsBeforeStopping(t *testing.T) {
	r := newRig(t)
	if out := r.start("A"); !strings.Contains(out, "Commit the files you master before you stop") {
		t.Fatalf("the briefing must state the rule:\n%s", out)
	}
}

func TestAMasterSittingOnOthersChangesIsNudged(t *testing.T) {
	r := newRig(t)
	r.start("A")
	r.start("B")
	r.edit("A", "a.go", "package x", "package x // A")
	r.edit("B", "a.go", "// A", "// A B")
	r.hook("prompt", "A", nil) // drains the edit notice
	r.advance(DefaultNudge + time.Minute)
	r.hook("prompt", "B", nil)
	out := r.hook("prompt", "A", nil)
	if !strings.Contains(out, "waiting for your commit") || !strings.Contains(out, "coord release") {
		t.Fatalf("A must be nudged:\n%s", out)
	}
	if out := r.hook("prompt", "A", nil); strings.Contains(out, "waiting for your commit") {
		t.Fatal("once per interval, not every prompt")
	}
}

// A deploy or install script builds the working tree, so running one while
// another session has uncommitted work in the repository would ship that
// work in no commit. It is refused like `git add -A`; the session's own
// work never blocks it, and reading the script is not running it.
func TestADeployIsRefusedWhileAnotherSessionHasUncommittedWork(t *testing.T) {
	r := newRig(t)
	r.start("A")
	r.start("B")
	run := func(sid, cmd, cwd string) string {
		return r.hook("pre-tool", sid, map[string]any{"cwd": cwd, "tool_name": "Bash", "tool_input": map[string]any{"command": cmd}})
	}
	elsewhere := realPath(t.TempDir())

	r.edit("A", "a.go", "package x", "package x // A")
	if out := run("A", "zsh scripts/deploy.sh", r.repo); denied(out) {
		t.Fatalf("A's own uncommitted work does not block A's deploy:\n%s", out)
	}

	r.edit("B", "b.go", "package x", "package x // B")
	for _, c := range []struct{ cmd, cwd string }{
		{"zsh scripts/deploy.sh", r.repo},
		{"cd " + r.repo + " && zsh scripts/deploy.sh 2>&1 | tail -3", elsewhere},
		{"bash " + filepath.Join(r.repo, "deploy-wordpress.sh"), elsewhere},
		{"./install.sh", r.repo},
		{"git status && scripts/deploy.sh", r.repo},
	} {
		out := run("A", c.cmd, c.cwd)
		if !denied(out) || !strings.Contains(out, "b.go") || !strings.Contains(out, "SHIP_UNCOMMITTED=1") {
			t.Fatalf("%q must be refused, naming B's file and the override:\n%s", c.cmd, out)
		}
	}
	if out := run("A", "zsh scripts/deploy.sh", r.repo); !strings.Contains(out, "--only-committed") {
		t.Fatalf("claude-burst's deploy is offered --only-committed:\n%s", out)
	}
	for _, cmd := range []string{
		"SHIP_UNCOMMITTED=1 zsh scripts/deploy.sh",
		"zsh scripts/deploy.sh --only-committed",
		"cat scripts/deploy.sh",
		"grep -n foo scripts/deploy.sh install.sh",
		"zsh scripts/deploy.sh", // from outside any repository with others' work
	} {
		cwd := r.repo
		if cmd == "zsh scripts/deploy.sh" {
			cwd = elsewhere
		}
		if out := run("A", cmd, cwd); denied(out) {
			t.Fatalf("%q must go through:\n%s", cmd, out)
		}
	}

	// B commits: nothing of anyone else's is left to ship.
	r.bash("B", "git add b.go && git commit -qm B")
	if out := run("A", "zsh scripts/deploy.sh", r.repo); denied(out) {
		t.Fatalf("B committed, A's deploy goes through:\n%s", out)
	}
}

func TestSessionsMessageEachOther(t *testing.T) {
	r := newRig(t)
	r.start("AAAA1111")
	r.start("BBBB2222")
	if _, err := r.c.Send("BBBB", "AAAA", "leave the config loader to me"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.c.Send("CCCC", "", "x"); err == nil {
		t.Fatal("an unknown session is an error")
	}
	out := r.hook("post-tool", "BBBB2222", map[string]any{"tool_name": "Read"})
	if !strings.Contains(out, "leave the config loader to me") || !strings.Contains(out, "AAAA1111") {
		t.Fatalf("message not delivered:\n%s", out)
	}
	if out := r.hook("prompt", "BBBB2222", nil); out != "" {
		t.Fatalf("delivered once:\n%s", out)
	}
}

func TestSessionStartBriefsAboutTheOthers(t *testing.T) {
	r := newRig(t)
	r.start("AAAA1111")
	r.edit("AAAA1111", "a.go", "package x", "package x // A")
	out := r.start("BBBB2222")
	for _, want := range []string{"SESSION COORDINATION", "is its MASTER", "`git add -A`", "AAAA1111", "SAME working directory", "master of: " + filepath.Join(r.repo, "a.go")} {
		if !strings.Contains(out, want) {
			t.Fatalf("briefing lacks %q:\n%s", want, out)
		}
	}
}

// Fail open: a corrupt state file starts over, and a lock held elsewhere
// makes the hook return an error (the caller exits 0) instead of waiting.
func TestItFailsOpen(t *testing.T) {
	r := newRig(t)
	os.MkdirAll(r.c.Dir, 0o755)
	os.WriteFile(filepath.Join(r.c.Dir, "state.json"), []byte("{not json"), 0o644)
	r.start("A")
	if ok, _ := r.edit("A", "a.go", "package x", "package x // A"); !ok {
		t.Fatal("a corrupt state file must not block an edit")
	}

	old := lockWait
	lockWait = 100 * time.Millisecond
	defer func() { lockWait = old }()
	lf, _ := os.OpenFile(filepath.Join(r.c.Dir, "state.lock"), os.O_RDWR, 0o644)
	defer lf.Close()
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := r.c.Hook("pre-tool", strings.NewReader(`{"session_id":"B","tool_name":"Write","tool_input":{"file_path":"/tmp/x"}}`), &out)
	if err == nil || out.Len() != 0 {
		t.Fatalf("a held lock must give up with an error and no output, got err=%v out=%q", err, out.String())
	}
}

func TestSyncHooksInstallsAndRemovesOnlyItsOwn(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	p := filepath.Join(home, ".claude", "settings.json")
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, []byte(`{"model":"opus","hooks":{"Stop":[{"matcher":"","hooks":[{"type":"command","command":"my-own-stop.sh"}]}]}}`), 0o644)

	if err := SyncHooks(true, "/Users/x/.local/bin/claude-burst"); err != nil {
		t.Fatal(err)
	}
	if !Installed() {
		t.Fatal("all six hooks should be installed")
	}
	b, _ := os.ReadFile(p)
	for _, want := range []string{`claude-burst\" coord pre-tool`, "Edit|Write|MultiEdit|NotebookEdit|Bash", "my-own-stop.sh", `"model": "opus"`} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("settings.json lacks %q:\n%s", want, b)
		}
	}
	if err := SyncHooks(true, "/Users/x/.local/bin/claude-burst"); err != nil {
		t.Fatal(err)
	}
	if b2, _ := os.ReadFile(p); strings.Count(string(b2), "coord stop") != 1 {
		t.Fatal("installing twice must not duplicate")
	}
	if err := SyncHooks(false, ""); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(p)
	if strings.Contains(string(b), " coord ") || !strings.Contains(string(b), "my-own-stop.sh") {
		t.Fatalf("removal must take only ours:\n%s", b)
	}
}

// The dashboard names a session the way Claude Code does: its given title
// unless that is only the folder name, else Claude Code's own title.
func TestSessionName(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.jsonl")
	os.WriteFile(p, []byte(`{"type":"ai-title","aiTitle":"Old title","sessionId":"x"}
{"type":"custom-title","customTitle":"repo","sessionId":"x"}
{"type":"ai-title","aiTitle":"Fix the \"rollback\" script","sessionId":"x"}
`), 0o644)
	if got := sessionName(p, "repo"); got != `Fix the "rollback" script` {
		t.Fatalf("a folder-name title gives way to the latest generated one, got %q", got)
	}
	os.WriteFile(p, []byte(`{"type":"custom-title","customTitle":"Release 0.4","sessionId":"x"}
{"type":"ai-title","aiTitle":"Something","sessionId":"x"}
`), 0o644)
	if got := sessionName(p, "repo"); got != "Release 0.4" {
		t.Fatalf("a title you gave wins, got %q", got)
	}
	if got := sessionName(filepath.Join(t.TempDir(), "none"), "repo"); got != "" {
		t.Fatalf("no transcript, no name, got %q", got)
	}
}
