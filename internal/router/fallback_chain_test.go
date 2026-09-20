package router

import (
	"bytes"
	"encoding/json"
	"log"
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

// recordingUpstream stands in for Anthropic: it refuses the models named in
// reject with a genuine subscription-limit response, and answers anything
// else. It records the model of every request it saw, in order, which is the
// only thing these tests actually need to assert on.
type recordingUpstream struct {
	mu     sync.Mutex
	seen   []string
	reject map[string]bool
	srv    *httptest.Server
}

func newRecordingUpstream(t *testing.T, reject ...string) *recordingUpstream {
	t.Helper()
	u := &recordingUpstream{reject: map[string]bool{}}
	for _, m := range reject {
		u.reject[m] = true
	}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		u.mu.Lock()
		u.seen = append(u.seen, body.Model)
		reject := u.reject[body.Model]
		u.mu.Unlock()
		if reject {
			w.Header().Set("anthropic-ratelimit-unified-status", "rejected")
			w.Header().Set("anthropic-ratelimit-unified-representative-claim", "seven_day_overage_included")
			w.Header().Set("anthropic-ratelimit-unified-reset", strconvI(time.Now().Add(48*time.Hour).Unix()))
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"type":"error","error":{"message":"usage limit reached"}}`))
			return
		}
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"type":"message","model":"` + body.Model + `","content":[]}`))
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *recordingUpstream) models() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.seen...)
}

func strconvI(n int64) string { return time.Unix(n, 0).UTC().Format(time.RFC3339) }

