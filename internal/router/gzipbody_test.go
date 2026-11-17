package router

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
)

func gz(t *testing.T, s string) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	zw.Write([]byte(s))
	zw.Close()
	return b.Bytes()
}

// A gzipped request is read as JSON by compaction (12 Nov 2026: sessions
// that gzipped their bodies were never measured, and went out at 368k
// under a 175k limit), and reaches Anthropic gzipped as the client sent it.
func TestGzippedRequestIsReadAndForwardedGzipped(t *testing.T) {
	const body = `{"model":"claude-opus-4-8","messages":[{"role":"user","content":"hello"}]}`
	var gotEnc string
	var gotBody []byte
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotEnc = r.Header.Get("Content-Encoding")
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			t.Errorf("upstream body is not gzip: %v", err)
			return
		}
		gotBody, _ = io.ReadAll(zr)
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"id":"msg_1","type":"message","model":"claude-opus-4-8","usage":{"input_tokens":2,"cache_read_input_tokens":368000,"output_tokens":5}}`))
	}))
	defer primary.Close()
	s, logs := newTestServer(t, primary.URL, "")
	s.SetCompaction(config.CompactionConfig{Enabled: true, NoPromptNotice: true})

	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/v1/messages?beta=true", bytes.NewReader(gz(t, body)))
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set("x-claude-code-session-id", "sess-gzip")
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status %d: %s\n%s", rr.Code, rr.Body, logs)
	}
	if gotEnc != "gzip" || string(gotBody) != body {
		t.Fatalf("upstream got encoding %q body %q", gotEnc, gotBody)
	}
	found := false
	for _, cs := range s.CompactionSessions() {
		if cs.Session == "sess-gzip" && cs.Model == "claude-opus-4-8" && cs.Context == 368002 {
			found = true
		}
	}
	if !found {
		t.Fatalf("compaction did not measure the gzipped session: %+v\n%s", s.CompactionSessions(), logs)
	}
	if !strings.Contains(logs.String(), `model="claude-opus-4-8"`) {
		t.Errorf("the model was not read:\n%s", logs)
	}
}

// A conversation of pictures near Anthropic's 32 MB limit, gzipped, is
// opened and sent on whole: neither max_request_mb's default nor the limit
// on what a gzipped body opens to is below what the API itself takes, and
// the pictures arrive byte for byte with Automask and compaction on
// (checked 17 Nov 2026).
func TestAnImageHeavyGzippedBodyNearTheAPILimitGoesThrough(t *testing.T) {
	pic := strings.Repeat("iVBORw0KGgoAAAANSUhEUgAA+/", 40_000) // about 1 MB of base64
	var b strings.Builder
	b.WriteString(`{"model":"claude-opus-5-5","messages":[`)
	for i := 0; i < 30; i++ {
		if i > 0 {
			b.WriteString(`,{"role":"assistant","content":"seen"},`)
		}
		b.WriteString(`{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + pic + `"}},{"type":"text","text":"and this one"}]}`)
	}
	b.WriteString(`]}`)
	body := b.String()
	if len(body) < 30_000_000 {
		t.Fatalf("setup: body is %d bytes", len(body))
	}
	var got []byte
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			t.Errorf("upstream body is not gzip: %v", err)
			return
		}
		got, _ = io.ReadAll(zr)
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"id":"msg_1","type":"message","model":"claude-opus-5-5","usage":{"input_tokens":2,"output_tokens":5}}`))
	}))
	defer primary.Close()
	s, logs := newTestServer(t, primary.URL, "")
	s.SetCompaction(config.CompactionConfig{Enabled: true, NoPromptNotice: true})
	s.SetAutomask(config.AutomaskConfig{Enabled: true})
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/v1/messages", bytes.NewReader(gz(t, body)))
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set("x-claude-code-session-id", "sess-big")
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status %d: %s\n%s", rr.Code, rr.Body, logs)
	}
	if string(got) != body {
		t.Fatalf("upstream got %d bytes, want the %d sent", len(got), len(body))
	}
}

// A body that says gzip and is not is refused, never forwarded as it came.
func TestBrokenGzipIsRefused(t *testing.T) {
	called := false
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	defer primary.Close()
	s, _ := newTestServer(t, primary.URL, "")
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/v1/messages", strings.NewReader(`{"not":"gzip"}`))
	req.Header.Set("Content-Encoding", "gzip")
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest || called {
		t.Fatalf("status %d, upstream called %v", rr.Code, called)
	}
}

// A turn Burst cannot read is reported, once a session an hour, not skipped
// in silence.
func TestUnreadableTurnIsReported(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"id":"msg_1","type":"message","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer primary.Close()
	s, logs := newTestServer(t, primary.URL, "")
	send := func() {
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/v1/messages", strings.NewReader("zstd bytes"))
		req.Header.Set("Content-Encoding", "zstd")
		req.Header.Set("x-claude-code-session-id", "sess-unread")
		s.ServeHTTP(httptest.NewRecorder(), req)
	}
	send()
	send()
	if n := strings.Count(logs.String(), "reason=unreadable session=sess-unread"); n != 1 {
		t.Fatalf("reported %d times, want once:\n%s", n, logs)
	}
	if !strings.Contains(logs.String(), "Content-Encoding zstd") {
		t.Errorf("the reason is not named:\n%s", logs)
	}
}
