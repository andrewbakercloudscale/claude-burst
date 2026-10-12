package router

import (
	"bytes"
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
)

// lockedBuffer is a log destination a handler goroutine and the test can
// both use.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// primaryOnlyGateway is a gateway with no secondary (so nothing here asks a
// Keychain) in front of upstream, served by a real HTTP server: cutting a
// connection is something only a real one can show.
func primaryOnlyGateway(t *testing.T, upstream string, edit func(*config.Config)) (gw *httptest.Server, logBuf *lockedBuffer, metricsPath string) {
	t.Helper()
	cfg := config.Default()
	cfg.AnthropicBaseURL = upstream
	cfg.Secondary = config.RouteConfig{Provider: config.ProviderNone}
	cfg.BedrockBaseURL = ""
	cfg.FallbackChain = nil
	if edit != nil {
		edit(&cfg)
	}
	dir := t.TempDir()
	logBuf = &lockedBuffer{}
	metricsPath = filepath.Join(dir, "metrics.jsonl")
	s, err := New(cfg, filepath.Join(dir, "state.json"), metricsPath, log.New(logBuf, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	s.probe = func() netProbe { return netProbe{dnsOK: true} }
	gw = httptest.NewServer(s)
	t.Cleanup(gw.Close)
	return gw, logBuf, metricsPath
}

// cutAfter answers with header and the given body, then drops the connection
// without ending the response: an upstream that died mid-reply.
func cutAfter(t *testing.T, header http.Header, part string) *httptest.Server {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		for k, v := range header {
			w.Header()[k] = v
		}
		_, _ = io.WriteString(w, part)
		w.(http.Flusher).Flush()
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		conn.Close()
	}))
	t.Cleanup(up.Close)
	return up
}

const halfAStream = "event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":25,"output_tokens":1}}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"The answer is"}}` + "\n\n"

func postMessages(t *testing.T, gw string, body string) (*http.Response, []byte, error) {
	t.Helper()
	resp, err := http.Post(gw+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("the reply had started, so the request itself must not fail: %v", err)
	}
	defer resp.Body.Close()
	b, rerr := io.ReadAll(resp.Body)
	return resp, b, rerr
}

// An upstream that dies half way through a streamed reply: the client is
// not handed a response that ends cleanly, the log has an error line and no
// ok line, and the metric is an error with the tokens that were used.
//
// Before, the gateway ended the response normally. Half a reply then looked
// to Claude Code exactly like a whole one, and to the log and the metrics
// like a success.
func TestStreamBrokenMidReplyIsNotEndedCleanly(t *testing.T) {
	up := cutAfter(t, http.Header{"Content-Type": {"text/event-stream"}}, halfAStream)
	gw, logBuf, metricsPath := primaryOnlyGateway(t, up.URL, nil)

	resp, got, rerr := postMessages(t, gw.URL, `{"model":"claude-sonnet-5","stream":true,"messages":[]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: the headers went out before the break", resp.StatusCode)
	}
	if string(got) != halfAStream {
		t.Fatalf("what arrived before the break must be passed on unchanged, got %q", got)
	}
	if rerr == nil {
		t.Fatal("the body ended cleanly: the client cannot tell half a reply from a whole one")
	}

	waitFor(t, func() bool { return strings.Contains(logBuf.String(), " done ") })
	logged := logBuf.String()
	if !strings.Contains(logged, "error stage=upstream_stream route=anthropic") {
		t.Fatalf("no error line for the broken stream:\n%s", logged)
	}
	if strings.Contains(logged, " ok route=") {
		t.Fatalf("a broken stream was logged as ok:\n%s", logged)
	}
	if strings.Contains(logged, "PANIC") {
		t.Fatalf("cutting the connection is not a handler fault and must not be logged as one:\n%s", logged)
	}
	if !strings.Contains(logged, `aborted="the upstream's reply broke mid-stream"`) {
		t.Fatalf("the done line must say the connection was cut:\n%s", logged)
	}
	m, err := os.ReadFile(metricsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(m), `"http_status":502`) || !strings.Contains(string(m), "upstream reply broke mid-stream") {
		t.Fatalf("the metric must record the break, not a 200:\n%s", m)
	}
	if !strings.Contains(string(m), `"input_tokens":25`) {
		t.Fatalf("the tokens used before the break are still counted:\n%s", m)
	}
}

