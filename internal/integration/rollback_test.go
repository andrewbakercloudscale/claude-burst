package integration

// scripts/rollback.sh (burst-off), run for real under zsh in a throwaway HOME.
//
// Run unguarded this script stops the developer's live gateway (launchctl
// bootout, pkill), kills whatever lsof reports on Burst's ports and edits
// launchd's environment and the macOS proxy settings. So every one of those
// tools is a stub on PATH that logs its call, the run is refused unless zsh
// resolves each of them to its stub first, and the only processes the script
// can signal are two `sleep`s this test started itself.
//
// What is pinned: a listener that is not Claude Burst is left running and
// named; only a local value on one of Burst's own ports is removed from
// settings.json, launchd's environment and the macOS proxy; and root runs the
// root-owned copy of the helper when there is one.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// rollbackStubs are every tool rollback.sh runs that would touch the real Mac.
var rollbackStubs = map[string]string{
	// `sudo -n true` is the script asking whether it has root. Anything else
	// is a root step: logged, never run.
	"sudo": `#!/bin/sh
echo "sudo $*" >> "$STUB/calls"
if [ "$*" = "-n true" ]; then [ -f "$STUB/have-root" ] && exit 0; exit 1; fi
exit 0
`,
	"launchctl": `#!/bin/sh
echo "launchctl $*" >> "$STUB/calls"
if [ "$1" = getenv ] && [ -f "$STUB/launchenv-$2" ]; then cat "$STUB/launchenv-$2"; fi
exit 0
`,
	"pkill": "#!/bin/sh\necho \"pkill $*\" >> \"$STUB/calls\"\nexit 0\n",
	"pgrep": "#!/bin/sh\necho \"pgrep $*\" >> \"$STUB/calls\"\nexit 1\n",
	// lsof -nP -t -iTCP:<port> -sTCP:LISTEN: the pids in $STUB/port-<port>.
	"lsof": `#!/bin/sh
echo "lsof $*" >> "$STUB/calls"
for a; do case "$a" in -iTCP:*) p="${a#-iTCP:}" ;; esac; done
[ -f "$STUB/port-$p" ] && cat "$STUB/port-$p"
exit 0
`,
	// ps -o comm= -p <pid>: the program path in $STUB/comm-<pid>.
	"ps": `#!/bin/sh
echo "ps $*" >> "$STUB/calls"
for a; do pid="$a"; done
[ -f "$STUB/comm-$pid" ] && cat "$STUB/comm-$pid"
exit 0
`,
	// The real output has four lines, and the fourth also contains "Enabled:".
	"networksetup": `#!/bin/sh
echo "networksetup $*" >> "$STUB/calls"
case "$1" in
  -listallnetworkservices) echo "An asterisk (*) denotes that a network service is disabled."; echo "Wi-Fi" ;;
  -getwebproxy|-getsecurewebproxy)
    if [ -f "$STUB/proxy$1" ]; then cat "$STUB/proxy$1"; else printf 'Enabled: No\nServer: \nPort: 0\nAuthenticated Proxy Enabled: 0\n'; fi ;;
esac
exit 0
`,
	"curl":     "#!/bin/sh\necho \"curl $*\" >> \"$STUB/calls\"\nprintf '200|160.79.104.10'\n",
	"security": "#!/bin/sh\necho \"security $*\" >> \"$STUB/calls\"\nexit 44\n",
}

type rollbackRig struct {
	t                *testing.T
	home, stub, bin  string
	libexec, script  string
	env              []string
	settings, config string
}

