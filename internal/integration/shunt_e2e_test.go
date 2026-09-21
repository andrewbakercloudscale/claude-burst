package integration

// End-to-end proof that token shunting works as an ASSEMBLED system.
//
// The unit tests cover the guard rules, the worker, the settings editor and the
// dashboard endpoints one at a time. None of them can tell whether those pieces
// work together the way Claude Code drives them, and that seam is where this
// feature is most likely to break: it is a subprocess speaking a stdin/exit-code
// protocol, editing a file that belongs to someone else, and calling a network
// endpoint. So this builds the real binary and drives it exactly that way, against
// a fake worker endpoint, in a throwaway HOME. It never touches the real
// ~/.claude, the real Keychain or a real provider.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

var (
	buildOnce sync.Once
	binPath   string
	buildErr  error
)

// shuntBinary builds cmd/claude-burst once per test run.
func shuntBinary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "claude-burst-e2e-bin")
		if err != nil {
			buildErr = err
			return
		}
		binPath = filepath.Join(dir, "claude-burst")
		cmd := exec.Command("go", "build", "-o", binPath, "./cmd/claude-burst")
		cmd.Dir = filepath.Join("..", "..")
		if out, err := cmd.CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("go build: %v\n%s", err, out)
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return binPath
}

// fakeWorker is an openai-compatible endpoint that records what it was sent.
type fakeWorker struct {
	mu      sync.Mutex
	prompts []string
	status  int
	reply   func(user string) string
	srv     *httptest.Server
}

func newFakeWorker(t *testing.T) *fakeWorker {
	t.Helper()
	f := &fakeWorker{reply: func(user string) string {
		if strings.Contains(user, "SPECIFICATION:") {
			return "```go\npackage gen\n\nfunc Generated() {}\n```"
		}
		return "the retry loop is at big.go:7"
	}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []map[string]string `json:"messages"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		user := body.Messages[len(body.Messages)-1]["content"]
		f.mu.Lock()
		f.prompts = append(f.prompts, user)
		st := f.status
		f.mu.Unlock()
		if st != 0 {
			http.Error(w, "upstream exploded", st)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": f.reply(user)}, "finish_reason": "stop"}},
			"usage":   map[string]int{"prompt_tokens": 1200, "completion_tokens": 30},
		})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeWorker) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.prompts...)
}

// rig is one throwaway machine: a HOME, a project, and a worker.
type rig struct {
	t       *testing.T
	bin     string
	home    string
	proj    string
	worker  *fakeWorker
	withKey bool
	session string // CLAUDE_CODE_SESSION_ID for commands run as Claude's Bash tool would
}

func newRig(t *testing.T, withKey bool) *rig {
	t.Helper()
	r := &rig{t: t, bin: shuntBinary(t), home: t.TempDir(), proj: t.TempDir(), worker: newFakeWorker(t), withKey: withKey}

	cfgDir := filepath.Join(r.home, ".config", "claude-burst")
	must(t, os.MkdirAll(cfgDir, 0o700))
	cfg := fmt.Sprintf(`{
  "secondary": {"provider":"openai-compatible","base_url":%q,"model":"e2e-model","keychain_service":"claude-burst-e2e"},
  "pricing": {"e2e-model": {"input_per_mtok": 1.0, "output_per_mtok": 4.0}}
}`, r.worker.srv.URL+"/v1")
	must(t, os.WriteFile(filepath.Join(cfgDir, "config.json"), []byte(cfg), 0o600))

	// A settings.json that already belongs to someone: their hook must survive.
	must(t, os.MkdirAll(filepath.Join(r.home, ".claude"), 0o700))
	must(t, os.WriteFile(r.settingsPath(), []byte(`{"model":"sonnet","hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"~/theirs.sh"}]}]}}`), 0o600))

	r.writeFile("big.go", numbered(800))
	r.writeFile("small.go", numbered(20))
	r.writeFile(".env", "API_KEY=sk-super-secret\n"+numbered(600))
	return r
}

func numbered(n int) string {
	var sb strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&sb, "func f%d() {}\n", i)
	}
	return sb.String()
}

func (r *rig) settingsPath() string { return filepath.Join(r.home, ".claude", "settings.json") }
func (r *rig) logPath() string {
	return filepath.Join(r.home, ".config", "claude-burst", "shunt.jsonl")
}

