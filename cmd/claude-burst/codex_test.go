package main

import (
	"io"
	"log"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
)

// A Codex port held by something else costs Codex its gateway, never Claude
// Code's: startCodexGateway returns at once, and the bind failure is the
// gateway's listen state for the dashboard.
func TestCodexPortTakenIsNotFatal(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XPC_SERVICE_NAME", "") // never claim (stop) a real port holder
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	cfg := config.Default()
	cfg.Codex.Listen = ln.Addr().String()
	g := startCodexGateway(cfg, log.New(io.Discard, "", 0))
	if g == nil {
		t.Fatal("no gateway")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		bound, why := g.ListenState()
		if why != "" || time.Now().After(deadline) {
			if bound || !strings.Contains(why, "bind "+ln.Addr().String()) {
				t.Fatalf("bound=%v why=%q", bound, why)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// "off" means no listener and no error.
func TestCodexListenOff(t *testing.T) {
	cfg := config.Default()
	cfg.Codex.Listen = "off"
	if g := startCodexGateway(cfg, log.New(io.Discard, "", 0)); g != nil {
		t.Fatalf("g=%v err=%q", g, codexStartErr)
	}
}