func newRollbackRig(t *testing.T) *rollbackRig {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("rollback.sh is macOS-only")
	}
	if _, err := exec.LookPath("zsh"); err != nil {
		t.Skip("zsh not available")
	}
	if os.Geteuid() == 0 {
		t.Skip("rollback.sh refuses to run as root")
	}
	dir := t.TempDir()
	r := &rollbackRig{
		t:       t,
		home:    filepath.Join(dir, "home"),
		stub:    filepath.Join(dir, "stub"),
		bin:     filepath.Join(dir, "bin"),
		libexec: filepath.Join(dir, "libexec"),
		script:  filepath.Join(repoRoot(t), "scripts", "rollback.sh"),
	}
	r.settings = filepath.Join(r.home, ".claude", "settings.json")
	r.config = filepath.Join(r.home, ".config", "claude-burst", "config.json")
	for _, d := range []string{filepath.Dir(r.settings), filepath.Dir(r.config), r.stub, r.bin, r.libexec} {
		must(t, os.MkdirAll(d, 0o755))
	}
	for name, body := range rollbackStubs {
		must(t, os.WriteFile(filepath.Join(r.bin, name), []byte(body), 0o755))
	}
	// Same reason as transparent_root_test.go: /usr/bin/python3 is a slow shim.
	if py, err := exec.LookPath("python3"); err == nil {
		must(t, os.Symlink(py, filepath.Join(r.bin, "python3")))
	}
	hosts := filepath.Join(dir, "hosts")
	must(t, os.WriteFile(hosts, []byte("127.0.0.1 localhost\n"), 0o644))
	// Built from nothing: none of the developer's environment may reach the
	// script (NODE_EXTRA_CA_CERTS and CODEX_HOME name real files).
	r.env = []string{
		"HOME=" + r.home, "ZDOTDIR=" + r.home,
		"PATH=" + r.bin + ":/usr/bin:/bin",
		"STUB=" + r.stub,
		"CLAUDE_BURST_HOSTS_FILE=" + hosts,
		"CLAUDE_BURST_PFHEAL_LIBEXEC=" + r.libexec,
		"CLAUDE_BURST_BIN=" + filepath.Join(dir, "no-such-binary"),
		"CLAUDE_BURST_AUDIT_FILE=" + filepath.Join(dir, "audit.jsonl"),
	}
	return r
}

func (r *rollbackRig) run() (out string, calls string, code int) {
	t := r.t
	t.Helper()
	// Refuse to start unless zsh, with this exact environment, finds every
	// dangerous tool in the stub directory. A missed stub here is not a
	// failed test: it is the developer's gateway stopped.
	names := make([]string, 0, len(rollbackStubs))
	for name := range rollbackStubs {
		names = append(names, name)
	}
	check := exec.Command("zsh", "-f", "-c", "whence -p "+strings.Join(names, " "))
	check.Env = r.env
	resolved, err := check.Output()
	must(t, err)
	lines := strings.Fields(string(resolved))
	if len(lines) != len(names) {
		t.Fatalf("zsh resolved %d of %d tools: %s", len(lines), len(names), resolved)
	}
	for _, l := range lines {
		if filepath.Dir(l) != r.bin {
			t.Fatalf("%s is not a stub: rollback.sh would run the real one", l)
		}
	}

	cmd := exec.Command("zsh", "-f", r.script)
	cmd.Env = r.env
	b, err := cmd.CombinedOutput()
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	calls = readFile(filepath.Join(r.stub, "calls"))
	// The stubs ran, so the real tools did not.
	for _, want := range []string{"launchctl bootout gui/", "pkill -f /claude-burst serve", "lsof -nP -t -iTCP:7777"} {
		if !strings.Contains(calls, want) {
			t.Fatalf("stub call %q missing:\n%s\noutput:\n%s", want, calls, b)
		}
	}
	return string(b), calls, code
}

func (r *rollbackRig) stubFile(name, body string) {
	r.t.Helper()
	must(r.t, os.WriteFile(filepath.Join(r.stub, name), []byte(body), 0o644))
}

// sleeper starts a process the script may signal, and reports whether it is
// still running.
func sleeper(t *testing.T) (pid int, alive func() bool) {
	t.Helper()
	cmd := exec.Command("/bin/sleep", "600")
	must(t, cmd.Start())
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	t.Cleanup(func() { _ = cmd.Process.Kill(); <-done })
	return cmd.Process.Pid, func() bool {
		select {
		case <-done:
			return false
		case <-time.After(200 * time.Millisecond):
			return cmd.Process.Signal(syscall.Signal(0)) == nil
		}
	}
}

