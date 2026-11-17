package router

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
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

// fakePDF is a PDF of pages page objects under one /Pages tree, as far as
// counting its pages goes.
func fakePDF(pages int) []byte {
	var b strings.Builder
	b.WriteString("%PDF-1.4\n2 0 obj << /Type /Pages /Count " + fmt.Sprint(pages) + " >> endobj\n")
	for i := 0; i < pages; i++ {
		fmt.Fprintf(&b, "%d 0 obj << /Type /Page /Parent 2 0 R >> endobj\n%s\n", i+3, strings.Repeat("BT (text) Tj ET ", 64))
	}
	b.WriteString("%%EOF\n")
	return []byte(b.String())
}

// A PDF costs its pages, not one screenshot: until 17 Nov 2026 a 30-page
// one counted as 1,600 tokens, so a history that held it was put some 100k
// too low once it was summarised away. Inside a tool result too, and
// whichever side of the data its media type is written.
func TestTokenBytesCountsAPDFByItsPages(t *testing.T) {
	data := base64.StdEncoding.EncodeToString(fakePDF(30))
	for _, doc := range []string{
		`{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"` + data + `"}}`,
		`{"type":"tool_result","tool_use_id":"t1","content":[{"type":"document","source":{"data":"` + data + `","media_type":"application/pdf","type":"base64"}}]}`,
	} {
		body := []byte(`{"messages":[{"role":"user","content":[` + doc + `]}]}`)
		want := int64(len(body)) - int64(len(data)) + 4*30*pdfPageTokens
		if got := tokenBytes(body); got != want {
			t.Errorf("tokenBytes = %d, want %d (30 pages at %d tokens) for %.80s", got, want, pdfPageTokens, doc)
		}
	}
	// An image beside it still counts as one picture.
	img := `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + data + `"}}`
	body := []byte(`{"messages":[{"role":"user","content":[` + img + `]}]}`)
	if got, want := tokenBytes(body), int64(len(body))-int64(len(data))+mediaBytes; got != want {
		t.Errorf("image tokenBytes = %d, want %d", got, want)
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
