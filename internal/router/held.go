package router

import (
	"context"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Remote Control's event stream is one request Claude Code holds open for
// as long as the session lives: the phone's messages arrive on it. After the
// Mac sleeps, or moves to another network (the lid opened onto a hotspot),
// the connection to Anthropic under it is dead and nothing says so: Claude
// Code's side of it is a loopback connection to this process, which is
// fine, and its heartbeats go out on new connections and are answered. So
// the session reads as connected and hears nothing, until something makes
// Claude Code open the stream again. On 2026-10-04 three sessions sat like
// that for 13 minutes after waking, heartbeats all 200.
//
// So the gateway ends the held streams itself when the Mac wakes or its
// network changes. A stream that ends is opened again by Claude Code within
// seconds, from the event it had reached, on a connection made now.

// heldStreams are the control-plane streams open now, each with the cancel
// that ends it.
type heldStreams struct {
	mu      sync.Mutex
	next    int
	cancels map[int]context.CancelFunc
}

// isHeldStream is whether path is a stream Claude Code holds open and
// opens again when it ends.
func isHeldStream(r *http.Request) bool {
	return r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/events/stream")
}

// hold makes r one that DropHeldStreams can end. done is called when the
// request is over.
func (h *heldStreams) hold(r *http.Request) (held *http.Request, done func()) {
	ctx, cancel := context.WithCancel(r.Context())
	h.mu.Lock()
	if h.cancels == nil {
		h.cancels = map[int]context.CancelFunc{}
	}
	id := h.next
	h.next++
	h.cancels[id] = cancel
	h.mu.Unlock()
	return r.WithContext(ctx), func() {
		h.mu.Lock()
		delete(h.cancels, id)
		h.mu.Unlock()
		cancel()
	}
}

// DropHeldStreams ends every held stream, so Claude Code opens each again
// on a new connection, and closes the idle connections to the upstream,
// which are as dead as the streams were. It returns how many it ended.
func (s *Server) DropHeldStreams(why string) int {
	s.held.mu.Lock()
	n := len(s.held.cancels)
	for _, cancel := range s.held.cancels {
		cancel()
	}
	s.held.mu.Unlock()
	for _, c := range []*http.Client{s.client, s.passthroughClient} {
		if c != nil {
			c.CloseIdleConnections()
		}
	}
	s.logger.Printf("remote control: %s, ended %d held stream(s) so Claude Code opens them again on a new connection", why, n)
	return n
}

// pathWatch notices the two things that leave a held stream dead: the Mac
// slept, or it is on another network than it was.
type pathWatch struct {
	last time.Time // the previous look, by the wall clock
	addr string    // the address this Mac last reached the internet from
}

// wakeGap is how far apart two looks must be to have had a sleep between
// them: the watcher looks every few seconds.
const wakeGap = 30 * time.Second

// look says why the held streams should be ended now, or "". now must be
// wall-clock time (the monotonic clock stops while a Mac sleeps), and addr
// is the local address the internet is reached from, "" when offline.
func (p *pathWatch) look(now time.Time, addr string) string {
	prev, was := p.last, p.addr
	p.last = now
	if addr != "" {
		p.addr = addr
	}
	switch {
	case prev.IsZero():
		return ""
	case now.Sub(prev) > wakeGap:
		return "the Mac woke after " + now.Sub(prev).Round(time.Second).String()
	// Offline keeps the address it had: going offline ends nothing, and
	// coming back on the same network at the same address is no change.
	case addr != "" && was != "" && addr != was:
		return "the network changed (" + was + " to " + addr + ")"
	}
	return ""
}

// localAddr is the address this Mac would reach the internet from, "" when
// it has no route out. A UDP "connection" sends nothing: it only asks the
// kernel which interface it would use.
func localAddr() string {
	c, err := net.Dial("udp4", "192.0.2.1:9")
	if err != nil {
		return ""
	}
	defer c.Close()
	if a, ok := c.LocalAddr().(*net.UDPAddr); ok {
		return a.IP.String()
	}
	return ""
}

// WatchNetwork ends the held streams whenever the Mac wakes or changes
// network, until ctx is done.
func (s *Server) WatchNetwork(ctx context.Context) {
	var p pathWatch
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		// Round(0) drops the monotonic reading, so the gap is the wall
		// clock's: the one that moves while the Mac sleeps.
		if why := p.look(time.Now().Round(0), localAddr()); why != "" {
			s.DropHeldStreams(why)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
