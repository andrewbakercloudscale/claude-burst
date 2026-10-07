package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/andrewbakercloudscale/claude-burst/internal/notice"
	"github.com/andrewbakercloudscale/claude-burst/internal/touchid"
)

const version = "0.20.15"

func main() {
	if len(os.Args) < 2 {
		serve(os.Args[1:])
		return
	}
	cliAction = auditedCommand(os.Args[1:])
	switch os.Args[1] {
	case "serve":
		serve(os.Args[2:])
	case "configure":
		configure(os.Args[2:])
	case "codex":
		codexCmd(os.Args[2:])
	case "console":
		consoleCmd(os.Args[2:])
	case "restore-config":
		restoreConfig(os.Args[2:])
	case "open":
		openCmd(os.Args[2:])
	case "app":
		appCmd(os.Args[2:])
	case touchid.HelperCommand:
		// Run by the gateway, never by hand: the authentication prompt in
		// a process of its own, so a fault in it cannot end the gateway.
		os.Exit(touchid.RunHelper(os.Args[2:]))
	case "keychain-set":
		keychainSet(os.Args[2:])
	case "enable":
		enable(os.Args[2:])
	case "disable":
		disable(os.Args[2:])
	case "passthrough":
		passthroughCmd(os.Args[2:])
	case "notice":
		noticeCmd(os.Args[2:])
	case "uninstall-hooks":
		uninstallHooks(os.Args[2:])
	case "ca-rotate":
		caRotate(os.Args[2:])
	case "status":
		status()
	case "reset":
		reset()
	case "force-secondary":
		forceSecondary(os.Args[2:])
	case "stats":
		stats(os.Args[2:])
	case "shunt":
		shuntCmd(os.Args[2:])
	case "coord":
		coordCmd(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Println(version)
	case "help", "--help", "-h":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	recordCLI(notice.Info, "done")
}

func usage() {
	fmt.Print(`claude-burst - route Claude Code through a primary provider, with a configurable secondary provider for overflow

Commands:
  serve             Run the local gateway (default: 127.0.0.1:7777)
  configure         Write/update config.json
  keychain-set      Store a secondary's API key in macOS Keychain (--provider together|openrouter|bedrock)
  enable            Point Claude Code at the local gateway via ~/.claude/settings.json
  disable           Remove Claude Burst from Claude Code settings
  ca-rotate         Replace the local intercept CA (only with no Claude Code session running)
  uninstall-hooks   Remove every hook, skill and command Burst put in ~/.claude (install.sh uninstall runs it)
  status            Show routing state
  reset             Clear overflow state immediately (back to primary)
  force-secondary   Route inference to the secondary for a while (testing)
  stats             Summarize local routing/token metrics
  shunt disable     Remove the hook and skill of token shunting (removed), if an earlier install left them
  coord             Session coordination: status, send, release (see: coord help)
  codex             Codex through Burst: enable, disable, status (gateway on 127.0.0.1:7779)
  restore-config    Put back the newest backup of config.json that loads, when the one in place does not
  console           support console: audit, log, restart and repair, up when the gateway is not (127.0.0.1:7789)
  open              Open the dashboard in the browser, or the support console when the gateway is down
  app               The Claude Burst app in ~/Applications (Spotlight, Dock) that does the same: install, remove
  version           Print version

Admin UI:
  A local control panel runs alongside the gateway on 127.0.0.1:7788 by
  default -- recent requests, response headers that drive failover, config
  changes, and a one-click revert to stock Claude. It binds loopback and has
  no login; disable it with: claude-burst configure --admin-listen off

Keeping Claude Code's Remote Control (optional):
  Claude Code disables Remote Control whenever ANTHROPIC_BASE_URL names a host
  other than api.anthropic.com, so the default "base-url" mode costs you that
  feature. "transparent" mode instead redirects at the DNS layer and terminates
  TLS locally, leaving the variable unset. It needs root once, for an
  /etc/hosts entry and a pf rule:
    claude-burst configure --intercept-mode transparent
    claude-burst enable          # prints the one sudo step that remains
  Undo with: sudo scripts/transparent-root.sh remove

Keeping Claude Code working with the lid shut (optional, default off):
  claude-burst configure --keep-awake-lid-closed true [--keep-awake-power ac|always]
  Sets pmset SleepDisabled (one sudo step) and turns off App Nap for Ghostty,
  so the session and Remote Control survive closing the lid. Power mode "ac"
  (the default) does this only while plugged in -- on battery the lid sleeps
  the Mac as normal; "always" also keeps it awake on battery. Undo with false.

Setup with a Claude Max/Pro subscription (default), Together AI overflow:
  claude-burst configure --secondary openai-compatible \
    --secondary-base-url https://api.together.xyz/v1 --secondary-model zai-org/GLM-5.3
  TOGETHER_API_KEY='...' claude-burst keychain-set --provider together
  claude-burst enable
  claude-burst serve

Setup with no subscription (metered Anthropic API key primary), Together AI overflow:
  claude-burst configure --primary anthropic-api-key --secondary openai-compatible \
    --secondary-base-url https://api.together.xyz/v1 --secondary-model zai-org/GLM-5.3
  TOGETHER_API_KEY='...' claude-burst keychain-set --provider together
  claude-burst enable
  # Then set ANTHROPIC_API_KEY in Claude Code's own settings env -- the
  # gateway never stores or injects an Anthropic credential itself, it only
  # forwards whatever auth header Claude Code already sent.
  claude-burst serve

Amazon Bedrock is also supported as an overflow secondary:
  export AWS_BEARER_TOKEN_BEDROCK='...' && claude-burst keychain-set
  claude-burst configure --secondary bedrock --region us-east-1
`)
}

// rootHelperPath locates transparent-root.sh for the instructions we print.
//
// The binary is installed to ~/.local/bin but the script lives in the repo, so
// deriving the path from the executable alone printed a command that does not
// exist -- an instruction the user cannot run is worse than no instruction,
// because they trust it and then have to work out why it failed.
func rootHelperPath() string { return scriptPath("transparent-root.sh") }

// scriptPath locates one of the repo's scripts; see rootHelperPath.
func scriptPath(name string) string {
	var candidates []string
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), name))
	}
	if wd, err := os.Getwd(); err == nil {
		candidates = append(candidates, filepath.Join(wd, "scripts", name))
	}
	recorded := ""
	if home, err := os.UserHomeDir(); err == nil {
		// Where install.sh and deploy.sh wrote the checkout's path and
		// copied the scripts the way out needs: the checkout can be
		// anywhere, and until 7 Oct 2026 only one place was looked in, so
		// the console's Repair and Diagnose did nothing on any other Mac.
		share := filepath.Join(home, ".local", "share", "claude-burst")
		if b, err := os.ReadFile(filepath.Join(share, "repo")); err == nil {
			if repo := strings.TrimSpace(string(b)); filepath.IsAbs(repo) {
				recorded = filepath.Join(repo, "scripts", name)
				candidates = append(candidates, recorded)
			}
		}
		candidates = append(candidates, filepath.Join(share, name),
			filepath.Join(home, "Desktop", "github", "claude-burst", "scripts", name))
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c
		}
	}
	// macOS can hide a checkout under ~/Desktop or ~/Documents from a
	// background process, so a path the installer recorded is used unseen:
	// the Terminal window that runs it can read it.
	if recorded != "" {
		return recorded
	}
	// Nothing found: name the file rather than a path that would not work.
	return "scripts/" + name + " (in the claude-burst repo)"
}

// portOf returns the port from a host:port listen address.
func portOf(listen string) string {
	if _, port, ok := strings.Cut(listen, ":"); ok {
		return port
	}
	return listen
}

// rejectArgs fails on stray arguments. These subcommands take none, and
// parsing nothing meant `claude-burst enable --help` silently EXECUTED enable
// instead of printing help.
func rejectArgs(cmd string, args []string) {
	if len(args) > 0 {
		fatal(fmt.Errorf("%s takes no arguments, got %q", cmd, strings.Join(args, " ")))
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	recordCLI(notice.Warn, "failed: "+err.Error())
	os.Exit(1)
}
