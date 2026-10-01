package coord

import (
	"regexp"

	"github.com/andrewbakercloudscale/claude-burst/internal/claudesettings"
)

// hookEvents is every hook coordination installs: event, matcher, subcommand.
var hookEvents = []struct{ event, matcher, sub string }{
	{"SessionStart", "", "session-start"},
	{"PreToolUse", "Edit|Write|MultiEdit|NotebookEdit|Bash", "pre-tool"},
	{"PostToolUse", "Edit|Write|MultiEdit|NotebookEdit|Bash", "post-tool"},
	{"UserPromptSubmit", "", "prompt"},
	{"Stop", "", "stop"},
	{"SubagentStop", "", "subagent-stop"},
	{"SessionEnd", "", "session-end"},
}

const hookTimeout = 10

// ours matches a coordination hook command, whatever the binary's path or
// name was when it was installed.
var ours = regexp.MustCompile(`" coord (session-start|pre-tool|pre-edit|post-tool|prompt|stop|subagent-stop|session-end)$`)

// IsOurs recognises a coordination hook command.
func IsOurs(cmd string) bool { return ours.MatchString(cmd) }

// SyncHooks installs the hooks in ~/.claude/settings.json when on is true
// and removes them when false, touching only its own entries. exe is the
// claude-burst binary the hooks run.
func SyncHooks(on bool, exe string) error {
	p, err := claudesettings.Path()
	if err != nil {
		return err
	}
	root, err := claudesettings.Read(p)
	if err != nil {
		return err
	}
	changed := false
	for _, h := range hookEvents {
		if on {
			cmd := `"` + exe + `" coord ` + h.sub
			changed = claudesettings.AddCommandHook(root, h.event, h.matcher, cmd, hookTimeout, IsOurs) || changed
		} else {
			changed = claudesettings.RemoveCommandHooks(root, h.event, IsOurs) > 0 || changed
		}
	}
	if !changed {
		return nil
	}
	return claudesettings.Write(p, root)
}

// Installed reports whether every coordination hook is in settings.json.
func Installed() bool {
	p, err := claudesettings.Path()
	if err != nil {
		return false
	}
	root, err := claudesettings.Read(p)
	if err != nil {
		return false
	}
	for _, h := range hookEvents {
		if !claudesettings.HasCommandHook(root, h.event, IsOurs) {
			return false
		}
	}
	return true
}
