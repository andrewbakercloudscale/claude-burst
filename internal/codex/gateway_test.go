package codex

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/metrics"
	"github.com/andrewbakercloudscale/claude-burst/internal/notice"
)

const completedSSE = "event: response.created\ndata: {\"type\":\"response.created\"}\n\n" +
	"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"pong\"}\n\n" +
	"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"model\":\"gpt-6.1-sol\"," +
	"\"usage\":{\"input_tokens\":20433,\"input_tokens_details\":{\"cached_tokens\":20000},\"output_tokens\":5}}}\n\n"

func newTestGateway(t *testing.T, upstream http.Handler) (*Gateway, string, *httptest.Server) {
	t.Helper()
	up := httptest.NewServer(upstream)
	t.Cleanup(up.Close)
	mp := filepath.Join(t.TempDir(), "codex-metrics.jsonl")
	g, err := New(up.URL, mp, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	gw := httptest.NewServer(g)
	t.Cleanup(gw.Close)
	return g, mp, gw
}

// A turn reaches ChatGPT exactly as Codex sent it, its stream comes back
// unchanged, and its tokens are recorded with cached input split out.
func TestTurnPassesThroughAndIsCounted(t *testing.T) {
	var gotAuth, gotAccount, gotPath, gotBody string
	g, mp, gw := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotAccount, gotPath = r.Header.Get("Authorization"), r.Header.Get("Chatgpt-Account-Id"), r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Codex-Primary-Used-Percent", "42")
		io.WriteString(w, completedSSE)
	}))

	req, _ := http.NewRequest("POST", gw.URL+"/backend-api/codex/responses", strings.NewReader(`{"model":"gpt-6.1-sol"}`))
	req.Header.Set("Authorization", "Bearer chatgpt-token")
	req.Header.Set("Chatgpt-Account-Id", "acct-1")
	req.Header.Set("Session-Id", "sess-1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if string(body) != completedSSE {
		t.Errorf("stream changed on the way back:\n%q", body)
	}
	if gotAuth != "Bearer chatgpt-token" || gotAccount != "acct-1" || gotPath != "/backend-api/codex/responses" || gotBody != `{"model":"gpt-6.1-sol"}` {
		t.Errorf("request changed on the way out: auth=%q account=%q path=%q body=%q", gotAuth, gotAccount, gotPath, gotBody)
	}
	if got := g.Limits().Headers["x-codex-primary-used-percent"]; got != "42" {
		t.Errorf("plan usage header not kept: %q", got)
	}

	evs := waitEvents(t, mp, 1)
	e := evs[0]
	if e.Model != "gpt-6.1-sol" || e.InputTokens != 433 || e.CacheReadTokens != 20000 || e.OutputTokens != 5 || e.SessionID != "sess-1" || e.HTTPStatus != 200 {
		t.Errorf("recorded %+v", e)
	}
}

