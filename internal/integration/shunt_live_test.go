package integration

// LIVE proof that token shunting works on THIS machine, driven by a real Claude
// Code session.
//
// shunt_e2e_test.go builds the binary and speaks the hook protocol itself, against
// a fake worker. That proves the pieces fit together, but it cannot prove the
// things only a real Claude Code can: that the hook payload has the shape we parse,
// that the refusal text reaches the model, that a real model acts on it, that
// CLAUDE_CODE_SESSION_ID really is in the environment Claude's Bash tool gives us,
// and that the worker call goes to the real provider. This test runs `claude -p`
// against a big file and then reads shunt.jsonl for THAT session's own events.
//
// It is opt-in, like TestLiveSecondaryAcceptsTranslatedRequest, because it spends
// real money (a few cents of Claude on haiku, a fraction of a cent of Together) and
// depends on this machine's real setup. It uses the INSTALLED binary and the REAL
// config and log, on purpose: the question it answers is "is the shunting I have
// deployed actually working?", not "does the source compile". Its events land in
// your real ~/.config/claude-burst/shunt.jsonl under a project named
// shunt-live-check-*, so they are easy to recognise in `claude-burst shunt log`.
//
//	CLAUDE_BURST_LIVE_SHUNT=1 go test ./internal/integration/ -run TestLiveShunt -v -timeout 10m
//
// Requires: `claude` on PATH and logged in, `claude-burst shunt enable` already
// run (hook installed, worker ready), and network access to the worker.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const liveEnv = "CLAUDE_BURST_LIVE_SHUNT"

// liveStep is one tool call or tool result from a Claude Code session, in order.
type liveStep struct {
	kind  string // "tool_use" | "tool_result"
	tool  string
	text  string
	isErr bool
}

func (s liveStep) String() string {
	t := strings.ReplaceAll(s.text, "\n", " | ")
	if len(t) > 220 {
		t = t[:220] + "..."
	}
	if s.kind == "tool_use" {
		return fmt.Sprintf("  CALL   %s %s", s.tool, t)
	}
	if s.isErr {
		return "  RESULT (error) " + t
	}
	return "  RESULT " + t
}

func transcript(steps []liveStep) string {
	var sb strings.Builder
	for _, s := range steps {
		sb.WriteString(s.String() + "\n")
	}
	return sb.String()
}

type liveRig struct {
	t       *testing.T
	bin     string // the installed claude-burst, which is what the real hook runs
	claude  string
	dir     string // the project Claude runs in
	session string
}

func newLiveRig(t *testing.T) *liveRig {
	t.Helper()
	if os.Getenv(liveEnv) == "" {
		t.Skipf("%s not set. This test drives a real Claude Code session and spends a few cents; run it with:\n  %s=1 go test ./internal/integration/ -run TestLiveShunt -v -timeout 10m", liveEnv, liveEnv)
	}
	claude, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude is not on PATH")
	}
	home, err := os.UserHomeDir()
	must(t, err)
	bin := filepath.Join(home, ".local", "bin", "claude-burst")
	if _, err := os.Stat(bin); err != nil {
		t.Fatalf("the installed binary %s is missing: %v", bin, err)
	}

	// Refuse to "pass" on a machine where shunting is off: the guard would allow
	// every read and the test would prove nothing.
	out, err := exec.Command(bin, "shunt", "status").CombinedOutput()
	if err != nil {
		t.Fatalf("shunt status failed: %v\n%s", err, out)
	}
	for _, need := range []string{"read=true", "shunt hook: installed", "shunt worker: ready"} {
		if !strings.Contains(string(out), need) {
			t.Fatalf("shunting is not fully enabled on this machine (missing %q). Run: claude-burst shunt enable\n%s", need, out)
		}
	}

	dir, err := os.MkdirTemp("", "shunt-live-check-")
	must(t, err)
	t.Cleanup(func() { os.RemoveAll(dir) })
	var sb strings.Builder
	sb.WriteString("package live\n\n")
	for i := 1; i <= 600; i++ {
		if i == 431 {
			sb.WriteString("const MAX_RETRIES = 7 // SHUNT-LIVE-CHECK: the retry limit\n")
			continue
		}
		fmt.Fprintf(&sb, "func helper%d() int { return %d }\n", i, i)
	}
	must(t, os.WriteFile(filepath.Join(dir, "big.go"), []byte(sb.String()), 0o644))

	return &liveRig{t: t, bin: bin, claude: claude, dir: dir, session: newUUID(t)}
}

func newUUID(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("uuidgen").Output()
	if err != nil {
		t.Fatalf("uuidgen: %v", err)
	}
	return strings.ToLower(strings.TrimSpace(string(out)))
}

