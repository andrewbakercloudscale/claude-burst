package router

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func gzipped(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// Claude Code gzips /v1/messages bodies (CLAUDE_CODE_GZIP_REQUEST_BODIES,
// or its server-side rollout). The gateway must read them as JSON, or the
// model, compaction and automask all see nothing.
func TestGzipRequestBodyIsDecoded(t *testing.T) {
	const plain = `{"model":"claude-opus-5-5","messages":[{"role":"user","content":"hi"}]}`
	var gotBody []byte
	var gotEncoding string
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotEncoding = r.Header.Get("Content-Encoding")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer primary.Close()

	s, logBuf := newTestServer(t, primary.URL, "")
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/v1/messages?beta=true", bytes.NewReader(gzipped(t, plain)))
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set("Content-Type", "application/json")
	s.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if gotEncoding != "" {
		t.Errorf("Content-Encoding forwarded as %q, want none", gotEncoding)
	}
	if string(gotBody) != plain {
		t.Errorf("upstream body = %q, want %q", gotBody, plain)
	}
	if !strings.Contains(logBuf.String(), `model="claude-opus-5-5"`) {
		t.Errorf("model not read from a gzip body; log:\n%s", logBuf.String())
	}
}

func TestGzipRequestBodyRejectedWhenCorrupt(t *testing.T) {
	called := false
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer primary.Close()

	s, _ := newTestServer(t, primary.URL, "")
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/v1/messages", strings.NewReader("not gzip"))
	req.Header.Set("Content-Encoding", "gzip")
	s.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
	if called {
		t.Error("a corrupt gzip body reached the provider")
	}
}

func TestGzipRequestBodyOverLimitRejected(t *testing.T) {
	called := false
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer primary.Close()

	s, _ := newTestServer(t, primary.URL, "")
	s.cfg.MaxRequestMB = 1
	big := `{"model":"claude-opus-5-5","pad":"` + strings.Repeat("a", 2*1024*1024) + `"}`
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/v1/messages", bytes.NewReader(gzipped(t, big)))
	req.Header.Set("Content-Encoding", "gzip")
	s.ServeHTTP(rr, req)

	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusRequestEntityTooLarge)
	}
	if called {
		t.Error("an oversized gzip body reached the provider")
	}
}
