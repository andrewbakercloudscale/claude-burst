package keepawake

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// desired runs the real root script's no-root "desired" check against a
// temporary state directory, with the lid state given.
func desired(t *testing.T, dir, lid string) string {
	t.Helper()
	cmd := exec.Command("zsh", "../../scripts/lid-awake-root.sh", "desired", "always")
	cmd.Env = append(os.Environ(), "CLAUDE_BURST_ROOT_STATE_DIR="+dir, "CLAUDE_BURST_TEST_LID="+lid)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("desired: %v: %s", err, out)
	}
	return strings.TrimSpace(string(out))
}

// The idle window: awake with the lid shut for IDLE minutes after the lid
// was last open or Claude Code was last used, then normal sleep.
func TestIdleWindow(t *testing.T) {
	dir := t.TempDir()
	lidopen := filepath.Join(dir, "lid-awake.lidopen")
	if got := desired(t, dir, "shut"); got != "1" {
		t.Fatalf("no window set: always awake, got %s", got)
	}
	os.WriteFile(filepath.Join(dir, "lid-awake.idle"), []byte("30\n"), 0o644)
	os.WriteFile(lidopen, nil, 0o644)
	if got := desired(t, dir, "shut"); got != "1" {
		t.Errorf("lid shut a moment ago: still awake, got %s", got)
	}
	old := time.Now().Add(-2 * time.Hour)
	os.Chtimes(lidopen, old, old)
	if got := desired(t, dir, "shut"); got != "0" {
		t.Errorf("shut for 2 hours, nothing used: sleep allowed, got %s", got)
	}
	// Recent activity in a file outside ~/.config/claude-burst is ignored:
	// the root daemon reads only the gateway's own file.
	bogus := filepath.Join(dir, "last-activity")
	os.WriteFile(bogus, nil, 0o644)
	os.WriteFile(filepath.Join(dir, "lid-awake.activity"), []byte(bogus+"\n"), 0o644)
	if got := desired(t, dir, "shut"); got != "0" {
		t.Errorf("activity path not allowed: ignored, got %s", got)
	}
	if got := desired(t, dir, "open"); got != "1" {
		t.Errorf("lid open: in use, got %s", got)
	}
	if got := desired(t, dir, "shut"); got != "1" {
		t.Errorf("lid just closed after being open: new window, got %s", got)
	}
}

// screen runs the real root script's no-root "screen" check.
func screen(t *testing.T, lid, sleepDisabled, displays string) string {
	t.Helper()
	return runScreen(t, "screen", lid, sleepDisabled, displays)
}

func runScreen(t *testing.T, sub, lid, sleepDisabled, displays string) string {
	t.Helper()
	cmd := exec.Command("zsh", "../../scripts/lid-awake-root.sh", sub)
	cmd.Env = append(os.Environ(), "CLAUDE_BURST_ROOT_STATE_DIR="+t.TempDir(), "CLAUDE_BURST_TEST_LID="+lid,
		"CLAUDE_BURST_TEST_SLEEPDISABLED="+sleepDisabled, "CLAUDE_BURST_TEST_DISPLAYS="+displays)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("screen: %v: %s", err, out)
	}
	return strings.TrimSpace(string(out))
}

const builtInOnly = `Graphics/Displays:
    Apple M4:
      Displays:
        Color LCD:
          Display Type: Built-in Liquid Retina Display
          Online: Yes
          Connection Type: Internal
`

const withMonitor = builtInOnly + `        LG HDR 4K:
          Resolution: 3840 x 2160
          Online: Yes
`

// The real lid and power checks, against stub ioreg and pmset that print the
// line looked for and then far more, the way the real ones do. Piped into
// grep -q under pipefail, the writer died of SIGPIPE and a shut lid read as
// open, while CLAUDE_BURST_TEST_LID hid it from every other test here.
func TestLidAndPowerReadWithRealSizedOutput(t *testing.T) {
	bin := t.TempDir()
	stub := func(name, first string) {
		body := "#!/bin/sh\ncat <<'EOF'\n" + first + "\nEOF\nyes 'padding padding padding padding' | head -c 2000000\n"
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	stub("ioreg", `  |   "AppleClamshellState" = Yes`)
	stub("pmset", `Now drawing from 'Battery Power'`)
	stub("system_profiler", "")
	cmd := exec.Command("zsh", "../../scripts/lid-awake-root.sh", "screen")
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "CLAUDE_BURST_ROOT_STATE_DIR="+t.TempDir(),
		"CLAUDE_BURST_TEST_SLEEPDISABLED=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("screen: %v: %s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "off" {
		t.Errorf("lid shut per ioreg: want off, got %s", got)
	}
	// Mode ac on battery: sleep allowed, so desired is 0.
	cmd = exec.Command("zsh", "../../scripts/lid-awake-root.sh", "desired", "ac")
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "CLAUDE_BURST_ROOT_STATE_DIR="+t.TempDir())
	out, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("desired: %v: %s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "0" {
		t.Errorf("on battery per pmset, mode ac: want 0, got %s", got)
	}
}