// ask runs one non-interactive Claude Code session and returns what it did.
func (l *liveRig) ask(prompt string) (steps []liveStep, result string) {
	l.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, l.claude, "-p", prompt,
		"--session-id", l.session,
		"--model", "haiku",
		"--output-format", "stream-json", "--verbose",
		"--allowedTools", "Read", "Bash",
		"--max-budget-usd", "0.50",
		"--no-session-persistence")
	cmd.Dir = l.dir
	cmd.Stdin = nil // /dev/null: without it claude waits 3s for piped input

	// This may itself be running inside a Claude Code session. Its CLAUDE_CODE_*
	// variables describe THAT session and would confuse the child, so drop them.
	for _, kv := range os.Environ() {
		k := strings.SplitN(kv, "=", 2)[0]
		if strings.HasPrefix(k, "CLAUDE_CODE_") || k == "CLAUDECODE" || k == "CLAUDE_PID" {
			continue
		}
		cmd.Env = append(cmd.Env, kv)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	must(l.t, err)
	must(l.t, cmd.Start())

	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 1<<20), 8<<20)
	for sc.Scan() {
		var o map[string]any
		if json.Unmarshal(sc.Bytes(), &o) != nil {
			continue
		}
		if o["type"] == "result" {
			result, _ = o["result"].(string)
			continue
		}
		msg, _ := o["message"].(map[string]any)
		content, _ := msg["content"].([]any)
		for _, b := range content {
			bm, _ := b.(map[string]any)
			switch bm["type"] {
			case "tool_use":
				in, _ := json.Marshal(bm["input"])
				steps = append(steps, liveStep{kind: "tool_use", tool: fmt.Sprint(bm["name"]), text: string(in)})
			case "tool_result":
				text := ""
				switch c := bm["content"].(type) {
				case string:
					text = c
				case []any:
					for _, part := range c {
						if pm, ok := part.(map[string]any); ok {
							text += fmt.Sprint(pm["text"])
						}
					}
				}
				isErr, _ := bm["is_error"].(bool)
				steps = append(steps, liveStep{kind: "tool_result", text: text, isErr: isErr})
			}
		}
	}
	if err := cmd.Wait(); err != nil && ctx.Err() == nil {
		l.t.Logf("claude exited with %v (stderr: %s)", err, strings.TrimSpace(stderr.String()))
	}
	if ctx.Err() != nil {
		l.t.Fatalf("claude did not finish within 4 minutes\n%s", transcript(steps))
	}
	return steps, strings.TrimSpace(result)
}

// events returns this session's shunt events, oldest first, via the installed
// binary's own `shunt log --json` so the test reads the log the way a person would.
func (l *liveRig) events() []map[string]any {
	l.t.Helper()
	out, err := exec.Command(l.bin, "shunt", "log", "--session", l.session, "--json", "-n", "200").Output()
	if err != nil {
		l.t.Fatalf("shunt log: %v", err)
	}
	var evs []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var m map[string]any
		must(l.t, json.Unmarshal([]byte(line), &m))
		evs = append(evs, m)
	}
	return evs
}

func (l *liveRig) plainLog() string {
	out, _ := exec.Command(l.bin, "shunt", "log", "--session", l.session).CombinedOutput()
	return strings.TrimSpace(string(out))
}

func ofKind(evs []map[string]any, kind string) []map[string]any {
	var out []map[string]any
	for _, e := range evs {
		if e["kind"] == kind {
			out = append(out, e)
		}
	}
	return out
}

// assertRefusedAndLogged checks the half that does not depend on what the model
// chooses to do next: a real Claude Code whole-file Read is blocked, the refusal
// text reaches the model, and the log names this session, this project, this file.
func (l *liveRig) assertRefusedAndLogged(steps []liveStep) []map[string]any {
	t := l.t
	t.Helper()

	var delivered bool
	for _, s := range steps {
		if s.kind == "tool_result" && s.isErr && strings.Contains(s.text, "shunt read --question") {
			delivered = true
		}
	}
	if !delivered {
		t.Fatalf("the refusal never reached the model: no Read came back blocked with the shunt read instructions.\nIf the model never attempted a whole-file Read, the guard had nothing to block; otherwise the hook is not firing.\n%s", transcript(steps))
	}

	evs := l.events()
	denies := ofKind(evs, "deny")
	if len(denies) == 0 {
		t.Fatalf("the model was blocked but nothing was logged for session %s.\nlog: %s\n%s", l.session, l.plainLog(), transcript(steps))
	}
	d := denies[0]
	if d["session_id"] != l.session {
		t.Errorf("the refusal must carry this session's id: %v", d)
	}
	if d["tool"] != "Read" || !strings.HasSuffix(fmt.Sprint(d["path"]), "big.go") {
		t.Errorf("the refusal must name the tool and file: %v", d)
	}
	if !strings.HasPrefix(filepath.Base(fmt.Sprint(d["cwd"])), "shunt-live-check-") {
		t.Errorf("the refusal must record the project directory: %v", d)
	}
	if lines, _ := d["lines"].(float64); lines < 350 {
		t.Errorf("the refusal must record the line count and threshold: %v", d)
	}
	for _, e := range denies {
		if rep, _ := e["repeat"].(float64); rep >= 2 {
			t.Errorf("the model was refused the same file %d times: a loop.\n%s", int(rep)+1, l.plainLog())
		}
	}
	return evs
}

