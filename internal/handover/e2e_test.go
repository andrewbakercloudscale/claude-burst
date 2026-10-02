package handover

// End-to-end tests of the installed hook scripts: they run end.sh, write.sh
// and start.sh for real, against a real git repository, with a fake `claude`
// first on PATH standing in for the writer. The installer tests prove the
// scripts are laid down; these prove that what is laid down does its job.

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeClaude records how it was called and does what the real writer is
// asked to do: add a section to the top of HANDOFF.md. The section carries
// an em dash, which write.sh must strip.
const fakeClaude = `#!/usr/bin/env bash
{
  echo "args: $*"
  echo "writer-env: ${CLAUDE_HANDOVER_WRITER:-unset}"
  echo "cwd: $PWD"
} >> "$FAKE_CLAUDE_LOG"
prompt=$(cat)
echo "prompt: ${prompt:0:80}" >> "$FAKE_CLAUDE_LOG"
if [[ "${FAKE_CLAUDE_MODE:-write}" == write ]]; then
  root=$(git rev-parse --show-toplevel)
  { printf '# Handover 2026-09-29\n\nDone: the widget \xe2\x80\x94 shipped.\n\n'; cat "$root/HANDOFF.md"; } > "$root/HANDOFF.md.new"
  mv "$root/HANDOFF.md.new" "$root/HANDOFF.md"
fi
echo '{"type":"result","result":"ok"}'
`

type rig struct {
	t      *testing.T
	dir    string // installed scripts
	repo   string // a git repository opted in with HANDOFF.md
	root   string // the repo as git names it (macOS resolves /var to /private/var)
	calls  string // fake claude's call log
	env    []string
	script func(name string) string
}

func newRig(t *testing.T) *rig {
	t.Helper()
	home(t)
	if err := Install(); err != nil {
		t.Fatal(err)
	}
	dir, _ := Dir()
	r := &rig{t: t, dir: dir}
	r.script = func(n string) string { return filepath.Join(dir, n) }

	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(fakeClaude), 0o755); err != nil {
		t.Fatal(err)
	}
	r.calls = filepath.Join(t.TempDir(), "calls.log")
	r.env = append(os.Environ(),
		"PATH="+bin+":"+os.Getenv("PATH"),
		"FAKE_CLAUDE_LOG="+r.calls,
		"CLAUDE_HANDOVER_NO_NOTIFY=1",
	)

	r.repo = t.TempDir()
	r.git("init", "-q", "-b", "main")
	r.git("config", "user.email", "test@example.com")
	r.git("config", "user.name", "Test")
	r.write("HANDOFF.md", "# Handover 2026-09-28\n\nEarlier work.\n")
	r.write("app.go", "package app\n")
	r.git("add", ".")
	r.git("commit", "-q", "-m", "start")
	r.root = r.git("rev-parse", "--show-toplevel")
	return r
}

