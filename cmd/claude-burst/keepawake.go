package main

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
	"github.com/andrewbakercloudscale/claude-burst/internal/keepawake"
)

// The logic lives in internal/keepawake, shared with the dashboard.
var (
	parseSleepDisabled = keepawake.ParseSleepDisabled
	sleepDisabled      = keepawake.SleepDisabled
	onACPower          = keepawake.OnACPower
	wantSleepDisabled  = keepawake.WantSleepDisabled
)

const (
	lidAwakeModeFile = keepawake.ModeFile
	lidAwakePlist    = keepawake.Plist
)

func ghosttyAppNapDisabled() bool { return keepawake.GhosttyAppNapDisabled() }

func applyKeepAwake(on bool, mode string, idle, minBattery int) {
	// User half first: it cannot fail for lack of sudo.
	if err := keepawake.SetGhosttyAppNap(on); err != nil {
		fmt.Printf("warning: could not change Ghostty App Nap setting: %v\n", err)
	} else {
		fmt.Printf("Ghostty App Nap: %s (takes effect when Ghostty is next launched)\n", map[bool]string{true: "disabled", false: "default"}[on])
	}

	script := scriptPath("lid-awake-root.sh")
	args := []string{"-n", script, "remove"}
	if on {
		args = []string{"-n", script, "apply", mode, strconv.Itoa(idle), keepawake.ActivityPath(), "--min-battery", strconv.Itoa(minBattery)}
		keepawake.MarkNow()
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
	minBatt := cfg.KeepAwakeMinBatteryPercent()
	reapply := "sudo " + script + " apply " + cfg.KeepAwakeLidClosedPower + " " + strconv.Itoa(cfg.KeepAwakeIdleMinutes) + " " + keepawake.ActivityPath() + " --min-battery " + strconv.Itoa(minBatt)
	if cfg.KeepAwakeIdleMinutes > 0 {
		fmt.Printf("  idle window: %d minutes after Claude Code was last used or the lid was last open\n", cfg.KeepAwakeIdleMinutes)
	}
	pct := keepawake.BatteryPercent(string(battOut))
	if minBatt > 0 {
		fmt.Printf("  minimum battery: %d%%, a shut lid sleeps the Mac on battery below it", minBatt)
		if keepawake.LowBattery(onAC, pct, minBatt) {
			fmt.Printf(" (now %d%%: sleep allowed)", pct)
		}
		fmt.Println()
	} else {
		fmt.Println("  minimum battery: off")
	}
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
	case (cfg.KeepAwakeLidClosedPower == config.KeepAwakeOnAC || cfg.KeepAwakeIdleMinutes > 0) && plistErr != nil:
		fmt.Println("  -> the keep-awake daemon is not installed; run: " + reapply)
	case keepawake.Read().AppliedIdle != cfg.KeepAwakeIdleMinutes:
		fmt.Println("  -> the machine is not applying the idle window; run: " + reapply)
	case keepawake.Read().AppliedMinBattery != minBatt:
		fmt.Println("  -> the machine is not applying the minimum battery; run: " + reapply)
	case cfg.KeepAwakeIdleMinutes == 0 && on != keepawake.WantSleepDisabledAt(cfg.KeepAwakeLidClosedPower, onAC, pct, minBatt):
		fmt.Printf("  -> SleepDisabled should be %s on %s; run: %s\n", state(!on), power, reapply)
	case !nap:
		fmt.Println("  -> Ghostty may App Nap; run: claude-burst configure --keep-awake-lid-closed true")
	}
}