// TestLiveShuntNaturalBehaviour asks a real model to read a big file and records
// what it does WITHOUT telling it how to react. It asserts only what is true
// whatever route the model takes: the block is real, delivered and logged, the
// model is not stuck retrying, and it still gets the right answer.
func TestLiveShuntNaturalBehaviour(t *testing.T) {
	l := newLiveRig(t)
	steps, result := l.ask("Use the Read tool on the whole file big.go (do not pass offset or limit) and tell me the value of MAX_RETRIES. Reply with just the number.")
	evs := l.assertRefusedAndLogged(steps)

	if !strings.Contains(result, "7") {
		t.Errorf("the model must still reach the right answer (7), got %q\n%s", result, transcript(steps))
	}

	// Informational: which route did it take after being blocked? Both are fine,
	// and knowing which is what tells you whether the worker is being used.
	route := "an alternative to shunt read (e.g. grep/sed)"
	if len(ofKind(evs, "read")) > 0 {
		route = "shunt read (the worker)"
	}
	t.Logf("after the block the model used %s\n%s\nshunt log for this session:\n%s", route, transcript(steps), l.plainLog())
}

// TestLiveShuntDelegatesToTheWorker is the proof that the whole redirect works
// with a real model: it is told to follow the refusal's instructions, does, and
// the log then shows the delegated read from the same session with a real
// provider call behind it.
func TestLiveShuntDelegatesToTheWorker(t *testing.T) {
	l := newLiveRig(t)
	steps, result := l.ask("Use the Read tool on the whole file big.go (do not pass offset or limit) and report the value of MAX_RETRIES. " +
		"Do not use grep, sed, head, tail, awk or any search. If the Read is blocked, follow the instructions in the error message exactly, then reply with just the number.")
	evs := l.assertRefusedAndLogged(steps)

	reads := ofKind(evs, "read")
	if len(reads) == 0 {
		t.Fatalf("the model was blocked and told to run shunt read, but no delegated read was logged for session %s.\nEither it did not follow the instructions, or `shunt read` ran without this session's id (CLAUDE_CODE_SESSION_ID missing from Claude's Bash environment).\nlog: %s\n%s", l.session, l.plainLog(), transcript(steps))
	}
	var ran bool
	for _, s := range steps {
		if s.kind == "tool_use" && s.tool == "Bash" && strings.Contains(s.text, "shunt read") {
			ran = true
		}
	}
	if !ran {
		t.Errorf("a delegated read is logged but the transcript shows no `shunt read` command:\n%s", transcript(steps))
	}

	r := reads[len(reads)-1]
	if r["ok"] != true {
		t.Fatalf("the delegated read failed: %v\nlog: %s", r, l.plainLog())
	}
	if r["session_id"] != l.session {
		t.Errorf("the delegated read must carry the session id from Claude's Bash environment: %v", r)
	}
	if !strings.Contains(fmt.Sprint(r["paths"]), "big.go") {
		t.Errorf("the delegated read must record the file it was asked about: %v", r)
	}
	if in, _ := r["input_tokens"].(float64); in <= 0 {
		t.Errorf("no input tokens recorded: was the worker actually called? %v", r)
	}
	if r["model"] == nil || r["model"] == "" || r["destination"] == nil {
		t.Errorf("the read must record which worker model and endpoint served it: %v", r)
	}
	if bytesIn, _ := r["bytes_in"].(float64); bytesIn < 10_000 {
		t.Errorf("the whole file should have gone to the worker, bytes_in=%v", r["bytes_in"])
	}

	// The refusal came before the delegation, and nothing looped.
	denyAt, readAt := ofKind(evs, "deny")[0]["time"].(string), r["time"].(string)
	if denyAt > readAt {
		t.Errorf("the read was logged before the refusal: %s > %s", denyAt, readAt)
	}
	if !strings.Contains(result, "7") {
		t.Errorf("the answer from the worker must reach the right conclusion (7), got %q\n%s", result, transcript(steps))
	}
	for _, e := range evs {
		if e["kind"] == "guard_error" || (e["ok"] == false && e["kind"] != "deny") {
			t.Errorf("unexpected problem in the log: %v", e)
		}
	}
	t.Logf("proved end to end with a real model:\n%s\nshunt log for this session:\n%s", transcript(steps), l.plainLog())
}
