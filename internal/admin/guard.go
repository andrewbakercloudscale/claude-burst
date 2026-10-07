package admin

import (
	_ "embed"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// mutationHeader must be present on any state-changing request. A cross-origin
// page cannot set it without a successful preflight, and this server answers
// no preflights.
const mutationHeader = "X-Claude-Burst-Admin"

// guard rejects requests whose Host header is not loopback. This is the
// DNS-rebinding defence: the attacker controls DNS, not the Host header the
// browser sends, so a page on evil.example resolving to 127.0.0.1 still
// arrives here with Host: evil.example.
func (s *Server) guard(next http.Handler) http.Handler { return guard(s.extraHost, next) }

// guard admits only loopback Host headers (and extraHost, when set), the
// DNS-rebinding defence, and sets the framing and sniffing headers.
func guard(extraHost string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The caller, not only the name it asked for: a listen address set
		// to every interface would otherwise let any machine on the network
		// send "Host: 127.0.0.1" and drive the dashboard, which has no
		// login because it is meant to be reachable from this Mac alone.
		if !loopbackPeer(r.RemoteAddr) {
			http.Error(w, "admin UI only answers this Mac (loopback)", http.StatusForbidden)
			return
		}
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		host = strings.ToLower(host)
		allowed := host == "127.0.0.1" || host == "localhost" || host == "::1" || host == "[::1]" ||
			(extraHost != "" && host == extraHost)
		if !allowed {
			http.Error(w, "admin UI only accepts loopback Host headers (got "+r.Host+")", http.StatusForbidden)
			return
		}
		// Another site's page may link here, never call the API: two GET
		// routes do work (a connection test, a git fetch), and an image tag
		// on any page could set them off. Browsers say where a request came
		// from; curl, Claude Code and the mod send neither header.
		if strings.HasPrefix(r.URL.Path, "/api/") && crossSite(r) {
			http.Error(w, "admin API only answers its own page", http.StatusForbidden)
			return
		}
		// Never emit CORS headers: without them a cross-origin page cannot
		// read a response even if it manages to send the request.
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		// A transparent iframe would pass the Host check above while the
		// page's own JS supplies the mutation header, so the UI can be
		// clickjacked into Restart, Force or Upgrade. Browsers honour these;
		// a plain curl or Claude Code fetch sends no Origin and is unaffected.
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

// crossSite reports a browser request that did not come from this server's
// own page: Sec-Fetch-Site says so on every current browser, and Origin on
// the requests that carry one.
func crossSite(r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		return true
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}
	u, err := url.Parse(origin)
	return err != nil || !strings.EqualFold(u.Host, r.Host)
}

// loopbackPeer reports whether a connection came from this Mac.
func loopbackPeer(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

func (s *Server) readOnly(h http.HandlerFunc) http.HandlerFunc { return readOnly(h) }

func readOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h(w, r)
	}
}

// mutating also records the change in the audit: every action taken from
// the dashboard is there for support to see.
func (s *Server) mutating(h http.HandlerFunc) http.HandlerFunc {
	return mutating(audited("dashboard", h))
}

func mutating(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if r.Header.Get(mutationHeader) == "" {
			http.Error(w, "missing "+mutationHeader+" header", http.StatusForbidden)
			return
		}
		// Every dashboard form is a few KB; nothing legitimate comes close.
		r.Body = http.MaxBytesReader(w, r.Body, 4<<20)
		h(w, r)
	}
}
