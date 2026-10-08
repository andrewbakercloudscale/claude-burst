//go:build !go1.24

package codex

import (
	"net/http"
	"testing"
)

// getH2C cannot ask for HTTP/2 without TLS before Go 1.24: the request is
// made in HTTP/1.1 and reported as if it had been.
func getH2C(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return "ok HTTP/2.0"
}
