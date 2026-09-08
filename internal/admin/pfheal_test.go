package admin

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
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

// TestPFHealStatusStates drives every state the guard row renders from, using
// fixtures rather than whatever happens to be installed on this Mac.
func TestPFHealStatusStates(t *testing.T) {
	dir := t.TempDir()
	plist := filepath.Join(dir, "daemon.plist")
	beat := filepath.Join(dir, "heartbeat")
	logf := filepath.Join(dir, "pf.log")

	oldPlist, oldBeat, oldLog := pfHealPlist, pfHealHeartbeat, pfHealLog
	pfHealPlist, pfHealHeartbeat, pfHealLog = plist, beat, logf
	t.Cleanup(func() { pfHealPlist, pfHealHeartbeat, pfHealLog = oldPlist, oldBeat, oldLog })

	writeBeat := func(age time.Duration) {
		ts := strconv.FormatInt(time.Now().Add(-age).Unix(), 10)
		if err := os.WriteFile(beat, []byte(ts+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// 1. Nothing installed. -1 is the sentinel the UI reads as "never
	// reported in"; a zero here would render as "last checked 0s ago".
	got := pfHealStatus("")
	if got.Installed || got.Running || got.LastCheckSeconds != -1 {
		t.Errorf("clean machine: want not installed / not running / -1, got %+v", got)
	}
	if got.LogPath == "" {
		t.Error("LogPath must always be set so the UI can name the file")
	}

	// 2. Armed and beating.
	if err := os.WriteFile(plist, []byte("<plist/>"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeBeat(30 * time.Second)
	got = pfHealStatus("/repo/scripts")
	if !got.Installed || !got.Running {
		t.Errorf("fresh heartbeat: want installed+running, got %+v", got)
	}
	if got.LastCheckSeconds < 25 || got.LastCheckSeconds > 40 {
		t.Errorf("age should be about 30s, got %d", got.LastCheckSeconds)
	}
	if got.InstallCmd == "" {
		t.Error("InstallCmd should be offered so the UI can print a copyable command")
	}

	// 3. Installed but dead -- the state that must not read as healthy. A
	// stale heartbeat is the ONLY evidence available for this: launchd will
	// not answer an unprivileged caller at all.
	writeBeat(pfHealStaleAfter + time.Minute)
	got = pfHealStatus("")
	if !got.Installed {
		t.Error("plist exists, so Installed should stay true")
	}
	if got.Running {
		t.Errorf("heartbeat older than %v must not read as running (age %ds)", pfHealStaleAfter, got.LastCheckSeconds)
	}

	// 4. A heartbeat from the future (clock change) is not proof of life.
	writeBeat(-time.Hour)
	if pfHealStatus("").Running {
		t.Error("a heartbeat an hour in the future must not read as running")
	}

	// 5. Garbage in the heartbeat must not panic or read as alive.
	if err := os.WriteFile(beat, []byte("not-a-timestamp"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := pfHealStatus(""); got.Running || got.LastCheckSeconds != -1 {
		t.Errorf("unparseable heartbeat: want not running / -1, got %+v", got)
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