func (r *rig) writeFile(name, content string) {
	must(r.t, os.WriteFile(filepath.Join(r.proj, name), []byte(content), 0o644))
}

// run executes the binary the way Claude Code would, with a minimal, explicit
// environment so nothing from the developer's shell leaks in.
func (r *rig) run(stdin string, args ...string) (stdout, stderr string, code int) {
	r.t.Helper()
	cmd := exec.Command(r.bin, args...)
	cmd.Dir = r.proj
	cmd.Env = []string{"HOME=" + r.home, "PATH=" + os.Getenv("PATH")}
	if r.withKey {
		cmd.Env = append(cmd.Env, "E2E_API_KEY=test-key") // claude-burst-e2e -> E2E_API_KEY
	}
	if r.session != "" {
		cmd.Env = append(cmd.Env, "CLAUDE_CODE_SESSION_ID="+r.session)
	}
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	err := cmd.Run()
	code = 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		r.t.Fatalf("running %v: %v", args, err)
	}
	return so.String(), se.String(), code
}

// guard feeds the hook the payload Claude Code sends, and returns the exit code
// (2 means "blocked", and stderr is what the model is told).
func (r *rig) guard(tool string, input map[string]any) (stderr string, code int) {
	return r.guardAs("e2e-session", tool, input)
}

func (r *rig) guardAs(session, tool string, input map[string]any) (stderr string, code int) {
	r.t.Helper()
	payload, _ := json.Marshal(map[string]any{
		"session_id": session, "cwd": r.proj, "hook_event_name": "PreToolUse",
		"tool_name": tool, "tool_input": input,
	})
	_, se, c := r.run(string(payload), "shunt", "guard")
	return se, c
}

func (r *rig) settings() map[string]any {
	r.t.Helper()
	b, err := os.ReadFile(r.settingsPath())
	must(r.t, err)
	var m map[string]any
	must(r.t, json.Unmarshal(b, &m))
	return m
}

func (r *rig) events() []map[string]any {
	r.t.Helper()
	f, err := os.Open(r.logPath())
	if os.IsNotExist(err) {
		return nil
	}
	must(r.t, err)
	defer f.Close()
	var out []map[string]any
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var m map[string]any
		must(r.t, json.Unmarshal(sc.Bytes(), &m))
		out = append(out, m)
	}
	return out
}

func kinds(evs []map[string]any) []string {
	var k []string
	for _, e := range evs {
		k = append(k, fmt.Sprint(e["kind"]))
	}
	return k
}

func preToolUseCommands(m map[string]any) []string {
	var out []string
	hooks, _ := m["hooks"].(map[string]any)
	groups, _ := hooks["PreToolUse"].([]any)
	for _, g := range groups {
		gm, _ := g.(map[string]any)
		inner, _ := gm["hooks"].([]any)
		for _, h := range inner {
			hm, _ := h.(map[string]any)
			out = append(out, fmt.Sprint(hm["command"]))
		}
	}
	return out
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func contains(t *testing.T, what, got string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("%s: missing %q in:\n%s", what, w, got)
		}
	}
}

