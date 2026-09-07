package admin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
)

// This file implements the dashboard's "Install Claude Burst" buttons.
//
// Installing is deliberately NOT done in-process. Two of the steps
// (transparent mode's /etc/hosts + pf redirect, and the System-keychain CA
// trust) need root, and the whole reason `claude-burst enable` prints those
// rather than running them is that a change affecting every process on this
// Mac should happen with the person at the keyboard watching it happen. A
// web page that silently escalated to root would be worse, not better: this
// server has no login, and its only defences are "the Host header names
// loopback" and "a cross-origin page cannot set our mutation header".
//
// So the button opens a Terminal window running a generated script, and the
// user sees every command, answers the sudo prompt themselves, and can read
// the output (and stop) at any point. What the button actually buys is that
// the ordering is right -- which is the part that has broken this machine
// before.
//
// That ordering, from ROLLBACK.md and scripts/install-proxy.sh:
//
//	1. back up ~/.claude/settings.json and config.json first
//	2. write the intercept mode into config.json
//	3. restart the gateway so it READS that mode, and wait for /healthz
//	4. only then `claude-burst enable`, which repoints Claude Code
//	5. (transparent only) the machine-wide root steps, last
//
// Step 3 before step 4 is not a nicety. `enable` rewrites settings.json
// immediately; if nothing is listening yet, the very next Claude Code
// request -- including one from a session running right now -- gets
// connection refused. That happened on 2026-08-30 and had to be recovered
// by hand-editing settings.json.

// installModes are the only values /api/install accepts. The mode is the
// ONLY part of the request that reaches the generated script, and it is
// matched against this list rather than interpolated, so nothing a caller
// sends can become shell.
var installModes = map[string]bool{"base-url": true, "transparent": true}

// launchTerminal is a variable so tests can substitute a recorder: the real
// implementation opens a GUI app, which a test can neither do nor assert on.
var launchTerminal = func(scriptPath string) error {
	// `open -a Terminal <executable file>` runs it in a new window and
	// brings Terminal to the front. Preferred over osascript's `do script`,
	// which would mean embedding the command inside an AppleScript string
	// literal inside a shell argument -- three quoting layers to get wrong.
	return exec.Command("open", "-a", "Terminal", scriptPath).Run()
}

// executablePath is os.Executable, indirected for tests. The admin server
// runs inside the gateway process, so this IS the claude-burst binary --
// which matters, because the binary in ~/.local/bin and the one being run
// are not always the same during development, and the script should drive
// the one the user is actually looking at the dashboard of.
var executablePath = os.Executable

type installRequest struct {
	Mode string `json:"mode"`
}

type installResponse struct {
	Mode string `json:"mode"`
	// Script is where the generated script was written. Surfaced so the
	// user can read it, re-run it, or run it by hand if Terminal never
	// appeared -- a button whose window failed to open should still leave
	// behind something runnable rather than just an error.
	Script    string `json:"script"`
	NeedsSudo bool   `json:"needs_sudo"`
	Detail    string `json:"detail"`
}

// handleInstall generates the install script for the requested mode, writes
// it, and opens it in Terminal.
func (s *Server) handleInstall(w http.ResponseWriter, r *http.Request) {
	var req installRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request body", http.StatusBadRequest)
		return
	}
	if !installModes[req.Mode] {
		http.Error(w, "unknown install mode "+strconv.Quote(req.Mode)+" (want base-url or transparent)", http.StatusBadRequest)
		return
	}
	cfg, err := config.Load()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	script, err := s.installScript(req.Mode, cfg)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	path, err := writeInstallScript(req.Mode, script)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	resp := installResponse{Mode: req.Mode, Script: path, NeedsSudo: req.Mode == "transparent"}
	if err := launchTerminal(path); err != nil {
		// The script exists and is runnable; only the window failed. Say
		// exactly that, and give the command, rather than reporting a
		// failure that sounds like nothing happened.
		writeJSON(w, installResponse{
			Mode: req.Mode, Script: path, NeedsSudo: resp.NeedsSudo,
			Detail: fmt.Sprintf("could not open Terminal (%v). The script is ready -- run it yourself:\n%s", err, path),
		})
		return
	}
	if req.Mode == "transparent" {
		resp.Detail = "A Terminal window is now running the install. It asks for your password (sudo) " +
			"before the machine-wide redirect, and prints every command it runs. Watch it to the end, then restart Claude Code."
	} else {
		resp.Detail = "A Terminal window is now running the install. No password is needed for base-url mode. " +
			"Watch it to the end, then restart Claude Code."
	}
	writeJSON(w, resp)
}

