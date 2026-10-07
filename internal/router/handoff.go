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
	// Full: the summary went in and the session still sends its limit or
	// more, so the mod is to compact Claude Code's own history with it now
	// (noteSessionContext).
	Full bool `json:"full,omitempty"`
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

// handoffLeftBehind: a conversation of a session not asked for this long,
// while another of its conversations was, is no longer the session's.
const handoffLeftBehind = 30 * time.Minute

func handoffDir(statePath string) string {
	if statePath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(statePath), "handoff")
}

// supersede takes the hand-off from every other conversation of this session
// whose summary this history opens with: Claude Code took that summary in
// (/compact-async-full), so it is in the session already and the mod would
// refuse it. A session's file is the hand-off covering the most messages, and
// on 2026-10-06 that kept a summary of 524 messages on offer after Claude
// Code had adopted it, over the summary of 169 made of the history that
// followed, so the next full compaction had nothing to give. Reports whether
// anything changed. Caller holds c.mu.
func (c *compactor) supersede(key string, first json.RawMessage) bool {
	sid, _, _ := strings.Cut(key, "|")
	text, read := "", false
	changed := false
	for k, d := range c.sessions {
		if k == key || d.hand == nil || d.summary == "" || !strings.HasPrefix(k, sid+"|") {
			continue
		}
		if !read {
			read = true
			var msg map[string]any
			if json.Unmarshal(first, &msg) != nil {
				return false
			}
			var b strings.Builder
			for _, blk := range contentBlocks(msg["content"]) {
				bm, _ := blk.(map[string]any)
				t, _ := bm["text"].(string)
				b.WriteString(t)
				b.WriteByte('\n')
			}
			text = b.String()
		}
		if strings.Contains(text, d.summary) {
			d.hand = nil
			changed = true
		}
	}
	return changed
}

// writeHandoffs makes the handoff directory hold exactly the summaries in
// force: one file per session, and none for a session whose summary was
// dropped, which must never be handed to Claude Code. Caller holds c.mu.
func (c *compactor) handoffFiles() (dir string, files map[string][]byte) {
	dir = handoffDir(c.path)
	if dir == "" {
		return "", nil
	}
	best := map[string]*Handoff{}
	// A conversation the session left behind (Claude Code took its summary
	// in, and the history has a new first message) is not asked for again
	// while the session goes on: its hand-off must not outlast it.
	latest := map[string]time.Time{}
	for k, st := range c.sessions {
		if sid, _, _ := strings.Cut(k, "|"); st.seen.After(latest[sid]) {
			latest[sid] = st.seen
		}
	}
	for k, st := range c.sessions {
		if st.hand == nil || st.summary == "" || st.swapAt == 0 || st.hand.Of != handoffOf(st.summary, st.p0) {
			continue
		}
		sid, _, _ := strings.Cut(k, "|")
		if latest[sid].Sub(st.seen) > handoffLeftBehind {
			continue
		}
		if st.rawContext > st.hand.Raw {
			st.hand.Raw = st.rawContext
		}
		// A session's subagents share its id. The main conversation is the
		// one whose summary covers the most.
		if b := best[sid]; b == nil || st.hand.Messages > b.Messages {
			best[sid] = st.hand
		}
	}
	files = map[string][]byte{}
	for sid, h := range best {
		if strings.ContainsAny(sid, "/\\") || sid == "" {
			continue
		}
		if b, err := json.Marshal(h); err == nil {
			files[sid] = b
		}
	}
	return dir, files
}

// writeHandoffFiles makes dir hold exactly files: one hand-off a session,
// and none for a session that has none. Disk only, so it runs off the
// compaction lock.
func writeHandoffFiles(dir string, files map[string][]byte) {
	if dir == "" {
		return
	}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if _, ok := files[strings.TrimSuffix(e.Name(), ".json")]; !ok {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
	if len(files) == 0 {
		return
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	for sid, b := range files {
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
