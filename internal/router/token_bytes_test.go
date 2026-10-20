package router

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// A screenshot's base64 counts as the tokens it costs, not its length: what
// Claude Code holds is not put at eight times the truth by a history of them.
func TestTokenBytesCountsAnImageAsItsTokens(t *testing.T) {
	img := `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + strings.Repeat("iVBORw0K+/=", 40_000) + `"}}`
	text := `{"type":"text","text":"` + strings.Repeat("a", 2000) + `"}`
	body := []byte(`{"messages":[{"role":"user","content":[` + img + `,` + text + `]}]}`)
	want := int64(len(body)) - int64(len(strings.Repeat("iVBORw0K+/=", 40_000))) + mediaBytes
	if got := tokenBytes(body); got != want {
		t.Fatalf("tokenBytes = %d, want %d", got, want)
	}
	// A short value, and one that is not base64, count as they are.
	plain := []byte(`{"data":"abc","x":{"data":"` + strings.Repeat("a b ", 1000) + `"}}`)
	if got := tokenBytes(plain); got != int64(len(plain)) {
		t.Fatalf("plain tokenBytes = %d, want %d", got, len(plain))
	}
}

// The hand-off file says what Claude Code holds now: an estimate that falls
// (a corrected one included) is written down, not kept at its highest.
func TestHandoffRawFollowsTheEstimateDown(t *testing.T) {
	s, _ := newTestServer(t, "", "")
	c := s.compaction
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.state("S|claude-opus-5-5|x")
	st.summary, st.p0, st.swapAt, st.seen = "sum", 3, 3, time.Now()
	st.hand = &Handoff{Of: handoffOf("sum", 3), Raw: 2_306_000}
	st.rawContext = 300_000
	_, files := c.handoffFiles()
	var h Handoff
	if err := json.Unmarshal(files["S"], &h); err != nil {
		t.Fatalf("no hand-off file: %v %q", err, files["S"])
	}
	if h.Raw != 300_000 {
		t.Fatalf("Raw = %d, want 300000", h.Raw)
	}
}
