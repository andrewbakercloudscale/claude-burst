// Package keepawake reads and changes keep_awake_lid_closed, shared by the
// CLI (claude-burst configure / status) and the dashboard.
//
// The setting has two halves with different privileges:
//
//   - pmset SleepDisabled, machine-wide, root. The only switch that overrides
//     clamshell sleep; `caffeinate` and `pmset sleep 0` do not. Delegated to
//     scripts/lid-awake-root.sh, which records and restores the prior value
//     and, in mode "ac", installs a root daemon that follows the power source.
//   - Ghostty's App Nap, per-user, no root. With the lid shut every window is
//     occluded, which is exactly when macOS naps an app and throttles the
//     Claude Code process running under it.
//
// Remote Control needs nothing of its own beyond these: it is Claude Code's
// long-poll, and it survives as long as the process runs and the network stays
// up, which it does while the machine is awake.
package keepawake

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
)

const GhosttyDomain = "com.mitchellh.ghostty"

// Written by lid-awake-root.sh, root-owned but world-readable.
const (
	ModeFile = "/etc/claude-burst/lid-awake.mode"
	IdleFile = "/etc/claude-burst/lid-awake.idle"
	Plist    = "/Library/LaunchDaemons/ninja.andrewbaker.claude-burst-lidawake.plist"
)

// ActivityPath is the file the gateway touches on every real Claude turn.
// The root daemon reads its mtime (never its contents) for the idle window,
// and accepts it only at this path.
func ActivityPath() string {
	dir, err := config.ConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "last-activity")
}

var (
	touchMu   sync.Mutex
	lastTouch time.Time
	// Off under go test, so router tests never touch the real file.
	touchOff = testing.Testing()
)

// Touch marks Claude Code as in use now. At most once every 30 seconds: the
// daemon checks once a minute, so finer is wasted writes.
func Touch() {
	touchMu.Lock()
	defer touchMu.Unlock()
	if touchOff || time.Since(lastTouch) < 30*time.Second {
		return
	}
	lastTouch = time.Now()
	p := ActivityPath()
	if p == "" {
		return
	}
	if err := os.Chtimes(p, lastTouch, lastTouch); os.IsNotExist(err) {
		_ = os.WriteFile(p, nil, 0o644)
	}
}

// MarkNow records activity now, even under go test's guard, unthrottled.
// For applying the setting: without a file the daemon would count the Mac
// as idle from the start. Test callers point HOME at a temporary dir.
func MarkNow() {
	p := ActivityPath()
	if p == "" {
		return
	}
	now := time.Now()
	if err := os.Chtimes(p, now, now); os.IsNotExist(err) {
		_ = os.WriteFile(p, nil, 0o644)
	}
}

// LastActivity is when Claude Code was last used, or zero.
func LastActivity() time.Time {
	fi, err := os.Stat(ActivityPath())
	if err != nil {
		return time.Time{}
	}
	return fi.ModTime()
}

// ParseSleepDisabled reads SleepDisabled out of `pmset -g` output.
func ParseSleepDisabled(out string) (on, known bool) {
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[0] == "SleepDisabled" {
			return f[1] == "1", f[1] == "0" || f[1] == "1"
		}
	}
	return false, false
}

func SleepDisabled() (on, known bool) {
	out, err := exec.Command("pmset", "-g").Output()
	if err != nil {
		return false, false
	}
	return ParseSleepDisabled(string(out))
}

func GhosttyAppNapDisabled() bool {
	out, err := exec.Command("defaults", "read", GhosttyDomain, "NSAppSleepDisabled").Output()
	return err == nil && strings.TrimSpace(string(out)) == "1"
}

// SetGhosttyAppNap disables Ghostty's App Nap (on) or restores the default.
func SetGhosttyAppNap(disabled bool) error {
	if disabled {
		return exec.Command("defaults", "write", GhosttyDomain, "NSAppSleepDisabled", "-bool", "YES").Run()
	}
	if GhosttyAppNapDisabled() {
		return exec.Command("defaults", "delete", GhosttyDomain, "NSAppSleepDisabled").Run()
	}
	return nil
}

// OnACPower reads the power source from `pmset -g batt`, whose first line is
// "Now drawing from 'AC Power'" or "... 'Battery Power'".
func OnACPower(battOut string) bool {
	first, _, _ := strings.Cut(battOut, "\n")
	return !strings.Contains(first, "'Battery Power'")
}

