package router

import (
	"encoding/json"
	"path/filepath"
	"strings"

	"github.com/andrewbakercloudscale/claude-burst/internal/ctxview"
)

// Items removed from a session's context in the inspector are left out of
// every request forwarded for it, a one-line note in their place. Claude
// Code's own history keeps them, so Restore simply stops leaving them out.

func removalsPath(statePath string) string {
	if statePath == "" {
		return ""
	}
	return ctxview.RemovalsPath(filepath.Dir(statePath))
}

// RemovalKey is the store key of a Claude Code session.
func RemovalKey(sid string) string { return "claude:" + sid }

// Removals is the store the inspector's Remove and Restore write.
func (s *Server) Removals() *ctxview.Store { return s.removals }

// applyRemovals returns body with sid's removed items replaced by their
// notes, or body itself when there are none or none are present.
func (s *Server) applyRemovals(sid string, body []byte) []byte {
	if sid == "" {
		return body
	}
	removed := s.removals.For(RemovalKey(sid))
	if len(removed) == 0 {
		return body
	}
	var top map[string]json.RawMessage
	if json.Unmarshal(body, &top) != nil {
		return body
	}
	var msgs []json.RawMessage
	if json.Unmarshal(top["messages"], &msgs) != nil {
		return body
	}
	changed := false
	for i, raw := range msgs {
		if out, ok := removeInMessage(raw, removed); ok {
			msgs[i], changed = out, true
		}
	}
	if !changed {
		return body
	}
	nm, err := json.Marshal(msgs)
	if err != nil {
		return body
	}
	top["messages"] = nm
	nb, err := json.Marshal(top)
	if err != nil {
		return body
	}
	return nb
}

// removeInMessage applies removals to one message: tool results whole, and
// the reminders (instruction files, skills, the rest) inside a user's text.
// Everything else in the message is kept byte for byte.
func removeInMessage(raw json.RawMessage, removed map[string]ctxview.Removal) (json.RawMessage, bool) {
	var msg map[string]json.RawMessage
	if json.Unmarshal(raw, &msg) != nil {
		return raw, false
	}
	var role string
	_ = json.Unmarshal(msg["role"], &role)
	if role != "user" {
		return raw, false
	}
	var str string
	if json.Unmarshal(msg["content"], &str) == nil {
		out, ok := removeInUserText(str, removed)
		if !ok {
			return raw, false
		}
		msg["content"], _ = json.Marshal(out)
		b, err := json.Marshal(msg)
		return b, err == nil
	}
	var blocks []map[string]json.RawMessage
	if json.Unmarshal(msg["content"], &blocks) != nil {
		return raw, false
	}
	changed := false
	for _, b := range blocks {
		var typ string
		_ = json.Unmarshal(b["type"], &typ)
		switch typ {
		case "text":
			var t string
			if json.Unmarshal(b["text"], &t) != nil {
				continue
			}
			if out, ok := removeInUserText(t, removed); ok {
				b["text"], _ = json.Marshal(out)
				changed = true
			}
		case "tool_result":
			text := strings.Join(textsOf(b["content"]), "\n")
			if r, ok := removed[ctxview.ID(grpResults, text)]; ok {
				b["content"], _ = json.Marshal(ctxview.Stub(r.Name, len(text), r.ID))
				changed = true
			}
		}
	}
	if !changed {
		return raw, false
	}
	msg["content"], _ = json.Marshal(blocks)
	b, err := json.Marshal(msg)
	return b, err == nil
}

// removeInUserText finds the items the inspector shows for this text and
// replaces each removed one with its note.
func removeInUserText(text string, removed map[string]ctxview.Removal) (string, bool) {
	if !strings.Contains(text, "<system-reminder>") {
		return text, false
	}
	var hits []ctxview.Removal
	var parts []string
	var items []ContextItem
	addUserText(&items, 0, text, func(group, name string, turn int, t string) {
		if !ctxview.Removable(group) {
			return
		}
		if r, ok := removed[ctxview.ID(group, t)]; ok && t != "" {
			hits = append(hits, r)
			parts = append(parts, t)
		}
	})
	if len(hits) == 0 {
		return text, false
	}
	for i, r := range hits {
		text = strings.Replace(text, parts[i], ctxview.Stub(r.Name, len(parts[i]), r.ID), 1)
	}
	return text, true
}
