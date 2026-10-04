package main

import (
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/codex"
	"github.com/andrewbakercloudscale/claude-burst/internal/config"
)

// codexGateway is the running Codex listener, for the dashboard; nil when
// it is off or failed to start.
var codexGateway *codex.Gateway

// codexStartErr says why the Codex listener is not running, for the
// dashboard: a dead port Codex is routed to must say why, not just "off".
var codexStartErr string

// startCodexGateway runs the Codex listener beside the Claude one. It runs
// whether or not Codex is routed here: a Codex session reads its config at
// startup and keeps sending to this port until it exits, so the port has to
// answer for as long as Burst does. A bind failure is logged, never fatal:
// Claude Code's gateway must not go down over Codex.
func startCodexGateway(cfg config.Config, logger *log.Logger) *codex.Gateway {
	addr := cfg.CodexListen()
	if addr == "" {
		return nil
	}
	mp, err := config.CodexMetricsPath()
	if err != nil {
		codexStartErr = err.Error()
		logger.Printf("codex: %v", err)
		return nil
	}
	g, err := codex.New(cfg.CodexUpstream(), mp, logger)
	if err != nil {
		codexStartErr = err.Error()
		logger.Printf("codex: %v", err)
		return nil
	}
	if launchedByAgent() {
		if err := claimPort(addr, logger); err != nil {
			logger.Printf("codex: port claim: %v", err)
		}
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		codexStartErr = fmt.Sprintf("bind %s: %v", addr, err)
		logger.Printf("codex: not listening, %s", codexStartErr)
		return nil
	}
	logger.Printf("codex: gateway listening on http://%s, forwarding to %s", addr, cfg.CodexUpstream())
	fmt.Printf("codex:  http://%s -> %s\n", addr, cfg.CodexUpstream())
	codexGateway = g
	go func() {
		srv := &http.Server{Handler: g, ReadHeaderTimeout: 30 * time.Second}
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Printf("codex: server stopped: %v", err)
		}
	}()
	return g
}

func codexBackupDir() string {
	d, err := config.ConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(d, "backups")
}

// codexCmd: claude-burst codex enable | disable | status.
func codexCmd(args []string) {
	cfg, err := config.Load()
	if err != nil {
		fatal(err)
	}
	path, err := codex.ConfigPath()
	if err != nil {
		fatal(err)
	}
	sub := "status"
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "enable":
		addr := cfg.CodexListen()
		if addr == "" {
			fatal(errors.New(`the Codex gateway is off (codex.listen is "off" in config.json)`))
		}
		if err := codex.Enable(path, addr, codexBackupDir()); err != nil {
			fatal(err)
		}
		fmt.Printf("Codex now goes through Claude Burst (%s).\nRestart Codex (the app, and any codex in a terminal): each session reads %s when it starts.\n", addr, path)
	case "disable":
		if err := codex.Disable(path, codexBackupDir()); err != nil {
			fatal(err)
		}
		fmt.Printf("Codex goes straight to ChatGPT again for sessions started from now on.\nSessions already open keep using Burst's port until restarted; the gateway keeps answering it while Burst runs.\n")
	case "status":
		st := codex.ReadStatus(path)
		switch {
		case !st.Installed:
			fmt.Println("Codex: not installed on this Mac (no", filepath.Dir(path), "folder)")
		case st.Enabled:
			fmt.Println("Codex: through Claude Burst,", st.BaseURL)
		case st.Conflict != "":
			fmt.Printf("Codex: straight to its own provider %q (set in %s)\n", st.Conflict, path)
		default:
			fmt.Println("Codex: straight to ChatGPT (not through Burst)")
		}
	default:
		fmt.Fprintln(os.Stderr, "usage: claude-burst codex [enable|disable|status]")
		os.Exit(2)
	}
}
