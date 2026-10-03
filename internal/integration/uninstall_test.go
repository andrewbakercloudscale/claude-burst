package integration

// The installer's uninstall path, run for real (the actual install.sh, the actual
// binary) in a throwaway HOME. Everything that would touch the real Mac is a stub
// on PATH that logs its call: launchctl (bootout would stop the developer's live
// gateway), sudo (every root script), security (the System keychain) and
// defaults. The machine-wide files install.sh reads (/etc/hosts, /etc/pf.conf,
// the pf anchor, /etc/claude-burst, /Library/LaunchDaemons) are pointed at a temp
// directory, so the developer's own transparent mode can neither leak into a run
// nor be touched by one.
//
// Uninstall used to delete the binary and the LaunchAgent and leave everything
// else: in transparent mode /etc/hosts still sent api.anthropic.com to a gateway
// that no longer existed, so Anthropic was unreachable for the whole Mac, and
// every Claude Code hook ran a deleted binary.

import (
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// sudoStub logs its call and, when $STUB_SYS/sudo-acts exists, does what the
// real root script would have done to the temp files. Without that file it
// fails, like a refused password. `sudo -n pfctl` answers from anchor-live.
const sudoStub = `#!/bin/zsh
echo "sudo $*" >> "$STUB_SYS/calls"
if [[ "$1" == -n ]]; then
  shift
  [[ "$1" == pfctl && -f "$STUB_SYS/anchor-live" ]] && echo 'rdr pass on lo0 inet proto tcp from any to 127.0.0.1 port = 443 -> 127.0.0.1 port 7777'
  exit 0
fi
[[ -f "$STUB_SYS/sudo-acts" ]] || exit 1
strip() { sed -i '' "/^# BEGIN claude-burst $2\$/,/^# END claude-burst $2\$/d" "$1"; }
case "${1:t} $2" in
  "transparent-root.sh remove")
    strip "$CLAUDE_BURST_HOSTS_FILE" hosts
    strip "$CLAUDE_BURST_PF_CONF" pf-rdr
    strip "$CLAUDE_BURST_PF_CONF" pf-load
    rm -f "$CLAUDE_BURST_PF_ANCHOR" "$CLAUDE_BURST_ROOT_STATE_DIR/transparent.state" "$STUB_SYS/anchor-live" ;;
  "transparent-root.sh admin-host-remove") strip "$CLAUDE_BURST_HOSTS_FILE" admin-host ;;
  "install-pf-heal.sh uninstall") rm -rf "$CLAUDE_BURST_PFHEAL_PLIST" "$CLAUDE_BURST_PFHEAL_LIBEXEC" ;;
  "untrust-ca-systemwide.sh ") rm -f "$STUB_SYS/ca-trusted" ;;
  "lid-awake-root.sh remove") rm -f "$CLAUDE_BURST_ROOT_STATE_DIR/lid-awake.state" ;;
esac
exit 0
`

const securityStub = `#!/bin/zsh
echo "security $*" >> "$STUB_SYS/calls"
[[ "$1" == find-certificate && -f "$STUB_SYS/ca-trusted" ]] && exit 0
exit 44
`

const logStub = `#!/bin/zsh
echo "${0:t} $*" >> "$STUB_SYS/calls"
exit 0
`

type uninstallRun struct {
	*homeRig
	sys, target string
	env         []string
}

// newUninstallRun puts the binary where install.sh expects it, the stubs on
// PATH, and an empty fake system under sys.
func newUninstallRun(t *testing.T) *uninstallRun {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("install.sh is macOS-only")
	}
	if _, err := exec.LookPath("zsh"); err != nil {
		t.Skip("zsh not available")
	}
	u := &uninstallRun{homeRig: newHomeRig(t), sys: t.TempDir()}
	// A config.json for the uninstall to keep (or purge), and for the
	// transparent-mode tests to edit.
	must(t, os.WriteFile(filepath.Join(u.home, ".config", "claude-burst", "config.json"), []byte(`{"listen":"127.0.0.1:17777"}`+"\n"), 0o600))

	installDir := filepath.Join(u.home, ".local", "bin")
	must(t, os.MkdirAll(installDir, 0o755))
	u.target = filepath.Join(installDir, "claude-burst")
	src, err := os.Open(u.bin)
	must(t, err)
	dst, err := os.OpenFile(u.target, os.O_CREATE|os.O_WRONLY, 0o755)
	must(t, err)
	_, err = io.Copy(dst, src)
	must(t, err)
	src.Close()
	dst.Close()

	stubs := t.TempDir()
	for name, body := range map[string]string{"sudo": sudoStub, "security": securityStub, "launchctl": logStub, "defaults": logStub, "claude": logStub} {
		must(t, os.WriteFile(filepath.Join(stubs, name), []byte(body), 0o755))
	}
	for _, d := range []string{"pf.anchors", "state", "LaunchDaemons"} {
		must(t, os.MkdirAll(filepath.Join(u.sys, d), 0o755))
	}
	must(t, os.WriteFile(u.sysPath("hosts"), []byte("127.0.0.1 localhost\n255.255.255.255 broadcasthost\n"), 0o644))
	must(t, os.WriteFile(u.sysPath("pf.conf"), []byte("scrub-anchor \"com.apple/*\"\nrdr-anchor \"com.apple/*\"\n"), 0o644))
	u.env = []string{
		"HOME=" + u.home,
		"PATH=" + stubs + ":/usr/bin:/bin:/usr/sbin:/sbin",
		"STUB_SYS=" + u.sys,
		"CLAUDE_BURST_HOSTS_FILE=" + u.sysPath("hosts"),
		"CLAUDE_BURST_PF_CONF=" + u.sysPath("pf.conf"),
		"CLAUDE_BURST_PF_ANCHOR=" + u.sysPath("pf.anchors/claude-burst"),
		"CLAUDE_BURST_ROOT_STATE_DIR=" + u.sysPath("state"),
		"CLAUDE_BURST_LAUNCHDAEMONS=" + u.sysPath("LaunchDaemons"),
		"CLAUDE_BURST_PFHEAL_PLIST=" + u.sysPath("LaunchDaemons/ninja.andrewbaker.claude-burst-pfheal.plist"),
		"CLAUDE_BURST_PFHEAL_LIBEXEC=" + u.sysPath("libexec"),
	}
	return u
}

