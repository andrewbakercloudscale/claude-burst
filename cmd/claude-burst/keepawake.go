package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
)

// keep_awake_lid_closed has two halves with different privileges:
//
//   - pmset SleepDisabled, machine-wide, root. The only switch that overrides
//     clamshell sleep; `caffeinate` and `pmset sleep 0` do not. Delegated to
//     scripts/lid-awake-root.sh, which records and restores the prior value.
//   - Ghostty's App Nap, per-user, no root. With the lid shut every window is
//     occluded, which is exactly when macOS naps an app and throttles the
//     Claude Code process running under it.
//
// Remote Control needs nothing of its own beyond these: it is Claude Code's
// long-poll, and it survives as long as the process runs and the network stays
// up, which it does while the machine is awake.

const ghosttyDomain = "com.mitchellh.ghostty"

// parseSleepDisabled reads SleepDisabled out of `pmset -g` output.
func parseSleepDisabled(out string) (on, known bool) {
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[0] == "SleepDisabled" {
			return f[1] == "1", f[1] == "0" || f[1] == "1"
		}
	}
	return false, false
}

func sleepDisabled() (on, known bool) {
	out, err := exec.Command("pmset", "-g").Output()
	if err != nil {
		return false, false
	}
	return parseSleepDisabled(string(out))
}

func ghosttyAppNapDisabled() bool {
	out, err := exec.Command("defaults", "read", ghosttyDomain, "NSAppSleepDisabled").Output()
	return err == nil && strings.TrimSpace(string(out)) == "1"
}

// Written by lid-awake-root.sh, root-owned but world-readable.
const (
	lidAwakeModeFile = "/etc/claude-burst/lid-awake.mode"
	lidAwakePlist    = "/Library/LaunchDaemons/ninja.andrewbaker.claude-burst-lidawake.plist"
)

// onACPower reads the power source from `pmset -g batt`, whose first line is
// "Now drawing from 'AC Power'" or "... 'Battery Power'".
func onACPower(battOut string) bool {
	first, _, _ := strings.Cut(battOut, "\n")
	return !strings.Contains(first, "'Battery Power'")
}

// wantSleepDisabled is what SleepDisabled should read right now for a power
// mode: always 1 in "always", and in "ac" only while plugged in.
func wantSleepDisabled(mode string, onAC bool) bool {
	return mode == config.KeepAwakeAlways || onAC
}

func applyKeepAwake(on bool, mode string) {
	// User half first: it cannot fail for lack of sudo.
	var err error
	if on {
		err = exec.Command("defaults", "write", ghosttyDomain, "NSAppSleepDisabled", "-bool", "YES").Run()
	} else if ghosttyAppNapDisabled() {
		err = exec.Command("defaults", "delete", ghosttyDomain, "NSAppSleepDisabled").Run()
	}
	if err != nil {
		fmt.Printf("warning: could not change Ghostty App Nap setting: %v\n", err)
	} else {
		fmt.Printf("Ghostty App Nap: %s (takes effect when Ghostty is next launched)\n", map[bool]string{true: "disabled", false: "default"}[on])
	}

	script := scriptPath("lid-awake-root.sh")
	args := []string{"-n", script, "remove"}
	if on {
		args = []string{"-n", script, "apply", mode}
	}
	// sudo -n: use cached credentials if there are any, never prompt from here.
	out, err := exec.Command("sudo", args...).CombinedOutput()
	if err == nil {
		fmt.Print(string(out))
		return
	}
	fmt.Printf("config saved, but the machine-wide half needs root. Run:\n  sudo %s\n", strings.Join(args[1:], " "))
	if on && mode == config.KeepAwakeAlways {
		fmt.Println("note: power mode \"always\" keeps the Mac awake on battery too -- heat and a flat battery in a bag.")
	}
}

func reportKeepAwake(cfg config.Config) {
	on, known := sleepDisabled()
	nap := ghosttyAppNapDisabled()
	battOut, _ := exec.Command("pmset", "-g", "batt").Output()
	onAC := onACPower(string(battOut))
	state := func(b bool) string {
		if b {
			return "on"
		}
		return "off"
	}
	power := map[bool]string{true: "AC", false: "battery"}[onAC]
	fmt.Printf("keep awake lid closed: %s, mode %s (now on %s: SleepDisabled %s, Ghostty App Nap disabled %s)\n",
		state(cfg.KeepAwakeLidClosed), cfg.KeepAwakeLidClosedPower, power,
		map[bool]string{true: state(on), false: "unknown"}[known], state(nap))
	if !known {
		return
	}
	script := scriptPath("lid-awake-root.sh")
	reapply := "sudo " + script + " apply " + cfg.KeepAwakeLidClosedPower
	if !cfg.KeepAwakeLidClosed {
		if on {
			fmt.Println("  -> SleepDisabled is set although this is off; the Mac will not sleep. Undo: sudo " + script + " remove")
		}
		return
	}
	appliedMode, _ := os.ReadFile(lidAwakeModeFile)
	_, plistErr := os.Stat(lidAwakePlist)
	switch {
	case strings.TrimSpace(string(appliedMode)) != cfg.KeepAwakeLidClosedPower:
		fmt.Printf("  -> the machine is not applying mode %s; run: %s\n", cfg.KeepAwakeLidClosedPower, reapply)
	case cfg.KeepAwakeLidClosedPower == config.KeepAwakeOnAC && plistErr != nil:
		fmt.Println("  -> mode ac but the power-source daemon is not installed; run: " + reapply)
	case on != wantSleepDisabled(cfg.KeepAwakeLidClosedPower, onAC):
		fmt.Printf("  -> SleepDisabled should be %s on %s; run: %s\n", state(!on), power, reapply)
	case !nap:
		fmt.Println("  -> Ghostty may App Nap; run: claude-burst configure --keep-awake-lid-closed true")
	}
}
