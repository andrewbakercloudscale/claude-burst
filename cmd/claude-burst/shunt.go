package main

// Token shunting was removed on 2026-10-02 (DECISION-token-shunting-off.md).
// What is left is the way out for a machine that enabled it before then: a
// PreToolUse hook in ~/.claude/settings.json that runs `claude-burst shunt
// guard` on every Read and Bash call, and a skill telling Claude to run
// `claude-burst shunt read`. Both name this binary, so both must keep working
// until they are removed:
//
//   - `shunt guard` allows everything. An unknown command exits 2, and exit 2
//     from a PreToolUse hook BLOCKS the tool call, so dropping the guard
//     outright would have stopped every Read and Bash in any session still
//     carrying the hook.
//   - `shunt disable`, `disable` and `install.sh uninstall` remove the hook
//     and the skill when they are present.

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/andrewbakercloudscale/claude-burst/internal/claudesettings"
)

const (
	legacyShuntHookEvent  = "PreToolUse"
	legacyShuntHookSuffix = " shunt guard"
	legacyShuntSkillName  = "claude-burst-shunt"
)

// isLegacyShuntHook recognises the guard hook by shape rather than by path,
// so a binary that has moved since it was installed is still found.
func isLegacyShuntHook(cmd string) bool {
	return strings.HasSuffix(cmd, legacyShuntHookSuffix) && strings.Contains(cmd, "claude-burst")
}

func shuntCmd(args []string) {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "guard":
		// Drain the payload so Claude Code never sees a broken pipe, then allow.
		_, _ = io.Copy(io.Discard, os.Stdin)
	case "disable":
		removed, err := removeLegacyShunt()
		if err != nil {
			fatal(err)
		}
		if removed {
			fmt.Println("removed the token-shunting hook and skill. Restart Claude Code.")
		} else {
			fmt.Println("token shunting is not installed; nothing to remove")
		}
	default:
		fmt.Fprintln(os.Stderr, "token shunting has been removed (DECISION-token-shunting-off.md).")
		fmt.Fprintln(os.Stderr, "claude-burst shunt disable removes a hook or skill left by an earlier install.")
		os.Exit(2)
	}
}

// removeLegacyShunt deletes the guard hook and the skill if either is present,
// and reports whether anything was removed. It touches nothing else in
// settings.json, and writes it only when the hook was there.
func removeLegacyShunt() (bool, error) {
	removed := false
	p, err := claudesettings.Path()
	if err != nil {
		return false, err
	}
	if _, err := os.Stat(p); err == nil {
		root, err := claudesettings.Read(p)
		if err != nil {
			return false, err
		}
		if claudesettings.RemoveCommandHooks(root, legacyShuntHookEvent, isLegacyShuntHook) > 0 {
			if err := claudesettings.Write(p, root); err != nil {
				return false, err
			}
			removed = true
		}
	} else if !os.IsNotExist(err) {
		return false, err
	}

	h, err := os.UserHomeDir()
	if err != nil {
		return removed, err
	}
	dir := filepath.Join(h, ".claude", "skills", legacyShuntSkillName)
	switch err := os.Remove(filepath.Join(dir, "SKILL.md")); {
	case err == nil:
		removed = true
	case !os.IsNotExist(err):
		return removed, fmt.Errorf("skill: %w", err)
	}
	_ = os.Remove(dir) // only succeeds when empty, so nothing of the user's goes with it
	return removed, nil
}

// legacyShuntHookInstalled reports whether settings.json still carries the
// guard hook, so `status` can say how to remove it.
func legacyShuntHookInstalled() bool {
	p, err := claudesettings.Path()
	if err != nil {
		return false
	}
	root, err := claudesettings.Read(p)
	if err != nil {
		return false
	}
	return claudesettings.HasCommandHook(root, legacyShuntHookEvent, isLegacyShuntHook)
}