// TestShuntEndToEnd is the whole life of the feature in order: switch it on,
// watch the guard block and pass the right things, delegate a read, generate a
// file, then switch it off and confirm the machine is back as it was.
func TestShuntEndToEnd(t *testing.T) {
	r := newRig(t, true)
	original := r.settings()

	// --- enable: hook installed, the user's own hook untouched
	out, errOut, code := r.run("", "shunt", "enable")
	if code != 0 {
		t.Fatalf("enable failed (%d): %s%s", code, out, errOut)
	}
	cmds := preToolUseCommands(r.settings())
	if len(cmds) != 2 || cmds[0] != "~/theirs.sh" || !strings.HasSuffix(cmds[1], " shunt guard") {
		t.Fatalf("want the user's hook then ours, got %v", cmds)
	}
	if r.settings()["model"] != "sonnet" {
		t.Errorf("an unrelated setting was disturbed")
	}
	skill := filepath.Join(r.home, ".claude", "skills", "claude-burst-shunt", "SKILL.md")
	if _, err := os.Stat(skill); err != nil {
		t.Errorf("skill not installed: %v", err)
	}

	// --- the guard, driven through the real hook protocol
	big := filepath.Join(r.proj, "big.go")
	blocked := []struct {
		name  string
		tool  string
		input map[string]any
	}{
		{"whole-file Read", "Read", map[string]any{"file_path": big}},
		{"cat via Bash", "Bash", map[string]any{"command": "cat big.go"}},
	}
	for _, b := range blocked {
		se, c := r.guard(b.tool, b.input)
		if c != 2 {
			t.Errorf("%s: exit %d, want 2 (blocked)", b.name, c)
		}
		contains(t, b.name+" message", se, "Do NOT retry", "shunt read --question", "offset and limit")
	}
	allowed := []struct {
		name  string
		tool  string
		input map[string]any
	}{
		{"windowed Read", "Read", map[string]any{"file_path": big, "offset": 1, "limit": 100}},
		{"small file", "Read", map[string]any{"file_path": filepath.Join(r.proj, "small.go")}},
		{"filtered cat", "Bash", map[string]any{"command": "cat big.go | grep f1"}},
		{"sed window", "Bash", map[string]any{"command": "sed -n '1,50p' big.go"}},
		{"credentials file", "Read", map[string]any{"file_path": filepath.Join(r.proj, ".env")}},
		{"a tool it does not guard", "Edit", map[string]any{"file_path": big}},
	}
	for _, a := range allowed {
		if se, c := r.guard(a.tool, a.input); c != 0 {
			t.Errorf("%s: exit %d, want 0 (allowed): %s", a.name, c, se)
		}
	}
	// Fails open: a guard that cannot understand its input must never block.
	if _, _, c := r.run("this is not json", "shunt", "guard"); c != 0 {
		t.Errorf("garbage on stdin must be allowed through, got exit %d", c)
	}

	// --- delegate a read, exactly as the refusal tells the model to
	out, errOut, code = r.run("", "shunt", "read", "--question", "where is the retry loop?", "big.go", ".env")
	if code != 0 {
		t.Fatalf("shunt read failed (%d): %s%s", code, out, errOut)
	}
	contains(t, "read output", out, "the retry loop is at big.go:7", "[shunt:", "kept out of context")
	prompts := r.worker.seen()
	if len(prompts) != 1 {
		t.Fatalf("want exactly one worker call, got %d", len(prompts))
	}
	contains(t, "worker prompt", prompts[0], "QUESTION: where is the retry loop?", "   800\tfunc f800() {}")
	if strings.Contains(prompts[0], "sk-super-secret") {
		t.Fatalf("a credentials file reached the worker")
	}
	contains(t, "skipped credentials note", out, ".env")

	// --- generate a file straight to disk, then overwrite it: previous version kept
	out, errOut, code = r.run("", "shunt", "write", "--spec", "make a generator", "--ref", "small.go", "--out", "gen/gen.go")
	if code != 0 {
		t.Fatalf("shunt write failed (%d): %s%s", code, out, errOut)
	}
	got, err := os.ReadFile(filepath.Join(r.proj, "gen", "gen.go"))
	must(t, err)
	if string(got) != "package gen\n\nfunc Generated() {}\n" {
		t.Errorf("fence must be stripped and the file written, got %q", got)
	}
	if strings.Contains(out, "func Generated") {
		t.Errorf("the generated code must not be echoed into the model's context")
	}
	r.worker.reply = func(string) string { return "package gen\n\nfunc Second() {}" }
	if _, _, code = r.run("", "shunt", "write", "--spec", "again", "--out", "gen/gen.go"); code != 0 {
		t.Fatalf("overwrite failed")
	}
	if bak, _ := os.ReadFile(filepath.Join(r.proj, "gen", "gen.go.bak")); string(bak) != "package gen\n\nfunc Generated() {}\n" {
		t.Errorf("the previous version must be kept as .bak, got %q", bak)
	}

	// --- the log tells the story
	if got, want := kinds(r.events()), []string{"deny", "deny", "guard_error", "read", "write", "write"}; !reflect.DeepEqual(got, want) {
		t.Errorf("shunt.jsonl kinds = %v, want %v", got, want)
	}
	for _, e := range r.events() {
		if e["kind"] == "read" || e["kind"] == "write" {
			if e["ok"] != true || e["model"] != "e2e-model" {
				t.Errorf("worker call not recorded properly: %v", e)
			}
		}
	}
	statusOut, _, _ := r.run("", "shunt", "status")
	contains(t, "status", statusOut, "shunt hook: installed", "shunt worker: ready", "reads=1 writes=2 denied_direct_reads=2")

	// --- disable: guard stands down immediately, the machine is back as it was
	if _, _, code := r.run("", "shunt", "disable"); code != 0 {
		t.Fatalf("disable failed")
	}
	if !reflect.DeepEqual(r.settings(), original) {
		t.Errorf("disable must restore settings.json exactly:\n got  %v\n want %v", r.settings(), original)
	}
	if _, err := os.Stat(skill); err == nil {
		t.Errorf("skill should be removed")
	}
	if se, c := r.guard("Read", map[string]any{"file_path": big}); c != 0 {
		t.Errorf("once off, a whole-file read must be allowed, exit %d: %s", c, se)
	}
	if _, se, c := r.run("", "shunt", "read", "--question", "x", "big.go"); c == 0 || !strings.Contains(se, "offset and limit") {
		t.Errorf("shunt read while off must refuse and say how to proceed, exit %d: %s", c, se)
	}
}

