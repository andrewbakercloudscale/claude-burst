package codex

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

// Codex polls its model list every few minutes. With this Mac offline that
// is no failure of Burst's or ChatGPT's: it filled the failures list, five
// rows per outage. A refused connection still is one.
func TestOfflineIsNoFailure(t *testing.T) {
	for _, tc := range []struct {
		up   string
		kept int
	}{
		{"https://chatgpt.invalid", 0}, // does not resolve, as offline
		{"http://127.0.0.1:1", 1},      // refused
	} {
		g, err := New(tc.up, filepath.Join(t.TempDir(), "m.jsonl"), log.New(io.Discard, "", 0))
		if err != nil {
			t.Fatal(err)
		}
		gw := httptest.NewServer(g)
		resp, err := http.Get(gw.URL + "/backend-api/codex/models")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		gw.Close()
		if resp.StatusCode != http.StatusBadGateway || len(g.Failures()) != tc.kept {
			t.Errorf("%s: status %d, %d failure(s) kept, want %d", tc.up, resp.StatusCode, len(g.Failures()), tc.kept)
		}
	}
}
