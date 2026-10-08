package codex

import (
	"bytes"
	"encoding/hex"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/metrics"
)

// What reaches the Codex port and is not a request Burst can read.
//
// Everything that is HTTP goes to ChatGPT whatever its method or path, and
// so does a WebSocket. A connection that is not HTTP at all is forwarded
// too, byte for byte, so that it is ChatGPT that answers it and never
// Burst (tunnel.go). What is left is a connection that starts like HTTP
// and then does not parse: Go's server answers that 400 itself and calls
// no handler. Until 8 Oct 2026 none of this was recorded anywhere, and a
// Codex feature failing that way would have looked like a fault of
// ChatGPT's. Each one is now an error line in the log, a row in the Codex
// requests table and a count on the Codex tab.

// Refused is what was not HTTP at the port since the gateway started,
// forwarded or turned away.
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

// noteOdd counts one connection that was not HTTP.
func (g *Gateway) noteOdd(what string) {
	g.mu.Lock()
	g.refused.Count++
	g.refused.Last, g.refused.What = time.Now(), what
	g.mu.Unlock()
}

// noteRefused logs one refusal. "codex: refused a connection" is what
// makes the line level=error (internal/logline).
func (g *Gateway) noteRefused(status int, what string) {
	g.noteOdd(what)
	g.logger.Printf("codex: refused a connection to the Codex port, nothing was passed on to ChatGPT, %s", what)
	g.keepFailure(metrics.Event{Time: time.Now(), Slot: "refused", Route: "REFUSED", Destination: "not passed on",
		HTTPStatus: status, Note: "refused at the Codex port, nothing was passed on to ChatGPT, " + what})
}

// Serve answers Codex on ln until it closes. Each connection is sorted by
// its first bytes: HTTP goes to the server, anything else is forwarded to
// ChatGPT as it came (tunnel.go).
func (g *Gateway) Serve(ln net.Listener) error {
	srv := &http.Server{Handler: g, ReadHeaderTimeout: 30 * time.Second}
	allowH2C(srv)
	sorted := &sortedListener{Listener: ln, conns: make(chan net.Conn), done: make(chan struct{})}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				sorted.fail(err)
				return
			}
			go g.sort(c, sorted)
		}
	}()
	return srv.Serve(sorted)
}

// sortedListener hands the HTTP server the connections that are HTTP.
type sortedListener struct {
	net.Listener
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
	err   error
}

func (l *sortedListener) fail(err error) {
	l.once.Do(func() { l.err = err; close(l.done) })
}

func (l *sortedListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, l.err
	}
}

func (l *sortedListener) Close() error {
	l.fail(net.ErrClosed)
	return l.Listener.Close()
}

// sort reads enough of a new connection to tell HTTP from anything else.
// A client may connect and send nothing for a while, so there is no
// deadline: the wait ends when it sends or hangs up.
func (g *Gateway) sort(c net.Conn, l *sortedListener) {
	first := make([]byte, 0, headBytes)
	for {
		n, err := c.Read(first[len(first):cap(first)])
		first = first[:len(first)+n]
		if err != nil && len(first) == 0 {
			c.Close()
			return
		}
		if err != nil || len(first) == cap(first) || !methodSoFar(first) {
			break
		}
	}
	if !looksHTTP(first) {
		g.tunnel(c, first)
		return
	}
	select {
	case l.conns <- &watchedConn{Conn: c, g: g, pre: first}:
	case <-l.done:
		c.Close()
	}
}

// methodSoFar reports whether b is only the capitals of an HTTP method
// with its space still to come: too little to decide on.
func methodSoFar(b []byte) bool {
	for _, ch := range b {
		if ch < 'A' || ch > 'Z' {
			return false
		}
	}
	return len(b) < 16
}

// headBytes is how much of what a client sent is kept to describe it.
const headBytes = 64

// watchedConn keeps the first bytes read since the last thing written,
// which is the start of the request being read, and watches for the
// server's own refusal going out.
type watchedConn struct {
	net.Conn
	g *Gateway

	pre []byte // read while sorting, handed to the server first

	mu    sync.Mutex
	head  []byte
	wrote bool
	gone  bool // a read failed: the client hung up or went quiet
}

func (c *watchedConn) Read(p []byte) (n int, err error) {
	if len(c.pre) > 0 {
		n = copy(p, c.pre)
		c.pre = c.pre[n:]
	} else {
		n, err = c.Conn.Read(p)
	}
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
		c.g.noteRefused(code, c.where()+": "+what)
	}
	return c.Conn.Write(p)
}

// where names the port the connection came to and the one it came from,
// which is what lsof needs to say which program that was.
func (c *watchedConn) where() string {
	return "on " + c.LocalAddr().String() + " from " + c.RemoteAddr().String()
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
			return i >= 3 && i <= 16
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
			return "with bytes that are not text (hex " + hex.EncodeToString(b[:min(len(b), 16)]) + ")"
		}
	}
	if i := bytes.IndexByte(line, '?'); i >= 0 {
		line = line[:i]
	}
	return strconv.Quote(string(line))
}
