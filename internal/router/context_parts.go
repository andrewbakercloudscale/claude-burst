package router

import (
	"bytes"
	"encoding/json"
	"strings"
)

// ContextPart is one slice of a session's context, for the band's context
// bar: what the request Burst actually sent was made of, after its own
// compaction, which Claude Code's /context does not know about.
type ContextPart struct {
	Name   string `json:"name"`
	Tokens int64  `json:"tokens"`
}

// contextPartNames is the bar's order, left to right: the fixed cost of a
// session first, then what the conversation adds.
var contextPartNames = []string{"System prompt", "System tools", "MCP tools", "Memory files", "Messages", "Tool results"}

// contextBytes measures each part of an inference request in bytes. Bytes,
// not tokens: only the response knows the token count, so scaleParts shares
// that out in these proportions.
func contextBytes(top map[string]json.RawMessage, msgs []json.RawMessage) []int64 {
	out := make([]int64, len(contextPartNames))
	out[0] = int64(len(top["system"]))
	var tools []json.RawMessage
	if json.Unmarshal(top["tools"], &tools) == nil {
		for _, t := range tools {
			var name struct {
				Name string `json:"name"`
			}
			_ = json.Unmarshal(t, &name)
			if strings.HasPrefix(name.Name, "mcp__") {
				out[2] += int64(len(t))
			} else {
				out[1] += int64(len(t))
			}
		}
	}
	for _, m := range msgs {
		var msg struct {
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(m, &msg) != nil {
			continue
		}
		var blocks []json.RawMessage
		if json.Unmarshal(msg.Content, &blocks) != nil {
			out[4] += int64(len(m))
			continue
		}
		rest := int64(len(m))
		for _, b := range blocks {
			var blk struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			_ = json.Unmarshal(b, &blk)
			switch {
			case blk.Type == "tool_result":
				out[5] += int64(len(b))
				rest -= int64(len(b))
			case blk.Type == "text" && isMemoryReminder(blk.Text):
				out[3] += int64(len(b))
				rest -= int64(len(b))
			}
		}
		if rest > 0 {
			out[4] += rest
		}
	}
	return out
}

// mediaBytes is what a base64 image or document counts for in tokenBytes:
// about 1,600 tokens (an image at Claude's largest size) at 4 bytes a token.
const mediaBytes = 6400

// tokenBytes is the body's size in bytes as text, for scaling a token count
// by: each base64 payload counts as mediaBytes, not its length. A screenshot
// is some 400k of base64 and costs some 1,600 tokens, so a history of 51 of
// them (aws-regulatory-risk-score, 20 Oct 2026) was put at 2,306k tokens
// held by Claude Code where it held about 300k.
func tokenBytes(body []byte) int64 {
	n := int64(len(body))
	key := []byte(`"data":"`)
	for i := 0; ; {
		j := bytes.Index(body[i:], key)
		if j < 0 {
			return n
		}
		start := i + j + len(key)
		end := start
		for end < len(body) && isBase64(body[end]) {
			end++
		}
		if end < len(body) && body[end] == '"' && end-start >= 1024 {
			n -= int64(end-start) - mediaBytes
		}
		i = end
	}
}

func isBase64(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '+' || c == '/' || c == '='
}

// isMemoryReminder is Claude Code's reminder carrying the CLAUDE.md files
// and the auto-memory index.
func isMemoryReminder(text string) bool {
	return strings.Contains(text, "<system-reminder>") &&
		(strings.Contains(text, "CLAUDE.md") || strings.Contains(text, "MEMORY.md")) &&
		strings.Contains(text, "Contents of ")
}

// scaleParts shares total out in proportion to the measured bytes, so the
// parts add up to the context the API reported. Empty parts are left out.
func scaleParts(sizes []int64, total int64) []ContextPart {
	var sum int64
	for _, n := range sizes {
		sum += n
	}
	if sum <= 0 || total <= 0 {
		return nil
	}
	var out []ContextPart
	var given int64
	last := -1
	for i, n := range sizes {
		if n > 0 {
			last = i
		}
	}
	for i, n := range sizes {
		if n <= 0 {
			continue
		}
		t := total * n / sum
		if i == last {
			t = total - given
		}
		given += t
		out = append(out, ContextPart{Name: contextPartNames[i], Tokens: t})
	}
	return out
}