// The same for a reply that is not streamed. Content-Length is not passed
// on, so a clean end was the only thing telling the client the JSON was
// whole.
func TestUnstreamedBodyBrokenMidReplyIsNotEndedCleanly(t *testing.T) {
	up := cutAfter(t, http.Header{"Content-Type": {"application/json"}, "Content-Length": {"500"}}, `{"type":"message","content":[{"type":"text","text":"The ans`)
	gw, logBuf, metricsPath := primaryOnlyGateway(t, up.URL, nil)

	_, _, rerr := postMessages(t, gw.URL, `{"model":"claude-sonnet-5","messages":[]}`)
	if rerr == nil {
		t.Fatal("half a JSON body ended cleanly")
	}
	waitFor(t, func() bool { return strings.Contains(logBuf.String(), " done ") })
	if logged := logBuf.String(); !strings.Contains(logged, "error stage=upstream_stream") || strings.Contains(logged, " ok route=") {
		t.Fatalf("want an error line and no ok line:\n%s", logged)
	}
	if m, _ := os.ReadFile(metricsPath); !strings.Contains(string(m), `"http_status":502`) {
		t.Fatalf("the metric must record the break:\n%s", m)
	}
}

// A reply that ends as it should is untouched by all of the above.
func TestWholeStreamStillEndsCleanlyAndCountsAsOK(t *testing.T) {
	whole := halfAStream + "event: message_delta\n" + `data: {"type":"message_delta","usage":{"output_tokens":7}}` + "\n\n" +
		"event: message_stop\n" + `data: {"type":"message_stop"}` + "\n\n"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, whole)
	}))
	t.Cleanup(up.Close)
	gw, logBuf, metricsPath := primaryOnlyGateway(t, up.URL, nil)

	_, got, rerr := postMessages(t, gw.URL, `{"model":"claude-sonnet-5","stream":true,"messages":[]}`)
	if rerr != nil || string(got) != whole {
		t.Fatalf("err=%v body=%q", rerr, got)
	}
	waitFor(t, func() bool { return strings.Contains(logBuf.String(), " done ") })
	if logged := logBuf.String(); !strings.Contains(logged, " ok route=anthropic") || strings.Contains(logged, "error stage=") {
		t.Fatalf("a whole reply is an ok line and nothing else:\n%s", logged)
	}
	if m, _ := os.ReadFile(metricsPath); !strings.Contains(string(m), `"http_status":200`) || !strings.Contains(string(m), `"output_tokens":7`) {
		t.Fatalf("metric: %s", m)
	}
}

