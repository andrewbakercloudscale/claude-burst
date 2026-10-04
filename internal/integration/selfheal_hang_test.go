package integration

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The self-heal watchdog restarts a gateway that is running but answers
// nothing, on the second check in a row, and never during a planned restart.
// launchctl, curl, osascript and kill are stubs: nothing here reaches the
// real gateway (a temporary HOME does not stop launchctl or kill).
func TestSelfHealKillsAHungGateway(t *testing.T) {
	home := t.TempDir()
	bin := filepath.Join(home, "bin")
	if err := os.MkdirAll(filepath.Join(home, ".config", "claude-burst"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	killed := filepath.Join(home, "killed")
	stubs := map[string]string{
		"launchctl": "#!/bin/sh\necho '\tpid = 4242'\n",
		"curl":      "#!/bin/sh\nexit 28\n", // every check times out: hung
		"osascript": "#!/bin/sh\nexit 0\n",
		"fakekill":  "#!/bin/sh\necho \"$@\" >> " + killed + "\n",
	}
	for name, body := range stubs {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	script := filepath.Join(repoRoot(t), "scripts", "self-heal-watchdog.sh")
	run := func() string {
		cmd := exec.Command("/bin/zsh", script)
		cmd.Env = []string{"HOME=" + home, "PATH=" + bin + ":/usr/bin:/bin", "USER=test",
			"CLAUDE_BURST_KILL=" + filepath.Join(bin, "fakekill")}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("watchdog: %v\n%s", err, out)
		}
		b, _ := os.ReadFile(killed)
		return string(b)
	}

	if got := run(); got != "" {
		t.Fatalf("killed on the first check: %q", got)
	}
	if got := run(); strings.TrimSpace(got) != "-9 4242" {
		t.Fatalf("second check: killed %q, want -9 4242", got)
	}
	logb, _ := os.ReadFile(filepath.Join(home, ".config", "claude-burst", "self-heal.log"))
	if !strings.Contains(string(logb), "hung for 2 checks") {
		t.Fatalf("log:\n%s", logb)
	}

	// A planned restart (an upgrade draining): never killed.
	os.Remove(killed)
	planned := filepath.Join(home, ".config", "claude-burst", "planned-restart")
	if err := os.WriteFile(planned, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	run()
	if got := run(); got != "" {
		t.Fatalf("killed during a planned restart: %q", got)
	}
}
