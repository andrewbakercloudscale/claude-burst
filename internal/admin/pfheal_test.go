package admin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPFHealEventsPicksConsequentialLines: the log carries a preamble and the
// indented stdout of every command the daemon ran, and the dashboard shows
// only a handful of lines. Showing the wrong five would make a repaired
// outage look like an ongoing one, or hide one entirely.
func TestPFHealEventsPicksConsequentialLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pf.log")
	body := strings.Join([]string{
		"2026-09-07 22:20:00 BROKEN: /etc/hosts still redirects but the pf rdr rule is NOT loaded",
		"    == rewriting anchor (port 443 -> 7777) ==",
		"    wrote /etc/pf.anchors/claude-burst",
		"    ruleset OK",
		"2026-09-07 22:20:03 HEALED: rdr rule reloaded successfully",
	}, "\n")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got := pfHealEvents(path, 5)
	if len(got) != 2 {
		t.Fatalf("expected the 2 event lines, got %d: %#v", len(got), got)
	}
	if !strings.Contains(got[0], "BROKEN") || !strings.Contains(got[1], "HEALED") {
		t.Errorf("wrong lines chosen: %#v", got)
	}
}

// TestPFHealEventsKeepsTheNewest: an old outage that healed must not be what
// the dashboard shows while a new one is in progress.
func TestPFHealEventsKeepsTheNewest(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pf.log")
	var lines []string
	for i := 0; i < 20; i++ {
		lines = append(lines, "HEALED: old event")
	}
	lines = append(lines, "BAILED OUT: newest event")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	got := pfHealEvents(path, 3)
	if len(got) != 3 {
		t.Fatalf("want 3 lines, got %d", len(got))
	}
	if !strings.Contains(got[len(got)-1], "BAILED OUT") {
		t.Errorf("newest event was dropped: %#v", got)
	}
}

// TestPFHealStatusWithoutDaemon: on a machine where none of this is installed
// the dashboard must say "not armed", not crash and not claim it is running.
// -1 is the sentinel the UI uses to mean "never reported in", so it has to
// survive rather than read as an age of zero seconds.
func TestPFHealStatusWithoutDaemon(t *testing.T) {
	info := pfHealStatus("")
	if info.Running {
		t.Error("reported Running with no heartbeat on this machine")
	}
	if info.LastCheckSeconds >= 0 && !info.Running {
		// A stale heartbeat is legitimate (a daemon that died); only an
		// absent one must be -1.
		if _, err := os.Stat(pfHealHeartbeat); os.IsNotExist(err) {
			t.Errorf("no heartbeat file exists, so age should be -1, got %d", info.LastCheckSeconds)
		}
	}
	if info.LogPath == "" {
		t.Error("LogPath should always be reported so the UI can name the file")
	}
}

// TestPFHealInstallScriptDelegates: the generated script must call the repo's
// installer, not carry its own copy of the plist and the file placement --
// two copies of a privileged install is how one of them ends up wrong.
func TestPFHealInstallScriptDelegates(t *testing.T) {
	script := pfHealInstallScript("/path with space/scripts")
	if !strings.Contains(script, "install-pf-heal.sh") {
		t.Errorf("script does not run the installer:\n%s", script)
	}
	if strings.Contains(script, "LaunchDaemons") || strings.Contains(script, "launchctl bootstrap") {
		t.Errorf("script reimplements the install instead of delegating:\n%s", script)
	}
	if !strings.Contains(script, `'/path with space/scripts'`) {
		t.Errorf("scripts directory was not shell-quoted:\n%s", script)
	}
	if !strings.Contains(script, "Press any key to close this window") {
		t.Errorf("script does not hold the Terminal window open:\n%s", script)
	}
}