func (u *uninstallRun) sysPath(p string) string { return filepath.Join(u.sys, p) }

// transparent lays down what transparent-root.sh install, install-pf-heal.sh
// and trust-ca-systemwide.sh leave behind.
func (u *uninstallRun) transparent() {
	t := u.t
	f, err := os.OpenFile(u.sysPath("hosts"), os.O_APPEND|os.O_WRONLY, 0)
	must(t, err)
	_, err = f.WriteString("# BEGIN claude-burst hosts\n127.0.0.1 api.anthropic.com\n# END claude-burst hosts\n")
	must(t, err)
	f.Close()
	f, err = os.OpenFile(u.sysPath("pf.conf"), os.O_APPEND|os.O_WRONLY, 0)
	must(t, err)
	_, err = f.WriteString("# BEGIN claude-burst pf-load\nload anchor \"claude-burst\" from \"/etc/pf.anchors/claude-burst\"\n# END claude-burst pf-load\n")
	must(t, err)
	f.Close()
	for _, p := range []string{"pf.anchors/claude-burst", "state/transparent.state", "anchor-live", "ca-trusted",
		"LaunchDaemons/ninja.andrewbaker.claude-burst-pfheal.plist"} {
		must(t, os.WriteFile(u.sysPath(p), []byte("x\n"), 0o644))
	}
	must(t, os.MkdirAll(u.sysPath("libexec"), 0o755))
	cfg := filepath.Join(u.home, ".config", "claude-burst", "config.json")
	b, err := os.ReadFile(cfg)
	must(t, err)
	must(t, os.WriteFile(cfg, []byte(strings.Replace(string(b), "{", `{"intercept":{"mode":"transparent"},`, 1)), 0o600))
}

func (u *uninstallRun) uninstall(args ...string) (out string, code int) {
	u.t.Helper()
	cmd := exec.Command("zsh", append([]string{"install.sh", "uninstall"}, args...)...)
	cmd.Dir = filepath.Join("..", "..")
	cmd.Env = u.env
	b, err := cmd.CombinedOutput()
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		u.t.Fatal(err)
	}
	return string(b), code
}

func (u *uninstallRun) calls() string {
	b, _ := os.ReadFile(u.sysPath("calls"))
	return string(b)
}

