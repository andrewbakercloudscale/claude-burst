package integration

// scripts/install-proxy.sh, run for real against a temp HOME with every
// command that could change this Mac stubbed. `claude-burst enable` has no
// health check of its own: install-proxy.sh is what guarantees the gateway
// answers before enable repoints Claude Code, and before the root helper
// redirects the machine. On 2026-08-30 an enable that ran before its gateway
// was listening broke a live session. TestInstallProxyUsesTheReloadHelperBeforeEnable
// checks the order in the source; this runs it.

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// healthStub is curl: it fails the first $STUB_DIR/unhealthy calls, then
// answers, and logs each with its outcome.
const healthStub = `#!/bin/sh
d="$STUB_DIR"
for a; do last="$a"; done
n=$(cat "$d/unhealthy")
if [ "$n" -gt 0 ]; then
  echo $((n-1)) > "$d/unhealthy"
  echo "curl fail $last" >> "$d/calls"
  exit 7
fi
echo "curl ok $last" >> "$d/calls"
echo '{"status":"ok","overflow":false}'
`

// launchctlNotLoaded: nothing holds the label, so reload does not wait.
const launchctlNotLoaded = `#!/bin/sh
echo "launchctl $1" >> "$STUB_DIR/calls"
[ "$1" = "print" ] && exit 113
exit 0
`

// sudoStub has cached credentials and runs only the test's own copies of
// the helpers; anything else is a test failure.
const sudoStub = `#!/bin/sh
[ "$1" = "-n" ] && shift
[ "$1" = "true" ] && exit 0
case "$1" in
  "$STUB_ROOT"/*) exec "$@" ;;
esac
echo "FORBIDDEN sudo $*" >> "$STUB_DIR/calls"
exit 1
`

func runInstallProxy(t *testing.T, unhealthy int) (calls []string, out string, code int) {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("macOS-only")
	}
	if _, err := exec.LookPath("zsh"); err != nil {
		t.Skip("zsh not available")
	}
	root := t.TempDir()
	home := filepath.Join(root, "home")
	scripts := filepath.Join(root, "scripts")
	bin := filepath.Join(root, "bin")
	stub := filepath.Join(root, "stub")
	for _, d := range []string{scripts, bin, stub, filepath.Join(home, ".local", "bin"), filepath.Join(home, "Library", "LaunchAgents")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	src := filepath.Join(repoRoot(t), "scripts")
	// The script under test and the helpers it sources, unchanged.
	for _, name := range []string{"install-proxy.sh", "health-diagnostics.sh", "launchagent-reload.sh"} {
		b, err := os.ReadFile(filepath.Join(src, name))
		if err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(scripts, name), string(b))
		os.Chmod(filepath.Join(scripts, name), 0o755)
	}
	// Every helper it runs, as a stub that logs its name.
	for _, name := range []string{"backup-config.sh", "transparent-root.sh", "trust-ca-systemwide.sh", "watchdog.sh", "install-pf-heal.sh", "rollback.sh"} {
		write(t, filepath.Join(scripts, name), loggingStub)
		os.Chmod(filepath.Join(scripts, name), 0o755)
	}
	write(t, filepath.Join(home, ".local", "bin", "claude-burst"), "#!/bin/sh\necho \"claude-burst $*\" >> \"$STUB_DIR/calls\"\n")
	os.Chmod(filepath.Join(home, ".local", "bin", "claude-burst"), 0o755)
	write(t, filepath.Join(home, "Library", "LaunchAgents", "ninja.andrewbaker.claude-burst.plist"), "<plist/>\n")
	write(t, filepath.Join(home, ".config", "claude-burst", "config.json"), `{"listen": "127.0.0.1:17777"}`)

	stubs := map[string]string{
		"curl": healthStub, "launchctl": launchctlNotLoaded, "sudo": sudoStub,
		"sleep": loggingStub, "lsof": loggingStub,
	}
	for _, name := range []string{"pfctl", "networksetup", "security", "defaults", "pmset", "ioreg", "dscacheutil", "killall"} {
		stubs[name] = forbiddenStub
	}
	for name, body := range stubs {
		write(t, filepath.Join(bin, name), body)
		os.Chmod(filepath.Join(bin, name), 0o755)
	}
	// gateway_port runs python3 on every poll; see newRootEnv for why the
	// /usr/bin shim is avoided.
	if py, err := exec.LookPath("python3"); err == nil {
		if err := os.Symlink(py, filepath.Join(bin, "python3")); err != nil {
			t.Fatal(err)
		}
	}
	write(t, filepath.Join(stub, "unhealthy"), strconv.Itoa(unhealthy))

	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "CLAUDE_BURST_") && !strings.HasPrefix(kv, "PATH=") &&
			!strings.HasPrefix(kv, "HOME=") && !strings.HasPrefix(kv, "ZDOTDIR=") {
			env = append(env, kv)
		}
	}
	env = append(env, "PATH="+bin+":/usr/bin:/bin", "HOME="+home, "ZDOTDIR="+home,
		"STUB_DIR="+stub, "STUB_ROOT="+root,
		"CLAUDE_BURST_ROLLED_BACK_MARKER="+filepath.Join(home, "rolled-back"))
	cmd := exec.Command("zsh", "-f", filepath.Join(scripts, "install-proxy.sh"))
	cmd.Env = env
	cmd.Stdin = nil // no tty: the script must not prompt
	b, err := cmd.CombinedOutput()
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(stub, "calls"))
	for _, l := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if l != "" {
			calls = append(calls, strings.ReplaceAll(l, root, "<tmp>"))
		}
	}
	if !hasPrefix(calls, "launchctl bootstrap") {
		t.Fatalf("the stub launchctl never bootstrapped: calls %q\n%s", calls, b)
	}
	for _, c := range calls {
		if strings.HasPrefix(c, "FORBIDDEN ") {
			t.Fatalf("install-proxy.sh ran %q\n%s", c, b)
		}
	}
	return calls, string(b), code
}

