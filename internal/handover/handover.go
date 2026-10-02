// Package handover installs and configures two Claude Code hooks that keep a
// repo's HANDOFF.md alive between sessions:
//
//   - SessionStart (start.sh) puts the latest handover section, the commits
//     since it was written and the working tree in front of Claude, so a new
//     session starts from where the last one stopped.
//   - SessionEnd (end.sh) fires when a session closes, including when its
//     Ghostty window is closed, and starts write.sh detached: it resumes a
//     fork of the finished session, has it update HANDOFF.md, and commits that
//     one file locally. It never pushes.
//
// Both are opt-in per repo: they do nothing unless the git root has a
// HANDOFF.md. This has nothing to do with routing; it lives in claude-burst
// because the dashboard is where the user can see and adapt it.
//
// The scripts are embedded and written to ~/.config/claude-burst/handover/.
// They read settings.json in that directory, which holds the EFFECTIVE
// settings (defaults filled in), so the scripts carry no defaults of their own
// and the default text lives in exactly one place, here.
package handover

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/andrewbakercloudscale/claude-burst/internal/atomicfile"
	"github.com/andrewbakercloudscale/claude-burst/internal/claudesettings"
	"github.com/andrewbakercloudscale/claude-burst/internal/config"
)

//go:embed scripts/*.sh
var scripts embed.FS

var scriptNames = []string{"start.sh", "end.sh", "write.sh"}

const (
	DefaultMinPrompts = 2
	DefaultModel      = "opus"
	MaxMinPrompts     = 50
	maxTextBytes      = 16 * 1024
	hookTimeout       = 10
)

// DefaultBriefing is what Claude is told at startup, above the handover
// itself. {{file}} is replaced with the HANDOFF.md path.
const DefaultBriefing = `HANDOVER BRIEFING (injected at startup from {{file}})

Before answering the first request: read the latest handover below, then skim the code it names (files, packages, scripts in 'State' and 'Open') so you know the current shape of the codebase. Treat the handover as claims to verify, not facts. Older handover sections are further down {{file}}; read them only when relevant.`

// DefaultInstructions is the prompt the writer is given at session close.
// {{file}}, {{now}} (local time) and {{session}} are replaced.
const DefaultInstructions = `This session has just been closed. Update {{file}} so the next session can start cold.

Rules:
- Prepend a new section at the very top, headed exactly: # Handover, {{now}}: <short topic>
  On the next line put: <!-- session: {{session}} -->
  If the top section already carries that same session marker, rewrite that section instead of adding one.
- Keep every older section below it byte for byte.
- Sections, as short bullets, in this order: State right now (start with 'Written at session close. Verify before acting.'; what is installed/deployed/pushed vs local only; uncommitted work), What was done (with commit hashes), Open (next steps in priority order, each with how to check it), Things that will bite you.
- Only facts from this conversation or checked now with read-only git commands. Mark anything you could not check as unverified.
- Local time only, never UTC. Never use em or en dashes.
- If nothing worth handing over happened (no changes, no findings, no decisions), leave the file untouched and reply NOTHING.
- Edit only {{file}}. Do not commit, push, deploy, or run anything except read-only git commands.`

// Config is what the user can change. Zero values mean "the default", so an
// empty or missing file is the recommended setup and a changed default text
// reaches everyone who never edited theirs.
type Config struct {
	NoBrief      bool   `json:"no_brief,omitempty"`
	NoWrite      bool   `json:"no_write,omitempty"`
	NoCommit     bool   `json:"no_commit,omitempty"`
	MinPrompts   int    `json:"min_prompts,omitempty"`
	Model        string `json:"model,omitempty"`
	Briefing     string `json:"briefing,omitempty"`
	Instructions string `json:"instructions,omitempty"`
}

// effective is what the scripts read: every field resolved.
type effective struct {
	Brief        bool   `json:"brief"`
	Write        bool   `json:"write"`
	Commit       bool   `json:"commit"`
	MinPrompts   int    `json:"min_prompts"`
	Model        string `json:"model"`
	Briefing     string `json:"briefing"`
	Instructions string `json:"instructions"`
}