func writeJSON(t *testing.T, p string, v any) {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	must(t, err)
	must(t, os.WriteFile(p, append(b, '\n'), 0o600))
}

// addBurstHooks puts the hooks the gateway installs at startup into
// settings.json, as it would have written them: coordination runs the binary,
// handover and the prompt notice run scripts under ~/.config/claude-burst.
func (u *uninstallRun) addBurstHooks() {
	t := u.t
	cfgDir := filepath.Join(u.home, ".config", "claude-burst")
	m := u.settings()
	hooks := m["hooks"].(map[string]any)
	hook := func(cmd string) []any {
		return []any{map[string]any{"matcher": "", "hooks": []any{map[string]any{"type": "command", "command": cmd, "timeout": 10}}}}
	}
	hooks["SessionStart"] = append(hook(`"`+u.target+`" coord session-start`), hook(filepath.Join(cfgDir, "handover", "start.sh"))...)
	hooks["SessionEnd"] = hook(filepath.Join(cfgDir, "handover", "end.sh"))
	hooks["UserPromptSubmit"] = hook(filepath.Join(cfgDir, "prompt-notice.sh"))
	writeJSON(t, u.settingsPath(), m)
}

func TestInstallScriptUninstallRemovesEveryHook(t *testing.T) {
	u := newUninstallRun(t)
	skill := u.installLegacyShunt()
	u.addBurstHooks()
	if !strings.Contains(strings.Join(preToolUseCommands(u.settings()), " "), "shunt guard") {
		t.Fatalf("precondition: the hook should be installed")
	}
	if _, err := os.Stat(skill); err != nil {
		t.Fatalf("precondition: the skill should be installed: %v", err)
	}

	out, code := u.uninstall()
	if code != 0 {
		t.Fatalf("uninstall failed (%d):\n%s", code, out)
	}

	if _, err := os.Stat(u.target); err == nil {
		t.Errorf("the binary should be gone")
	}
	raw, _ := os.ReadFile(u.settingsPath())
	if strings.Contains(string(raw), "claude-burst") {
		t.Errorf("settings.json still names claude-burst:\n%s", raw)
	}
	cmds := preToolUseCommands(u.settings())
	if len(cmds) != 1 || cmds[0] != "~/theirs.sh" {
		t.Errorf("uninstall must remove only Burst's hooks, leaving the user's own: %v", cmds)
	}
	if _, err := os.Stat(skill); err == nil {
		t.Errorf("the skill tells Claude to run a binary that no longer exists; it must be removed")
	}
	if u.settings()["model"] != "sonnet" {
		t.Errorf("an unrelated setting was disturbed")
	}
	if _, err := os.Stat(filepath.Join(u.home, ".config", "claude-burst", "config.json")); err != nil {
		t.Errorf("config.json must be kept without --purge: %v", err)
	}
	contains(t, "uninstall output", out, "Uninstalled Claude Burst", "Kept "+filepath.Join(u.home, ".config", "claude-burst"))

	calls := u.calls()
	// Nothing machine-wide was installed, so nothing may ask for a password.
	for _, line := range strings.Split(calls, "\n") {
		if strings.HasPrefix(line, "sudo ") && !strings.HasPrefix(line, "sudo -n ") {
			t.Errorf("a base-url uninstall ran a root step: %q", line)
		}
	}
	contains(t, "stub calls", calls,
		"launchctl bootout gui/", "ninja.andrewbaker.claude-burst-selfheal", // the watchdog, through its own script
		"defaults delete com.mitchellh.ghostty", "security find-certificate",
		"claude plugin marketplace remove burst") // the burst-band mod, through update-mod.sh
	// The watchdog reloads the gateway, and a gateway that starts reinstalls
	// its hooks: both must be stopped before the hooks come out.
	if strings.Index(calls, "claude-burst-selfheal") > strings.Index(calls, "/ninja.andrewbaker.claude-burst\n") {
		t.Errorf("the watchdog must go before the gateway is booted out:\n%s", calls)
	}
}