// Enabling must never leave a hook that blocks reads with nothing to redirect
// them to. A machine with no key is exactly that.
func TestShuntWillNotEnableWithoutAWorker(t *testing.T) {
	r := newRig(t, false)
	before := r.settings()
	_, se, code := r.run("", "shunt", "enable")
	if code == 0 {
		t.Fatalf("enable must fail with no worker key")
	}
	contains(t, "enable error", se, "cannot enable", "keychain-set")
	if !reflect.DeepEqual(r.settings(), before) {
		t.Errorf("a refused enable must not touch settings.json")
	}
	if _, c := r.guard("Read", map[string]any{"file_path": filepath.Join(r.proj, "big.go")}); c != 0 {
		t.Errorf("with the feature never enabled the guard must allow, exit %d", c)
	}
}

// A worker that fails must fail LOUDLY and leave a trace, and the model must be
// told how to carry on without it.
func TestShuntWorkerFailureIsReportedAndLogged(t *testing.T) {
	r := newRig(t, true)
	if _, se, c := r.run("", "shunt", "enable"); c != 0 {
		t.Fatalf("enable: %s", se)
	}
	r.worker.status = 500

	_, se, code := r.run("", "shunt", "read", "--question", "anything", "big.go")
	if code == 0 {
		t.Fatalf("a failed worker call must exit non-zero")
	}
	contains(t, "failure message", se, "HTTP 500", "offset and limit")

	var failed int
	for _, e := range r.events() {
		if e["kind"] == "read" && e["ok"] == false {
			failed++
			if !strings.Contains(fmt.Sprint(e["note"]), "500") {
				t.Errorf("the failure's reason must be in the log: %v", e)
			}
		}
	}
	if failed != 1 {
		t.Errorf("want exactly one logged failure, got %d (%v)", failed, kinds(r.events()))
	}
}

// A guard that reads a broken config must let the call through, not block every
// read on the machine.
func TestShuntGuardFailsOpenOnBrokenConfig(t *testing.T) {
	r := newRig(t, true)
	if _, se, c := r.run("", "shunt", "enable"); c != 0 {
		t.Fatalf("enable: %s", se)
	}
	must(t, os.WriteFile(filepath.Join(r.home, ".config", "claude-burst", "config.json"), []byte("{ not json"), 0o600))
	if se, c := r.guard("Read", map[string]any{"file_path": filepath.Join(r.proj, "big.go")}); c != 0 {
		t.Errorf("a broken config must not turn the guard into a wall, exit %d: %s", c, se)
	}
}

// last returns the newest event of a kind.
func (r *rig) last(kind string) map[string]any {
	evs := r.events()
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i]["kind"] == kind {
			return evs[i]
		}
	}
	return nil
}