func (c Config) effective() effective {
	e := effective{Brief: !c.NoBrief, Write: !c.NoWrite, Commit: !c.NoCommit,
		MinPrompts: c.MinPrompts, Model: c.Model, Briefing: c.Briefing, Instructions: c.Instructions}
	if e.MinPrompts == 0 {
		e.MinPrompts = DefaultMinPrompts
	}
	if e.Model == "" {
		e.Model = DefaultModel
	}
	if strings.TrimSpace(e.Briefing) == "" {
		e.Briefing = DefaultBriefing
	}
	if strings.TrimSpace(e.Instructions) == "" {
		e.Instructions = DefaultInstructions
	}
	return e
}

// Normalize stores a text or model identical to the default as "" so it keeps
// following the default, and checks the bounds.
func (c Config) Normalize() (Config, error) {
	c.Model = strings.TrimSpace(c.Model)
	if c.Model == DefaultModel {
		c.Model = ""
	}
	if strings.TrimSpace(c.Briefing) == strings.TrimSpace(DefaultBriefing) {
		c.Briefing = ""
	}
	if strings.TrimSpace(c.Instructions) == strings.TrimSpace(DefaultInstructions) {
		c.Instructions = ""
	}
	if c.MinPrompts == DefaultMinPrompts {
		c.MinPrompts = 0
	}
	switch {
	case c.MinPrompts < 0 || c.MinPrompts > MaxMinPrompts:
		return c, fmt.Errorf("minimum prompts must be between 1 and %d", MaxMinPrompts)
	case strings.ContainsAny(c.Model, " \t\n'\"$`\\;&|<>"):
		return c, errors.New("model must be a single model name or alias, such as opus or claude-sonnet-5")
	case len(c.Briefing) > maxTextBytes || len(c.Instructions) > maxTextBytes:
		return c, fmt.Errorf("texts are limited to %d KB each", maxTextBytes/1024)
	}
	return c, nil
}

// Dir is where the scripts, their settings and the log live.
func Dir() (string, error) {
	d, err := config.ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "handover"), nil
}

func configPath() (string, error) {
	d, err := config.ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "handover.json"), nil
}

func LogPath() (string, error) {
	d, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "handover.log"), nil
}

// Load returns the saved config, or the zero (all defaults) config when there
// is none.
func Load() (Config, error) {
	var c Config
	p, err := configPath()
	if err != nil {
		return c, err
	}
	b, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("%s does not parse: %w", p, err)
	}
	return c, nil
}