func TestInstallScriptUninstallPurge(t *testing.T) {
	u := newUninstallRun(t)
	out, code := u.uninstall("--purge")
	if code != 0 {
		t.Fatalf("uninstall --purge failed (%d):\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(u.home, ".config", "claude-burst")); err == nil {
		t.Errorf("--purge must remove ~/.config/claude-burst")
	}
	contains(t, "uninstall output", out, "Purged")
}

// Transparent mode, with every root piece present: each is removed by its own
// script under sudo, in an order that never strands the redirect, and only then
// is success reported.
func TestInstallScriptUninstallRemovesTransparentMode(t *testing.T) {
	u := newUninstallRun(t)
	u.transparent()
	must(t, os.WriteFile(u.sysPath("sudo-acts"), nil, 0o644))

	out, code := u.uninstall()
	if code != 0 {
		t.Fatalf("uninstall failed (%d):\n%s\ncalls:\n%s", code, out, u.calls())
	}
	contains(t, "uninstall output", out, "sudo will ask for your password", "api.anthropic.com redirect", "System keychain", "Uninstalled Claude Burst")

	hosts, _ := os.ReadFile(u.sysPath("hosts"))
	if strings.Contains(string(hosts), "claude-burst") || !strings.Contains(string(hosts), "broadcasthost") {
		t.Errorf("hosts must lose only Burst's block:\n%s", hosts)
	}
	calls := u.calls()
	order := []string{"install-pf-heal.sh uninstall", "transparent-root.sh remove", "untrust-ca-systemwide.sh", "sudo -n pfctl -a claude-burst -s nat"}
	last := -1
	for _, step := range order {
		i := strings.Index(calls, step)
		if i < 0 {
			t.Fatalf("%q was never run:\n%s", step, calls)
		}
		if i < last {
			t.Errorf("%q ran out of order:\n%s", step, calls)
		}
		last = i
	}
	// The redirect goes before the gateway does.
	if strings.Index(calls, "transparent-root.sh remove") > strings.Index(calls, "/ninja.andrewbaker.claude-burst\n") {
		t.Errorf("the gateway was stopped while the redirect still pointed at it:\n%s", calls)
	}
}

// sudo refused: the redirect is still there, so nothing else may be removed.
// Stopping the gateway now would leave the Mac unable to reach Anthropic.
func TestInstallScriptUninstallStopsWhenTheRedirectStays(t *testing.T) {
	u := newUninstallRun(t)
	u.transparent()

	out, code := u.uninstall()
	if code == 0 {
		t.Fatalf("must fail while /etc/hosts still redirects:\n%s", out)
	}
	contains(t, "uninstall output", out, "UNINSTALL STOPPED", "still redirects api.anthropic.com", "sudo "+filepath.Join(repoRoot(t), "scripts", "transparent-root.sh")+" remove")
	if strings.Contains(out, "Uninstalled Claude Burst") {
		t.Errorf("success printed after a failure:\n%s", out)
	}
	if _, err := os.Stat(u.target); err != nil {
		t.Errorf("the binary must stay while the gateway is still needed: %v", err)
	}
	if strings.Contains(u.calls(), "launchctl bootout") {
		t.Errorf("nothing may be booted out:\n%s", u.calls())
	}
}

// The redirect came out but other root pieces did not: the uninstall carries
// on, then fails its check naming each piece and the command that removes it.
func TestInstallScriptUninstallReportsWhatIsLeft(t *testing.T) {
	u := newUninstallRun(t)
	u.transparent()
	// Only the hosts block is gone, as if transparent-root.sh remove had
	// half-run; sudo itself is refused.
	hosts, _ := os.ReadFile(u.sysPath("hosts"))
	must(t, os.WriteFile(u.sysPath("hosts"), []byte(strings.Split(string(hosts), "# BEGIN")[0]), 0o644))
	// A hook nothing knows how to remove.
	m := u.settings()
	m["statusLine"] = map[string]any{"type": "command", "command": "~/.local/bin/claude-burst stats"}
	writeJSON(t, u.settingsPath(), m)

	out, code := u.uninstall()
	if code == 0 {
		t.Fatalf("must fail while pieces remain:\n%s", out)
	}
	contains(t, "uninstall output", out, "UNINSTALL INCOMPLETE",
		"pf anchor", "transparent-root.sh remove",
		"pf self-heal LaunchDaemon", "install-pf-heal.sh uninstall",
		"System keychain", "untrust-ca-systemwide.sh",
		"settings.json still mentions claude-burst", "claude-burst stats")
	if strings.Contains(out, "Uninstalled Claude Burst") {
		t.Errorf("success printed after a failure:\n%s", out)
	}
}
