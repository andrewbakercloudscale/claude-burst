package router

import (
	"bytes"
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
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
	totals   map[string]int      // rule -> values masked since `since`
	since    time.Time           // when the counting began
	recent   []AutomaskHit       // the last maskRecentMax new masks, oldest first
	// path is where the above survives a restart, "" for memory only; key
	// is what each session's table is hashed with, so no value is in it.
	path    string
	key     []byte
	logger  *log.Logger
	seq     uint64 // saves decided, guarded by mu
	writeMu sync.Mutex
	written uint64 // guarded by writeMu
}

// savedMasker is the masker on disk. Until 8 Oct 2026 it was memory only,
// and every restart began each session's masks at 1 again: a session that
// sends only what is new could then be given [APIKEY-1] for a second key
// while the history the API holds used it for the first, and a session
// that sends its history whole was told of every old value once more.
// The values are not here: each is filed under a hash keyed with
// automask.key, a file beside this one.
type savedMasker struct {
	Since    time.Time                   `json:"since"`
	Totals   map[string]int              `json:"totals,omitempty"`
	Recent   []AutomaskHit               `json:"recent,omitempty"`
	Sessions map[string]savedMaskSession `json:"sessions,omitempty"`
}

type savedMaskSession struct {
	Masks  map[string]string `json:"masks"`
	Counts map[string]int    `json:"counts"`
	Used   time.Time         `json:"used"`
}

// automaskStatePath is where the masker is kept, beside the gateway state.
func automaskStatePath(statePath string) string {
	if statePath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(statePath), "automask-state.json")
}

// maskKey reads the key the tables are hashed with, making it the first
// time. nil when it can be neither read nor made: the masker then keeps
// everything in memory, as it did before.
func maskKey(path string) []byte {
	if b, err := os.ReadFile(path); err == nil {
		if k, err := hex.DecodeString(string(bytes.TrimSpace(b))); err == nil && len(k) == 32 {
			return k
		}
	}
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		return nil
	}
	if os.WriteFile(path, []byte(hex.EncodeToString(k)+"\n"), 0600) != nil {
		return nil
	}
	return k
}

func (m *masker) load() {
	if m.path == "" {
		return
	}
	if m.key = maskKey(filepath.Join(filepath.Dir(m.path), "automask.key")); m.key == nil {
		if m.logger != nil {
			m.logger.Printf("error stage=automask_load path=%s: no key could be read or written beside it, so masks are kept in memory only", m.path)
		}
		m.path = ""
		return
	}
	b, err := os.ReadFile(m.path)
	if err != nil {
		return
	}
	var saved savedMasker
	if err := json.Unmarshal(b, &saved); err != nil {
		if m.logger != nil {
			m.logger.Printf("error stage=automask_load path=%s err=%v (starting with no masks)", m.path, err)
		}
		return
	}
	if !saved.Since.IsZero() {
		m.since = saved.Since
	}
	for k, v := range saved.Totals {
		m.totals[k] = v
	}
	m.recent = saved.Recent
	for sid, sv := range saved.Sessions {
		ms := &maskSession{masks: automask.NewSessionFrom(m.key, sv.Masks, sv.Counts), cache: map[[20]byte][]byte{}, env: map[string]bool{}, used: sv.Used}
		ms.masks.SetWords(m.cfg.Words)
		m.sessions[sid] = ms
	}
	m.pruneSessions()
}

// snapshot is the masker as it goes to disk, nil when it is memory only.
// Called with mu held.
func (m *masker) snapshot() ([]byte, uint64) {
	if m.path == "" {
		return nil, 0
	}
	out := savedMasker{Since: m.since, Totals: m.totals, Recent: m.recent, Sessions: map[string]savedMaskSession{}}
	for sid, ms := range m.sessions {
		masks, counts := ms.masks.Snapshot()
		if len(masks) == 0 {
			continue
		}
		out.Sessions[sid] = savedMaskSession{Masks: masks, Counts: counts, Used: ms.used}
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil, 0
	}
	m.seq++
	return b, m.seq
}

// write puts a snapshot on disk, off the lock. Two can arrive out of
// order: the older loses.
func (m *masker) write(b []byte, seq uint64) {
	if b == nil {
		return
	}
	m.writeMu.Lock()
	defer m.writeMu.Unlock()
	if seq < m.written {
		return
	}
	m.written = seq
	tmp := m.path + ".tmp"
	err := os.WriteFile(tmp, b, 0600)
	if err == nil {
		err = os.Rename(tmp, m.path)
	}
	if err != nil && m.logger != nil {
		m.logger.Printf("error stage=automask_save path=%s err=%v", m.path, err)
	}
}

// AutomaskSince is when the counts of AutomaskTotals began.
func (s *Server) AutomaskSince() time.Time {
	s.automask.mu.Lock()
	defer s.automask.mu.Unlock()
	return s.automask.since
}

// AutomaskHit is one new mask as the dashboard lists it. The mask only:
// the value is never kept.
type AutomaskHit struct {
	At      time.Time `json:"at"`
	Session string    `json:"session"`
	Source  string    `json:"source,omitempty"` // "Codex"; empty is Claude Code
	Repo    string    `json:"repo,omitempty"`
	Rule    string    `json:"rule"`
	Name    string    `json:"name"`
	Where   string    `json:"where"`
	Mask    string    `json:"mask"`
}

const maskRecentMax = 50

