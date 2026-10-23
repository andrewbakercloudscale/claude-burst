package codex

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

// Codex polls its model list every few minutes. With this Mac offline that
// is no failure of Burst's or ChatGPT's: it filled the failures list, five
// rows per outage. A refused connection still is one, and so is ChatGPT's
// name alone failing to resolve, which a filter blocking it does.
func TestOfflineIsNoFailure(t *testing.T) {
	old := controlHost
	t.Cleanup(func() { controlHost = old; controlAt = time.Time{} })
	for _, tc := range []struct {
		up, control string
		kept        int
	}{
		{"https://chatgpt.invalid", "control.invalid", 0}, // nothing resolves: offline
		{"https://chatgpt.invalid", "localhost", 1},       // only ChatGPT's name fails: blocked
		{"http://127.0.0.1:1", "control.invalid", 1},      // refused
	} {
		controlHost, controlAt = tc.control, time.Time{}
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
