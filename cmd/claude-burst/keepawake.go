package main

import (
	"fmt"
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

func applyKeepAwake(on bool) {
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

	action := "remove"
	if on {
		action = "apply"
	}
	script := scriptPath("lid-awake-root.sh")
	// sudo -n: use cached credentials if there are any, never prompt from here.
	out, err := exec.Command("sudo", "-n", script, action).CombinedOutput()
	if err == nil {
		fmt.Print(string(out))
		return
	}
	fmt.Printf("config saved, but the machine-wide half needs root. Run:\n  sudo %s %s\n", script, action)
	if on {
		fmt.Println("note: the Mac will then not sleep at all, lid shut or not -- battery and heat, especially in a bag.")
	}
}

func reportKeepAwake(cfg config.Config) {
	on, known := sleepDisabled()
	nap := ghosttyAppNapDisabled()
	state := func(b bool) string {
		if b {
			return "on"
		}
		return "off"
	}
	fmt.Printf("keep awake lid closed: %s (SleepDisabled %s, Ghostty App Nap disabled %s)\n",
		state(cfg.KeepAwakeLidClosed), map[bool]string{true: state(on), false: "unknown"}[known], state(nap))
	if !known {
		return
	}
	if cfg.KeepAwakeLidClosed && !on {
		fmt.Println("  -> configured ON but the lid will still sleep the Mac; run: sudo " +
			scriptPath("lid-awake-root.sh") + " apply")
	} else if cfg.KeepAwakeLidClosed && !nap {
		fmt.Println("  -> configured ON but Ghostty may App Nap; run: claude-burst configure --keep-awake-lid-closed true")
	} else if !cfg.KeepAwakeLidClosed && on {
		fmt.Println("  -> SleepDisabled is set although this is off; the Mac will not sleep. Undo: sudo " +
			scriptPath("lid-awake-root.sh") + " remove")
	}
}
