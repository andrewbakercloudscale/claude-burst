package tlswatch

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Real lines from launchd.err.log, one per class seen there.
func TestClassifyRealLogLines(t *testing.T) {
	cases := map[string]Class{
		"EOF": Hangup,
		"read tcp 127.0.0.1:17777->127.0.0.1:64354: read: connection reset by peer": Hangup,
		"write tcp 127.0.0.1:17777->127.0.0.1:50211: write: broken pipe":            Hangup,
		"remote error: tls: unknown certificate":                                    Distrust,
		"remote error: tls: unknown certificate authority":                          Distrust,
		"remote error: tls: bad certificate":                                        Distrust,
		"client sent an HTTP request to an HTTPS server":                            PlainHTTP,
		"local error: tls: bad record MAC":                                          Other,
		"read tcp 127.0.0.1:17777->127.0.0.1:1: read: operation timed out":          Other,
	}
	for msg, want := range cases {
		if got := Classify(msg); got != want {
			t.Errorf("Classify(%q) = %s, want %s", msg, got, want)
		}
	}
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func logLine(w io.Writer, port int, msg string) {
	fmt.Fprintf(w, "2026/10/02 12:50:00 http: TLS handshake error from 127.0.0.1:%d: %s\n", port, msg)
}

type fakeConn struct{ net.Conn }

// The 2026-10-02 outage, replayed: 75 hangups in a minute with nothing
// completing must report a rejection; the 2026-08-31 background of 20
// distrust alerts in 5 minutes beside working sessions must not.
func TestSnapshotRejectsOnlyOnARateThatOutnumbersSuccesses(t *testing.T) {
	ck := &clock{t: time.Date(2026, 10, 2, 12, 50, 0, 0, time.Local)}
	var out bytes.Buffer
	w := newWithClock(&out, ck.now)
	l := log.New(w, "", log.LstdFlags)

	for i := 0; i < 20; i++ {
		l.Printf("http: TLS handshake error from 127.0.0.1:%d: remote error: tls: unknown certificate", 50000+i)
	}
	for i := 0; i < 5; i++ {
		w.ConnState(&fakeConn{}, http.StateActive)
	}
	if s := w.Snapshot(); s.Rejecting || s.Failures != 20 || s.Successes != 5 || s.ByClass["distrust"] != 20 {
		t.Fatalf("background noise must not read as a rejection: %+v", s)
	}
	if got := strings.Count(out.String(), "TLS handshake error"); got != 20 {
		t.Fatalf("every line must still reach the log, got %d", got)
	}

	// Five minutes on, the background has aged out; then the outage.
	ck.add(Window + time.Second)
	for i := 0; i < 60; i++ {
		logLine(w, 64000+i, "EOF")
	}
	for i := 0; i < 15; i++ {
		logLine(w, 64100+i, "read tcp 127.0.0.1:17777->127.0.0.1:64354: read: connection reset by peer")
	}
	logLine(w, 64200, "client sent an HTTP request to an HTTPS server")
	s := w.Snapshot()
	if !s.Rejecting || s.Failures != 75 || s.Successes != 0 || s.ByClass["hangup"] != 75 || s.ByClass["plain_http"] != 1 {
		t.Fatalf("the outage must read as a rejection, plain HTTP not counted: %+v", s)
	}
	if s.LastClass != "hangup" || !strings.Contains(s.LastError, "connection reset") {
		t.Fatalf("last failure must be the last trust-relevant one: %+v", s)
	}

	// Sessions restarted: they complete more handshakes than fail.
	for i := 0; i < 80; i++ {
		w.ConnState(&fakeConn{}, http.StateActive)
	}
	if s := w.Snapshot(); s.Rejecting {
		t.Fatalf("successes outnumbering failures is not a rejection: %+v", s)
	}
}

// A kept-alive connection is one handshake however many requests it carries.
func TestConnStateCountsAHandshakeOncePerConnection(t *testing.T) {
	w := New(io.Discard)
	c := &fakeConn{}
	for i := 0; i < 5; i++ {
		w.ConnState(c, http.StateActive)
		w.ConnState(c, http.StateIdle)
	}
	w.ConnState(c, http.StateClosed)
	if s := w.Snapshot(); s.Successes != 1 {
		t.Fatalf("want 1 handshake, got %+v", s)
	}
	if len(w.active) != 0 {
		t.Fatal("a closed connection must be forgotten")
	}
}

// End to end against a real TLS listener: a client that does not trust the
// certificate is counted as a failure with its alert classified, a client
// that does is counted as a success, and the error line still reaches the
// log.
func TestWatcherOnARealTLSServer(t *testing.T) {
	var logged bytes.Buffer
	var mu sync.Mutex
	w := New(writerFunc(func(p []byte) (int, error) { mu.Lock(); defer mu.Unlock(); return logged.Write(p) }))
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {}))
	srv.Config.ErrorLog = log.New(w, "", log.LstdFlags)
	srv.Config.ConnState = w.ConnState
	srv.StartTLS()
	defer srv.Close()

	// Distrusting client: empty root pool, so Go's client sends an alert.
	bad := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: x509.NewCertPool()}}}
	if _, err := bad.Get(srv.URL); err == nil {
		t.Fatal("a client with no roots must fail")
	}
	good := srv.Client()
	for i := 0; i < 2; i++ {
		resp, err := good.Get(srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		s := w.Snapshot()
		if s.ByClass["distrust"] == 1 && s.Successes == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("want 1 distrust and 1 success (keep-alive reuses the connection), got %+v", s)
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(logged.String(), "TLS handshake error") {
		t.Fatalf("the handshake error must still be logged, got %q", logged.String())
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