// AutomaskRecent is the last 50 new masks, newest first.
func (s *Server) AutomaskRecent() []AutomaskHit {
	s.automask.mu.Lock()
	defer s.automask.mu.Unlock()
	out := make([]AutomaskHit, 0, len(s.automask.recent))
	for i := len(s.automask.recent) - 1; i >= 0; i-- {
		out = append(out, s.automask.recent[i])
	}
	return out
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

func newMasker(c config.AutomaskConfig, path string, logger *log.Logger) *masker {
	m := &masker{cfg: c, sessions: map[string]*maskSession{}, notices: map[string][]string{}, totals: map[string]int{},
		since: time.Now(), path: path, logger: logger}
	m.load()
	return m
}

// SetAutomask applies c to the running gateway; the dashboard's switches.
// Cached scans were made under the old rules, so they go.
func (s *Server) SetAutomask(c config.AutomaskConfig) {
	s.automask.mu.Lock()
	defer s.automask.mu.Unlock()
	s.automask.cfg = c
	for _, ms := range s.automask.sessions {
		ms.cache = map[[20]byte][]byte{}
		ms.masks.SetWords(c.Words)
	}
}

// AutomaskTotals is how many values each rule has masked since
// AutomaskSince, for the dashboard.
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

// The providers Automask can be limited to (config automask.providers).
const (
	MaskAnthropic = "anthropic"
	MaskSecondary = "secondary"
	MaskChatGPT   = "chatgpt"
)

// MaskProviders is every provider, in the order the dashboard lists them.
var MaskProviders = []string{MaskAnthropic, MaskSecondary, MaskChatGPT}

// Covers reports whether Automask under c applies to what goes to
// provider: it is on, and either applies everywhere or names it.
func Covers(c config.AutomaskConfig, provider string) bool {
	if !c.Enabled {
		return false
	}
	if len(c.Providers) == 0 {
		return true
	}
	for _, p := range c.Providers {
		if p == provider {
			return true
		}
	}
	return false
}

// AutomaskCovers is Covers for the running gateway's settings.
func (s *Server) AutomaskCovers(provider string) bool {
	s.automask.mu.Lock()
	defer s.automask.mu.Unlock()
	return Covers(s.automask.cfg, provider)
}

// applyAutomask masks a Claude Code request before anything else reads it,
// when what is sent to Anthropic is to be masked. The summary, the
// compaction hash and a secondary that takes over all then see the masked
// history, whether or not the secondary is named itself.
func (s *Server) applyAutomask(in *http.Request, body []byte) []byte {
	return s.applyAutomaskFor(in, body, MaskAnthropic)
}

// applyAutomaskAtSecondary masks a request on its way to the secondary
// when Automask covers the secondary and not Anthropic: the history was
// left as written for Anthropic, so it is masked here, for this request
// only, and nothing kept about the conversation changes.
func (s *Server) applyAutomaskAtSecondary(in *http.Request, body []byte) []byte {
	if body == nil || !isInference(in.URL.Path) || s.AutomaskCovers(MaskAnthropic) {
		return body
	}
	return s.applyAutomaskFor(in, body, MaskSecondary)
}

// applyAutomaskFor returns body with personal data masked, or body itself
// when automask does not cover provider or nothing matched. It never
// refuses a request.
func (s *Server) applyAutomaskFor(in *http.Request, body []byte, provider string) []byte {
	m := s.automask
	m.mu.Lock()
	cfg := m.cfg
	m.mu.Unlock()
	if !Covers(cfg, provider) {
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
		ms = &maskSession{masks: automask.NewSessionFrom(m.key, nil, nil), cache: map[[20]byte][]byte{}, env: map[string]bool{}, used: time.Now()}
		ms.masks.SetWords(cfg.Words)
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
		s.noteMasked(sid, "", "", hits)
	}
	return nb
}

// noteMasked logs each new mask (the mask only, never the value) and says
// so in the session and on the panel.
// source is "" for Claude Code, whose repository is looked up, or "Codex",
// which names its own.
func (s *Server) noteMasked(sid, source, repo string, hits []automask.Hit) {
	from, reader := "", "Claude"
	if source != "" {
		from, reader = " source="+strings.ToLower(source), "ChatGPT"
	}
	for _, h := range hits {
		s.logger.Printf("automask session=%s%s rule=%s where=%s mask=%s", sid, from, h.Rule.ID, h.Where, h.Mask)
	}
	sum := automask.Summary(hits)
	if source == "" {
		repo, _ = s.repos.Resolve(sid)
	}
	now := time.Now()
	s.automask.mu.Lock()
	for _, h := range hits {
		s.automask.totals[h.Rule.ID]++
		s.automask.recent = append(s.automask.recent, AutomaskHit{At: now, Session: sid, Source: source, Repo: repo,
			Rule: h.Rule.ID, Name: h.Rule.Name, Where: h.Where, Mask: h.Mask})
	}
	if n := len(s.automask.recent); n > maskRecentMax {
		s.automask.recent = append([]AutomaskHit(nil), s.automask.recent[n-maskRecentMax:]...)
	}
	// Under the prompt in Claude Code only: Codex has no such line.
	if sid != "" && source == "" {
		s.automask.notices[sid] = append(s.automask.notices[sid],
			"⚡ Claude Burst, automask: masked "+sum+" before sending. Claude sees only the masks; the originals never leave this Mac")
	}
	// A table changes only here, so this is every save it needs.
	b, seq := s.automask.snapshot()
	s.automask.mu.Unlock()
	s.automask.write(b, seq)
	where := ""
	if repo != "" {
		where = " in " + repo
	}
	notice.PublishFor(sid, "automask", notice.Warn, "Sensitive data masked",
		"Masked "+sum+where+" before it left this Mac. "+reader+" sees only the masks.")
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
