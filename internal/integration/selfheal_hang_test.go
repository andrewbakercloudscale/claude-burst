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

// watchdogHome is a temporary HOME with stubs for everything the watchdog
// would otherwise do to the real Mac. The gateway answers (curl succeeds)
// and launchd reports whatever pid is in the file `pid`.
func watchdogHome(t *testing.T) (home string, run func(env ...string)) {
	t.Helper()
	home = t.TempDir()
	bin := filepath.Join(home, "bin")
	for _, d := range []string{filepath.Join(home, ".config", "claude-burst"), bin} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	stubs := map[string]string{
		"launchctl": "#!/bin/sh\np=$(cat " + filepath.Join(home, "pid") + " 2>/dev/null)\n[ -n \"$p\" ] && echo \"\tpid = $p\"\nexit 0\n",
		"curl":      "#!/bin/sh\nexit 0\n",
		"osascript": "#!/bin/sh\necho \"$@\" >> " + filepath.Join(home, "notified") + "\n",
		"fakekill":  "#!/bin/sh\nexit 0\n",
	}
	for name, body := range stubs {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	script := filepath.Join(repoRoot(t), "scripts", "self-heal-watchdog.sh")
	run = func(env ...string) {
		t.Helper()
		cmd := exec.Command("/bin/zsh", script)
		cmd.Env = append([]string{"HOME=" + home, "PATH=" + bin + ":/usr/bin:/bin", "USER=test",
			"CLAUDE_BURST_KILL=" + filepath.Join(bin, "fakekill")}, env...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("watchdog: %v\n%s", err, out)
		}
	}
	return home, run
}

func readFile(path string) string { b, _ := os.ReadFile(path); return string(b) }

// A gateway that launchd restarts over and over was "healthy" on every
// check. The watchdog now counts the new pids and says so at the limit,
// once, in the log, a notification and the audit trail.
func TestSelfHealEscalatesACrashLoop(t *testing.T) {
	home, run := watchdogHome(t)
	cfg := filepath.Join(home, ".config", "claude-burst")
	pid := func(p string) {
		if err := os.WriteFile(filepath.Join(home, "pid"), []byte(p), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	audit := func() string { return readFile(filepath.Join(cfg, "audit.jsonl")) }

	// The same pid check after check is a gateway that is staying up.
	pid("100")
	for i := 0; i < 6; i++ {
		run()
	}
	if got := readFile(filepath.Join(cfg, "self-heal-restarts")); got != "" {
		t.Fatalf("a steady gateway was counted as restarting: %q", got)
	}

	// Four new pids: restarts, under the limit of five, nothing said.
	for _, p := range []string{"101", "102", "103", "104"} {
		pid(p)
		run()
	}
	if strings.Contains(audit(), "keeps restarting") || readFile(filepath.Join(home, "notified")) != "" {
		t.Fatalf("escalated under the limit:\n%s", audit())
	}

	// The fifth: said once, however long the loop goes on this hour.
	for _, p := range []string{"105", "106", "107"} {
		pid(p)
		run()
	}
	if n := strings.Count(audit(), "Watchdog: the gateway keeps restarting"); n != 1 {
		t.Fatalf("the audit names the crash loop %d times, want 1:\n%s", n, audit())
	}
	if !strings.Contains(audit(), `"severity": "error"`) || !strings.Contains(audit(), "burst-repair") || !strings.Contains(audit(), "burst-off") {
		t.Errorf("the entry must be an error naming both ways out:\n%s", audit())
	}
	if n := strings.Count(readFile(filepath.Join(home, "notified")), "restarted"); n != 1 {
		t.Errorf("notified %d times, want 1", n)
	}
	if !strings.Contains(readFile(filepath.Join(cfg, "self-heal.log")), "CRASH LOOP") {
		t.Errorf("log:\n%s", readFile(filepath.Join(cfg, "self-heal.log")))
	}

	// Staying up for a whole window ends it, and that is recorded too. The
	// window is shrunk to nothing instead of waiting ten minutes.
	run("CLAUDE_BURST_CRASH_WINDOW=0")
	if !strings.Contains(audit(), "Watchdog: the gateway is staying up again") {
		t.Errorf("the end of the loop was not recorded:\n%s", audit())
	}
	if _, err := os.Stat(filepath.Join(cfg, "self-heal-crashloop")); err == nil {
		t.Error("the crash-loop marker is still there after the gateway stayed up")
	}
}

// A deploy restarts the gateway on purpose, several times an hour on a
// developer's Mac. Those are not crashes, whether the watchdog runs while
// the marker is there or after the gateway replaced it with its .done.
func TestSelfHealDoesNotCountPlannedRestarts(t *testing.T) {
	home, run := watchdogHome(t)
	cfg := filepath.Join(home, ".config", "claude-burst")
	for i, marker := range []string{"planned-restart", "planned-restart.done", "planned-restart", "planned-restart.done", "planned-restart", "planned-restart.done", "planned-restart"} {
		os.Remove(filepath.Join(cfg, "planned-restart"))
		os.Remove(filepath.Join(cfg, "planned-restart.done"))
		if err := os.WriteFile(filepath.Join(cfg, marker), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, "pid"), []byte{'2', '0', byte('0' + i)}, 0o600); err != nil {
			t.Fatal(err)
		}
		run()
	}
	if got := readFile(filepath.Join(cfg, "self-heal-restarts")); got != "" {
		t.Errorf("planned restarts were counted: %q", got)
	}
	if strings.Contains(readFile(filepath.Join(cfg, "audit.jsonl")), "keeps restarting") {
		t.Error("planned restarts were reported as a crash loop")
	}
}

// What the watchdog does to the gateway is in the audit trail, not only in
// a log file: here, killing one that stopped answering.
func TestSelfHealAuditsTheKill(t *testing.T) {
	home, run := watchdogHome(t)
	if err := os.WriteFile(filepath.Join(home, "pid"), []byte("4242"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "bin", "curl"), []byte("#!/bin/sh\nexit 28\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	run()
	run()
	got := readFile(filepath.Join(home, ".config", "claude-burst", "audit.jsonl"))
	if !strings.Contains(got, "Watchdog: killed a gateway that had stopped answering") || !strings.Contains(got, "pid 4242") {
		t.Fatalf("audit:\n%s", got)
	}
}

// Every script that changes the Mac records how it ended, and the two
// installers that copy scripts out of the checkout take the audit writer
// with them: burst-off and the watchdog run from those copies.
func TestScriptsThatChangeThingsWriteTheAudit(t *testing.T) {
	root := repoRoot(t)
	for _, rel := range []string{
		"install.sh", "scripts/deploy.sh", "scripts/rollback.sh", "scripts/repair.sh",
		"scripts/watchdog.sh", "scripts/self-heal-watchdog.sh",
		"scripts/install-burst-off.sh", "scripts/install-selfheal-watchdog.sh",
	} {
		if !strings.Contains(readFile(filepath.Join(root, rel)), "audit-add.sh") {
			t.Errorf("%s does not use scripts/audit-add.sh", rel)
		}
	}
}

// rollback.sh's own exit decides what the audit says. Its trap is run here
// on its own: the script itself stops the real gateway.
func TestRollbackRecordsHowItEnded(t *testing.T) {
	root := repoRoot(t)
	var trap string
	for _, line := range strings.Split(readFile(filepath.Join(root, "scripts", "rollback.sh")), "\n") {
		if strings.HasPrefix(line, "trap ") && strings.Contains(line, "audit-add.sh") {
			trap = line
		}
	}
	if trap == "" {
		t.Fatal("rollback.sh has no EXIT trap that writes the audit")
	}
	for code, want := range map[string]string{"0": `"severity": "warn"`, "2": "ended with exit 2"} {
		file := filepath.Join(t.TempDir(), "audit.jsonl")
		for _, shell := range []string{"/bin/zsh", "/bin/bash"} {
			cmd := exec.Command(shell, "-c", "DIR="+filepath.Join(root, "scripts")+"\n"+trap+"\nexit "+code)
			cmd.Env = append(os.Environ(), "CLAUDE_BURST_AUDIT_FILE="+file)
			_ = cmd.Run()
		}
		got := readFile(file)
		if strings.Count(got, "Script: rollback.sh") != 2 || !strings.Contains(got, want) {
			t.Errorf("exit %s under zsh and bash: audit is\n%s\nwant two entries containing %s", code, got, want)
		}
	}
}