// WantSleepDisabled is what SleepDisabled should read right now for a power
// mode: always 1 in "always", and in "ac" only while plugged in.
func WantSleepDisabled(mode string, onAC bool) bool {
	return mode == config.KeepAwakeAlways || onAC
}

// Blocker is another process holding the Mac awake. Only PreventSystemSleep
// counts: the idle assertions every `caffeinate -i` takes (Claude Code holds
// several) do not stop lid-close sleep. On 2026-09-29 the Mac slept with the
// lid shut while those were held; on 2026-09-30 it stayed awake on battery
// with SleepDisabled 0, and the only new assertion was a `caffeinate -dimsu`
// holding PreventSystemSleep.
type Blocker struct {
	PID     string `json:"pid"`
	Process string `json:"process"`
	For     string `json:"for,omitempty"` // "caffeinate asserting on behalf of ..."
}

var assertionLine = regexp.MustCompile(`^\s*pid (\d+)\(([^)]*)\): \[[^\]]*\] \S+ PreventSystemSleep\b`)

// ParseBlockers reads PreventSystemSleep holders from `pmset -g assertions`.
func ParseBlockers(out string) []Blocker {
	var bs []Blocker
	lines := strings.Split(out, "\n")
	for i, l := range lines {
		m := assertionLine.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		b := Blocker{PID: m[1], Process: m[2]}
		if i+1 < len(lines) {
			if d, ok := strings.CutPrefix(strings.TrimSpace(lines[i+1]), "Details: "); ok {
				b.For = d
			}
		}
		bs = append(bs, b)
	}
	return bs
}

// Status is the live state, for `claude-burst status` and the dashboard.
type Status struct {
	SleepDisabled      bool      `json:"sleep_disabled"`
	SleepDisabledKnown bool      `json:"sleep_disabled_known"`
	OnAC               bool      `json:"on_ac"`
	Battery            string    `json:"battery,omitempty"` // "85%; charging"
	AppliedMode        string    `json:"applied_mode"`      // what the root half is doing: "", ac, always
	AppliedIdle        int       `json:"applied_idle"`      // idle window the root half applies, minutes; 0 none
	LastActivity       time.Time `json:"last_activity,omitempty"`
	DaemonInstalled    bool      `json:"daemon_installed"`
	GhosttyNapOff      bool      `json:"ghostty_nap_off"`
	Blockers           []Blocker `json:"blockers"`
}

var batteryLine = regexp.MustCompile(`\t(\d+%; [a-zA-Z ]+);`)

func Read() Status {
	var st Status
	st.SleepDisabled, st.SleepDisabledKnown = SleepDisabled()
	batt, _ := exec.Command("pmset", "-g", "batt").Output()
	st.OnAC = OnACPower(string(batt))
	if m := batteryLine.FindStringSubmatch(string(batt)); m != nil {
		st.Battery = m[1]
	}
	mode, _ := os.ReadFile(ModeFile)
	st.AppliedMode = strings.TrimSpace(string(mode))
	idle, _ := os.ReadFile(IdleFile)
	st.AppliedIdle, _ = strconv.Atoi(strings.TrimSpace(string(idle)))
	st.LastActivity = LastActivity()
	_, err := os.Stat(Plist)
	st.DaemonInstalled = err == nil
	st.GhosttyNapOff = GhosttyAppNapDisabled()
	if out, err := exec.Command("pmset", "-g", "assertions").Output(); err == nil {
		st.Blockers = ParseBlockers(string(out))
	}
	return st
}

// Problem says what is wrong with the live state for a configured setting,
// or "" when the machine is doing what the config asks.
func (st Status) Problem(on bool, mode string, idle int) string {
	if !st.SleepDisabledKnown {
		return "could not read SleepDisabled from pmset"
	}
	if !on {
		if st.AppliedMode != "" {
			return "off in config, but the machine-wide half is still applied"
		}
		return ""
	}
	switch {
	case st.AppliedMode != mode:
		return "the machine is not applying mode " + mode
	case st.AppliedIdle != idle:
		return "the machine is not applying the idle window"
	case (mode == config.KeepAwakeOnAC || idle > 0) && !st.DaemonInstalled:
		return "the keep-awake daemon is not installed"
	case idle > 0:
		// The daemon follows the clock too; SleepDisabled legitimately
		// reads 0 once the window has passed.
	case st.SleepDisabled != WantSleepDisabled(mode, st.OnAC):
		return "SleepDisabled does not match the power source"
	case !st.GhosttyNapOff:
		return "Ghostty may App Nap"
	}
	return ""
}