// Anything but a turn (the model list, say) passes through and is not counted.
func TestOtherCallsAreNotCounted(t *testing.T) {
	_, mp, gw := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"models":[]}`)
	}))
	resp, err := http.Get(gw.URL + "/backend-api/codex/models")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != `{"models":[]}` {
		t.Errorf("got %q", b)
	}
	time.Sleep(50 * time.Millisecond)
	if evs, _ := metrics.Recent(mp, 10); len(evs) != 0 {
		t.Errorf("a non-turn was recorded: %+v", evs)
	}
}

// A usage-limit refusal raises one alert, recorded with its reason, and the
// next accepted turn clears it.
func TestUsageLimitAlertsAndClears(t *testing.T) {
	dir := t.TempDir()
	pub := notice.New(notice.Path(dir), log.New(io.Discard, "", 0))
	notice.SetDefault(pub)
	t.Cleanup(func() { notice.SetDefault(nil) })

	limited := true
	_, mp, gw := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if limited {
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"error":{"type":"usage_limit_reached","message":"limit","resets_in_seconds":3600}}`)
			return
		}
		io.WriteString(w, completedSSE)
	}))
	post := func() {
		resp, err := http.Post(gw.URL+"/backend-api/codex/responses", "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		io.ReadAll(resp.Body)
		resp.Body.Close()
	}
	post()
	post()
	limited = false
	post()
	pub.Flush(2 * time.Second)

	evs, err := notice.Read(notice.Path(dir))
	if err != nil {
		t.Fatal(err)
	}
	var titles []string
	for _, e := range evs {
		titles = append(titles, e.Severity+": "+e.Title)
	}
	want := []string{"warn: Codex hit its ChatGPT usage limit", "ok: Codex is answering again"}
	if strings.Join(titles, "|") != strings.Join(want, "|") {
		t.Errorf("alerts %q, want %q", titles, want)
	}
	recs := waitEvents(t, mp, 3)
	if recs[0].HTTPStatus != 429 || !strings.Contains(recs[0].Note, "usage_limit_reached") {
		t.Errorf("refusal recorded as %+v", recs[0])
	}
}

func waitEvents(t *testing.T, path string, n int) []metrics.Event {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		evs, _ := metrics.Recent(path, 50)
		if len(evs) >= n || time.Now().After(deadline) {
			if len(evs) < n {
				t.Fatalf("%d events recorded, want %d", len(evs), n)
			}
			// Recent is newest first; tests read oldest first.
			for i, j := 0, len(evs)-1; i < j; i, j = i+1, j-1 {
				evs[i], evs[j] = evs[j], evs[i]
			}
			return evs
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The model list's context windows are learned on the way through, and the
// list reaches Codex unchanged.
func TestModelWindowsLearned(t *testing.T) {
	const list = `{"models":[{"slug":"gpt-6.1-sol","context_window":272000,"base_instructions":"..."},{"slug":"gpt-5.5","context_window":128000}]}`
	g, _, gw := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, list)
	}))
	resp, err := http.Get(gw.URL + "/backend-api/codex/models?client_version=0.159.2")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != list {
		t.Errorf("list changed: %q", b)
	}
	if w := g.Windows(); w["gpt-6.1-sol"] != 272000 || w["gpt-5.5"] != 128000 {
		t.Errorf("windows %v", w)
	}
}

// Limits and windows survive a restart, so the Codex tab is not blank until
// Codex next sends something.
func TestReadingsSurviveRestart(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			io.WriteString(w, `{"models":[{"slug":"m","context_window":1000}]}`)
			return
		}
		w.Header().Set("X-Codex-Primary-Used-Percent", "8")
		w.Header().Set("X-Codex-Turn-State", "opaque")
		io.WriteString(w, completedSSE)
	}))
	defer up.Close()
	mp := filepath.Join(t.TempDir(), "codex-metrics.jsonl")
	g, _ := New(up.URL, mp, log.New(io.Discard, "", 0))
	gw := httptest.NewServer(g)
	defer gw.Close()
	for _, req := range []func() (*http.Response, error){
		func() (*http.Response, error) { return http.Get(gw.URL + "/backend-api/codex/models") },
		func() (*http.Response, error) {
			return http.Post(gw.URL+"/backend-api/codex/responses", "application/json", strings.NewReader("{}"))
		},
	} {
		resp, err := req()
		if err != nil {
			t.Fatal(err)
		}
		io.ReadAll(resp.Body)
		resp.Body.Close()
	}
	g2, _ := New(up.URL, mp, log.New(io.Discard, "", 0))
	if g2.Windows()["m"] != 1000 || g2.Limits().Headers["x-codex-primary-used-percent"] != "8" {
		t.Errorf("after restart: windows %v, limits %v", g2.Windows(), g2.Limits())
	}
	if _, ok := g2.Limits().Headers["x-codex-turn-state"]; ok {
		t.Error("the opaque turn-state token was kept")
	}
}
