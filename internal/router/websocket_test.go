package router

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A WebSocket upgrade reaches the primary and the two ends are joined:
// Claude Code's /api/frame/sync was answered 502 on every try until 12 Nov
// 2026, because it was sent over HTTP/2, which has no Upgrade.
func TestWebSocketUpgradeIsJoinedToThePrimary(t *testing.T) {
	gotPath := make(chan string, 1)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath <- r.URL.Path + "|" + r.Header.Get("Upgrade") + "|" + r.Header.Get("Authorization")
		conn, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer conn.Close()
		rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		rw.Flush()
		line, _ := rw.ReadString('\n')
		rw.WriteString("echo " + line)
		rw.Flush()
	}))
	defer up.Close()
	s, _ := newTestServer(t, up.URL, "")
	gw := httptest.NewServer(s)
	defer gw.Close()

	c, err := net.Dial("tcp", strings.TrimPrefix(gw.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	io.WriteString(c, "GET /api/frame/sync HTTP/1.1\r\nHost: 127.0.0.1\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nAuthorization: Bearer oauth\r\n\r\n")
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status %d, want 101", resp.StatusCode)
	}
	if got := <-gotPath; got != "/api/frame/sync|websocket|Bearer oauth" {
		t.Errorf("upstream saw %q", got)
	}
	io.WriteString(c, "hello\n")
	line, err := br.ReadString('\n')
	if err != nil || line != "echo hello\n" {
		t.Fatalf("relay: %q %v", line, err)
	}
}

func TestIsWebSocket(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	if isWebSocket(r) {
		t.Fatal("plain GET taken for a WebSocket")
	}
	r.Header.Set("Upgrade", "WebSocket")
	r.Header.Set("Connection", "keep-alive, Upgrade")
	if !isWebSocket(r) {
		t.Fatal("upgrade not seen")
	}
}
