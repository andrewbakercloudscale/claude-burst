package codex

import (
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
)

// hostCodexSends is the Host a Codex session puts on its requests for a
// gateway listening on listen: the authority of the base_url Block writes
// into config.toml, which is the only place Codex learns the address from.
func hostCodexSends(t *testing.T, listen string) string {
	t.Helper()
	var doc struct {
		Providers map[string]struct {
			BaseURL string `toml:"base_url"`
		} `toml:"model_providers"`
	}
	if _, err := toml.Decode(Block(listen), &doc); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(doc.Providers[ProviderID].BaseURL)
	if err != nil || u.Host == "" {
		t.Fatalf("base_url for %q: %q (%v)", listen, doc.Providers[ProviderID].BaseURL, err)
	}
	return u.Host
}

// The guard must never refuse Codex: whatever codex.listen is, the Host
// Codex is configured to send is accepted, with the headers Codex sends.
func TestTheGuardAcceptsWhatCodexSends(t *testing.T) {
	for _, listen := range []string{"127.0.0.1:7779", "localhost:7779", "[::1]:7779", "0.0.0.0:7779", "192.168.1.20:7779", "burst.internal:7779"} {
		var reached int
		g, _, _ := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached++ }))
		g.AcceptHost(listen) // what serve tells it
		r := httptest.NewRequest("GET", "/backend-api/codex/models", nil)
		r.Host = hostCodexSends(t, listen)
		r.Header.Set("Originator", "codex_cli_rs")
		r.Header.Set("Authorization", "Bearer chatgpt-token")
		w := httptest.NewRecorder()
		g.ServeHTTP(w, r)
		if w.Code != http.StatusOK || reached != 1 {
			t.Errorf("listen %s, Host %s: status %d, reached ChatGPT %d times", listen, r.Host, w.Code, reached)
		}
		if rf := g.Refused(); rf.Count != 0 {
			t.Errorf("listen %s: counted as refused: %+v", listen, rf)
		}
	}
}

// A request a web page made is answered 403 here and goes nowhere: not to
// ChatGPT, not into "Codex last sent".
func TestTheGuardRefusesAWebPage(t *testing.T) {
	for _, c := range []struct {
		name, host, header, value, want string
	}{
		{"a cross-origin fetch", "127.0.0.1:7779", "Origin", "https://evil.example", "Origin"},
		{"a sandboxed frame", "127.0.0.1:7779", "Origin", "null", "Origin"},
		{"a page's plain GET", "127.0.0.1:7779", "Sec-Fetch-Site", "cross-site", "Sec-Fetch-Site"},
		{"another port of this Mac", "localhost:7779", "Sec-Fetch-Site", "same-site", "Sec-Fetch-Site"},
		{"DNS rebinding", "evil.example:7779", "", "", "Host"},
		{"a name nobody configured", "burst.internal:7779", "", "", "Host"},
	} {
		var reached atomic.Int64
		out := &lockedBuf{}
		g, _, _ := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached.Add(1) }))
		g.logger.SetOutput(out)
		r := httptest.NewRequest("POST", "/backend-api/codex/responses", strings.NewReader(`{"model":"x"}`))
		r.Host = c.host
		if c.header != "" {
			r.Header.Set(c.header, c.value)
		}
		w := httptest.NewRecorder()
		g.ServeHTTP(w, r)
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), c.want) {
			t.Errorf("%s: status %d body %q", c.name, w.Code, w.Body.String())
		}
		if reached.Load() != 0 {
			t.Errorf("%s: reached ChatGPT", c.name)
		}
		if !g.LastRequest().IsZero() {
			t.Errorf("%s: counted as Codex having sent something", c.name)
		}
		fs := g.Failures()
		if rf := g.Refused(); rf.Count != 1 || len(fs) != 1 || fs[0].HTTPStatus != http.StatusForbidden || fs[0].Route != "REFUSED" {
			t.Errorf("%s: refused %+v, listed %+v", c.name, rf, fs)
		}
		if !strings.Contains(out.String(), "codex: refused a connection") {
			t.Errorf("%s: not logged: %s", c.name, out.String())
		}
	}
}