// Esc in Claude Code ends the upstream read too, with the client's own
// cancellation. That is not the upstream breaking: no error line, no 502.
func TestClientLeavingMidStreamIsNotAnUpstreamBreak(t *testing.T) {
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, halfAStream)
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(up.Close)
	t.Cleanup(func() { close(release) })
	gw, logBuf, metricsPath := primaryOnlyGateway(t, up.URL, nil)

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, gw.URL+"/v1/messages", strings.NewReader(`{"model":"claude-sonnet-5","stream":true,"messages":[]}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, len(halfAStream))
	if _, err := io.ReadFull(resp.Body, buf); err != nil {
		t.Fatal(err)
	}
	cancel()
	resp.Body.Close()

	waitFor(t, func() bool { return strings.Contains(logBuf.String(), " done ") })
	if logged := logBuf.String(); strings.Contains(logged, "error stage=upstream_stream") {
		t.Fatalf("the client leaving was logged as the upstream breaking:\n%s", logged)
	}
	if m, _ := os.ReadFile(metricsPath); strings.Contains(string(m), `"http_status":502`) {
		t.Fatalf("the client leaving was counted as an upstream failure:\n%s", m)
	}
}

// Remote Control's event stream is not a model's reply. Its connection being
// ended from the far side is routine, Claude Code opens it again when it
// ends, and it ends the way it always did: cleanly, with no error counted.
func TestControlPlaneStreamEndingEarlyIsNotAFailedReply(t *testing.T) {
	up := cutAfter(t, http.Header{"Content-Type": {"text/event-stream"}}, "event: ping\ndata: {}\n\n")
	gw, logBuf, metricsPath := primaryOnlyGateway(t, up.URL, nil)

	resp, err := http.Get(gw.URL + "/v1/code/sessions/abc/worker/events/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if got, rerr := io.ReadAll(resp.Body); rerr != nil || string(got) != "event: ping\ndata: {}\n\n" {
		t.Fatalf("a control-plane stream must end as it always did: %q, %v", got, rerr)
	}
	waitFor(t, func() bool { return strings.Contains(logBuf.String(), "reply ended early") })
	if logged := logBuf.String(); strings.Contains(logged, "error stage=") {
		t.Fatalf("logged as an error:\n%s", logged)
	}
	waitFor(t, func() bool { m, _ := os.ReadFile(metricsPath); return len(m) > 0 })
	if m, _ := os.ReadFile(metricsPath); strings.Contains(string(m), `"http_status":502`) {
		t.Fatalf("counted as an upstream failure, which can raise an alert:\n%s", m)
	}
}

// An event-stream line longer than the gateway will hold is still relayed
// whole, the usage on the lines after it is still read, and the log says a
// line passed the limit. Before, the line was read into one string however
// long it was.
func TestOverlongStreamLineIsRelayedWholeAndBounded(t *testing.T) {
	long := "data: " + strings.Repeat("x", maxSSELineBytes+sseReadBytes) + "\n\n"
	whole := halfAStream + long + "event: message_delta\n" + `data: {"type":"message_delta","usage":{"output_tokens":9}}` + "\n\n"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, whole)
	}))
	t.Cleanup(up.Close)
	gw, logBuf, metricsPath := primaryOnlyGateway(t, up.URL, nil)

	_, got, rerr := postMessages(t, gw.URL, `{"model":"claude-sonnet-5","stream":true,"messages":[]}`)
	if rerr != nil || string(got) != whole {
		t.Fatalf("err=%v, %d bytes arrived of %d", rerr, len(got), len(whole))
	}
	waitFor(t, func() bool { return strings.Contains(logBuf.String(), " done ") })
	if n := strings.Count(logBuf.String(), "warn stage=relay an event-stream line passed"); n != 1 {
		t.Fatalf("want one line saying the limit was passed, got %d", n)
	}
	if m, _ := os.ReadFile(metricsPath); !strings.Contains(string(m), `"input_tokens":25`) || !strings.Contains(string(m), `"output_tokens":9`) {
		t.Fatalf("usage on the lines around the long one must still be read: %s", m)
	}
}

// A secondary's reply that is not streamed is held whole to be translated,
// so it has a bound, and passing it is an error the client is told about.
func TestUnstreamedSecondaryReplyHasABound(t *testing.T) {
	rec := httptest.NewRecorder()
	_, err := translateOpenAINonStream(rec, io.LimitReader(zeros{}, maxUnstreamedReplyBytes+1024), "claude-sonnet-5")
	if err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("want the limit named, got %v", err)
	}
	if rec.Body.Len() != 0 {
		t.Fatal("nothing may be written before the error, so the caller can still answer 502")
	}
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// A reply that is not streamed sends its headers only when it is complete,
// so it is not held to the timeout sized for a stream's first byte: at the
// configured timeout (one second here) it used to be cut and counted as a
// failure of the primary however healthy the upstream was.
func TestUnstreamedReplyIsNotCutAtTheStreamTimeout(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		time.Sleep(1500 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"type":"message","usage":{"input_tokens":3,"output_tokens":4}}`)
	}))
	t.Cleanup(up.Close)
	gw, _, _ := primaryOnlyGateway(t, up.URL, func(c *config.Config) { c.ResponseHeaderTimeoutSeconds = 1 })

	resp, _, rerr := postMessages(t, gw.URL, `{"model":"claude-sonnet-5","messages":[]}`)
	if rerr != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("a slow reply that is not streamed must be waited for: status %d err %v", resp.StatusCode, rerr)
	}
}

// The two transports are told apart by the request, and the stream one is
// the default: nothing but an unstreamed Messages request is marked.
func TestOnlyAnUnstreamedMessagesRequestWaitsLonger(t *testing.T) {
	for body, want := range map[string]bool{
		`{"model":"m","messages":[]}`:                false,
		`{"model":"m","stream":false,"messages":[]}`: false,
		`{"model":"m","stream":true,"messages":[]}`:  true,
		`not json`: false,
	} {
		if got := wantsStream([]byte(body)); got != want {
			t.Errorf("wantsStream(%s) = %v, want %v", body, got, want)
		}
	}
	var marked []bool
	s := &Server{client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		slow, _ := r.Context().Value(slowHeadersKey{}).(bool)
		marked = append(marked, slow)
		return nil, net.ErrClosed
	})}}
	out, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1:1/v1/messages", nil)
	for _, c := range []struct{ path, body string }{
		{"/v1/messages", `{"messages":[]}`},
		{"/v1/messages", `{"stream":true,"messages":[]}`},
		{"/v1/messages/count_tokens", `{"messages":[]}`},
	} {
		in := httptest.NewRequest(http.MethodPost, "http://127.0.0.1"+c.path, nil)
		_, _ = s.send(in, []byte(c.body), out)
	}
	if len(marked) != 3 || !marked[0] || marked[1] || marked[2] {
		t.Fatalf("marked for the longer wait: %v, want only the first", marked)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
