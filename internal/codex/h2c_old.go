//go:build !go1.24

package codex

import "net/http"

// allowH2C needs Go 1.24: built with an older one the port is HTTP/1.1 only.
func allowH2C(*http.Server) {}
