package integration

// scripts/launchagent-reload.sh, run for real under zsh with launchctl and
// sleep stubbed on PATH. The stubs are the point: bootout on the real
// launchctl would stop the developer's live gateway.
//
// On 2026-09-30 two clicks of Install each booted the gateway out and
// bootstrapped it again in the same instant. bootout returns while the
// gateway is still draining, launchd still held the label, bootstrap failed
// with "Input/output error", and Install stopped a healthy gateway and
// changed nothing else.

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// launchctlStub keeps its state in files under dir:
//
//	held       prints that still find the label after bootout (the drain)
//	bootfails  bootstrap calls that fail regardless
//	calls      one line per invocation, "launchctl <verb>" or "sleep <n>"
const launchctlStub = `#!/bin/zsh
d="$STUB_DIR"
echo "launchctl $1" >> "$d/calls"
case "$1" in
  bootout) exit 0 ;;
  print)
    n=$(cat "$d/held")
    if (( n > 0 )); then echo $((n-1)) > "$d/held"; exit 0; fi
    exit 113 ;;
  bootstrap)
    # launchd refuses while the old job still holds the label.
    if (( $(cat "$d/held") > 0 )); then echo "Bootstrap failed: 5: Input/output error" >&2; exit 5; fi
    f=$(cat "$d/bootfails")
    if (( f > 0 )); then echo $((f-1)) > "$d/bootfails"; echo "Bootstrap failed: 5: Input/output error" >&2; exit 5; fi
    exit 0 ;;
  kickstart) exit 0 ;;
esac
exit 0
`

const sleepStub = `#!/bin/zsh
echo "sleep $1" >> "$STUB_DIR/calls"
`

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Clean(filepath.Join(wd, "..", ".."))
}

type reloadRun struct {
	calls  []string
	stderr string
	code   int
}

func runReload(t *testing.T, held, bootfails int) reloadRun {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("launchd is macOS-only")
	}
	if _, err := exec.LookPath("zsh"); err != nil {
		t.Skip("zsh not available")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"launchctl": launchctlStub, "sleep": sleepStub} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, n := range map[string]int{"held": held, "bootfails": bootfails} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(strconv.Itoa(n)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	helper := filepath.Join(repoRoot(t), "scripts", "launchagent-reload.sh")
	cmd := exec.Command("zsh", "-c", `source "$1"; reload_launchagent test.label /tmp/test.plist`, "zsh", helper)
	cmd.Env = append(os.Environ(), "PATH="+bin+":/usr/bin:/bin", "STUB_DIR="+dir)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "calls"))
	calls := strings.Split(strings.TrimSpace(string(raw)), "\n")
	// Assert the stub ran: a helper that reached the real launchctl would
	// leave no calls here and still might exit 0.
	if len(calls) == 0 || calls[0] != "launchctl bootout" {
		t.Fatalf("stub launchctl was not the one called: %q", calls)
	}
	return reloadRun{calls: calls, stderr: stderr.String(), code: code}
}

func count(calls []string, want string) int {
	n := 0
	for _, c := range calls {
		if c == want {
			n++
		}
	}
	return n
}

// The 2026-09-30 case: the old gateway holds the label for a few polls. The
// helper must wait it out and bootstrap once, after the label is gone.
func TestReloadWaitsForTheDrainBeforeBootstrap(t *testing.T) {
	r := runReload(t, 3, 0)
	if r.code != 0 {
		t.Fatalf("exit %d, stderr: %s", r.code, r.stderr)
	}
	want := []string{
		"launchctl bootout",
		"launchctl print", "sleep 1",
		"launchctl print", "sleep 1",
		"launchctl print", "sleep 1",
		"launchctl print",
		"launchctl bootstrap",
		"launchctl kickstart",
	}
	if strings.Join(r.calls, "|") != strings.Join(want, "|") {
		t.Fatalf("calls:\n got %q\nwant %q", r.calls, want)
	}
}

// Already unloaded: no waiting at all.
func TestReloadDoesNotWaitWhenNothingIsLoaded(t *testing.T) {
	r := runReload(t, 0, 0)
	if r.code != 0 || count(r.calls, "sleep 1") != 0 || count(r.calls, "launchctl bootstrap") != 1 {
		t.Fatalf("exit %d, calls %q", r.code, r.calls)
	}
}

// One transient refusal is retried, not treated as the end.
func TestReloadRetriesATransientBootstrapFailure(t *testing.T) {
	r := runReload(t, 0, 1)
	if r.code != 0 {
		t.Fatalf("exit %d, stderr: %s", r.code, r.stderr)
	}
	if count(r.calls, "launchctl bootstrap") != 2 || count(r.calls, "launchctl kickstart") != 1 {
		t.Fatalf("calls %q", r.calls)
	}
	if !strings.Contains(r.stderr, "Input/output error") {
		t.Fatalf("the failed attempt must be shown, stderr: %s", r.stderr)
	}
}

// launchd refusing every time: fail loudly, and never kickstart a job that
// is not loaded (that is the silent failure this replaced).
func TestReloadFailsLoudlyWhenLaunchdKeepsRefusing(t *testing.T) {
	r := runReload(t, 0, 99)
	if r.code == 0 {
		t.Fatal("must exit non-zero when launchd never loads the job")
	}
	if count(r.calls, "launchctl bootstrap") != 5 || count(r.calls, "launchctl kickstart") != 0 {
		t.Fatalf("calls %q", r.calls)
	}
	if !strings.Contains(r.stderr, "would not load test.label") {
		t.Fatalf("stderr: %s", r.stderr)
	}
}

// A label that never disappears: the wait is bounded, then bootstrap is
// still tried (and here, refused), so the helper cannot hang an install.
func TestReloadWaitIsBounded(t *testing.T) {
	r := runReload(t, 1000, 0)
	if r.code == 0 {
		t.Fatal("a label that never unloads must end in failure, not success")
	}
	if n := count(r.calls, "launchctl print"); n != 65 {
		t.Fatalf("polled %d times, want the 65s bound", n)
	}
}

// install-proxy.sh must use the helper, and nothing past it may run when it
// fails: enable and the root steps would point traffic at a gateway that is
// not running.
func TestInstallProxyUsesTheReloadHelperBeforeEnable(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "scripts", "install-proxy.sh"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	if strings.Contains(src, "launchctl bootstrap") {
		t.Fatal("install-proxy.sh bootstraps directly again; use reload_launchagent")
	}
	if !strings.Contains(src, `source "$DIR/launchagent-reload.sh"`) {
		t.Fatal("install-proxy.sh does not source launchagent-reload.sh")
	}
	reload := strings.Index(src, "if ! reload_launchagent")
	enable := strings.Index(src, `"$BIN" enable`)
	if reload < 0 || enable < 0 || reload > enable {
		t.Fatalf("reload_launchagent (at %d) must be checked before enable (at %d)", reload, enable)
	}
	if !strings.Contains(src[reload:enable], "exit 1") {
		t.Fatal("a failed reload must exit before enable")
	}
}
