package main

import (
	"strings"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
	"github.com/andrewbakercloudscale/claude-burst/internal/notice"
)

// The commands that change something, which the audit trail records the way
// it records a button on the dashboard. A command with sub-commands lists
// the ones that change something; nil is all of it. Everything else (status,
// stats, the hooks' own calls) only reads, or runs on every prompt.
var cliChanges = map[string][]string{
	"configure":       nil,
	"keychain-set":    nil,
	"enable":          nil,
	"disable":         nil,
	"uninstall-hooks": nil,
	"ca-rotate":       nil,
	"reset":           nil,
	"force-secondary": nil,
	"restore-config":  nil,
	"codex":           {"enable", "disable"},
	"shunt":           {"disable"},
}

// cliAction is the command being run, as the audit will name it, or empty
// for one that is not recorded.
var cliAction string

// auditedCommand turns the arguments into what the audit names: the command,
// its sub-command, and the names of the flags given. Never a flag's value:
// one of them is an API key.
func auditedCommand(args []string) string {
	if len(args) == 0 {
		return ""
	}
	subs, ok := cliChanges[args[0]]
	if !ok {
		return ""
	}
	name := args[0]
	rest := args[1:]
	if subs != nil {
		if len(rest) == 0 || !contains(subs, rest[0]) {
			return ""
		}
		name += " " + rest[0]
		rest = rest[1:]
	}
	for _, a := range rest {
		if !strings.HasPrefix(a, "-") || a == "-" || a == "--" {
			continue
		}
		if i := strings.IndexByte(a, '='); i >= 0 {
			a = a[:i]
		}
		name += " " + a
	}
	return name
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// recordCLI writes how the command ended. Best effort: a command must not
// fail because the audit file could not be written.
func recordCLI(severity, outcome string) {
	if cliAction == "" {
		return
	}
	action := cliAction
	cliAction = "" // once: fatal exits, and a command that returns is recorded by main
	dir, err := config.ConfigDir()
	if err != nil {
		return
	}
	if len(outcome) > 300 {
		outcome = outcome[:300]
	}
	_ = notice.AppendAudit(notice.AuditPath(notice.Path(dir)), "action", severity, "Command line: claude-burst "+action, outcome)
}
