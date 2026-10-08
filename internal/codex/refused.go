package codex

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/metrics"
)

// A connection the Codex port turned away without passing anything on.
//
// Everything that is HTTP goes to ChatGPT whatever its method or path, and
// so does a WebSocket. What cannot be passed on is what Go's server could
// not read as a request at all: it answers 400 itself (or says nothing)
// and calls no handler, so until 8 Oct 2026 nothing recorded it, and a
// Codex feature failing that way would have looked like a fault of
// ChatGPT's. Each one is now an error line in the log and a count on the
// Codex tab.

// Refused is what the port has turned away since the gateway started.
type Refused struct {
	Count int64     `json:"count"`
	Last  time.Time `json:"last"`
	What  string    `json:"what"`
}

// Refused returns the count and the latest refusal.
func (g *Gateway) Refused() Refused {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.refused
}

// noteRefused logs one refusal. "codex: refused a connection" is what
// makes the line level=error (internal/logline).
func (g *Gateway) noteRefused(status int, what string) {
	g.mu.Lock()
	g.refused.Count++
	g.refused.Last, g.refused.What = time.Now(), what
	g.mu.Unlock()
	g.logger.Printf("codex: refused a connection on the Codex port, nothing was passed on to ChatGPT: %s", what)
	g.keepFailure(metrics.Event{Time: time.Now(), Slot: "refused", Route: "REFUSED", Destination: "not passed on",
		HTTPStatus: status, Note: "refused at the Codex port, nothing was passed on to ChatGPT: " + what})
}

// Serve answers Codex on ln until it closes.
func (g *Gateway) Serve(ln net.Listener) error {
	srv := &http.Server{
		Handler:           g,
		ReadHeaderTimeout: 30 * time.Second,
		ConnState: func(c net.Conn, s http.ConnState) {
			if wc, ok := c.(*watchedConn); ok && s == http.StateClosed {
				wc.closed()
			}
		},
	}
	allowH2C(srv)
	return srv.Serve(&watchedListener{Listener: ln, g: g})
}

type watchedListener struct {
	net.Listener
	g *Gateway
}

func (l *watchedListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &watchedConn{Conn: c, g: l.g}, nil
}

// headBytes is how much of what a client sent is kept to describe it.
const headBytes = 64

// watchedConn keeps the first bytes read since the last thing written,
// which is the start of the request being read, and watches for the
// server's own refusal going out.
type watchedConn struct {
	net.Conn
	g *Gateway

	mu    sync.Mutex
	head  []byte
	wrote bool
	gone  bool // a read failed: the client hung up or went quiet
}

func (c *watchedConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.mu.Lock()
		if room := headBytes - len(c.head); room > 0 {
			c.head = append(c.head, p[:min(n, room)]...)
		}
		c.mu.Unlock()
	}
	if err != nil {
		c.mu.Lock()
		c.gone = true
		c.mu.Unlock()
	}
	return n, err
}

func (c *watchedConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	head, gone := c.head, c.gone
	c.head, c.wrote = nil, true
	c.mu.Unlock()
	// Codex hanging up part way through its headers also ends in a 400,
	// written to nobody: that is Codex's choice, not a refusal.
	if status := ownRefusal(p); status != "" && !(gone && looksHTTP(head)) {
		what := "Burst answered " + status + " itself"
		// Over-long headers are refused part way through them: the bytes
		// held are not a request line and could be a credential.
		if status[:3] != "431" {
			what += "; it began " + describe(head)
		}
		code, _ := strconv.Atoi(status[:3])
		c.g.noteRefused(code, what)
	}
	return c.Conn.Write(p)
}

// closed is called once the connection is gone: bytes that were not HTTP
// and were never answered are a refusal too (a client that waits for the
// server to speak first, or one cut off at the header timeout).
func (c *watchedConn) closed() {
	c.mu.Lock()
	head, wrote := c.head, c.wrote
	c.mu.Unlock()
	if !wrote && len(head) > 0 && !looksHTTP(head) {
		c.g.noteRefused(0, "it was closed with no answer; it began "+describe(head))
	}
}

// refusal matches the reply Go's server writes by itself when it cannot
// read a request: a status line, then exactly these two headers. A reply
// from a handler, ChatGPT's included, always carries a Date as well.
var refusal = regexp.MustCompile(`^HTTP/1\.1 (\d{3} [^\r\n]*)\r\nContent-Type: text/plain; charset=utf-8\r\nConnection: close\r\n\r\n`)

func ownRefusal(p []byte) string {
	if !bytes.HasPrefix(p, []byte("HTTP/1.1 ")) {
		return ""
	}
	if m := refusal.FindSubmatch(p); m != nil {
		return string(m[1])
	}
	return ""
}

// looksHTTP reports whether b starts like an HTTP/1 request line, or the
// part of one that has arrived.
func looksHTTP(b []byte) bool {
	for i, ch := range b {
		if ch == ' ' {
			return i >= 3
		}
		if ch < 'A' || ch > 'Z' {
			return false
		}
	}
	return true
}

// describe says what a client sent, from its first bytes: the protocol
// when it is a known one, else the first line with any query cut off.
func describe(b []byte) string {
	switch {
	case len(b) == 0:
		return "with nothing"
	case len(b) >= 2 && b[0] == 0x16 && b[1] == 0x03:
		return "with a TLS handshake (https spoken to a plain http port)"
	case bytes.HasPrefix(b, []byte("PRI * HTTP/2")):
		return "with an HTTP/2 preface"
	case bytes.HasPrefix(b, []byte("SSH-")):
		return "like SSH"
	}
	line := b
	if i := bytes.IndexAny(line, "\r\n"); i >= 0 {
		line = line[:i]
	}
	for _, ch := range line {
		if ch < 0x20 || ch > 0x7e {
			return fmt.Sprintf("with bytes that are not text (hex %x)", b[:min(len(b), 16)])
		}
	}
	if i := bytes.IndexByte(line, '?'); i >= 0 {
		line = line[:i]
	}
	return fmt.Sprintf("%q", line)
}