// Two listeners on Burst's ports. The one running a program named
// claude-burst is stopped. The other is somebody else's server: it is left
// running and the output says so. Until 2026-10-12 both were killed.
func TestRollbackKillsOnlyBurstOnItsPorts(t *testing.T) {
	r := newRollbackRig(t)
	burst, burstAlive := sleeper(t)
	foreign, foreignAlive := sleeper(t)
	r.stubFile("port-7777", fmt.Sprintf("%d\n%d\n", burst, foreign))
	r.stubFile("port-17777", fmt.Sprintf("%d\n", foreign))
	r.stubFile(fmt.Sprintf("comm-%d", burst), "/Users/someone/.local/bin/claude-burst\n")
	r.stubFile(fmt.Sprintf("comm-%d", foreign), "/opt/homebrew/bin/node\n")

	out, calls, code := r.run()
	if code != 0 {
		t.Fatalf("rollback exit %d:\n%s", code, out)
	}
	if !strings.Contains(calls, fmt.Sprintf("ps -o comm= -p %d", foreign)) {
		t.Fatalf("the script never asked what pid %d is:\n%s", foreign, calls)
	}
	if burstAlive() {
		t.Errorf("the claude-burst listener (pid %d) was left running:\n%s", burst, out)
	}
	if !foreignAlive() {
		t.Fatalf("a listener that is not Claude Burst (pid %d, node) was killed:\n%s", foreign, out)
	}
	contains(t, "rollback output", out,
		fmt.Sprintf("port 7777: stopping pid %d", burst),
		fmt.Sprintf("port 7777: left pid %d (/opt/homebrew/bin/node) running: it is not Claude Burst", foreign),
		fmt.Sprintf("port 17777: left pid %d", foreign))
}

// Only a value naming this Mac on one of Burst's ports is Burst's. The user's
// own local proxy, on another port, stays in all three places it can live.
func TestRollbackRemovesOnlyBurstsOwnLocalValues(t *testing.T) {
	r := newRollbackRig(t)
	// Burst's gateway listens on 4000 here, so 4000 is one of its ports.
	must(t, os.WriteFile(r.config, []byte(`{"listen":"127.0.0.1:4000"}`+"\n"), 0o600))
	writeJSON(t, r.settings, map[string]any{
		"model": "sonnet",
		"env": map[string]any{
			"ANTHROPIC_BASE_URL": "http://127.0.0.1:4000", // Burst's, by config.json
			"HTTP_PROXY":         "http://localhost:7788", // a Burst default port: dead once it stops
			"HTTPS_PROXY":        "http://127.0.0.1:8080", // the user's own proxy
			"ALL_PROXY":          "http://proxy.corp.example:3128",
			"https_proxy":        "http://127.0.0.1:77770", // a port that only starts like one of Burst's
		},
	})
	r.stubFile("launchenv-ANTHROPIC_BASE_URL", "http://127.0.0.1:17777\n")
	r.stubFile("launchenv-HTTPS_PROXY", "http://127.0.0.1:8080\n")
	r.stubFile("proxy-getwebproxy", "Enabled: Yes\nServer: 127.0.0.1\nPort: 8080\nAuthenticated Proxy Enabled: 0\n")
	r.stubFile("proxy-getsecurewebproxy", "Enabled: Yes\nServer: 127.0.0.1\nPort: 7777\nAuthenticated Proxy Enabled: 0\n")

	out, calls, code := r.run()
	if code != 0 {
		t.Fatalf("rollback exit %d:\n%s", code, out)
	}
	var got map[string]any
	if env, ok := (&homeRig{t: t, home: r.home}).settings()["env"].(map[string]any); ok {
		got = env
	}
	for _, gone := range []string{"ANTHROPIC_BASE_URL", "HTTP_PROXY"} {
		if _, ok := got[gone]; ok {
			t.Errorf("%s points at one of Burst's ports and was left: %v", gone, got)
		}
	}
	for _, kept := range []string{"HTTPS_PROXY", "ALL_PROXY", "https_proxy"} {
		if _, ok := got[kept]; !ok {
			t.Errorf("%s is not Burst's and was removed: %v\n%s", kept, got, out)
		}
	}
	contains(t, "rollback output", out, "left as it is (not ours): HTTPS_PROXY")

	// launchd's environment.
	if !strings.Contains(calls, "launchctl unsetenv ANTHROPIC_BASE_URL") {
		t.Errorf("launchd's ANTHROPIC_BASE_URL on a Burst port was left:\n%s", calls)
	}
	if strings.Contains(calls, "launchctl unsetenv HTTPS_PROXY") {
		t.Errorf("the user's own launchd HTTPS_PROXY (port 8080) was removed:\n%s", calls)
	}
	contains(t, "rollback output", out, "left launchd HTTPS_PROXY=http://127.0.0.1:8080 as it is")

	// The macOS proxy.
	if !strings.Contains(calls, "networksetup -setsecurewebproxystate Wi-Fi off") {
		t.Errorf("a macOS proxy on Burst's port 7777 was left on:\n%s", calls)
	}
	if strings.Contains(calls, "networksetup -setwebproxystate") {
		t.Errorf("the user's own macOS proxy (port 8080) was switched off:\n%s", calls)
	}
	contains(t, "rollback output", out, "left the web proxy on Wi-Fi as it is: 127.0.0.1:8080")
}