// A refusal has to say WHICH session, WHICH project, WHICH file and how many
// times, or a looping session cannot be told apart from a healthy one. A real
// session was refused six times in three minutes and nothing recorded who.
func TestShuntLogNamesSessionProjectFileAndRepeats(t *testing.T) {
	r := newRig(t, true)
	if _, se, c := r.run("", "shunt", "enable"); c != 0 {
		t.Fatalf("enable: %s", se)
	}
	const loop = "aaaa1111-2222-3333-4444-555555555555"
	const other = "bbbb9999-2222-3333-4444-555555555555"
	cat := map[string]any{"command": "cat big.go"}

	var msgs []string
	for i := 0; i < 3; i++ {
		se, c := r.guardAs(loop, "Bash", cat)
		if c != 2 {
			t.Fatalf("refusal %d: exit %d", i+1, c)
		}
		msgs = append(msgs, se)
	}
	if strings.Contains(msgs[0], "REFUSAL #") {
		t.Errorf("the first refusal must not claim to be a repeat:\n%s", msgs[0])
	}
	contains(t, "second refusal", msgs[1], "REFUSAL #2")
	contains(t, "third refusal", msgs[2], "REFUSAL #3", "Retrying cannot succeed", "shunt read --question")

	var denies []map[string]any
	for _, e := range r.events() {
		if e["kind"] == "deny" {
			denies = append(denies, e)
		}
	}
	if len(denies) != 3 {
		t.Fatalf("want 3 refusals logged, got %d", len(denies))
	}
	for i, e := range denies {
		if e["session_id"] != loop || e["cwd"] != r.proj || e["tool"] != "Bash cat" || e["path"] != filepath.Join(r.proj, "big.go") {
			t.Errorf("refusal %d does not say who/where/what: %v", i+1, e)
		}
		if int(e["threshold"].(float64)) != 350 || int(e["lines"].(float64)) < 350 {
			t.Errorf("refusal %d must record the lines and threshold: %v", i+1, e)
		}
		if got, _ := e["repeat"].(float64); int(got) != i {
			t.Errorf("refusal %d: repeat = %v, want %d", i+1, e["repeat"], i)
		}
	}

	// another session hitting the same file is NOT a loop
	r.guardAs(other, "Bash", cat)
	if last := r.last("deny"); last["repeat"] != nil {
		t.Errorf("a different session must start its own count: %v", last)
	}

	// following the redirect resets the count for the session that did
	r.session = loop
	if _, se, c := r.run("", "shunt", "read", "--question", "where?", "big.go"); c != 0 {
		t.Fatalf("shunt read: %s", se)
	}
	// A process's working directory is the symlink-resolved path (/private/var
	// on macOS), while the hook payload carries whatever Claude Code sent.
	resolved, err := filepath.EvalSymlinks(r.proj)
	must(t, err)
	if got := r.last("read"); got["session_id"] != loop || got["cwd"] != resolved {
		t.Errorf("a delegated read must record its session and project: %v", got)
	}
	r.guardAs(loop, "Bash", cat)
	if last := r.last("deny"); last["repeat"] != nil {
		t.Errorf("an answered read must reset the repeat count: %v", last)
	}

	// the plain-text view
	out, _, code := r.run("", "shunt", "log", "-n", "50")
	if code != 0 {
		t.Fatalf("shunt log failed")
	}
	contains(t, "shunt log", out, "LOOP", "refused a direct Bash cat of big.go", "retrying instead of running shunt read",
		"session aaaa1111", "session bbbb9999", filepath.Base(r.proj), "READ ", "delegated read of 1 file(s)")
	probs, _, _ := r.run("", "shunt", "log", "--problems")
	if !strings.Contains(probs, "LOOP") || strings.Contains(probs, "delegated read") || strings.Contains(probs, "session bbbb9999") {
		t.Errorf("--problems must show only what needs a look:\n%s", probs)
	}
	only, _, _ := r.run("", "shunt", "log", "--session", "bbbb")
	if !strings.Contains(only, "session bbbb9999") || strings.Contains(only, "session aaaa1111") {
		t.Errorf("--session must filter:\n%s", only)
	}
	raw, _, _ := r.run("", "shunt", "log", "--json", "-n", "2")
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Errorf("--json must print one JSON object per line: %q", line)
		}
	}
	status, _, _ := r.run("", "shunt", "status")
	contains(t, "status", status, "shunt PROBLEMS")
}

