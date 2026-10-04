package main

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
)

// The pass-through waits for the gateway to give up the port, then forwards
// every request unchanged (path, query, headers, streamed body) and exits on
// its own once unused.
func TestPassthroughTakesOverThePortAndForwards(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".config", "claude-burst"), 0o700); err != nil {
		t.Fatal(err)
	}
	passthroughTick = 50 * time.Millisecond
	t.Cleanup(func() { passthroughTick = 15 * time.Second })

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host == "" || strings.HasPrefix(r.Host, "127.0.0.1:"+portOf(gatewayAddrForTest)) {
			t.Errorf("Host header not rewritten to the target: %q", r.Host)
		}
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, r.Method+" "+r.URL.RequestURI()+" auth="+r.Header.Get("Authorization")+" body="+string(b))
	}))
	defer upstream.Close()

	// The "gateway" still holds the port when the pass-through starts.
	gw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gatewayAddrForTest = gw.Addr().String()

	done := make(chan error, 1)
	go func() { done <- runPassthrough(gatewayAddrForTest, upstream.URL, 400*time.Millisecond) }()
	time.Sleep(300 * time.Millisecond)
	gw.Close() // the gateway exits

	var resp *http.Response
	for i := 0; i < 40; i++ {
		req, _ := http.NewRequest("POST", "http://"+gatewayAddrForTest+"/v1/messages?beta=true", strings.NewReader(`{"m":1}`))
		req.Header.Set("Authorization", "Bearer tok")
		if resp, err = http.DefaultClient.Do(req); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("pass-through never took the port: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if got, want := string(b), `POST /v1/messages?beta=true auth=Bearer tok body={"m":1}`; got != want {
		t.Fatalf("forwarded as %q, want %q", got, want)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("exit after idle: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pass-through did not exit once unused")
	}
	if _, err := os.Stat(filepath.Join(home, ".config", "claude-burst", "passthrough.pid")); !os.IsNotExist(err) {
		t.Fatalf("pid file left behind: %v", err)
	}
}

var gatewayAddrForTest = "127.0.0.1:0"

// A temporary HOME (every test, install.sh's included) must not reach the
// real Mac: no detached pass-through, no launchctl on the live gateway agent.
// On 2026-10-04 one test run did both and took this Mac's gateway down.
func TestTempHomeLeavesTheMacAlone(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if homeIsThisUsers() {
		t.Fatal("a temporary HOME passed for this account's home")
	}
	var cfg config.Config
	cfg.Listen = "127.0.0.1:1"
	started, why, err := startPassthrough(cfg, cfg.Listen, "https://example.invalid", time.Minute)
	if started || err != nil || why != notThisMac {
		t.Fatalf("startPassthrough under a temp HOME: started=%v why=%q err=%v", started, why, err)
	}
}
