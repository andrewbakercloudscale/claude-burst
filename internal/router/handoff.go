package router

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Hand-off: Burst's summary, written where the Burst mod can read it
// with the gateway down, so Claude Code can take the summary as its own
// compaction instead of writing another.
//
// Burst compacts what it sends and Claude Code keeps everything, so a
// session Burst holds at 135k sends all 994k the moment Burst is out of the
// path ($7.75 for one turn on 2026-10-04). Claude Code's own /compact would
// shorten its copy, but it reads the whole history to do it and stops the
// session while it does. The summary Burst already has covers the same
// messages: the mod answers Claude Code's compaction with it, which costs no
// request and no pause.
//
// The mod sees Claude Code's transcript, not the request, so the cut cannot
// be a message count. It is two anchors instead: the last message the
// summary covers and the first one kept, each named by a tool call id (unique
// in a conversation) or, without one, by the start of its text. A message
// too short to be named that way is the one right after a message that is.

// handAnchor names one message of the conversation.
type handAnchor struct {
	Role string `json:"role"`
	// Tool is the id of the message's first tool call (a reply) or of the
	// first call it answers (tool results).
	Tool string `json:"tool,omitempty"`
	// Text is the start of what the message says, whitespace removed, without
	// Claude Code's <system-reminder> blocks.
	Text string `json:"text,omitempty"`
}

// Handoff is one session's file in ~/.config/claude-burst/handoff.
type Handoff struct {
	Session string `json:"session"`
	// Lead is the message that stands for everything before First.
	Lead  string     `json:"lead"`
	Last  handAnchor `json:"last"`
	First handAnchor `json:"first"`
	// Messages is how many messages of the request the summary replaced.
	Messages int `json:"messages"`
	// Raw is Claude Code's own history in tokens when this was written.
	Raw int64     `json:"raw,omitempty"`
	At  time.Time `json:"at"`
	// Of ties the hand-off to the summary it was built from.
	Of string `json:"of"`
}

// handAnchorText is how much of a message's text names it.
const handAnchorText = 120

// handAnchorMin is the least text that can name a message by itself: "yes"
// is said many times in a conversation.
const handAnchorMin = 16

func handoffOf(summary string, p0 int) string {
	h := sha256.Sum256([]byte(strconv.Itoa(p0) + "|" + summary))
	return hex.EncodeToString(h[:8])
}

func anchorOf(m json.RawMessage) handAnchor {
	var msg map[string]any
	if json.Unmarshal(m, &msg) != nil {
		return handAnchor{}
	}
	a := handAnchor{}
	a.Role, _ = msg["role"].(string)
	for _, b := range contentBlocks(msg["content"]) {
		bm, ok := b.(map[string]any)
		if !ok {
			continue
		}
		switch bm["type"] {
		case "tool_use":
			if a.Tool == "" {
				a.Tool, _ = bm["id"].(string)
			}
		case "tool_result":
			if a.Tool == "" {
				a.Tool, _ = bm["tool_use_id"].(string)
			}
		case "text":
			t, _ := bm["text"].(string)
			if a.Text != "" || strings.Contains(t, "<system-reminder>") {
				continue
			}
			var sb strings.Builder
			n := 0
			for _, r := range t {
				if unicode.IsSpace(r) {
					continue
				}
				sb.WriteRune(r)
				if n++; n == handAnchorText {
					break
				}
			}
			a.Text = sb.String()
		}
	}
	return a
}

func (a handAnchor) names() bool { return a.Tool != "" || len(a.Text) >= handAnchorMin }

// buildHandoff describes the cut at p0 for the mod, or nil when the cut
// cannot be named, or the first message kept is one Claude Code could not start a
// conversation with (tool results whose calls are in the summary).
func buildHandoff(sid string, msgs []json.RawMessage, summary string, p0 int, raw int64, now time.Time) *Handoff {
	if summary == "" || p0 <= 0 || p0 >= len(msgs) {
		return nil
	}
	first, last := anchorOf(msgs[p0]), anchorOf(msgs[p0-1])
	// A short message ("Done.", "do both") cannot name itself, but the one
	// just before it can: the mod then looks for the pair. Until 5 Oct 2026
	// only tool results counted as that message, so a cut at a short prompt
	// after a reply in words had no hand-off at all: a session holding
	// 1,320k had none to give when it was closed, because its last
	// compaction cut at "do both".
	if !(first.names() || last.names()) || (first.Role == "user" && first.Tool != "") {
		return nil
	}
	lead := "<system-reminder>\nThe earlier part of this conversation was compacted by claude-burst to save context. Summary of it:\n<summary>\n" +
		summary + "\n</summary>\n</system-reminder>"
	if first.Role != "user" {
		if t := latestPromptText(msgs[:p0]); t != "" {
			lead += "\n<system-reminder>\nThe user's most recent message before this point, word for word (the summary above covers the work done on it so far):\n<latest-user-message>\n" +
				t + "\n</latest-user-message>\n</system-reminder>"
		}
	}
	return &Handoff{Session: sid, Lead: lead, Last: last, First: first, Messages: p0, Raw: raw, At: now, Of: handoffOf(summary, p0)}
}

func handoffDir(statePath string) string {
	if statePath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(statePath), "handoff")
}

// writeHandoffs makes the handoff directory hold exactly the summaries in
// force: one file per session, and none for a session whose summary was
// dropped, which must never be handed to Claude Code. Caller holds c.mu.
func (c *compactor) writeHandoffs() {
	dir := handoffDir(c.path)
	if dir == "" {
		return
	}
	best := map[string]*Handoff{}
	for k, st := range c.sessions {
		if st.hand == nil || st.summary == "" || st.swapAt == 0 || st.hand.Of != handoffOf(st.summary, st.p0) {
			continue
		}
		sid, _, _ := strings.Cut(k, "|")
		if st.rawContext > st.hand.Raw {
			st.hand.Raw = st.rawContext
		}
		// A session's subagents share its id. The main conversation is the
		// one whose summary covers the most.
		if b := best[sid]; b == nil || st.hand.Messages > b.Messages {
			best[sid] = st.hand
		}
	}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if sid := strings.TrimSuffix(e.Name(), ".json"); best[sid] == nil {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
	if len(best) == 0 {
		return
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	for sid, h := range best {
		if strings.ContainsAny(sid, "/\\") || sid == "" {
			continue
		}
		b, err := json.Marshal(h)
		if err != nil {
			continue
		}
		p := filepath.Join(dir, sid+".json")
		if old, err := os.ReadFile(p); err == nil && string(old) == string(b) {
			continue
		}
		tmp := p + ".tmp"
		if os.WriteFile(tmp, b, 0o600) == nil {
			_ = os.Rename(tmp, p)
		}
	}
}
