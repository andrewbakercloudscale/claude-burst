package router

import (
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// The gateway listener had no request guard while the admin listener did
// (admin.guard), found by the 2026-10-02 review. That gap mattered more on
// this side: while an overflow window is open, any POST /v1/messages is sent
// to the PAID secondary with the API key from the Keychain, and the body is
// parsed whatever its Content-Type. A web page can send exactly that as a
// CORS "simple" request (text/plain, no preflight) to 127.0.0.1:7777, and a
// DNS-rebinding page can go further and read the reply. Either spends real
// credit on a stranger's prompt.
//
// Two checks, both chosen so that Claude Code itself can never trip them:
//
//   - Origin. Browsers attach it to every cross-origin POST (and every
//     fetch with a non-GET method); Claude Code's Node client never sends
//     it. Sec-Fetch-* headers are deliberately NOT used: newer Node fetch
//     (undici) has been seen experimenting with them, and a guard that one
//     day refused Claude Code would be a total outage, not a hardening.
//   - Host. A rebinding page talks to us under its own hostname, so its Host
//     is that hostname. Legitimate traffic arrives as a loopback name, the
//     configured listen host (base-url mode), or the intercepted upstream
//     host (transparent mode, where Claude Code believes it is talking to
//     api.anthropic.com).
//
// /healthz is answered before the guard: pf-heal, install.sh and the admin
// panel all probe it under different Host values, it only reads, and a
// refused health probe would be misread as a broken gateway and "healed".

// guardLogEvery bounds how often a refusal is logged. A page retrying in a
// loop must not be able to fill the log, and one line a minute is enough to
// say "something is knocking".
const guardLogEvery = time.Minute

// requestRefusal returns a non-empty reason when r must not be served.
func (s *Server) requestRefusal(r *http.Request) string {
	if r.Header.Get("Origin") != "" {
		return "cross-origin request (Origin header present)"
	}
	if !s.hostAllowed(r.Host) {
		return "Host not accepted"
	}
	return ""
}

func (s *Server) hostAllowed(hostport string) bool {
	host := hostOnly(hostport)
	switch host {
	case "127.0.0.1", "localhost", "::1":
		return true
	}
	if ih := strings.ToLower(s.cfg.Intercept.Host); ih != "" && host == ih {
		return true
	}
	lh := hostOnly(s.cfg.Listen)
	if lh != "" && host == lh {
		return true
	}
	// Listening on every interface (":7777", "0.0.0.0:7777"): any IP literal
	// is then a legitimate way in. A rebinding attack always arrives under a
	// hostname, so allowing literals here gives it nothing; a plain cross-site
	// POST to an IP still carries Origin and is refused above.
	if lh == "" || lh == "0.0.0.0" || lh == "::" {
		return net.ParseIP(host) != nil
	}
	return false
}

// hostOnly lowercases a Host header or listen address and strips its port
// and any IPv6 brackets.
func hostOnly(hostport string) string {
	h := hostport
	if hh, _, err := net.SplitHostPort(hostport); err == nil {
		h = hh
	}
	h = strings.TrimSuffix(strings.TrimPrefix(h, "["), "]")
	return strings.ToLower(h)
}

// logRefusal logs at most one line per guardLogEvery, carrying a count of the
// refusals it stands for.
func (s *Server) logRefusal(rid, reason string, r *http.Request) {
	n := s.guardRefused.Add(1)
	now := time.Now().UnixNano()
	last := s.guardLoggedAt.Load()
	if last != 0 && time.Duration(now-last) < guardLogEvery {
		return
	}
	if !s.guardLoggedAt.CompareAndSwap(last, now) {
		return
	}
	s.guardRefused.Store(0)
	// %q: Host and Origin are attacker-supplied, see ServeHTTP's done line.
	s.logger.Printf("req=%s refused reason=%q host=%q origin=%q path=%q refused_since_last_line=%d",
		rid, reason, r.Host, r.Header.Get("Origin"), r.URL.Path, n)
}

// guardCounters lives on Server; declared here so the guard's state sits
// beside the code that uses it.
type guardCounters struct {
	guardLoggedAt atomic.Int64
	guardRefused  atomic.Int64
}