func newChainServer(t *testing.T, primaryURL string, chain map[string][]string) (*Server, *bytes.Buffer) {
	t.Helper()
	cfg := config.Default()
	cfg.AnthropicBaseURL = primaryURL
	cfg.Primary = config.RouteConfig{Provider: "oauth-passthrough", BaseURL: primaryURL, FailoverStrategy: "subscription-limit"}
	cfg.Secondary = config.RouteConfig{}
	cfg.FallbackChain = chain
	dir := t.TempDir()
	var logBuf bytes.Buffer
	s, err := New(cfg, filepath.Join(dir, "state.json"), filepath.Join(dir, "metrics.jsonl"), log.New(&logBuf, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	return s, &logBuf
}

func messagesRequest(model string) *http.Request {
	return httptest.NewRequest(http.MethodPost, "http://local/v1/messages",
		strings.NewReader(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`))
}

// The whole point of the change: Anthropic refusing Fable is evidence about
// Fable. Before model scoping it armed one account-wide window, so the very
// next Opus request -- on a subscription that was answering Opus fine -- was
// sent to a paid secondary for as long as the window lasted (two days, on
// 2026-09-20).
func TestFableRejectionDoesNotDivertOpus(t *testing.T) {
	up := newRecordingUpstream(t, "claude-fable-5-1")
	s, _ := newChainServer(t, up.srv.URL, nil)

	s.ServeHTTP(httptest.NewRecorder(), messagesRequest("claude-fable-5-1"))
	if !s.modelInOverflow("claude-fable-5-1", time.Now()) {
		t.Fatal("fable should be inside its own rejection window")
	}
	if s.modelInOverflow("claude-opus-5", time.Now()) {
		t.Fatal("opus was never refused; nothing may put it in a window")
	}

	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, messagesRequest("claude-opus-5"))
	if rr.Code != http.StatusOK {
		t.Fatalf("opus should still be served by the primary, got %d: %s", rr.Code, rr.Body.String())
	}
	if got := up.models(); len(got) != 2 || got[1] != "claude-opus-5" {
		t.Fatalf("opus request did not reach the primary as opus: %v", got)
	}
}

func TestRefusedModelIsReplayedDownTheChain(t *testing.T) {
	up := newRecordingUpstream(t, "claude-fable-5-1")
	s, logBuf := newChainServer(t, up.srv.URL, map[string][]string{
		"claude-fable-5-1": {"claude-opus-5"},
	})

	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, messagesRequest("claude-fable-5-1"))

	if rr.Code != http.StatusOK {
		t.Fatalf("the downgraded request should have succeeded, got %d: %s", rr.Code, rr.Body.String())
	}
	got := up.models()
	if len(got) != 2 || got[0] != "claude-fable-5-1" || got[1] != "claude-opus-5" {
		t.Fatalf("expected fable then opus on the primary, got %v", got)
	}
	if !strings.Contains(logBuf.String(), `replaying on the subscription as "claude-opus-5"`) {
		t.Fatalf("the downgrade was not logged:\n%s", logBuf.String())
	}
}

// Once Fable's window is open, later Fable requests must not be sent to
// Anthropic again just to be refused a second time -- they go straight to the
// rung.
func TestOpenWindowRoutesStraightToTheRung(t *testing.T) {
	up := newRecordingUpstream(t, "claude-fable-5-1")
	s, _ := newChainServer(t, up.srv.URL, map[string][]string{
		"claude-fable-5-1": {"claude-opus-5"},
	})

	s.ServeHTTP(httptest.NewRecorder(), messagesRequest("claude-fable-5-1")) // arms the window
	before := len(up.models())

	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, messagesRequest("claude-fable-5-1"))
	if rr.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rr.Code, rr.Body.String())
	}
	got := up.models()[before:]
	if len(got) != 1 || got[0] != "claude-opus-5" {
		t.Fatalf("second fable request should have gone out as opus only, got %v", got)
	}
}

// A rung can be refused too. The chain must carry on rather than stop at the
// first closed door, and with no secondary configured the client gets a clear
// error instead of a hang or a nil-provider panic.
func TestEveryRungRefusedAndNoSecondary(t *testing.T) {
	up := newRecordingUpstream(t, "claude-fable-5-1", "claude-opus-5", "claude-sonnet-5")
	s, _ := newChainServer(t, up.srv.URL, map[string][]string{
		"claude-fable-5-1": {"claude-opus-5", "claude-sonnet-5"},
	})

	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, messagesRequest("claude-fable-5-1"))

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 once every rung is refused, got %d: %s", rr.Code, rr.Body.String())
	}
	got := up.models()
	want := []string{"claude-fable-5-1", "claude-opus-5", "claude-sonnet-5"}
	if len(got) != len(want) {
		t.Fatalf("expected each rung tried once, got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("chain took the wrong order: got %v want %v", got, want)
		}
	}
	for _, m := range want {
		if !s.modelInOverflow(m, time.Now()) {
			t.Fatalf("%s was refused and should have its own window", m)
		}
	}
}

// A chain that names the model it is attached to would replay the request to
// the model that was just refused, forever.
func TestChainIgnoresSelfReference(t *testing.T) {
	up := newRecordingUpstream(t, "claude-fable-5-1")
	s, _ := newChainServer(t, up.srv.URL, map[string][]string{
		"claude-fable-5-1": {"claude-fable-5-1", "claude-opus-5"},
	})

	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, messagesRequest("claude-fable-5-1"))
	if rr.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rr.Code, rr.Body.String())
	}
	if got := up.models(); len(got) != 2 || got[1] != "claude-opus-5" {
		t.Fatalf("self-reference should have been skipped, got %v", got)
	}
}

func TestDowngradeToggleOffGoesStraightPastTheChain(t *testing.T) {
	up := newRecordingUpstream(t, "claude-fable-5-1")
	s, _ := newChainServer(t, up.srv.URL, map[string][]string{
		"claude-fable-5-1": {"claude-opus-5"},
	})
	s.SetDowngradeEnabled(false)

	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, messagesRequest("claude-fable-5-1"))

	if got := up.models(); len(got) != 1 || got[0] != "claude-fable-5-1" {
		t.Fatalf("with downgrade off nothing may be replayed on the subscription, got %v", got)
	}
	if rr.Code == http.StatusOK {
		t.Fatal("no secondary and no chain: the client must see the failure, not a success")
	}
	if !s.DowngradeEnabled() {
		// and the choice must survive a reload of the same state file
		s2, _ := New(s.cfg, s.statePath, filepath.Join(t.TempDir(), "m.jsonl"), log.New(&bytes.Buffer{}, "", 0))
		if s2.DowngradeEnabled() {
			t.Fatal("the toggle did not persist across a restart")
		}
	}
}

// A state file written before model scoping carries a days-long account-wide
// window that was only ever one model's rejection. Honouring it would send
// every model to the paid secondary for the rest of it.
func TestLegacyAccountWideWindowIsDropped(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	legacy := State{
		OverflowUntil: time.Now().Add(48 * time.Hour).Unix(),
		LimitClaim:    "seven_day_overage_included",
		LastReason:    "anthropic-ratelimit-unified-status=rejected",
	}
	b, _ := json.Marshal(legacy)
	if err := os.WriteFile(statePath, b, 0600); err != nil {
		t.Fatal(err)
	}

	var logBuf bytes.Buffer
	s, err := New(config.Default(), statePath, filepath.Join(dir, "m.jsonl"), log.New(&logBuf, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	if s.inOverflow(time.Now()) {
		t.Fatal("a pre-scoping window must not survive the upgrade")
	}
	if !strings.Contains(logBuf.String(), "dropping pre-model-scoping overflow window") {
		t.Fatalf("the drop was silent:\n%s", logBuf.String())
	}
}

// A forced window IS an instruction about the whole account, and must be
// honoured on restart exactly as written -- including past the chain, since
// its only purpose is to exercise the secondary.
func TestForcedWindowSurvivesAndBypassesTheChain(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	forced := State{OverflowUntil: time.Now().Add(time.Hour).Unix(), LimitClaim: "forced", LastReason: "forced from the admin UI"}
	b, _ := json.Marshal(forced)
	if err := os.WriteFile(statePath, b, 0600); err != nil {
		t.Fatal(err)
	}
	s, err := New(config.Default(), statePath, filepath.Join(dir, "m.jsonl"), log.New(&bytes.Buffer{}, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	if !s.forcedOverflow(time.Now()) {
		t.Fatal("a forced window is a deliberate instruction and must survive a restart")
	}
}

func TestWithModelLeavesEverythingElseAlone(t *testing.T) {
	in := []byte(`{"model":"claude-fable-5-1","max_tokens":7,"messages":[{"role":"user","content":"hi"}],"tools":[{"name":"Bash","input_schema":{"type":"object"}}]}`)
	out, err := withModel(in, "claude-opus-5")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got["model"] != "claude-opus-5" {
		t.Fatalf("model not rewritten: %v", got["model"])
	}
	if got["max_tokens"] != float64(7) || got["messages"] == nil || got["tools"] == nil {
		t.Fatalf("the rest of the request did not survive the rewrite: %v", got)
	}
}

// "Back to primary" clears windows. It must not also switch the chain back
// on: a toggle that quietly undoes itself is worse than no toggle.
func TestClearOverflowKeepsTheDowngradeChoice(t *testing.T) {
	s, _ := newChainServer(t, "http://127.0.0.1:0", nil)
	s.SetDowngradeEnabled(false)
	s.ForceOverflow(time.Hour, "test")
	s.ClearOverflow()
	if s.inOverflow(time.Now()) {
		t.Fatal("ClearOverflow left a window open")
	}
	if s.DowngradeEnabled() {
		t.Fatal("ClearOverflow re-enabled a chain the user had turned off")
	}
}