func (r *rig) git(args ...string) string {
	r.t.Helper()
	out, err := exec.Command("git", append([]string{"-C", r.repo}, args...)...).CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (r *rig) write(name, body string) {
	r.t.Helper()
	if err := os.WriteFile(filepath.Join(r.repo, name), []byte(body), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

// transcript writes a Claude Code transcript with n typed prompts, plus the
// entries end.sh must not count as prompts.
func (r *rig) transcript(n int) string {
	r.t.Helper()
	var lines []string
	for i := 0; i < n; i++ {
		lines = append(lines, `{"type":"user","message":{"role":"user","content":"do thing `+string(rune('a'+i))+`"}}`)
	}
	lines = append(lines,
		`{"type":"user","isMeta":true,"message":{"role":"user","content":"meta"}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":"x"}]}}`,
		`{"type":"user","message":{"role":"user","content":"<command-name>/clear</command-name>"}}`,
	)
	p := filepath.Join(r.t.TempDir(), "s.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		r.t.Fatal(err)
	}
	return p
}

func (r *rig) run(name string, input any, extraEnv ...string) string {
	r.t.Helper()
	b, _ := json.Marshal(input)
	cmd := exec.Command(r.script(name))
	cmd.Env = append(append([]string{}, r.env...), extraEnv...)
	cmd.Stdin = strings.NewReader(string(b))
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("%s: %v\n%s", name, err, out)
	}
	return string(out)
}

func (r *rig) end(prompts int, extraEnv ...string) {
	r.t.Helper()
	r.run("end.sh", map[string]any{
		"session_id": "sess-1", "transcript_path": r.transcript(prompts),
		"cwd": r.repo, "reason": "other",
	}, extraEnv...)
}

func (r *rig) log() string {
	b, _ := os.ReadFile(filepath.Join(r.dir, "handover.log"))
	return string(b)
}

// waitFor polls the log: write.sh is detached from end.sh on purpose.
//
// The opposite check needs no wait. end.sh writes its "queue" line before it
// detaches write.sh, and r.run returns only once end.sh has exited, so a log
// without that line means no writer was started and none can appear later.
func (r *rig) waitFor(substr string) string {
	r.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if l := r.log(); strings.Contains(l, substr) {
			return l
		}
		time.Sleep(100 * time.Millisecond)
	}
	r.t.Fatalf("log never showed %q:\n%s", substr, r.log())
	return ""
}

func (r *rig) calls_() string {
	b, _ := os.ReadFile(r.calls)
	return string(b)
}

func TestHandoverWritesAndCommitsWhenASessionEnds(t *testing.T) {
	r := newRig(t)
	head := r.git("rev-parse", "HEAD")
	r.end(2)
	log := r.waitFor("committed")

	for _, want := range []string{"queue  " + r.root + ": 2 typed prompts", "write  " + r.root + " with opus", "wrote  " + r.root + ", committed"} {
		if !strings.Contains(log, want) {
			t.Fatalf("log is missing %q:\n%s", want, log)
		}
	}
	calls := r.calls_()
	for _, want := range []string{"--resume sess-1", "--fork-session", "--model opus", "writer-env: 1", "cwd: " + r.repo} {
		if !strings.Contains(calls, want) {
			t.Fatalf("writer was not called with %q:\n%s", want, calls)
		}
	}

	body, _ := os.ReadFile(filepath.Join(r.repo, "HANDOFF.md"))
	if !strings.HasPrefix(string(body), "# Handover 2026-09-29") || !strings.Contains(string(body), "Earlier work.") {
		t.Fatalf("new section missing or old one lost:\n%s", body)
	}
	if strings.ContainsAny(string(body), "\u2014\u2013") {
		t.Fatalf("dashes must be stripped:\n%s", body)
	}

	if r.git("rev-parse", "HEAD~1") != head {
		t.Fatal("expected exactly one new commit")
	}
	msg := r.git("log", "-1", "--format=%B")
	if !strings.HasPrefix(msg, "Update the handover at session close") || strings.Contains(strings.ToLower(msg), "claude") {
		t.Fatalf("commit message: %q", msg)
	}
	if files := r.git("show", "--name-only", "--format=", "HEAD"); files != "HANDOFF.md" {
		t.Fatalf("the commit must hold HANDOFF.md only, got %q", files)
	}
	if _, err := os.Stat(filepath.Join(r.repo, ".git", "handover.lock")); !os.IsNotExist(err) {
		t.Fatal("the repo lock must be released")
	}
}

func TestHandoverSkipsShortSessions(t *testing.T) {
	r := newRig(t)
	r.end(1) // meta, tool results and slash commands do not count
	if log := r.log(); !strings.Contains(log, "skip   "+r.root+": 1 typed prompt(s), fewer than 2") {
		t.Fatalf("want a skip line:\n%s", log)
	}
	if strings.Contains(r.log(), "queue") || r.calls_() != "" {
		t.Fatal("the writer must not run for a short session")
	}
}

func TestHandoverIgnoresReposWithoutHandoff(t *testing.T) {
	r := newRig(t)
	r.git("rm", "-q", "HANDOFF.md")
	r.git("commit", "-q", "-m", "opt out")
	r.end(3)
	if r.log() != "" || r.calls_() != "" {
		t.Fatalf("a repo without HANDOFF.md is not opted in:\nlog %q\ncalls %q", r.log(), r.calls_())
	}
}

func TestHandoverLeavesUncommittedEditsUncommitted(t *testing.T) {
	r := newRig(t)
	r.write("HANDOFF.md", "# Handover 2026-09-28\n\nEarlier work, edited by hand.\n")
	head := r.git("rev-parse", "HEAD")
	r.end(2)
	r.waitFor("not committed: HANDOFF.md already had uncommitted edits")
	if r.git("rev-parse", "HEAD") != head {
		t.Fatal("must not commit over the user's own uncommitted edits")
	}
}

func TestHandoverReportsAWriterThatChangesNothing(t *testing.T) {
	r := newRig(t)
	head := r.git("rev-parse", "HEAD")
	r.end(2, "FAKE_CLAUDE_MODE=none")
	r.waitFor("none   " + r.root + ": nothing worth handing over")
	if r.git("rev-parse", "HEAD") != head {
		t.Fatal("nothing changed, so nothing to commit")
	}
}

func TestHandoverNeverRunsFromTheWritersOwnSession(t *testing.T) {
	r := newRig(t)
	r.end(5, "CLAUDE_HANDOVER_WRITER=1")
	if r.log() != "" {
		t.Fatalf("the writer's session ending must not queue another writer:\n%s", r.log())
	}
}

func TestSessionStartBriefsFromTheLatestSection(t *testing.T) {
	r := newRig(t)
	r.write("HANDOFF.md", "# Handover 2026-09-29\n\nLatest.\n\n# Handover 2026-09-28\n\nOlder.\n")
	r.git("commit", "-q", "-am", "handover")
	r.write("app.go", "package app\n\nfunc F() {}\n")
	r.git("commit", "-q", "-am", "work after the handover")

	out := r.run("start.sh", map[string]any{"source": "startup", "cwd": r.repo})
	var hook struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &hook); err != nil {
		t.Fatalf("start.sh must print hook JSON: %v\n%s", err, out)
	}
	ctx := hook.HookSpecificOutput.AdditionalContext
	if hook.HookSpecificOutput.HookEventName != "SessionStart" || !strings.Contains(ctx, "Latest.") || strings.Contains(ctx, "Older.") {
		t.Fatalf("want only the latest section:\n%s", ctx)
	}
	if !strings.Contains(ctx, "work after the handover") {
		t.Fatalf("want the commits since the handover:\n%s", ctx)
	}

	if out := r.run("start.sh", map[string]any{"source": "resume", "cwd": r.repo}); strings.TrimSpace(out) != "" {
		t.Fatalf("a resumed session already has its conversation: %s", out)
	}
}

