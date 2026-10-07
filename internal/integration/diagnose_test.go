package integration

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// diagnoseHome is a Mac as diagnose.sh sees it: a base-url install in a
// temporary HOME, with launchctl, curl, the binary and pbcopy as stubs so
// nothing is asked of the real gateway and the clipboard is left alone.
// curl answers with the status in the file `http`; launchctl reports a pid
// only while the file `pid` exists.
func diagnoseHome(t *testing.T) (home string, run func(args ...string) (string, int)) {
	t.Helper()
	home = t.TempDir()
	bin := filepath.Join(home, "bin")
	cfg := filepath.Join(home, ".config", "claude-burst")
	for _, d := range []string{bin, cfg, filepath.Join(home, ".local", "bin"), filepath.Join(home, ".claude")} {
		must(t, os.MkdirAll(d, 0o700))
	}
	must(t, os.WriteFile(filepath.Join(cfg, "config.json"), []byte(`{"listen":"127.0.0.1:7777","intercept":{"mode":"base-url"},"codex":{"listen":"off"}}`), 0o600))
	must(t, os.WriteFile(filepath.Join(home, ".claude", "settings.json"), []byte(`{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:7777"}}`), 0o600))
	must(t, os.WriteFile(filepath.Join(home, "http"), []byte("200"), 0o600))
	must(t, os.WriteFile(filepath.Join(home, "pid"), []byte("4242"), 0o600))
	stubs := map[string]string{
		"launchctl": "#!/bin/sh\n[ -f " + filepath.Join(home, "pid") + " ] && echo '\tpid = 4242'\nexit 0\n",
		"curl":      "#!/bin/sh\nprintf '%s' \"$(cat " + filepath.Join(home, "http") + ")\"\n",
		"pbcopy":    "#!/bin/sh\ncat > " + filepath.Join(home, "copied") + "\n",
	}
	for name, body := range stubs {
		must(t, os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755))
	}
	must(t, os.WriteFile(filepath.Join(home, ".local", "bin", "claude-burst"), []byte("#!/bin/sh\necho 9.9.9\n"), 0o755))
	script := filepath.Join(repoRoot(t), "scripts", "diagnose.sh")
	run = func(args ...string) (string, int) {
		t.Helper()
		cmd := exec.Command("/bin/bash", append([]string{script}, args...)...)
		cmd.Env = []string{"HOME=" + home, "PATH=" + bin + ":/usr/bin:/bin:/usr/sbin:/sbin", "USER=test"}
		out, err := cmd.CombinedOutput()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatalf("diagnose.sh: %v\n%s", err, out)
		}
		return string(out), code
	}
	return home, run
}

// One count and one exit code: a script, or a person in a hurry, gets the
// answer without reading the report.
func TestDiagnoseCountsItsChecksAndExitsOnTheResult(t *testing.T) {
	home, run := diagnoseHome(t)
	cfg := filepath.Join(home, ".config", "claude-burst")

	out, code := run("--check")
	if code != 0 || !strings.Contains(out, "11 checks, 0 failed, 2 skipped") || strings.Contains(out, "FAIL") {
		t.Fatalf("a working install: exit %d\n%s", code, out)
	}
	if !strings.Contains(out, "PASS  claude-burst 9.9.9 is installed and runs") || !strings.Contains(out, "SKIP  the Codex gateway is off") {
		t.Errorf("checks:\n%s", out)
	}
	if entries, _ := filepath.Glob(filepath.Join(home, "burst-diagnose-*.txt")); len(entries) != 0 {
		t.Errorf("--check wrote a report: %v", entries)
	}

	// The gateway gone and nothing answering: each is its own failure with
	// what to run, and the exit code says so.
	must(t, os.Remove(filepath.Join(home, "pid")))
	must(t, os.WriteFile(filepath.Join(home, "http"), []byte("000"), 0o600))
	out, code = run("--check")
	if code != 1 || !strings.Contains(out, "11 checks, 6 failed") {
		t.Fatalf("a dead gateway on a dead network: exit %d\n%s", code, out)
	}
	for _, want := range []string{"FAIL  the gateway is not running", "run: burst-repair", "FAIL  this Mac has no working network", "FAIL  Anthropic cannot be reached"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}

	// A config that does not parse, and the crash-loop marker.
	must(t, os.WriteFile(filepath.Join(home, "pid"), []byte("1"), 0o600))
	must(t, os.WriteFile(filepath.Join(home, "http"), []byte("200"), 0o600))
	must(t, os.WriteFile(filepath.Join(cfg, "self-heal-crashloop"), nil, 0o600))
	must(t, os.WriteFile(filepath.Join(cfg, "config.json"), []byte(`{"listen":`), 0o600))
	out, code = run("--check")
	if code != 1 || !strings.Contains(out, "FAIL  config.json does not load") || !strings.Contains(out, "claude-burst restore-config") ||
		!strings.Contains(out, "FAIL  the gateway keeps restarting") {
		t.Fatalf("exit %d\n%s", code, out)
	}

	// Turned off on purpose is not a list of failures.
	must(t, os.Remove(filepath.Join(cfg, "self-heal-crashloop")))
	must(t, os.WriteFile(filepath.Join(cfg, "config.json"), []byte(`{"console_listen":"off"}`), 0o600))
	must(t, os.WriteFile(filepath.Join(cfg, "rolled-back"), nil, 0o600))
	must(t, os.Remove(filepath.Join(home, "pid")))
	out, code = run("--check")
	if code != 0 || !strings.Contains(out, "0 failed") || !strings.Contains(out, "SKIP  Burst was turned off by hand") {
		t.Fatalf("burst-off must not read as broken: exit %d\n%s", code, out)
	}
}

// The full report opens with the same checks and count, and its exit code
// is theirs too.
func TestDiagnoseReportOpensWithTheChecks(t *testing.T) {
	home, run := diagnoseHome(t)
	must(t, os.Remove(filepath.Join(home, "pid")))
	out, code := run()
	if code != 1 || !strings.Contains(out, "11 checks, 1 failed, 2 skipped") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	reports, _ := filepath.Glob(filepath.Join(home, "burst-diagnose-*.txt"))
	if len(reports) != 1 {
		t.Fatalf("reports: %v", reports)
	}
	report := readFile(reports[0])
	head := report[:min(len(report), 400)]
	if !strings.Contains(head, "===== Checks: 11 checks, 1 failed, 2 skipped =====") || !strings.Contains(report, "FAIL  the gateway is not running") {
		t.Errorf("the report does not open with the checks:\n%s", head)
	}
	if readFile(filepath.Join(home, "copied")) != report {
		t.Error("the report was not what was copied")
	}
}