func indexOf(calls []string, prefix string) int {
	for i, c := range calls {
		if strings.HasPrefix(c, prefix) {
			return i
		}
	}
	return -1
}

// A gateway that never answers: nothing points at it.
func TestInstallProxyNeverEnablesAnUnhealthyGateway(t *testing.T) {
	t.Parallel()
	calls, out, code := runInstallProxy(t, 1_000_000)
	if code == 0 {
		t.Fatalf("exit 0 with the gateway down:\n%s", out)
	}
	for _, never := range []string{"claude-burst enable", "transparent-root.sh", "trust-ca-systemwide.sh", "watchdog.sh"} {
		if i := indexOf(calls, never); i >= 0 {
			t.Fatalf("%q ran with the gateway down: %q", never, calls)
		}
	}
	if indexOf(calls, "curl fail https://127.0.0.1:17777/healthz") < 0 {
		t.Fatalf("health was not checked on the configured port: %q", calls)
	}
	if !strings.Contains(out, "not touching settings.json") {
		t.Fatalf("output:\n%s", out)
	}
}

// A gateway that takes a few polls to answer: enable only after the first
// answer, and the machine-wide redirect only after enable.
func TestInstallProxyEnablesOnlyAfterHealthThenRedirects(t *testing.T) {
	t.Parallel()
	calls, out, code := runInstallProxy(t, 7)
	if code != 0 {
		t.Fatalf("exit %d:\n%s\ncalls %q", code, out, calls)
	}
	boot := indexOf(calls, "launchctl bootstrap")
	healthy := indexOf(calls, "curl ok ")
	enable := indexOf(calls, "claude-burst enable")
	redirect := indexOf(calls, "transparent-root.sh install")
	if boot < 0 || healthy < 0 || enable < 0 || redirect < 0 {
		t.Fatalf("missing a step: %q", calls)
	}
	if !(boot < healthy && healthy < enable && enable < redirect) {
		t.Fatalf("order must be bootstrap < healthy < enable < redirect, got %d %d %d %d: %q", boot, healthy, enable, redirect, calls)
	}
	if n := len(only(calls, "claude-burst enable")); n != 1 {
		t.Fatalf("enable ran %d times", n)
	}
	if indexOf(calls, "transparent-root.sh install --host api.anthropic.com --gateway-port 17777") < 0 {
		t.Fatalf("the redirect must name the configured port explicitly: %q", calls)
	}
}