// Failures used to call fatal() BEFORE the log write, so a disabled feature, a
// missing worker or a bad argument left no trace and looked like a refusal that
// nothing ever followed up.
func TestShuntEveryFailurePathLeavesATrace(t *testing.T) {
	r := newRig(t, true)
	r.session = "cccc0000-1111-2222-3333-444444444444"
	stageOf := func() (string, string) {
		evs := r.events()
		if len(evs) == 0 {
			return "", ""
		}
		e := evs[len(evs)-1]
		return fmt.Sprint(e["stage"]), fmt.Sprint(e["session_id"])
	}
	expect := func(what, want string, args ...string) {
		t.Helper()
		before := len(r.events())
		_, se, code := r.run("", args...)
		if code == 0 {
			t.Errorf("%s: must exit non-zero", what)
		}
		if len(r.events()) != before+1 {
			t.Fatalf("%s: a failure must log exactly one event (stderr: %s)", what, se)
		}
		if st, sess := stageOf(); st != want || sess != r.session {
			t.Errorf("%s: stage=%q session=%q, want stage %q session %q", what, st, sess, want, r.session)
		}
	}

	expect("read while off", "disabled", "shunt", "read", "--question", "x", "big.go")
	expect("write while off", "disabled", "shunt", "write", "--spec", "x", "--out", "a.go")

	if _, se, c := r.run("", "shunt", "enable"); c != 0 {
		t.Fatalf("enable: %s", se)
	}
	expect("read without a question", "args", "shunt", "read", "big.go")
	expect("read without a file", "args", "shunt", "read", "--question", "x")
	expect("a bad flag", "args", "shunt", "read", "--no-such-flag")
	expect("write without a spec", "args", "shunt", "write", "--out", "a.go")
	expect("write to a credentials file", "args", "shunt", "write", "--spec", "x", "--out", ".env")
	expect("read of only secrets", "args", "shunt", "read", "--question", "x", ".env")
	if n := len(r.worker.seen()); n != 0 {
		t.Errorf("none of those should have reached the worker, got %d calls", n)
	}

	r.worker.reply = func(string) string { return "I'm sorry, but I can't help with that." }
	expect("a refusing worker", "validate", "shunt", "write", "--spec", "make it", "--out", "a.go")
	if _, err := os.Stat(filepath.Join(r.proj, "a.go")); err == nil {
		t.Errorf("a rejected generation must not leave a file")
	}

	r.worker.status = 500
	expect("a failing provider", "worker_call", "shunt", "read", "--question", "x", "big.go")

	r.withKey = false // the key goes missing after it was enabled
	expect("a vanished key", "worker_init", "shunt", "read", "--question", "x", "big.go")

	out, _, _ := r.run("", "shunt", "log", "--problems", "-n", "50")
	contains(t, "problems view", out, "READ-FAIL", "WRITE-FAIL", "FAILED at disabled", "FAILED at args", "FAILED at validate", "FAILED at worker_call", "FAILED at worker_init")
}

// A guard that cannot decide lets the call through; that must not be invisible.
func TestShuntGuardErrorsAreLogged(t *testing.T) {
	r := newRig(t, true)
	if _, se, c := r.run("", "shunt", "enable"); c != 0 {
		t.Fatalf("enable: %s", se)
	}
	if _, _, c := r.run("this is not json", "shunt", "guard"); c != 0 {
		t.Fatalf("garbage must be allowed through")
	}
	if e := r.last("guard_error"); e == nil || e["stage"] != "input" || e["ok"] != false {
		t.Errorf("garbage stdin must log a guard_error at stage input: %v", e)
	}

	must(t, os.WriteFile(filepath.Join(r.home, ".config", "claude-burst", "config.json"), []byte("{ not json"), 0o600))
	if se, c := r.guardAs("dddd0000-1111", "Read", map[string]any{"file_path": filepath.Join(r.proj, "big.go")}); c != 0 {
		t.Fatalf("broken config must not block: %s", se)
	}
	if e := r.last("guard_error"); e["stage"] != "config" || e["session_id"] != "dddd0000-1111" {
		t.Errorf("a broken config must log a guard_error at stage config with the session: %v", e)
	}
	out, _, _ := r.run("", "shunt", "log", "--problems")
	contains(t, "log", out, "GUARD-ERR", "the call was ALLOWED through")
}
