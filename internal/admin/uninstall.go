package admin

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/andrewbakercloudscale/claude-burst/internal/claudesettings"
	"github.com/andrewbakercloudscale/claude-burst/internal/config"
	"github.com/andrewbakercloudscale/claude-burst/internal/coord"
	"github.com/andrewbakercloudscale/claude-burst/internal/handover"
	"github.com/andrewbakercloudscale/claude-burst/internal/shunt"
)

// Uninstalling. Every feature that puts something in ~/.claude has its own
// remover; this runs all of them, for `claude-burst uninstall-hooks`, which
// install.sh calls while the binary still exists. Left behind, each hook runs
// a deleted binary or script on every session, prompt or tool call.
//
// config.json is not touched. Switching each feature off would also remove
// its hooks, but a later reinstall should come back with the user's choices
// intact, so the removers are given a copy with everything off instead.

// RemoveAllHooks removes the shunt guard hook and skill, the coordination
// hooks, the handover hooks, the prompt notice hooks and /compact-async. It
// carries on past a failure so one broken remover cannot strand the others,
// and returns every error it met.
func RemoveAllHooks() error {
	off := config.Default()
	off.Shunt = config.ShuntConfig{}
	off.PrimaryCompaction.Enabled = false
	var errs []error
	if err := shunt.Apply(off, shunt.SelfPath()); err != nil {
		errs = append(errs, fmt.Errorf("shunt hook and skill: %w", err))
	}
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
	if sp, err := shunt.SkillPath(); err == nil {
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
