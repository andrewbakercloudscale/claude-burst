package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/admin"
	"github.com/andrewbakercloudscale/claude-burst/internal/config"
	"github.com/andrewbakercloudscale/claude-burst/internal/coord"
)

const coordUsage = `claude-burst coord: session coordination between Claude Code sessions on this Mac

  coord status                                   who is running, which files are shared, who masters them
  coord send <session> <message> [--from <id>]   message a session (id prefix); it sees it at its next tool call or prompt
  coord take <path> --session <id>               become a file's master: now if its master is idle or ended, else ask it
  coord release <path> [--session <id>]          hand a file on, as if its master had ended

Hooks (run by Claude Code, not typed): session-start, pre-tool, post-tool, prompt, stop, subagent-stop, session-end.
Turn it on or off in the dashboard, under Session coordination.
`

// newCoordinator builds the coordinator from config.json. The state lives
// in ~/.config/claude-burst/coord, or $CLAUDE_BURST_COORD_DIR.
func newCoordinator(cfg config.Config) (*coord.Coordinator, error) {
	return admin.Coordinator(cfg)
}

var coordHooks = map[string]bool{"session-start": true, "pre-tool": true, "post-tool": true, "prompt": true, "stop": true, "subagent-stop": true, "session-end": true}

func coordCmd(args []string) {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Print(coordUsage)
		return
	}
	cfg, cfgErr := config.Load()
	if coordHooks[args[0]] {
		// A hook fails open: whatever goes wrong, exit 0 with no output, so
		// the session carries on as if coordination were off. It is also a
		// no-op while switched off, so a hook left behind does nothing.
		// CLAUDE_BURST_COORD_FORCE runs it regardless of config.json, for
		// scripts/coord-live-test.sh, which installs its hooks with --settings.
		on := cfgErr == nil && cfg.SessionCoordination.Enabled || os.Getenv("CLAUDE_BURST_COORD_FORCE") != ""
		if !on || os.Getenv("CLAUDE_BURST_COORD_DISABLE") != "" {
			return
		}
		c, err := newCoordinator(cfg)
		if err != nil {
			return
		}
		defer func() {
			if r := recover(); r != nil {
				c.Log("PANIC in %s: %v", args[0], r)
			}
		}()
		if err := c.Hook(args[0], os.Stdin, os.Stdout); err != nil {
			c.Log("ERROR in %s: %v", args[0], err)
		}
		return
	}
	if cfgErr != nil {
		fmt.Fprintln(os.Stderr, "config.json:", cfgErr)
		os.Exit(1)
	}
	c, err := newCoordinator(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	flag := func(name string) (string, []string) {
		rest := args[1:]
		for i, a := range rest {
			if a == name && i+1 < len(rest) {
				return rest[i+1], append(append([]string{}, rest[:i]...), rest[i+2:]...)
			}
		}
		return "", rest
	}
	switch args[0] {
	case "status":
		if !cfg.SessionCoordination.Enabled && os.Getenv("CLAUDE_BURST_COORD_FORCE") == "" {
			fmt.Println("Session coordination is OFF (turn it on in the dashboard, Session coordination).")
		}
		st, err := c.Status()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println("SESSIONS")
		if len(st.Sessions) == 0 {
			fmt.Println("  none active")
		}
		for _, s := range st.Sessions {
			fmt.Printf("  %-12s %-8s %s, active %s ago\n", s.Label, s.ID[:min(8, len(s.ID))], s.Cwd, (time.Duration(s.ActiveAgoS) * time.Second).String())
		}
		fmt.Println("FILES BEING EDITED")
		if len(st.Files) == 0 {
			fmt.Println("  none")
		}
		for _, f := range st.Files {
			also := ""
			if len(f.Contributors) > 0 {
				also = "; also changed by " + strings.Join(f.Contributors, ", ")
			}
			idle := ""
			if f.TakeOver {
				idle = fmt.Sprintf(", idle %s: another session's edit takes it over", (time.Duration(f.IdleS) * time.Second).String())
			}
			if f.Wanted != "" {
				also += "; asked for by " + f.Wanted
			}
			fmt.Printf("  %s\n      master %s for %s%s%s\n", f.Path, f.MasterLabel, (time.Duration(f.SinceS) * time.Second).String(), idle, also)
		}
	case "send":
		from, rest := flag("--from")
		if len(rest) < 2 {
			fmt.Fprint(os.Stderr, coordUsage)
			os.Exit(2)
		}
		to, err := c.Send(rest[0], from, strings.Join(rest[1:], " "))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Printf("queued for %s; it sees it at its next tool call or prompt\n", to[:min(8, len(to))])
	case "take":
		sess, rest := flag("--session")
		if len(rest) != 1 || sess == "" {
			fmt.Fprint(os.Stderr, coordUsage)
			os.Exit(2)
		}
		res, err := c.Take(rest[0], sess)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(res)
	case "release":
		sess, rest := flag("--session")
		if len(rest) != 1 {
			fmt.Fprint(os.Stderr, coordUsage)
			os.Exit(2)
		}
		if err := c.Release(rest[0], sess); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println("released")
	default:
		fmt.Fprint(os.Stderr, coordUsage)
		os.Exit(2)
	}
}
