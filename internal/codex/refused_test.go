package codex

import (
	"bytes"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/logline"
	"github.com/andrewbakercloudscale/claude-burst/internal/metrics"
)

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// servedGateway is a gateway on a real port through Serve, the way the
// daemon runs it, with its log kept.
func servedGateway(t *testing.T, upstream http.Handler) (*Gateway, string, *lockedBuf) {
	t.Helper()
	up := httptest.NewServer(upstream)
	t.Cleanup(up.Close)
	out := &lockedBuf{}
	g, err := New(up.URL, filepath.Join(t.TempDir(), "codex-metrics.jsonl"), log.New(&logline.Writer{W: out}, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go g.Serve(ln)
	return g, ln.Addr().String(), out
}

func waitRefused(t *testing.T, g *Gateway, n int64) Refused {
	t.Helper()
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if rf := g.Refused(); rf.Count >= n {
			return rf
		}
	}
	t.Fatalf("want %d refused, got %+v", n, g.Refused())
	return Refused{}
}

// Bytes that are not a request are answered 400 by Go's server with no
// handler called: that is counted and is an error line, without the query.
func TestAConnectionThatIsNotHTTPIsLoggedAsAnError(t *testing.T) {
	g, addr, out := servedGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("nothing may reach ChatGPT")
	}))
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	io.WriteString(c, "hello burst?token=secret-1 are you there\r\n\r\n")
	reply, _ := io.ReadAll(c)
	if !strings.HasPrefix(string(reply), "HTTP/1.1 400") {
		t.Fatalf("reply %q", reply)
	}
	rf := waitRefused(t, g, 1)
	if !strings.Contains(rf.What, "400 Bad Request") || !strings.Contains(rf.What, `"hello burst"`) {
		t.Errorf("what = %q", rf.What)
	}
	if fs := g.Failures(); len(fs) != 1 || fs[0].Slot != "refused" || fs[0].HTTPStatus != 400 || !strings.Contains(fs[0].Note, `"hello burst"`) {
		t.Errorf("the requests table must list it: %+v", fs)
	}
	line := out.String()
	if !strings.Contains(line, " level=error codex: refused a connection") {
		t.Errorf("want an error line, got %q", line)
	}
	if strings.Contains(line, "secret-1") {
		t.Errorf("the query must not be logged: %q", line)
	}
}

// A client that speaks first in another protocol and hangs up unanswered
// is recorded too, by what it was.
func TestATLSHandshakeLeftUnansweredIsLogged(t *testing.T) {
	g, addr, _ := servedGateway(t, http.NotFoundHandler())
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	c.Write([]byte{0x16, 0x03, 0x01, 0x00, 0x05, 0x01, 0x00, 0x00, 0x01, 0x00})
	time.Sleep(50 * time.Millisecond)
	c.Close()
	if rf := waitRefused(t, g, 1); !strings.Contains(rf.What, "TLS handshake") {
		t.Errorf("what = %q", rf.What)
	}
}

// What is passed on is never counted as refused: a request ChatGPT itself
// answers 400, one in HTTP/2 without TLS, and one Codex gave up on.
func TestWhatIsPassedOnIsNotRefused(t *testing.T) {
	var protos []string
	var mu sync.Mutex
	g, addr, out := servedGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		protos = append(protos, r.URL.Path)
		mu.Unlock()
		if r.URL.Path == "/bad" {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, "400 Bad Request")
			return
		}
		io.WriteString(w, "ok")
	}))
	resp, err := http.Get("http://" + addr + "/bad")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d", resp.StatusCode)
	}

	if got := getH2C(t, "http://"+addr+"/two"); got != "ok HTTP/2.0" {
		t.Errorf("HTTP/2 without TLS: %q", got)
	}

	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(c, "GET /half HTTP/1.1\r\nHost: x\r\n")
	time.Sleep(50 * time.Millisecond)
	c.Close()
	time.Sleep(100 * time.Millisecond)

	if rf := g.Refused(); rf.Count != 0 {
		t.Errorf("refused %+v\n%s", rf, out.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(protos, " ") != "/bad /two" {
		t.Errorf("reached ChatGPT: %v", protos)
	}
}

// A request that is not a turn and that ChatGPT refuses is listed for the
// requests table with the client that sent it, survives a restart, and is
// kept out of the turns that the counts and checks read.
func TestAFailedRequestThatIsNotATurnIsListed(t *testing.T) {
	g, mp, gw := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":{"message":"Your authentication token has expired."}}`)
	}))
	req, _ := http.NewRequest("GET", gw.URL+"/backend-api/codex/models?client_version=1", nil)
	req.Header.Set("Originator", "codex-browser-use")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	fs := g.Failures()
	if len(fs) != 1 || fs[0].HTTPStatus != 401 || !strings.HasSuffix(fs[0].Destination, "/backend-api/codex/models") ||
		!strings.Contains(fs[0].Note, "sent by codex-browser-use") || !strings.Contains(fs[0].Note, "token has expired") {
		t.Fatalf("failures = %+v", fs)
	}
	if evs, _ := metrics.Recent(mp, 10); len(evs) != 0 {
		t.Errorf("it is not a turn and must not be counted as one: %+v", evs)
	}
	again, err := New(gw.URL, mp, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	if fs := again.Failures(); len(fs) != 1 || fs[0].HTTPStatus != 401 {
		t.Errorf("after a restart: %+v", fs)
	}
}
