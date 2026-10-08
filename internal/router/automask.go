package router

import (
	"bytes"
	"crypto/sha1"
	"encoding/json"
	"net/http"
	"regexp"
	"sync"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/automask"
	"github.com/andrewbakercloudscale/claude-burst/internal/config"
	"github.com/andrewbakercloudscale/claude-burst/internal/notice"
)

// masker is automask's state in the gateway: the switches, each session's
// masks, and a cache of messages already scanned. Claude Code resends the
// whole conversation every request, so only messages not seen before are
// scanned; the rest reuse their masked bytes.
type masker struct {
	mu       sync.Mutex
	cfg      config.AutomaskConfig
	sessions map[string]*maskSession
	notices  map[string][]string // session -> lines for under the prompt
	totals   map[string]int      // rule -> values masked since the gateway started
}

type maskSession struct {
	masks *automask.Session
	cache map[[20]byte][]byte // raw message -> what to send (itself when clean)
	env   map[string]bool     // tool calls that name a .env file, by id
	used  time.Time
}

// maskCacheMax bounds one session's cache; past it the cache starts again
// (the masks themselves are kept, so nothing changes on the wire).
const maskCacheMax = 20000

func newMasker(c config.AutomaskConfig) *masker {
	return &masker{cfg: c, sessions: map[string]*maskSession{}, notices: map[string][]string{}, totals: map[string]int{}}
}

// SetAutomask applies c to the running gateway; the dashboard's switches.
// Cached scans were made under the old rules, so they go.
func (s *Server) SetAutomask(c config.AutomaskConfig) {
	s.automask.mu.Lock()
	defer s.automask.mu.Unlock()
	s.automask.cfg = c
	for _, ms := range s.automask.sessions {
		ms.cache = map[[20]byte][]byte{}
	}
}

// AutomaskTotals is how many values each rule has masked since the gateway
// started, for the dashboard.
func (s *Server) AutomaskTotals() map[string]int {
	s.automask.mu.Lock()
	defer s.automask.mu.Unlock()
	out := make(map[string]int, len(s.automask.totals))
	for k, v := range s.automask.totals {
		out[k] = v
	}
	return out
}

// RuleOn reports whether rule r is on under c.
func RuleOn(c config.AutomaskConfig, r *automask.Rule) bool {
	if v, ok := c.Rules[r.ID]; ok {
		return v
	}
	return r.Default
}

func (s *Server) takeAutomaskNotices(sid string) []string {
	s.automask.mu.Lock()
	defer s.automask.mu.Unlock()
	out := s.automask.notices[sid]
	delete(s.automask.notices, sid)
	return out
}

