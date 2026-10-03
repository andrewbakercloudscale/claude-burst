package router

import (
	"bytes"
	"errors"
	"log"
	"strings"
	"testing"
)

// An unchanged network is logged in full once, then in short form; a
// change is logged in full again.
func TestNetworkSnapshotRepeatsShort(t *testing.T) {
	var buf bytes.Buffer
	s := &Server{logger: log.New(&buf, "", 0)}
	up := netProbe{ifaces: []string{"192.168.0.9"}, dnsOK: true}
	s.logSnapshot("r1", "anthropic", errors.New("timeout 1"), up)
	s.logSnapshot("r2", "anthropic", errors.New("timeout 2"), up)
	s.logSnapshot("r3", "anthropic", errors.New("timeout 3"), netProbe{ifaces: []string{"172.20.10.2"}, dnsOK: true})
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("want 3 lines:\n%s", buf.String())
	}
	if !strings.Contains(lines[0], "local_ifaces=192.168.0.9") {
		t.Errorf("first is full: %s", lines[0])
	}
	if strings.Contains(lines[1], "local_ifaces") || !strings.Contains(lines[1], "timeout 2") || !strings.Contains(lines[1], "network unchanged since") {
		t.Errorf("repeat is short and keeps its error: %s", lines[1])
	}
	if !strings.Contains(lines[2], "local_ifaces=172.20.10.2") {
		t.Errorf("a changed network is full again: %s", lines[2])
	}
}
