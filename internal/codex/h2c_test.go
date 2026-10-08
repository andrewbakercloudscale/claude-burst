//go:build go1.24

package codex

import (
	"io"
	"net/http"
	"testing"
)

// getH2C fetches url in HTTP/2 without TLS and returns "body proto".
func getH2C(t *testing.T, url string) string {
	t.Helper()
	p := new(http.Protocols)
	p.SetUnencryptedHTTP2(true)
	tr := &http.Transport{Protocols: p}
	defer tr.CloseIdleConnections()
	resp, err := (&http.Client{Transport: tr}).Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b) + " " + resp.Proto
}
