package integration

// The installer's uninstall path, run for real (the actual install.sh, the actual
// binary) in a throwaway HOME. launchctl is stubbed, since bootout would touch the
// developer's real LaunchAgents.
//
// Token shunting (removed 2026-10-02) installed a hook into ~/.claude/settings.json
// that runs this binary on every Read and Bash call, and a skill that tells Claude
// to run it. An uninstall that removes the binary but leaves those behind leaves a
// hook pointing at nothing and a skill instructing Claude to run a command that no
// longer exists.

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestInstallScriptUninstallRemovesShuntHookAndSkill(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("install.sh is macOS-only")
	}
	if _, err := exec.LookPath("zsh"); err != nil {
		t.Skip("zsh not available")
	}
	r := newHomeRig(t)
	skill := r.installLegacyShunt()
	if !strings.Contains(strings.Join(preToolUseCommands(r.settings()), " "), "shunt guard") {
		t.Fatalf("precondition: the hook should be installed")
	}
	if _, err := os.Stat(skill); err != nil {
		t.Fatalf("precondition: the skill should be installed: %v", err)
	}

	// Put the binary where install.sh expects it, and stub launchctl.
	installDir := filepath.Join(r.home, ".local", "bin")
	must(t, os.MkdirAll(installDir, 0o755))
	target := filepath.Join(installDir, "claude-burst")
	src, err := os.Open(r.bin)
	must(t, err)
	dst, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY, 0o755)
	must(t, err)
	_, err = io.Copy(dst, src)
	must(t, err)
	src.Close()
	dst.Close()
	stubs := t.TempDir()
	must(t, os.WriteFile(filepath.Join(stubs, "launchctl"), []byte("#!/bin/sh\nexit 0\n"), 0o755))
	// install.sh uninstall also runs `sudo lid-awake-root.sh remove` when
	// this machine's /etc/claude-burst says keep-awake is applied, and
	// `defaults delete` on Ghostty's App Nap. Neither honours HOME: unstubbed,
	// every test run switched this Mac's Ghostty setting off, and with sudo's
	// password cached it would have removed the real keep-awake too.
	touched := filepath.Join(stubs, "touched")
	for _, name := range []string{"sudo", "defaults"} {
		must(t, os.WriteFile(filepath.Join(stubs, name), []byte("#!/bin/sh\necho \""+name+" $*\" >> "+touched+"\nexit 0\n"), 0o755))
	}

	cmd := exec.Command("zsh", "install.sh", "uninstall")
	cmd.Dir = filepath.Join("..", "..")
	cmd.Env = []string{"HOME=" + r.home, "PATH=" + stubs + ":/usr/bin:/bin:/usr/sbin:/sbin"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("uninstall failed: %v\n%s", err, out)
	}

	if _, err := os.Stat(target); err == nil {
		t.Errorf("the binary should be gone")
	}
	r.assertLegacyShuntGone(skill)
	contains(t, "uninstall output", string(out), "token-shunting")
	if log, _ := os.ReadFile(touched); !strings.Contains(string(log), "defaults delete com.mitchellh.ghostty") {
		t.Errorf("the defaults stub was not used, so the real one may have run: %q", log)
	}
}
