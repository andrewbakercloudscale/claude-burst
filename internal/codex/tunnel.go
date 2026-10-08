package codex

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"regexp"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/metrics"
)

// A connection that is not HTTP is forwarded, not refused: Burst cannot
// read it, so it carries the bytes to ChatGPT and back untouched and lets
// ChatGPT be the one to answer. A TLS handshake goes on as it is, so the
// client ends up talking TLS to ChatGPT itself; anything else is carried
// inside Burst's own TLS connection, as an HTTP request would be.
//
// It goes straight to the upstream host: a proxy set in the environment,
// which HTTP requests honour, is not used.

// tunnelIdle ends a forwarded connection once the client has finished
// sending and ChatGPT has said nothing more for this long.
const tunnelIdle = 30 * time.Second

func (g *Gateway) upstreamAddr() string {
	if g.upstream.Port() != "" {
		return g.upstream.Host
	}
	if g.upstream.Scheme == "https" {
		return net.JoinHostPort(g.upstream.Hostname(), "443")
	}
	return net.JoinHostPort(g.upstream.Hostname(), "80")
}

// replyStatus reads an HTTP status from the first bytes of a reply.
var replyStatus = regexp.MustCompile(`^HTTP/\d(?:\.\d)? (\d{3})`)

// tunnel forwards c, whose first bytes were already read, to ChatGPT.
func (g *Gateway) tunnel(c net.Conn, first []byte) {
	defer c.Close()
	g.inFlight.Add(1)
	defer g.inFlight.Add(-1)
	start := time.Now()
	g.mu.Lock()
	g.last = start
	g.mu.Unlock()
	where := "on " + c.LocalAddr().String() + " from " + c.RemoteAddr().String()
	began := describe(first)
	addr := g.upstreamAddr()

	isTLS := len(first) >= 2 && first[0] == 0x16 && first[1] == 0x03
	var up net.Conn
	var err error
	d := &net.Dialer{Timeout: 15 * time.Second}
	if g.upstream.Scheme == "https" && !isTLS {
		up, err = tls.DialWithDialer(d, "tcp", addr, &tls.Config{ServerName: g.upstream.Hostname()})
	} else {
		up, err = d.Dial("tcp", addr)
	}
	if err != nil {
		g.noteRefused(0, fmt.Sprintf("%s: it was not HTTP and %s could not be reached to forward it (%v); it began %s", where, addr, err, began))
		return
	}
	defer up.Close()

	var sent, got atomic.Int64
	var reply []byte
	back := make(chan struct{})
	go func() {
		defer close(back)
		buf := make([]byte, 32<<10)
		for {
			n, rerr := up.Read(buf)
			if n > 0 {
				if len(reply) < headBytes {
					reply = append(reply, buf[:min(n, headBytes-len(reply))]...)
				}
				got.Add(int64(n))
				if _, werr := c.Write(buf[:n]); werr != nil {
					return
				}
			}
			if rerr != nil {
				return
			}
		}
	}()
	go func() {
		io.Copy(up, &countedReader{r: io.MultiReader(bytes.NewReader(first), c), n: &sent})
		// The client is done sending: say so, and give the reply a while.
		if cw, ok := up.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		}
		up.SetReadDeadline(time.Now().Add(tunnelIdle))
	}()
	<-back

	status := 0
	answer := "ChatGPT sent nothing back"
	if m := replyStatus.FindSubmatch(reply); m != nil {
		status, _ = strconv.Atoi(string(m[1]))
		answer = "ChatGPT answered " + string(m[1])
	} else if got.Load() > 0 {
		answer = "ChatGPT answered " + describe(reply)
	}
	what := fmt.Sprintf("%s: forwarded to %s as it came; it began %s; %d bytes sent, %d back, %s", where, addr, began, sent.Load(), got.Load(), answer)
	g.noteOdd(what)
	// "codex: not HTTP" is what makes the line level=error (internal/logline).
	g.logger.Printf("codex: not HTTP, %s ms=%d", what, time.Since(start).Milliseconds())
	g.keepFailure(metrics.Event{Time: start, Slot: "not http", Route: "TUNNEL", Destination: g.upstream.Scheme + "://" + g.upstream.Host,
		HTTPStatus: status, DurationMS: time.Since(start).Milliseconds(), Note: "not HTTP, " + what})
}

// countedReader counts what is read through it as it goes, so the total
// is right whichever side ends the connection.
type countedReader struct {
	r io.Reader
	n *atomic.Int64
}

func (c *countedReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}
