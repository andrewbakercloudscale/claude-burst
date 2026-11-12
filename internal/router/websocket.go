package router

import (
	"net/http"
	"net/http/httputil"
	"strings"
)

// WebSocket upgrades, passed through. Claude Code 2.1.322 (12 Nov 2026)
// opens a WebSocket to /api/frame/sync. The ordinary forward path sends
// every request over the shared HTTP/2 connection to Anthropic, and HTTP/2
// has no Upgrade: Go refused each one ("http2: invalid Upgrade request
// header"), the gateway answered 502, Claude Code tried again, and five in
// two minutes raised "Requests are failing" over a gateway whose model
// requests were all succeeding. An upgrade goes to the primary's address
// over HTTP/1.1, which is how WebSockets work, and the two connections are
// joined both ways. Nothing in it is a model request, so nothing in Burst
// reads, compacts or masks it.

// isWebSocket says r asks to become a WebSocket.
func isWebSocket(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket") &&
		headerHasToken(r.Header, "Connection", "upgrade")
}

func headerHasToken(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for _, t := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(t), token) {
				return true
			}
		}
	}
	return false
}

// proxyWebSocket joins r to the same path at the primary's address.
func (s *Server) proxyWebSocket(w http.ResponseWriter, r *http.Request, rid string) {
	pp, ok := s.primary.(*PassthroughProvider)
	if !ok || s.wsTransport == nil {
		s.logger.Printf("req=%s websocket path=%q refused: the primary (%s) does not take WebSockets", rid, r.URL.Path, s.primary.Name())
		http.Error(w, "claude-burst: this primary does not take WebSockets", http.StatusNotImplemented)
		return
	}
	base := pp.base
	rp := &httputil.ReverseProxy{
		Transport: s.wsTransport,
		Rewrite: func(pr *httputil.ProxyRequest) {
			u := *base
			u.Path = strings.TrimRight(base.Path, "/") + pr.In.URL.Path
			u.RawPath = ""
			u.RawQuery = pr.In.URL.RawQuery
			pr.Out.URL = &u
			pr.Out.Host = base.Host
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			s.logger.Printf("req=%s error stage=websocket route=%s path=%q err=%q", rid, pp.Name(), r.URL.Path, err)
			http.Error(w, "claude-burst: the WebSocket could not be opened upstream", http.StatusBadGateway)
		},
	}
	rp.ServeHTTP(w, r)
}