// applyAutomask returns body with personal data masked, or body itself when
// automask is off or nothing matched. It never refuses a request.
func (s *Server) applyAutomask(in *http.Request, body []byte) []byte {
	m := s.automask
	m.mu.Lock()
	cfg := m.cfg
	m.mu.Unlock()
	if !cfg.Enabled {
		return body
	}
	on := func(r *automask.Rule) bool { return RuleOn(cfg, r) }
	sid := in.Header.Get("x-claude-code-session-id")

	var top map[string]json.RawMessage
	if json.Unmarshal(body, &top) != nil {
		return body
	}
	var msgs []json.RawMessage
	if json.Unmarshal(top["messages"], &msgs) != nil {
		return body
	}

	m.mu.Lock()
	ms := m.sessions[sid]
	if ms == nil {
		ms = &maskSession{masks: automask.NewSession(), cache: map[[20]byte][]byte{}, env: map[string]bool{}, used: time.Now()}
		m.sessions[sid] = ms
		m.pruneSessions()
	}
	ms.used = time.Now()
	if len(ms.cache) > maskCacheMax {
		ms.cache = map[[20]byte][]byte{}
	}
	m.mu.Unlock()

	var hits []automask.Hit
	changed := false
	for i, raw := range msgs {
		key := sha1.Sum(raw)
		m.mu.Lock()
		out, ok := ms.cache[key]
		m.mu.Unlock()
		if !ok {
			var h []automask.Hit
			// Messages are scanned in order, so a tool call is noted
			// before the message that carries its output.
			noteEnvCalls(ms.env, raw)
			out, h = maskMessage(ms.masks, raw, on, ms.env)
			hits = append(hits, h...)
			m.mu.Lock()
			ms.cache[key] = out
			m.mu.Unlock()
		}
		if len(out) != len(raw) || string(out) != string(raw) {
			msgs[i] = out
			changed = true
		}
	}
	if sys, ok := top["system"]; ok {
		if out, h, c := maskContent(ms.masks, sys, "system", on, nil, false); c {
			top["system"] = out
			hits = append(hits, h...)
			changed = true
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
	if len(hits) > 0 {
		s.noteMasked(sid, hits)
	}
	return nb
}

// noteMasked logs each new mask (the mask only, never the value) and says
// so in the session and on the panel.
func (s *Server) noteMasked(sid string, hits []automask.Hit) {
	for _, h := range hits {
		s.logger.Printf("automask session=%s rule=%s where=%s mask=%s", sid, h.Rule.ID, h.Where, h.Mask)
	}
	sum := automask.Summary(hits)
	s.automask.mu.Lock()
	for _, h := range hits {
		s.automask.totals[h.Rule.ID]++
	}
	if sid != "" {
		s.automask.notices[sid] = append(s.automask.notices[sid],
			"⚡ Claude Burst, automask: masked "+sum+" before sending. Claude sees only the masks; the originals never leave this Mac")
	}
	s.automask.mu.Unlock()
	where := ""
	if name, _ := s.repos.Resolve(sid); name != "" {
		where = " in " + name
	}
	notice.PublishFor(sid, "automask", notice.Warn, "Sensitive data masked",
		"Masked "+sum+where+" before it left this Mac. Claude sees only the masks.")
}

// pruneSessions drops sessions idle for a day. Called with mu held.
func (m *masker) pruneSessions() {
	cut := time.Now().Add(-24 * time.Hour)
	for k, ms := range m.sessions {
		if ms.used.Before(cut) {
			delete(m.sessions, k)
		}
	}
}

// envFileNamed finds a .env file named in a tool call's input: .env,
// .env.local, config/.env.production. Not .envrc, and not "environment".
var envFileNamed = regexp.MustCompile(`(?:^|[/\s"'=])\.env(?:\.[A-Za-z0-9_-]+)*(?:["'\s\\]|$)`)

// noteEnvCalls records the tool calls in raw that name a .env file, so the
// message that carries their output has its values masked.
func noteEnvCalls(env map[string]bool, raw json.RawMessage) {
	if !bytes.Contains(raw, []byte(`.env`)) || !bytes.Contains(raw, []byte(`"tool_use"`)) {
		return
	}
	var msg struct {
		Content []struct {
			Type  string          `json:"type"`
			ID    string          `json:"id"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
	}
	if json.Unmarshal(raw, &msg) != nil {
		return
	}
	for _, b := range msg.Content {
		if b.Type == "tool_use" && b.ID != "" && envFileNamed.Match(b.Input) {
			env[b.ID] = true
		}
	}
}

// maskMessage masks one message's text: plain string content, text blocks
// and tool results. Thinking blocks, tool calls, images and cache_control
// are never touched. Returns raw itself when nothing matched.
func maskMessage(ms *automask.Session, raw json.RawMessage, on func(*automask.Rule) bool, env map[string]bool) (json.RawMessage, []automask.Hit) {
	var msg map[string]json.RawMessage
	if json.Unmarshal(raw, &msg) != nil {
		return raw, nil
	}
	var role string
	_ = json.Unmarshal(msg["role"], &role)
	out, hits, changed := maskContent(ms, msg["content"], role, on, env, false)
	if !changed {
		return raw, hits
	}
	msg["content"] = out
	nb, err := json.Marshal(msg)
	if err != nil {
		return raw, nil
	}
	return nb, hits
}

// maskContent masks a content value: a string, or an array of blocks.
// inEnv says c is the output of a tool call that named a .env file.
func maskContent(ms *automask.Session, c json.RawMessage, where string, on func(*automask.Rule) bool, env map[string]bool, inEnv bool) (json.RawMessage, []automask.Hit, bool) {
	var str string
	if json.Unmarshal(c, &str) == nil {
		mask := ms.Mask
		if inEnv {
			mask = ms.MaskEnvFile
		}
		out, hits, changed := mask(str, where, on)
		if !changed {
			return c, hits, false
		}
		nb, err := json.Marshal(out)
		if err != nil {
			return c, nil, false
		}
		return nb, hits, true
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
		field, w, e := "", where, inEnv
		switch typ {
		case "text":
			field = "text"
		case "tool_result":
			field, w = "content", "tool_result"
			var id string
			_ = json.Unmarshal(blk["tool_use_id"], &id)
			e = env[id]
		default:
			continue
		}
		v, ok := blk[field]
		if !ok {
			continue
		}
		out, h, changed := maskContent(ms, v, w, on, env, e)
		hits = append(hits, h...)
		if !changed {
			continue
		}
		blk[field] = out
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