// A base URL the user pointed at their own local gateway is neither removed
// nor reported as a rollback that failed.
func TestRollbackLeavesTheUsersOwnLocalBaseURL(t *testing.T) {
	r := newRollbackRig(t)
	writeJSON(t, r.settings, map[string]any{"env": map[string]any{"ANTHROPIC_BASE_URL": "http://localhost:4000"}})
	out, _, code := r.run()
	if code != 0 {
		t.Fatalf("rollback failed over a base URL that is not Burst's (exit %d):\n%s", code, out)
	}
	if !strings.Contains(readFile(r.settings), "http://localhost:4000") {
		t.Fatalf("the user's own ANTHROPIC_BASE_URL was removed:\n%s", readFile(r.settings))
	}
	contains(t, "rollback output", out, "left as it is (not ours): ANTHROPIC_BASE_URL")

	// The same value on Burst's own port, left behind (settings.json cannot
	// be parsed, so nothing could be removed), still fails the rollback.
	must(t, os.WriteFile(r.settings, []byte("{\n  \"env\": {\n    \"ANTHROPIC_BASE_URL\": \"http://127.0.0.1:7777\",\n  }\n}\n"), 0o600))
	out, _, code = r.run()
	if code != 2 || !strings.Contains(out, "still sends Claude Code to this Mac") {
		t.Fatalf("a Burst base URL left behind must fail the rollback: exit %d\n%s", code, out)
	}
}

// With root available, the helper root runs is the root-owned copy when there
// is one. A copy the user owns is not trusted for that, wherever it is.
func TestRollbackRunsTheRootOwnedHelperAsRoot(t *testing.T) {
	r := newRollbackRig(t)
	r.stubFile("have-root", "")
	beside := filepath.Join(repoRoot(t), "scripts", "transparent-root.sh")

	// A copy in the libexec directory that this user owns: not root's.
	must(t, os.WriteFile(filepath.Join(r.libexec, "transparent-root.sh"), []byte("#!/bin/zsh\n"), 0o755))
	out, calls, _ := r.run()
	if !strings.Contains(calls, "sudo -n "+beside+" remove") {
		t.Fatalf("expected the copy beside the script to run:\n%s", calls)
	}
	if strings.Contains(calls, "sudo -n "+r.libexec) {
		t.Fatalf("a helper the user owns was run as root from %s:\n%s", r.libexec, calls)
	}
	contains(t, "rollback output", out, "is not a root-owned copy")

	// The real installed copy, where this Mac has one: root's, so preferred.
	installed := "/usr/local/libexec/claude-burst/transparent-root.sh"
	fi, err := os.Stat(installed)
	if err != nil {
		t.Skipf("no installed helper at %s to check the root-owned case against", installed)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != 0 || fi.Mode().Perm()&0o022 != 0 {
		t.Skipf("%s is not a root-owned, root-only-writable file here", installed)
	}
	for i, kv := range r.env {
		if strings.HasPrefix(kv, "CLAUDE_BURST_PFHEAL_LIBEXEC=") {
			r.env = append(r.env[:i], r.env[i+1:]...)
			break
		}
	}
	os.Remove(filepath.Join(r.stub, "calls"))
	out, calls, _ = r.run()
	if !strings.Contains(calls, "sudo -n "+installed+" remove") {
		t.Fatalf("the root-owned helper was not the one run:\n%s\n%s", calls, out)
	}
	if strings.Contains(out, "is not a root-owned copy") {
		t.Fatalf("warned about a root-owned copy:\n%s", out)
	}
}
