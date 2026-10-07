package admin

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func postStatusLine(t *testing.T, h http.Handler, body string) int {
	t.Helper()
	req := localRequest(http.MethodPost, "http://x/api/statusline", strings.NewReader(body))
	req.Host = "127.0.0.1"
	req.Header.Set(mutationHeader, "1")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr.Code
}

func TestStatusLineIsAddedAndRemovedOnlyWhenOurs(t *testing.T) {
	s := newTestServer(t)
	h := s.Handler()
	old := ccusageCommand
	ccusageCommand = func() string { return "ccusage statusline -B text" }
	t.Cleanup(func() { ccusageCommand = old })
	p := filepath.Join(os.Getenv("HOME"), ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(`{"model":"opus"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if c := postStatusLine(t, h, `{"on":true}`); c != http.StatusOK {
		t.Fatalf("on: %d", c)
	}
	b, _ := os.ReadFile(p)
	if !strings.Contains(string(b), "ccusage statusline") || !strings.Contains(string(b), `"model"`) {
		t.Fatalf("after on: %s", b)
	}
	if c := postStatusLine(t, h, `{"on":false}`); c != http.StatusOK {
		t.Fatalf("off: %d", c)
	}
	b, _ = os.ReadFile(p)
	if strings.Contains(string(b), "statusLine") || !strings.Contains(string(b), `"model"`) {
		t.Fatalf("after off: %s", b)
	}

	// Someone else's status line: on is refused, off leaves it alone.
	mine := `{"statusLine":{"type":"command","command":"~/my-line.sh"}}`
	if err := os.WriteFile(p, []byte(mine), 0o600); err != nil {
		t.Fatal(err)
	}
	if c := postStatusLine(t, h, `{"on":true}`); c != http.StatusConflict {
		t.Fatalf("on over a user's line: %d, want 409", c)
	}
	if c := postStatusLine(t, h, `{"on":false}`); c != http.StatusOK {
		t.Fatalf("off with a user's line: %d", c)
	}
	if b, _ := os.ReadFile(p); !strings.Contains(string(b), "my-line.sh") {
		t.Fatalf("the user's status line was touched: %s", b)
	}
}
