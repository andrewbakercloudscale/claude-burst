package integration

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// install.sh's check after it restarts the gateway, with the gateway played
// by a function: it is "healthy" when the installed binary's contents are
// listed in the file `healthy`. launchctl is a stub that records the restart.
func installHealth(t *testing.T, healthy string, withBackup bool) (rc int, target, out string, restarts int, shaFile string) {
	t.Helper()
	home := t.TempDir()
	bin := filepath.Join(home, "bin")
	must(t, os.MkdirAll(bin, 0o700))
	must(t, os.WriteFile(filepath.Join(bin, "launchctl"), []byte("#!/bin/sh\necho \"$@\" >> "+filepath.Join(home, "restarts")+"\n"), 0o755))
	target = filepath.Join(home, "claude-burst")
	backup := filepath.Join(home, "claude-burst-bin.latest.bak")
	shaFile = filepath.Join(home, ".claude-burst.build-sha")
	must(t, os.WriteFile(target, []byte("new"), 0o755))
	must(t, os.WriteFile(shaFile, []byte("newsha\n"), 0o644))
	if withBackup {
		must(t, os.WriteFile(backup, []byte("old"), 0o644))
	}
	must(t, os.WriteFile(filepath.Join(home, "healthy"), []byte(healthy), 0o600))
	root := repoRoot(t)
	script := `
set -euo pipefail
source "` + filepath.Join(root, "scripts", "install-health.sh") + `"
gateway_healthy() { grep -qx "$(cat "` + target + `")" "` + filepath.Join(home, "healthy") + `"; }
rc=0
burst_health_or_rollback "` + target + `" "` + backup + `" test.label "` + shaFile + `" oldsha || rc=$?
exit $rc
`
	cmd := exec.Command("/bin/zsh", "-c", script)
	cmd.Env = []string{"HOME=" + home, "PATH=" + bin + ":/usr/bin:/bin", "USER=test",
		"CLAUDE_BURST_INSTALL_HEALTH_TIMEOUT=2", "CLAUDE_BURST_INSTALL_HEALTH_SLEEP=0"}
	b, err := cmd.CombinedOutput()
	if ee, ok := err.(*exec.ExitError); ok {
		rc = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	restarts = strings.Count(readFile(filepath.Join(home, "restarts")), "kickstart")
	return rc, target, string(b), restarts, shaFile
}

func TestInstallKeepsANewBinaryThatAnswers(t *testing.T) {
	rc, target, out, restarts, sha := installHealth(t, "new\n", true)
	if rc != 0 || readFile(target) != "new" || restarts != 0 || readFile(sha) != "newsha\n" {
		t.Fatalf("rc %d, binary %q, %d restarts, sha %q\n%s", rc, readFile(target), restarts, readFile(sha), out)
	}
}

// The case this exists for: the new build passes --help and dies on start.
func TestInstallGoesBackWhenTheNewBinaryDoesNotAnswer(t *testing.T) {
	rc, target, out, restarts, sha := installHealth(t, "old\n", true)
	if rc != 2 {
		t.Fatalf("rc %d, want 2 (rolled back, healthy)\n%s", rc, out)
	}
	if readFile(target) != "old" {
		t.Errorf("the installed binary is %q, want the previous one", readFile(target))
	}
	if st, err := os.Stat(target); err != nil || st.Mode().Perm()&0o111 == 0 {
		t.Errorf("the binary put back is not executable: %v", err)
	}
	if restarts != 1 {
		t.Errorf("%d restarts after putting the binary back, want 1", restarts)
	}
	if readFile(sha) != "oldsha\n" {
		t.Errorf("the build id still names the build that was thrown out: %q", readFile(sha))
	}
	if !strings.Contains(out, "NOT installed") || !strings.Contains(out, "launchd.err.log") {
		t.Errorf("it must say the new build is not installed and where to look:\n%s", out)
	}
}

// A first install has nothing to go back to: it fails and says so, and
// does not restore some other run's backup.
func TestInstallWithNothingToGoBackToSaysSo(t *testing.T) {
	rc, target, out, restarts, _ := installHealth(t, "", false)
	if rc != 1 || readFile(target) != "new" || restarts != 0 || !strings.Contains(out, "no previous binary") {
		t.Fatalf("rc %d, binary %q, %d restarts\n%s", rc, readFile(target), restarts, out)
	}
}

// Neither binary answers: not the build. The previous binary is left in
// place and the message points at the diagnosis, not at the build.
func TestInstallSaysWhenTheBuildIsNotTheCause(t *testing.T) {
	rc, target, out, _, _ := installHealth(t, "", true)
	if rc != 3 || readFile(target) != "old" || !strings.Contains(out, "the build is not the cause") || !strings.Contains(out, "burst-off") {
		t.Fatalf("rc %d, binary %q\n%s", rc, readFile(target), out)
	}
}

// install.sh acts on the result: any non-zero is a failed install.
func TestInstallScriptRunsTheHealthCheck(t *testing.T) {
	s := readFile(filepath.Join(repoRoot(t), "install.sh"))
	i := strings.Index(s, `launchctl kickstart -k "gui/$UID/$LABEL"`)
	j := strings.Index(s, "burst_health_or_rollback ")
	k := strings.Index(s, "install-selfheal-watchdog.sh\" >/dev/null")
	if i < 0 || j < i || k < j {
		t.Fatalf("the health check must come after the restart and before the rest of the install (restart %s, check %s, watchdog %s)",
			strconv.Itoa(i), strconv.Itoa(j), strconv.Itoa(k))
	}
	if !strings.Contains(s[j:k], "exit 1") {
		t.Error("a gateway that does not answer must fail the install")
	}
}
