package main

import (
	"io"
	"log"
	"net"
	"strings"
	"testing"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
)

// A Codex port held by something else costs Codex its gateway, never Claude
// Code's: startCodexGateway returns nil and says why for the dashboard.
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
	codexStartErr = ""
	if g := startCodexGateway(cfg, log.New(io.Discard, "", 0)); g != nil {
		t.Fatal("started on a taken port")
	}
	if !strings.Contains(codexStartErr, "bind "+ln.Addr().String()) {
		t.Errorf("codexStartErr = %q", codexStartErr)
	}
}

// "off" means no listener and no error.
func TestCodexListenOff(t *testing.T) {
	cfg := config.Default()
	cfg.Codex.Listen = "off"
	codexStartErr = ""
	if g := startCodexGateway(cfg, log.New(io.Discard, "", 0)); g != nil || codexStartErr != "" {
		t.Fatalf("g=%v err=%q", g, codexStartErr)
	}
}
