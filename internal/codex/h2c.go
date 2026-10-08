//go:build go1.24

package codex

import "net/http"

// allowH2C lets a client speak HTTP/2 without TLS to the port, as well as
// HTTP/1.1: the one other protocol a client can use on an http:// address,
// and refused with a 400 until 8 Oct 2026.
func allowH2C(srv *http.Server) {
	p := new(http.Protocols)
	p.SetHTTP1(true)
	p.SetUnencryptedHTTP2(true)
	srv.Protocols = p
}