// A page retrying in a loop is counted every time and written down once.
func TestRefusalsAreListedOnceAMinute(t *testing.T) {
	out := &lockedBuf{}
	g, _, _ := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	g.logger.SetOutput(out)
	for i := 0; i < 20; i++ {
		r := httptest.NewRequest("GET", "/x", nil)
		r.Header.Set("Origin", "https://evil.example")
		g.ServeHTTP(httptest.NewRecorder(), r)
	}
	if rf := g.Refused(); rf.Count != 20 {
		t.Errorf("counted %d of 20", rf.Count)
	}
	if n := strings.Count(out.String(), "codex: refused a connection"); n != 1 {
		t.Errorf("%d log lines for 20 refusals", n)
	}
	if n := len(g.Failures()); n != 1 {
		t.Errorf("%d rows for 20 refusals", n)
	}
}

// What a sender chose (a path, an id, an upstream's error text) cannot
// start a line of its own in the log or pass for another field.
func TestLogLinesCannotBeForged(t *testing.T) {
	out := &lockedBuf{}
	g, mp, gw := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/responses") {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, completedSSE)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, "expired\n2026-10-12T00:00:00.000+02:00 level=info req=forged ok")
	}))
	g.logger.SetOutput(out)

	resp, err := http.Get(gw.URL + "/models%0A2026-10-12T00:00:00.000+02:00%20level=info%20forged")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	req, _ := http.NewRequest("POST", gw.URL+"/backend-api/codex/responses", strings.NewReader(`{}`))
	req.Header.Set("Session-Id", "sess status=500 forged=1")
	req.Header.Set("X-Client-Request-Id", "0199c1d2-7a5b-7000-8000-000000000001")
	if resp, err = http.DefaultClient.Do(req); err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	waitEvents(t, mp, 1)

	log := out.String()
	for _, line := range strings.Split(strings.TrimSpace(log), "\n") {
		if !strings.HasPrefix(line, "codex: ") {
			t.Errorf("a line the gateway did not start: %q", line)
		}
	}
	if !strings.Contains(log, `session="sess status=500 forged=1"`) {
		t.Errorf("an id that is not one must be quoted:\n%s", log)
	}
	if !strings.Contains(log, "req=0199c1d2-7a5b-7000-8000-000000000001 ") {
		t.Errorf("an ordinary id must read as before:\n%s", log)
	}
}

// panicOnce is a listener whose first Accept panics.
type panicOnce struct {
	net.Listener
	once sync.Once
}

func (l *panicOnce) Accept() (net.Conn, error) {
	fire := false
	l.once.Do(func() { fire = true })
	if fire {
		panic("accept went wrong")
	}
	return l.Listener.Accept()
}

// A panic in the accept loop is logged with its stack and the port goes on
// answering: before, it ended the process, Claude Code's gateway included.
func TestAPanicInTheAcceptLoopDoesNotStopThePort(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }))
	t.Cleanup(up.Close)
	out := &lockedBuf{}
	g, err := New(up.URL, filepath.Join(t.TempDir(), "codex-metrics.jsonl"), log.New(out, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go g.Serve(&panicOnce{Listener: ln})

	resp, err := http.Get("http://" + ln.Addr().String() + "/after")
	if err != nil {
		t.Fatalf("the port stopped answering: %v\n%s", err, out.String())
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "ok" {
		t.Errorf("body %q", b)
	}
	if l := out.String(); !strings.Contains(l, "PANIC") || !strings.Contains(l, "accept went wrong") || !strings.Contains(l, "goroutine ") {
		t.Errorf("the panic and its stack must be in the log:\n%s", l)
	}
}

// A connection opened and left silent is hung up on after sortWait, and
// one that is only slow to start is still served.
func TestASilentConnectionIsNotHeldForEver(t *testing.T) {
	old := sortWait
	sortWait = 300 * time.Millisecond
	t.Cleanup(func() { sortWait = old })
	_, addr, _ := servedGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }))

	silent, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer silent.Close()
	slow, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer slow.Close()
	time.Sleep(100 * time.Millisecond)
	io.WriteString(slow, "GET /slow HTTP/1.1\r\nHost: "+addr+"\r\nConnection: close\r\n\r\n")
	slow.SetReadDeadline(time.Now().Add(3 * time.Second))
	if b, _ := io.ReadAll(slow); !strings.HasSuffix(string(b), "ok") {
		t.Errorf("a client slow to start was not served: %q", b)
	}

	silent.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, err = silent.Read(make([]byte, 1))
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		t.Fatal("a connection that sent nothing was still open long after sortWait")
	}
	if err == nil {
		t.Fatal("a connection that sent nothing was answered")
	}
}
