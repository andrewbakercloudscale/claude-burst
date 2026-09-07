package admin

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// This file surfaces the pf self-heal daemon (scripts/pf-heal.sh, installed by
// scripts/install-pf-heal.sh) on the dashboard: whether it is actually running,
// what it has caught, and a button to arm it.
//
// It exists because a guard nobody can see is a guard nobody knows is missing.
// The whole 2026-09-07 outage was a piece of machinery that had quietly stopped
// covering anything while every visible indicator stayed green, and shipping
// its replacement as a script you had to already know about would have repeated
// the shape of the mistake at one remove.

const (
	pfHealLabel     = "ninja.andrewbaker.claude-burst-pfheal"
	pfHealPlist     = "/Library/LaunchDaemons/" + pfHealLabel + ".plist"
	pfHealHeartbeat = "/etc/claude-burst/pf-heal.heartbeat"
	pfHealLog       = "/var/log/claude-burst-pf.log"

	// A cycle runs every 120s. Two missed cycles plus slack: long enough that
	// a wake-from-sleep gap is not reported as death, short enough that a
	// genuinely dead daemon is visible within minutes.
	pfHealStaleAfter = 6 * time.Minute
)

// pfHealInfo is what the dashboard can honestly say about the daemon.
//
// Note what is NOT here: launchd's opinion. `launchctl print system/<label>`
// returns 113 (not permitted) to an unprivileged process -- for every system
// daemon, not just this one -- and the gateway deliberately runs unprivileged.
// A field derived from that call would report "not loaded" whenever we merely
// could not look, which is the exact failure this whole feature is a response
// to. So liveness comes from a heartbeat the daemon itself writes each cycle,
// which an unprivileged reader can actually verify.
type pfHealInfo struct {
	// Installed means the LaunchDaemon plist is on disk.
	Installed bool `json:"installed"`
	// Running means a heartbeat exists and is recent. This is the field the
	// UI leads with; Installed only distinguishes "never set up" from
	// "set up but dead".
	Running bool `json:"running"`
	// LastCheckSeconds is the heartbeat's age, -1 when there is no heartbeat.
	LastCheckSeconds int64 `json:"last_check_seconds"`
	// Events is the tail of consequential log lines -- repairs, bail-outs.
	// Empty is the good case and says so in the UI rather than looking broken.
	Events     []string `json:"events,omitempty"`
	LogPath    string   `json:"log_path"`
	HasLog     bool     `json:"has_log"`
	InstallCmd string   `json:"install_cmd,omitempty"`
}

func pfHealStatus(scriptsDir string) pfHealInfo {
	info := pfHealInfo{LogPath: pfHealLog, LastCheckSeconds: -1}
	if _, err := os.Stat(pfHealPlist); err == nil {
		info.Installed = true
	}
	if b, err := os.ReadFile(pfHealHeartbeat); err == nil {
		if secs, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64); err == nil {
			age := time.Since(time.Unix(secs, 0))
			info.LastCheckSeconds = int64(age.Seconds())
			info.Running = age < pfHealStaleAfter && age > -time.Minute
		}
	}
	if st, err := os.Stat(pfHealLog); err == nil && st.Size() > 0 {
		info.HasLog = true
		info.Events = pfHealEvents(pfHealLog, 5)
	}
	if scriptsDir != "" {
		info.InstallCmd = "sudo " + scriptsDir + "/install-pf-heal.sh"
	}
	return info
}

// pfHealEventKeywords are the lines worth putting in front of someone without
// them asking. Routine cycles write nothing at all, so this is not a filter
// against volume -- it is a filter against the log's own preamble and the
// indented command output it captures under each event.
var pfHealEventKeywords = []string{"BROKEN", "HEALED", "recovered", "GIVING UP", "BAILED OUT", "FATAL", "repair FAILED"}

func pfHealEvents(path string, max int) []string {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var hits []string
	for _, line := range strings.Split(string(b), "\n") {
		for _, kw := range pfHealEventKeywords {
			if strings.Contains(line, kw) {
				hits = append(hits, strings.TrimSpace(line))
				break
			}
		}
	}
	if len(hits) > max {
		hits = hits[len(hits)-max:]
	}
	return hits
}

