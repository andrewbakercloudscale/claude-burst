package shunt

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/andrewbakercloudscale/claude-burst/internal/claudesettings"
	"github.com/andrewbakercloudscale/claude-burst/internal/config"
)

// SelfPath is the absolute, symlink-resolved path of the running binary, which
// is what the hook and the skill must name: Claude Code's PATH is not ours.
func SelfPath() string {
	p, err := os.Executable()
	if err != nil {
		return "claude-burst"
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	return p
}

// Apply makes ~/.claude/settings.json and the skill agree with cfg: the guard
// hook is present exactly when bulk read is on -- write has nothing to guard,
// and a hook that runs on every Read and Bash call to do nothing is pure cost
// -- and the skill describes exactly what is on. It is the single implementation behind both
// `shunt enable/disable` and the dashboard, so the two cannot disagree.
func Apply(cfg config.Config, bin string) error {
	p, err := claudesettings.Path()
	if err != nil {
		return err
	}
	root, err := claudesettings.Read(p)
	if err != nil {
		return err
	}
	var changed bool
	if cfg.Shunt.Read {
		changed = InstallHook(root, bin)
	} else {
		changed = UninstallHook(root)
	}
	if changed {
		if err := claudesettings.Write(p, root); err != nil {
			return err
		}
	}
	if err := WriteSkill(cfg.Shunt, bin); err != nil {
		return fmt.Errorf("skill: %w", err)
	}
	return nil
}