// scriptsDir returns the directory holding the repo's helper scripts,
// derived from the already-resolved transparent-root.sh path rather than
// searched for again. rootHelperPath falls back to a descriptive string
// when it finds nothing, so an existence check here is what distinguishes
// "we know where the scripts are" from "we are about to write a script full
// of paths that do not exist".
func (s *Server) scriptsDir() (string, bool) {
	if s.rootHelper == "" {
		return "", false
	}
	dir := filepath.Dir(s.rootHelper)
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return "", false
	}
	return dir, true
}

// installScript builds the shell script for a mode. Kept separate from the
// handler (and from anything that runs it) so its exact text is testable:
// the ordering it encodes is the whole value of this feature, and a test
// that could only observe "a Terminal opened" would not notice the ordering
// silently changing.
func (s *Server) installScript(mode string, cfg config.Config) (string, error) {
	bin, err := executablePath()
	if err != nil {
		return "", fmt.Errorf("could not locate the claude-burst binary: %w", err)
	}
	dir, ok := s.scriptsDir()
	if !ok {
		return "", fmt.Errorf("could not locate the claude-burst repo's scripts/ directory "+
			"(looked next to %q) -- run the install from a terminal in the repo instead", s.rootHelper)
	}

	var b strings.Builder
	fmt.Fprintf(&b, `#!/bin/zsh
# Generated by the Claude Burst admin UI. Rewritten on every click of the
# Install button -- edit scripts/ in the repo, not this file.
set -uo pipefail

BIN=%s
SCRIPTS=%s
LABEL="ninja.andrewbaker.claude-burst"
PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"

# The one liveness check, shared with deploy.sh and watchdog.sh. It probes
# the direct port AND the real traffic path, because direct SYNs to the
# gateway port are intermittently dropped while the gateway is perfectly
# healthy (issue #1) -- a check that only did the former has already caused
# an unattended rollback of a working build.
source "$SCRIPTS/health-diagnostics.sh"

wait_healthy() {
  echo "waiting for the gateway to answer /healthz..."
  for i in $(seq 1 20); do
    if gateway_healthy; then echo "gateway healthy"; return 0; fi
    sleep 1
  done
  return 1
}

echo "== Claude Burst install: %s mode =="
echo

`, shellQuote(bin), shellQuote(dir), mode)

	host := cfg.Intercept.Host
	if host == "" {
		host = "api.anthropic.com"
	}

	if mode == "transparent" {
		fmt.Fprintf(&b, `# Ask for the password up front, while there is a human here to type it.
# install-proxy.sh checks for cached credentials with 'sudo -n' and stops
# rather than hanging on a prompt nothing is attached to; priming them here
# is what turns that stop into a normal run.
echo "Transparent mode changes settings that affect EVERY process on this Mac:"
echo "  - /etc/hosts sends %s to 127.0.0.1"
echo "  - a pf rule redirects :443 to the local gateway"
echo "  - the local CA is trusted in the System keychain, so other apps"
echo "    (Claude Desktop's updater, for one) don't fail TLS against it"
echo "Undo all of it any time with: sudo $SCRIPTS/transparent-root.sh remove"
echo
sudo -v || { echo "no sudo credentials -- nothing has been changed" >&2; exit 1; }
echo

echo "== writing intercept mode to config.json =="
"$BIN" configure --intercept-mode transparent || exit 1
echo

# Everything that follows -- backup, gateway restart, health wait, enable,
# both root steps, watchdog -- is install-proxy.sh, in the order ROLLBACK.md
# requires. Deliberately not reimplemented here: two copies of an ordering
# this consequential is exactly how one of them ends up wrong.
"$SCRIPTS/install-proxy.sh"
`, host)
	} else {
		fmt.Fprintf(&b, `# Refuse to half-switch. Transparent mode's redirect sends every
# connection to %s -- from any process on this Mac -- to
# the gateway on :443, where it expects TLS. In base-url mode the gateway
# serves plain HTTP and nothing listens on :443 at all, so leaving the
# redirect in place while switching does not merely bypass burst: it takes
# %s away from the whole machine. Removing it needs
# root, which this script does not have in base-url mode, so stop here
# rather than complete a switch that breaks things elsewhere.
if awk -v h=%s '
  { sub(/#.*/, "") }
  ($1 == "127.0.0.1" || $1 == "::1") {
    for (i = 2; i <= NF; i++) if (tolower($i) == tolower(h)) { found = 1 }
  }
  END { exit !found }
' /etc/hosts 2>/dev/null; then
  echo "STOPPING: transparent mode's /etc/hosts redirect is still installed." >&2
  echo "Remove it first, then run this again:" >&2
  echo "    sudo $SCRIPTS/transparent-root.sh remove" >&2
  echo "Nothing has been changed." >&2
  exit 1
fi

echo "Base-url mode points Claude Code at the gateway with ANTHROPIC_BASE_URL"
echo "in ~/.claude/settings.json. No root, nothing machine-wide -- but Claude"
echo "Code disables Remote Control while that variable names a non-Anthropic"
echo "host. Use transparent mode if you need Remote Control."
echo

echo "== 1. backing up settings.json and config.json =="
"$SCRIPTS/backup-config.sh" || exit 1
echo

echo "== 2. writing intercept mode to config.json =="
"$BIN" configure --intercept-mode base-url || exit 1
echo

# The gateway only reads config at startup, so the mode written above is not
# live until it restarts -- and 'enable' must not run until something is
# actually listening (see the header comment).
# Clear the "a human rolled this back" marker scripts/rollback.sh leaves
# behind, so the self-heal watchdog starts minding the gateway again. The
# transparent path gets this from install-proxy.sh; base-url does its own
# steps, so it needs its own line rather than inheriting the omission.
rm -f "$HOME/.config/claude-burst/rolled-back" "$HOME/.config/claude-burst/rolled-back.noted"

echo "== 3. restarting the gateway and waiting for it to be healthy =="
if [[ -f "$PLIST" ]]; then
  launchctl enable "gui/$UID/$LABEL" >/dev/null 2>&1 || true
  launchctl kickstart -k "gui/$UID/$LABEL" >/dev/null 2>&1 \
    || { launchctl bootstrap "gui/$UID" "$PLIST" && launchctl kickstart -k "gui/$UID/$LABEL"; }
else
  echo "no LaunchAgent at $PLIST -- assuming you started the gateway by hand"
fi
if ! wait_healthy; then
  dump_health_diagnostics "install (base-url): gateway never came healthy"
  echo >&2
  echo "The gateway is not answering, so settings.json has NOT been touched." >&2
  echo "Claude Code is still talking to Anthropic directly, which is the safe" >&2
  echo "state to be stuck in. See ~/.config/claude-burst/health-check-failures.log" >&2
  exit 1
fi
echo

echo "== 4. pointing Claude Code at the gateway =="
"$BIN" enable || exit 1
echo

echo "install complete -- restart Claude Code, then click Test connection in the dashboard"
`, host, host, shellQuote(host))
	}

	// Both paths end by holding the window open. Without this the Terminal
	// window closes (or looks finished) the instant the script exits, which
	// throws away the output on exactly the runs where it matters.
	b.WriteString("\necho\nread -k 1 \"?Press any key to close this window...\"\n")
	return b.String(), nil
}

// writeGeneratedScript puts a generated script somewhere stable and
// predictable rather than in a random temp path: the response names this
// file, and a name the user can find again is the difference between
// "re-run it" and "click the button again and hope". Shared with the Revert
// button (revert.go), which needs exactly the same properties.
func writeGeneratedScript(name, body string) (string, error) {
	dir, err := config.ConfigDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, name+".command")
	// 0700: `open -a Terminal` only RUNS a file that is executable --
	// otherwise Terminal opens it as a document and nothing happens.
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		return "", err
	}
	return path, nil
}

func writeInstallScript(mode, body string) (string, error) {
	return writeGeneratedScript("install-"+mode, body)
}

// shellQuote wraps a string in single quotes for zsh, escaping any single
// quotes within. Every value this script interpolates is server-derived (a
// binary path, a directory), not user input -- quoting them anyway costs
// nothing and means a path with a space stops being a latent bug.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