// handlePFHealLog serves the tail of the daemon's log. Separate from
// handleLog rather than a mode switch on it: that one resolves its path from
// config and serves the gateway's own log, and folding two different files
// behind one endpoint invites a query parameter that could name a third.
func (s *Server) handlePFHealLog(w http.ResponseWriter, r *http.Request) {
	f, err := os.Open(pfHealLog)
	if err != nil {
		// Not an error worth a 500: no log means the daemon has never had
		// anything to say, which is the outcome we want.
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintf(w, "%s does not exist yet.\n\n"+
			"That means one of two things: the pf self-heal daemon is not installed, or it is\n"+
			"installed and has never needed to do anything. The dashboard's guard row says which.\n", pfHealLog)
		return
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if st.Size() > logTailBytes {
		if _, err := f.Seek(-logTailBytes, io.SeekEnd); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		fmt.Fprintf(w, "(showing the last %dKB of %s -- %d bytes total)\n\n", logTailBytes/1024, pfHealLog, st.Size())
	}
	_, _ = io.Copy(w, f)
}

// pfHealInstallScript arms the daemon the same way every other privileged
// step in this project happens: in a Terminal window the user is watching,
// answering the sudo prompt themselves. See install.go's header comment.
func pfHealInstallScript(scriptsDir string) string {
	return fmt.Sprintf(`#!/bin/zsh
# Generated by the Claude Burst admin UI. Rewritten on every click --
# edit scripts/install-pf-heal.sh in the repo, not this file.
set -uo pipefail

SCRIPTS=%s

echo "== Claude Burst: arm the pf self-heal daemon =="
echo
echo "Transparent mode's pf rdr rule can be dropped from the loaded ruleset by"
echo "other software on this Mac (VPN and endpoint-security clients reload pf"
echo "on network change and on wake). When that happens while /etc/hosts still"
echo "redirects, EVERY process here is refused for the intercepted host."
echo
echo "This installs a root LaunchDaemon that checks every 2 minutes and:"
echo "  - reloads the rule if it has gone missing"
echo "  - removes the redirect entirely if it cannot, so this Mac can still"
echo "    reach Anthropic directly"
echo
echo "It asks for your password. Log: /var/log/claude-burst-pf.log"
echo

if [[ ! -x "$SCRIPTS/install-pf-heal.sh" ]]; then
  echo "cannot find $SCRIPTS/install-pf-heal.sh -- nothing has been changed" >&2
  echo
  read -k 1 "?Press any key to close this window..."
  exit 1
fi

sudo "$SCRIPTS/install-pf-heal.sh"
rc=$?
echo
if (( rc == 0 )); then
  echo "== armed -- click Refresh in the dashboard to confirm =="
else
  echo "== install-pf-heal.sh exited $rc; read the output above ==" >&2
fi
echo
read -k 1 "?Press any key to close this window..."
exit $rc
`, shellQuote(scriptsDir))
}

func (s *Server) handlePFHealInstall(w http.ResponseWriter, r *http.Request) {
	dir, ok := s.scriptsDir()
	if !ok {
		http.Error(w, "could not locate the claude-burst repo's scripts/ directory "+
			"(looked next to "+strconv.Quote(s.rootHelper)+") -- run install-pf-heal.sh from a terminal instead",
			http.StatusBadRequest)
		return
	}
	path, err := writeGeneratedScript("arm-pf-heal", pfHealInstallScript(dir))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	resp := map[string]string{"script": path}
	if err := launchTerminal(path); err != nil {
		resp["detail"] = fmt.Sprintf("could not open Terminal (%v). The script is ready -- run it yourself:\n%s", err, path)
		writeJSON(w, resp)
		return
	}
	resp["detail"] = "A Terminal window is now arming the pf self-heal daemon. It asks for your password. " +
		"When it finishes, click Refresh here and the guard row should say RUNNING."
	writeJSON(w, resp)
}
