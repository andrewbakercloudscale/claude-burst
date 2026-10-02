package admin

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/andrewbakercloudscale/claude-burst/internal/claudesettings"
	"github.com/andrewbakercloudscale/claude-burst/internal/config"
	"github.com/andrewbakercloudscale/claude-burst/internal/coord"
	"github.com/andrewbakercloudscale/claude-burst/internal/handover"
)

// Uninstalling. Every feature that puts something in ~/.claude has its own
// remover; this runs all of them, for `claude-burst uninstall-hooks`, which
// install.sh calls while the binary still exists. Left behind, each hook runs
// a deleted binary or script on every session, prompt or tool call.
//
// config.json is not touched. Switching each feature off would also remove
// its hooks, but a later reinstall should come back with the user's choices
// intact, so the removers are given a copy with everything off instead.

// RemoveAllHooks removes the coordination hooks, the handover hooks, the prompt notice hooks and /compact-async. It
// carries on past a failure so one broken remover cannot strand the others,
// and returns every error it met. The hook and skill of token shunting
// (removed 2026-10-02) are taken out by the caller, `uninstall-hooks`, which
// owns that legacy cleanup.
func RemoveAllHooks() error {
	off := config.Default()
	off.PrimaryCompaction.Enabled = false
	var errs []error
	if err := coord.SyncHooks(false, ""); err != nil {
		errs = append(errs, fmt.Errorf("coordination hooks: %w", err))
	}
	if err := handover.Uninstall(); err != nil {
		errs = append(errs, fmt.Errorf("handover hooks: %w", err))
	}
	// Also removes /compact-async, through SyncCompactCommand.
	if err := SyncPromptNoticeHook(off); err != nil {
		errs = append(errs, fmt.Errorf("prompt notice hooks and /compact-async: %w", err))
	}
	return errors.Join(errs...)
}

// HookLeftovers lists what still names Claude Burst after RemoveAllHooks:
// each line of settings.json that mentions claude-burst, and the skill and
// command files if they are still there. Empty means clean. It reads the
// file as text rather than walking the hooks, so an entry no remover knows
// about (a renamed binary, a hand-added hook) is reported, not missed.
func HookLeftovers() ([]string, error) {
	var left []string
	p, err := claudesettings.Path()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	for i, line := range strings.Split(string(b), "\n") {
		if strings.Contains(line, "claude-burst") {
			left = append(left, fmt.Sprintf("%s:%d: %s", p, i+1, strings.TrimSpace(line)))
		}
	}
	if h, err := os.UserHomeDir(); err == nil {
		sp := filepath.Join(h, ".claude", "skills", "claude-burst-shunt", "SKILL.md")
		if _, err := os.Stat(sp); err == nil {
			left = append(left, sp)
		}
	}
	if cp, err := compactCommandPath(); err == nil {
		if old, err := os.ReadFile(cp); err == nil && strings.Contains(string(old), "claude-burst") {
			left = append(left, cp)
		}
	}
	return left, nil
}