// The screen goes off with the lid shut while the Mac is being kept awake,
// and only then: never with the lid open, never when the Mac would sleep
// anyway, and never with an external monitor (clamshell mode, in use).
func TestScreenOffBehindAShutLid(t *testing.T) {
	cases := []struct{ lid, sd, displays, want string }{
		{"shut", "1", builtInOnly, "off"},
		{"open", "1", builtInOnly, "leave"},
		{"shut", "0", builtInOnly, "leave"},
		{"shut", "1", withMonitor, "leave"},
	}
	for _, c := range cases {
		if got := screen(t, c.lid, c.sd, c.displays); got != c.want {
			t.Errorf("lid %s, SleepDisabled %s, monitor %v: got %s, want %s", c.lid, c.sd, strings.Contains(c.displays, "LG"), got, c.want)
		}
	}
}

// apply ends with one line saying which is true right now, in the words the
// user asked for: "Screen Turned Off" or "Screen Turned On", and why.
func TestApplySaysWhetherTheScreenIsOff(t *testing.T) {
	cases := []struct{ lid, sd, displays, want string }{
		{"shut", "1", builtInOnly, "Screen Turned Off"},
		{"open", "1", builtInOnly, "Screen Turned On (lid open"},
		{"shut", "0", builtInOnly, "Screen Turned On (the Mac is not being kept awake"},
		{"shut", "1", withMonitor, "Screen Turned On (an external display"},
	}
	for _, c := range cases {
		if got := runScreen(t, "screen-line", c.lid, c.sd, c.displays); !strings.HasPrefix(got, c.want) {
			t.Errorf("lid %s, SleepDisabled %s: got %q, want prefix %q", c.lid, c.sd, got, c.want)
		}
	}
}

// screenSteps feeds lid readings through the daemon's screen_step, with stub
// ioreg, pmset and system_profiler on PATH that only record being called.
func screenSteps(t *testing.T, events, sleepDisabled, displays string) (out, calls string) {
	t.Helper()
	stubs := t.TempDir()
	record := filepath.Join(stubs, "calls")
	for _, name := range []string{"ioreg", "pmset", "system_profiler", "sleep"} {
		os.WriteFile(filepath.Join(stubs, name), []byte("#!/bin/sh\necho "+name+" >> "+record+"\n"), 0o755)
	}
	cmd := exec.Command("zsh", "../../scripts/lid-awake-root.sh", "screen-steps")
	cmd.Env = append(os.Environ(), "PATH="+stubs+":"+os.Getenv("PATH"),
		"CLAUDE_BURST_ROOT_STATE_DIR="+t.TempDir(), "CLAUDE_BURST_TEST_LID_EVENTS="+events,
		"CLAUDE_BURST_TEST_SLEEPDISABLED="+sleepDisabled, "CLAUDE_BURST_TEST_DISPLAYS="+displays)
	b, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("screen-steps: %v: %s", err, b)
	}
	c, _ := os.ReadFile(record)
	return strings.TrimSpace(string(b)), strings.TrimSpace(string(c))
}

// With the lid open the watcher's readings start no process at all: the old
// loop ran an ioreg and a sleep every second, all day.
func TestLidOpenStartsNoProcess(t *testing.T) {
	out, calls := screenSteps(t, strings.Repeat("open\n", 20), "1", builtInOnly)
	if out != "" || calls != "" {
		t.Errorf("lid open: want nothing run, got output %q, calls %q", out, calls)
	}
}

// Shut with nothing external: the screen goes dark at once and is not asked
// again every second; a monitor connected means it is left alone.
func TestLidShutDarkensOnceAndDecidesRarely(t *testing.T) {
	out, calls := screenSteps(t, "open\nshut\nshut\nshut\nshut\nopen", "1", builtInOnly)
	if out != "displaysleepnow" {
		t.Errorf("shut for a moment: want one displaysleepnow, got %q", out)
	}
	if strings.Contains(calls, "ioreg") || strings.Contains(calls, "sleep") {
		t.Errorf("shut: want no ioreg or sleep, got calls %q", calls)
	}
	out, _ = screenSteps(t, "open\nshut\nshut\nshut", "1", withMonitor)
	if out != "" {
		t.Errorf("shut with a monitor: want the screen left alone, got %q", out)
	}
}
