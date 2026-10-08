package main

import (
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"

	"github.com/andrewbakercloudscale/claude-burst/internal/codex"
	"github.com/andrewbakercloudscale/claude-burst/internal/config"
)

// codexStartErr says why there is no Codex gateway at all (a broken
// config), for the dashboard. A port problem is the gateway's own listen
// state (codex.Gateway.ListenState).
var codexStartErr string

// startCodexGateway runs the Codex listener beside the Claude one. It runs
// whether or not Codex is routed here: a Codex session reads its config at
// startup and keeps sending to this port until it exits, so the port has to
// answer for as long as Burst does.
//
// Claiming and binding the port happen in the background: claimPort can wait
// over a minute for another Burst process to let go of it, and Claude Code's
// gateway must never wait on Codex's. A bind failure is logged and shown on
// the dashboard, never fatal.
func startCodexGateway(cfg config.Config, logger *log.Logger) *codex.Gateway {
	addr := cfg.CodexListen()
	if addr == "" {
		return nil
	}
	mp, err := config.CodexMetricsPath()
	if err == nil {
		var g *codex.Gateway
		if g, err = codex.New(cfg.CodexUpstream(), mp, logger); err == nil {
			go serveCodex(g, addr, cfg.CodexUpstream(), logger)
			return g
		}
	}
	codexStartErr = err.Error()
	logger.Printf("codex: %v", err)
	return nil
}

func serveCodex(g *codex.Gateway, addr, upstream string, logger *log.Logger) {
	if launchedByAgent() {
		if err := claimPort(addr, logger); err != nil {
			logger.Printf("codex: port claim: %v", err)
		}
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		g.SetListenState(false, fmt.Sprintf("bind %s: %v", addr, err))
		logger.Printf("codex: not listening, bind %s: %v", addr, err)
		return
	}
	g.SetListenState(true, "")
	logger.Printf("codex: gateway listening on http://%s, forwarding to %s", addr, upstream)
	if err := g.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		g.SetListenState(false, "stopped: "+err.Error())
		logger.Printf("codex: server stopped: %v", err)
	}
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
