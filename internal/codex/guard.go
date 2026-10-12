package codex

import (
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// The Codex port had no request guard while Claude Code's gateway did
// (router/guard.go). A web page cannot give it a ChatGPT token, so what it
// could do was smaller than there, but not nothing: every request it sent
// went on to chatgpt.com from this Mac, was written to the log and the
// requests table, and a 429 in reply raised the usage limit alert. A
// DNS-rebinding page could also read what came back.
//
// Three checks, chosen so that Codex itself cannot trip them:
//
//   - Origin. A browser attaches it to every cross-origin POST and to every
//     fetch that is not a plain GET. Codex's client is not a browser and
//     sends none.
//   - Sec-Fetch-Site. It covers what Origin does not, a plain GET made by a
//     page (an image tag, a link). Only "cross-site" and "same-site" are
//     refused: a client that sends "none" or "same-origin", or nothing, is
//     let through. The Claude gateway leaves this header alone because Node
//     has experimented with sending it; Codex's client is not Node.
//   - Host. A rebinding page reaches the port under its own hostname, so
//     that is its Host. Codex is given its address by Block, as
//     http://<listen>/backend-api/codex, and sends that listen address back
//     as Host: a loopback name or an IP address unless codex.listen was set
//     to something else, which AcceptHost is told about. Every IP literal
//     is accepted, since rebinding always arrives under a name and a plain
//     cross-site request to an address still carries Origin.

// guardLogEvery bounds how often a refusal is logged and listed: a page
// retrying in a loop must not fill the log or rewrite codex-state.json on
// every try.
const guardLogEvery = time.Minute

// guardState is the guard's own state on Gateway.
type guardState struct {
	// accepted is the lower-case host of the configured listen address,
	// "" when it is an IP literal or was never given.
	accepted      atomic.Value // string
	guardLoggedAt atomic.Int64
	guardRefused  atomic.Int64
}

// AcceptHost tells the guard the address Codex is configured to send to
// (codex.listen), so a listen address given as a hostname is not refused.
func (g *Gateway) AcceptHost(listen string) {
	g.guard.accepted.Store(hostOnly(listen))
}

// requestRefusal returns a non-empty reason when r must not be served.
func (g *Gateway) requestRefusal(r *http.Request) string {
	if r.Header.Get("Origin") != "" {
		return "cross-origin request (Origin header present)"
	}
	switch strings.ToLower(r.Header.Get("Sec-Fetch-Site")) {
	case "cross-site", "same-site":
		return "request made by a web page (Sec-Fetch-Site)"
	}
	if !g.hostAllowed(r.Host) {
		return "Host not accepted"
	}
	return ""
}

func (g *Gateway) hostAllowed(hostport string) bool {
	host := hostOnly(hostport)
	if host == "localhost" || net.ParseIP(host) != nil {
		return true
	}
	accepted, _ := g.guard.accepted.Load().(string)
	return accepted != "" && host == accepted
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

// refuse answers 403 and counts the refusal. At most one in guardLogEvery
// is logged and listed in the requests table, carrying a count of the
// refusals it stands for.
func (g *Gateway) refuse(w http.ResponseWriter, r *http.Request, reason string) {
	http.Error(w, "Claude Burst's Codex port refused this request: "+reason, http.StatusForbidden)
	// %q: Host, Origin and the path are whatever the sender chose.
	what := fmt.Sprintf("%s, host=%q origin=%q path=%q", reason, r.Host, r.Header.Get("Origin"), r.URL.Path)
	if a, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok {
		what = "on " + a.String() + " from " + r.RemoteAddr + ": " + what
	}
	g.noteOdd(what)
	n := g.guard.guardRefused.Add(1)
	now := time.Now().UnixNano()
	last := g.guard.guardLoggedAt.Load()
	if last != 0 && time.Duration(now-last) < guardLogEvery {
		return
	}
	if !g.guard.guardLoggedAt.CompareAndSwap(last, now) {
		return
	}
	g.guard.guardRefused.Store(0)
	what += fmt.Sprintf(" refused_since_last_line=%d", n)
	g.listRefused(http.StatusForbidden, what)
}
