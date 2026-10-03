package router

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// Proxy-side compaction of primary sessions (experimental, off by default).
//
// WHY THE PROXY DOES IT. Claude Code compacts on its own only near the end
// of the context window, and its threshold is not configurable. On the
// primary, cost is context size times turns: cache reads were 53% of the
// primary's API-equivalent spend, and a turn at 400k costs four of one at
// 100k. Neither a hook nor a proxy can run /compact for the user, and
// Anthropic's server-side compaction returns a block Claude Code does not
// know to send back. So the proxy summarises the old part of a session
// itself, once, and swaps the summary in for those messages on every later
// request. Claude Code keeps its full transcript and never sees a
// difference; the model sees the summary.
//
// THE SHAPE is Anthropic's documented keep-tail compaction:
//   - the summary covers messages [0, p0), where p0 is a plain user prompt,
//     never a tool_result, so no call is summarised away from its result;
//   - the swap is applied first on a request that ends in a plain user
//     prompt, never mid tool round;
//   - retained turns from before the swap lose their thinking blocks: those
//     were produced with the old history present, and the API rejects or
//     drops them after it changes. Turns produced after the swap keep them.
//   - Claude Code's <system-reminder> blocks in the first message (CLAUDE.md,
//     skills, environment) are carried over verbatim: a summary might not.
//
// If the history stops matching (the user ran /clear or /compact, or
// rewound), the swap is dropped and requests pass through untouched.

// promptBoundaries returns the indexes of plain user prompts: user messages
// with no tool_result block.
func promptBoundaries(msgs []json.RawMessage) []int {
	var out []int
	for i, m := range msgs {
		var msg struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(m, &msg) != nil || msg.Role != "user" {
			continue
		}
		var blocks []struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(msg.Content, &blocks) != nil {
			out = append(out, i) // plain string content
			continue
		}
		plain := true
		for _, b := range blocks {
			if b.Type == "tool_result" {
				plain = false
				break
			}
		}
		if plain {
			out = append(out, i)
		}
	}
	return out
}

// prefixHash identifies messages [0, n). Each message is re-marshalled so
// key order and whitespace never cause a false mismatch; any change to what
// was said does.
func prefixHash(msgs []json.RawMessage, n int) string {
	h := sha256.New()
	for i := 0; i < n && i < len(msgs); i++ {
		var v any
		if json.Unmarshal(msgs[i], &v) != nil {
			h.Write(msgs[i])
		} else {
			b, _ := json.Marshal(hashForm(v))
			h.Write(b)
		}
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// hashForm drops what Claude Code changes in a message it has already sent:
// the cache_control breakpoint, which moves forward every request, and
// thinking blocks, which it clears from older turns. Neither is part of the
// conversation a summary covers. On 2026-10-03 the message just before the
// boundary changed after every summary, so each one was dropped as soon as
// it was ready and a new one started: six summaries and eighteen notices in
// one session, none of them swapped in.
func hashForm(v any) any {
	msg, ok := v.(map[string]any)
	if !ok {
		return v
	}
	blocks, ok := msg["content"].([]any)
	if !ok {
		return v
	}
	kept := make([]any, 0, len(blocks))
	for _, b := range blocks {
		bm, ok := b.(map[string]any)
		if !ok {
			kept = append(kept, b)
			continue
		}
		if t, _ := bm["type"].(string); t == "thinking" || t == "redacted_thinking" {
			continue
		}
		if _, has := bm["cache_control"]; has {
			c := make(map[string]any, len(bm))
			for k, x := range bm {
				if k != "cache_control" {
					c[k] = x
				}
			}
			bm = c
		}
		kept = append(kept, bm)
	}
	out := make(map[string]any, len(msg))
	for k, x := range msg {
		out[k] = x
	}
	out["content"] = kept
	return out
}

// rewriteWithSummary returns messages [p0, len) with the summary and the
// first message's system reminders prepended to message p0, and thinking
// blocks removed from messages [p0, swapAt).
func rewriteWithSummary(msgs []json.RawMessage, summary string, p0, swapAt int) []json.RawMessage {
	if p0 <= 0 || p0 >= len(msgs) {
		return msgs
	}
	lead := []any{map[string]any{"type": "text", "text": "<system-reminder>\nThe earlier part of this conversation was compacted by claude-burst to save context. Summary of it:\n<summary>\n" +
		summary + "\n</summary>\n</system-reminder>"}}
	lead = append(lead, systemReminders(msgs[0])...)

	out := make([]json.RawMessage, 0, len(msgs)-p0)
	for i := p0; i < len(msgs); i++ {
		var msg map[string]any
		if json.Unmarshal(msgs[i], &msg) != nil {
			out = append(out, msgs[i])
			continue
		}
		blocks := contentBlocks(msg["content"])
		if i < swapAt {
			kept := blocks[:0]
			for _, b := range blocks {
				if bm, ok := b.(map[string]any); ok && (bm["type"] == "thinking" || bm["type"] == "redacted_thinking") {
					continue
				}
				kept = append(kept, b)
			}
			blocks = kept
		}
		if i == p0 {
			blocks = append(append([]any{}, lead...), blocks...)
		}
		msg["content"] = blocks
		b, err := json.Marshal(msg)
		if err != nil {
			out = append(out, msgs[i])
			continue
		}
		out = append(out, b)
	}
	return out
}

// contentBlocks returns a message's content as a block list, turning the
// plain-string form into one text block.
func contentBlocks(c any) []any {
	switch v := c.(type) {
	case []any:
		return v
	case string:
		return []any{map[string]any{"type": "text", "text": v}}
	}
	return nil
}

// systemReminders returns the text blocks of a message that carry Claude
// Code's <system-reminder> context.
func systemReminders(m json.RawMessage) []any {
	var msg map[string]any
	if json.Unmarshal(m, &msg) != nil {
		return nil
	}
	var out []any
	for _, b := range contentBlocks(msg["content"]) {
		if bm, ok := b.(map[string]any); ok && bm["type"] == "text" {
			if t, _ := bm["text"].(string); strings.Contains(t, "<system-reminder>") {
				out = append(out, map[string]any{"type": "text", "text": t})
			}
		}
	}
	return out
}

// summaryFromText takes the text inside <summary></summary> when present,
// else the whole text.
func summaryFromText(s string) string {
	if i := strings.Index(s, "<summary>"); i >= 0 {
		rest := s[i+len("<summary>"):]
		if j := strings.Index(rest, "</summary>"); j >= 0 {
			rest = rest[:j]
		}
		return strings.TrimSpace(rest)
	}
	return strings.TrimSpace(s)
}

// endsInPrompt reports whether a request is a fresh turn: its last message,
// ignoring trailing mid-conversation system messages (Claude Code appends
// one after the prompt), is a plain user prompt rather than a tool result.
func endsInPrompt(msgs []json.RawMessage) bool {
	for i := len(msgs) - 1; i >= 0; i-- {
		var msg struct {
			Role string `json:"role"`
		}
		if json.Unmarshal(msgs[i], &msg) != nil {
			return false
		}
		if msg.Role == "system" {
			continue
		}
		b := promptBoundaries(msgs[i : i+1])
		return len(b) == 1
	}
	return false
}