// The audit lists what the writer changed, the dashboard can read exactly
// those files and nothing else, and delete-all moves them aside, recoverably.
func TestAuditListsReadsAndDeletesWhatTheWriterWrote(t *testing.T) {
	r := newRig(t)
	r.end(2)
	r.waitFor("committed")
	sha := r.git("rev-parse", "--short", "HEAD")

	entries, err := Audit()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("want one audited file, got %+v", entries)
	}
	e := entries[0]
	if e.Root != r.root || !e.Exists || e.Writes != 1 || e.Runs != 1 || len(e.Commits) != 1 || e.Commits[0] != sha {
		t.Fatalf("audit entry: %+v (want commit %s)", e, sha)
	}

	body, err := ReadAudited(r.root)
	if err != nil || !strings.Contains(body, "# Handover 2026-09-29") {
		t.Fatalf("reading the audited file: %v\n%s", err, body)
	}
	if _, err := ReadAudited(filepath.Dir(r.root)); err != ErrNotAudited {
		t.Fatalf("a root the writer never touched must not be readable: %v", err)
	}

	backup, results, err := DeleteAudited()
	if err != nil || len(results) != 1 || results[0].Error != "" || !results[0].Tracked {
		t.Fatalf("delete: %v %+v", err, results)
	}
	if _, err := os.Stat(filepath.Join(r.root, "HANDOFF.md")); !os.IsNotExist(err) {
		t.Fatal("HANDOFF.md must be gone")
	}
	saved, _ := os.ReadFile(results[0].Backup)
	if string(saved) != body {
		t.Fatal("the backup must hold the deleted file exactly")
	}
	if m, _ := os.ReadFile(filepath.Join(backup, "MANIFEST.tsv")); !strings.Contains(string(m), filepath.Join(r.root, "HANDOFF.md")) {
		t.Fatalf("manifest: %s", m)
	}
	// Deleting it opts the repository out: the next session end does nothing.
	before := r.log()
	r.end(3)
	if r.log() != before {
		t.Fatal("a repository without HANDOFF.md must be left alone")
	}
}