// Save stores the config and, when the hooks are installed, rewrites the
// effective settings the scripts read, so a change applies to the next
// session with nothing to restart.
func Save(c Config) error {
	c, err := c.Normalize()
	if err != nil {
		return err
	}
	if err := config.EnsureDir(); err != nil {
		return err
	}
	p, err := configPath()
	if err != nil {
		return err
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	if err := atomicfile.Write(p, append(b, '\n'), 0600); err != nil {
		return err
	}
	return writeFiles(c)
}

// writeFiles writes the scripts and the effective settings. Content that is
// already right is left alone, so calling it on every status read is cheap.
func writeFiles(c Config) error {
	dir, err := Dir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	for _, n := range scriptNames {
		b, err := scripts.ReadFile("scripts/" + n)
		if err != nil {
			return err
		}
		if err := writeIfChanged(filepath.Join(dir, n), b, 0755); err != nil {
			return err
		}
	}
	b, _ := json.MarshalIndent(c.effective(), "", "  ")
	return writeIfChanged(filepath.Join(dir, "settings.json"), append(b, '\n'), 0600)
}

func writeIfChanged(p string, b []byte, mode os.FileMode) error {
	if old, err := os.ReadFile(p); err == nil && bytes.Equal(old, b) {
		return nil
	}
	if err := atomicfile.Write(p, b, mode); err != nil {
		return err
	}
	return os.Chmod(p, mode) // atomicfile.Write keeps an existing file's mode
}

// isStart and isEnd match our hook commands, plus the hand-installed
// ~/.claude/hooks/handover-*.sh these replaced, so installing takes over
// from them instead of running both.
func isStart(cmd string) bool {
	return strings.HasSuffix(cmd, "/handover/start.sh") || strings.HasSuffix(cmd, "/.claude/hooks/handover-start.sh")
}
func isEnd(cmd string) bool {
	return strings.HasSuffix(cmd, "/handover/end.sh") || strings.HasSuffix(cmd, "/.claude/hooks/handover-end.sh")
}

// Install writes the scripts and adds both hooks to ~/.claude/settings.json.
func Install() error {
	c, err := Load()
	if err != nil {
		return err
	}
	if err := writeFiles(c); err != nil {
		return err
	}
	dir, err := Dir()
	if err != nil {
		return err
	}
	return editSettings(func(root map[string]any) bool {
		// Remove first so a hand-installed legacy entry is replaced rather
		// than updated in place under its old group.
		changed := claudesettings.RemoveCommandHooks(root, "SessionStart", isLegacy) > 0
		changed = claudesettings.RemoveCommandHooks(root, "SessionEnd", isLegacy) > 0 || changed
		changed = claudesettings.AddCommandHook(root, "SessionStart", "", filepath.Join(dir, "start.sh"), hookTimeout, isStart) || changed
		changed = claudesettings.AddCommandHook(root, "SessionEnd", "", filepath.Join(dir, "end.sh"), hookTimeout, isEnd) || changed
		return changed
	})
}

func isLegacy(cmd string) bool { return strings.Contains(cmd, "/.claude/hooks/handover-") }

func ours(match func(string) bool) func(string) bool {
	return func(cmd string) bool { return match(cmd) && !isLegacy(cmd) }
}

// Uninstall removes both hooks. The scripts, settings and log stay, so
// reinstalling restores the same setup.
func Uninstall() error {
	return editSettings(func(root map[string]any) bool {
		n := claudesettings.RemoveCommandHooks(root, "SessionStart", isStart)
		n += claudesettings.RemoveCommandHooks(root, "SessionEnd", isEnd)
		return n > 0
	})
}

func editSettings(edit func(map[string]any) bool) error {
	p, err := claudesettings.Path()
	if err != nil {
		return err
	}
	root, err := claudesettings.Read(p)
	if err != nil {
		return err
	}
	if !edit(root) {
		return nil
	}
	return claudesettings.Write(p, root)
}

// Status is what the dashboard shows.
type Status struct {
	// Installed means both hooks are in settings.json.
	Installed bool `json:"installed"`
	StartHook bool `json:"start_hook"`
	EndHook   bool `json:"end_hook"`
	// Legacy means the hand-installed ~/.claude/hooks/handover-*.sh pair is
	// still in settings.json. It works, but ignores the settings on this page.
	Legacy bool   `json:"legacy"`
	Config Config `json:"config"`
	// Effective is Config with the defaults filled in, for the form.
	Effective effective `json:"effective"`
	Defaults  effective `json:"defaults"`
	Dir       string    `json:"dir"`
	// Log is the newest lines of handover.log, newest last.
	Log []string `json:"log"`
	// Error is a config or settings.json problem worth showing.
	Error string `json:"error,omitempty"`
}

const logLines = 25

// GetStatus reports the hooks and settings. When the hooks are installed it
// also rewrites any script or settings file that differs from what this
// binary would write, so a deploy that changes a script reaches the machine
// without a reinstall.
func GetStatus() Status {
	st := Status{Defaults: Config{}.effective()}
	st.Dir, _ = Dir()
	c, loadErr := Load()
	if loadErr != nil {
		st.Error = loadErr.Error()
	}
	st.Config, st.Effective = c, c.effective()

	if p, err := claudesettings.Path(); err == nil {
		if root, err := claudesettings.Read(p); err == nil {
			// Only our own scripts count: the legacy pair does not read these
			// settings, so calling it installed would make every edit on the
			// page look applied when it is not.
			st.StartHook = claudesettings.HasCommandHook(root, "SessionStart", ours(isStart))
			st.EndHook = claudesettings.HasCommandHook(root, "SessionEnd", ours(isEnd))
			st.Legacy = claudesettings.HasCommandHook(root, "SessionStart", isLegacy) ||
				claudesettings.HasCommandHook(root, "SessionEnd", isLegacy)
		} else if st.Error == "" {
			st.Error = err.Error()
		}
	}
	st.Installed = st.StartHook && st.EndHook
	// Never rewrite from a config that did not parse: that would replace the
	// user's settings with the defaults.
	if st.Installed && loadErr == nil {
		if werr := writeFiles(c); werr != nil {
			st.Error = "refreshing the hook scripts: " + werr.Error()
		}
	}
	if lp, err := LogPath(); err == nil {
		st.Log = tail(lp, logLines)
	}
	return st
}

func tail(p string, n int) []string {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	if len(lines) == 1 && lines[0] == "" {
		return nil
	}
	return lines
}
