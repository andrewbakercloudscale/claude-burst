package integration

// Graceful restart against the real binary: a deploy's SIGTERM must let a
// streaming reply finish instead of cutting it (the "API Error: Connection
// lost mid-response" two deploys caused on 2026-09-28), and must not keep an
// idle gateway waiting.

import (
	"bufio"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
)

type drainRig struct {
	cmd     *exec.Cmd
	addr    string
	exited  chan error
	logPath string
}

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func startGateway(t *testing.T, upstreamURL string) *drainRig {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, ".config", "claude-burst")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.AnthropicBaseURL = upstreamURL
	cfg.AdminListen = "" // never collide with a gateway running on this machine
	addr := freePort(t)
	cfg.Listen = addr
	b, _ := json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), b, 0600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(burstBinary(t), "serve")
	cmd.Env = append(os.Environ(), "HOME="+home)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	r := &drainRig{cmd: cmd, addr: addr, exited: make(chan error, 1), logPath: filepath.Join(dir, "claude-burst.log")}
	go func() { r.exited <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if resp, err := http.Get("http://" + addr + "/healthz"); err == nil {
			resp.Body.Close()
			return r
		}
	}
	t.Fatal("gateway never came up")
	return nil
}

func (r *drainRig) log(t *testing.T) string {
	t.Helper()
	b, _ := os.ReadFile(r.logPath)
	return string(b)
}

func TestSIGTERMLetsAStreamingReplyFinish(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"type\":\"ping\"}\n\n")
		w.(http.Flusher).Flush()
		<-release
		_, _ = io.WriteString(w, "data: {\"type\":\"message_stop\"}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()
	defer close(release)
	g := startGateway(t, upstream.URL)

	resp, err := http.Post("http://"+g.addr+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-sonnet-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)
	if line, err := br.ReadString('\n'); err != nil || !strings.Contains(line, "ping") {
		t.Fatalf("first chunk: %q %v", line, err)
	}

	// The reply is mid-stream. This is the moment a deploy used to kill it.
	if err := g.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	select {
	case err := <-g.exited:
		t.Fatalf("gateway exited with a reply still streaming: %v\nlog:\n%s", err, g.log(t))
	default:
	}
	// Still serving while it drains: a request arriving now is answered, not refused.
	if h, err := http.Get("http://" + g.addr + "/healthz"); err != nil {
		t.Fatalf("a draining gateway must keep serving: %v", err)
	} else {
		h.Body.Close()
	}

	release <- struct{}{}
	rest, err := io.ReadAll(br)
	if err != nil || !strings.Contains(string(rest), "[DONE]") {
		t.Fatalf("the streaming reply was not completed: err=%v body=%q", err, rest)
	}
	select {
	case err := <-g.exited:
		if err != nil {
			t.Fatalf("drained exit should be clean: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("gateway did not exit once the reply finished")
	}
	if !strings.Contains(g.log(t), "drained in") {
		t.Fatalf("log must say it drained:\n%s", g.log(t))
	}
}

func TestSIGTERMOnAnIdleGatewayExitsPromptly(t *testing.T) {
	upstream := httptest.NewServer(http.NotFoundHandler())
	defer upstream.Close()
	g := startGateway(t, upstream.URL)

	start := time.Now()
	if err := g.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-g.exited:
		if err != nil {
			t.Fatalf("exit: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("an idle gateway must not wait out the drain deadline")
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("idle exit took %s", d)
	}
}
