package router

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/automask"
)

// Automask for Codex. A Codex turn is a Responses API body: "instructions"
// is a string and "input" the whole history, sent again with every turn.
// The text of a message and the output of a tool call are masked with the
// same rules, word list and counts as Claude Code's. Tool calls the model
// wrote, encrypted reasoning and the tool list are never touched.

// codexMaskKey keeps a Codex session's masks apart from a Claude Code
// session that happened to have the same id.
func codexMaskKey(sid string) string { return "codex:" + sid }

// MaskCodex returns a Codex turn's body with personal data masked, or body
// itself when Automask does not cover ChatGPT or nothing matched. repo
// names the session's folder for the dashboard's list.
func (s *Server) MaskCodex(sid, repo string, body []byte) []byte {
	m := s.automask
	m.mu.Lock()
	cfg := m.cfg
	m.mu.Unlock()
	if !Covers(cfg, MaskChatGPT) {
		return body
	}
	on := func(r *automask.Rule) bool { return RuleOn(cfg, r) }

	var top map[string]rawRef
	if json.Unmarshal(body, &top) != nil {
		return body
	}
	var items []rawRef
	if raw, ok := top["input"]; ok && json.Unmarshal(raw, &items) != nil {
		// A plain string input, or a shape not known: the instructions
		// are still masked below.
		items = nil
	}

	key := codexMaskKey(sid)
	m.mu.Lock()
	ms := m.sessions[key]
	if ms == nil {
		ms = &maskSession{masks: automask.NewSessionFrom(m.key, nil, nil), cache: map[[20]byte][]byte{}, env: map[string]bool{}, used: time.Now()}
		ms.masks.SetWords(cfg.Words)
		m.sessions[key] = ms
		m.pruneSessions()
	}
	ms.used = time.Now()
	if len(ms.cache) > maskCacheMax {
		ms.cache = map[[20]byte][]byte{}
	}
	m.mu.Unlock()

	var hits []automask.Hit
	changed := false
	outs := make([]json.RawMessage, len(items))
	for i, ref := range items {
		raw := json.RawMessage(ref)
		out, h, c := m.cached(ms, raw, func() (json.RawMessage, []automask.Hit) {
			return maskCodexItem(ms.masks, raw, on, ms.env)
		})
		hits = append(hits, h...)
		outs[i] = out
		changed = changed || c
	}
	var ins json.RawMessage
	if ref, ok := top["instructions"]; ok {
		if out, h, c := maskContent(ms.masks, json.RawMessage(ref), "system", on, nil, false); c {
			ins = out
			hits = append(hits, h...)
			changed = true
		}
	}
	if !changed {
		return body
	}
	full := topOf(top)
	if items != nil {
		nb, err := json.Marshal(outs)
		if err != nil {
			return body
		}
		full["input"] = nb
	}
	if ins != nil {
		full["instructions"] = ins
	}
	nb, err := json.Marshal(full)
	if err != nil {
		return body
	}
	if len(hits) > 0 {
		s.noteMasked(sid, "Codex", repo, hits)
	}
	return nb
}

// maskCodexItem masks one input item: a message's text, or a tool call's
// output. A call that names a .env file is noted, so that its output has
// its values masked. Returns raw itself when nothing matched.
func maskCodexItem(ms *automask.Session, raw json.RawMessage, on func(*automask.Rule) bool, env map[string]bool) (json.RawMessage, []automask.Hit) {
	var it map[string]json.RawMessage
	if json.Unmarshal(raw, &it) != nil {
		return raw, nil
	}
	var typ, role, call string
	_ = json.Unmarshal(it["type"], &typ)
	_ = json.Unmarshal(it["role"], &role)
	_ = json.Unmarshal(it["call_id"], &call)
	field, where, inEnv := "", role, false
	switch {
	case typ == "message" || (typ == "" && role != ""):
		field = "content"
	case strings.HasSuffix(typ, "_call_output"):
		field, where, inEnv = "output", "tool_result", env[call]
	case strings.HasSuffix(typ, "_call"):
		if call != "" && envFileNamed.Match(raw) {
			env[call] = true
		}
		return raw, nil
	default:
		return raw, nil
	}
	v, ok := it[field]
	if !ok {
		return raw, nil
	}
	out, hits, changed := maskCodexContent(ms, v, where, on, inEnv)
	if !changed {
		return raw, hits
	}
	it[field] = out
	nb, err := json.Marshal(it)
	if err != nil {
		return raw, nil
	}
	return nb, hits
}

// maskCodexContent masks a string, or the text of input_text and
// output_text blocks. Images and anything else are left as they are.
func maskCodexContent(ms *automask.Session, c json.RawMessage, where string, on func(*automask.Rule) bool, inEnv bool) (json.RawMessage, []automask.Hit, bool) {
	var str string
	if json.Unmarshal(c, &str) == nil {
		return maskContent(ms, c, where, on, nil, inEnv)
	}
	var blocks []json.RawMessage
	if json.Unmarshal(c, &blocks) != nil {
		return c, nil, false
	}
	var hits []automask.Hit
	any := false
	for i, b := range blocks {
		var blk map[string]json.RawMessage
		if json.Unmarshal(b, &blk) != nil {
			continue
		}
		var typ string
		_ = json.Unmarshal(blk["type"], &typ)
		if typ != "input_text" && typ != "output_text" && typ != "text" {
			continue
		}
		out, h, changed := maskContent(ms, blk["text"], where, on, nil, inEnv)
		hits = append(hits, h...)
		if !changed {
			continue
		}
		blk["text"] = out
		nb, err := json.Marshal(blk)
		if err != nil {
			continue
		}
		blocks[i] = nb
		any = true
	}
	if !any {
		return c, hits, false
	}
	nb, err := json.Marshal(blocks)
	if err != nil {
		return c, nil, false
	}
	return nb, hits, true
}
