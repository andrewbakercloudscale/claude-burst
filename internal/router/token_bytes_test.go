package router

import (
	"strings"
	"testing"
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
