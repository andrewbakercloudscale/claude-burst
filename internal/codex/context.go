package codex

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/ctxview"
)

// Codex's side of the context inspector: the latest request of each Codex
// session, kept IN MEMORY ONLY as Claude Code's are, split into items, and
// the items removed in the dashboard left out of what goes to ChatGPT.
//
// A Codex request is a Responses API body: "input" is the whole history
// (developer instructions, AGENTS.md, the user's prompts, the model's
// messages, tool calls and their outputs, encrypted reasoning), sent again
// with every turn.

type captured struct {
	body []byte
	at   time.Time
}

type inspectStore struct {
	mu   sync.Mutex
	reqs map[string]*captured // session id -> latest request
}

const (
	inspectKeep    = 24 * time.Hour
	inspectMaxReqs = 40
	// inspectMaxBody: a request this large is passed through untouched,
	// neither read for the inspector nor edited.
	inspectMaxBody = 64 << 20
)

// inspectLimit is inspectMaxBody; a variable so a test need not send 64MB.
var inspectLimit int64 = inspectMaxBody

// RemovalKey is the store key of a Codex session.
func RemovalKey(sid string) string { return "codex:" + sid }

// Removals is the store the inspector's Remove and Restore write.
func (g *Gateway) Removals() *ctxview.Store { return g.removals }

// prepareTurn applies the session's removals to a turn's body and keeps it
// for the inspector. Anything it cannot read (compressed, too large, not
// JSON) goes on exactly as Codex sent it.
func (g *Gateway) prepareTurn(r *http.Request) {
	sid := r.Header.Get("Session-Id")
	if sid == "" || r.Body == nil || r.Header.Get("Content-Encoding") != "" || r.ContentLength > inspectLimit {
		return
	}
	orig := r.Body
	body, err := io.ReadAll(io.LimitReader(orig, inspectLimit+1))
	if err != nil {
		_ = orig.Close()
		r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(body), errReader{err}))
		return
	}
	if int64(len(body)) > inspectLimit {
		// A body with no declared length (chunked) that turns out larger
		// than the limit: what was read goes on, followed by the rest of
		// the stream. Until 7 Oct 2026 the rest was dropped, so ChatGPT
		// got the first 64MB of the turn and nothing after it.
		r.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(body), orig), orig}
		return
	}
	_ = orig.Close()
	if out, ok := applyRemovals(body, g.removals.For(RemovalKey(sid))); ok {
		body = out
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	r.Header.Set("Content-Length", fmt.Sprint(len(body)))
	g.capture(sid, body)
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) {
	if e.err == nil {
		return 0, io.EOF
	}
	return 0, e.err
}

func (g *Gateway) capture(sid string, body []byte) {
	st := &g.inspect
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.reqs == nil {
		st.reqs = map[string]*captured{}
	}
	// A session's subagents share its id; its largest request stands for it.
	if c := st.reqs[sid]; c != nil && len(c.body) > len(body) && time.Since(c.at) < time.Minute {
		return
	}
	st.reqs[sid] = &captured{body: body, at: time.Now()}
	if len(st.reqs) > inspectMaxReqs {
		var oldest string
		for k, c := range st.reqs {
			if time.Since(c.at) > inspectKeep {
				delete(st.reqs, k)
			} else if oldest == "" || c.at.Before(st.reqs[oldest].at) {
				oldest = k
			}
		}
		if len(st.reqs) > inspectMaxReqs && oldest != "" {
			delete(st.reqs, oldest)
		}
	}
}

// InspectSession is one Codex session the inspector can show.
type InspectSession struct {
	Session  string    `json:"session"`
	Repo     string    `json:"repo,omitempty"`
	Model    string    `json:"model"`
	At       time.Time `json:"at"`
	Messages int       `json:"messages"`
	Bytes    int       `json:"bytes"`
}

// InspectSessions lists the sessions with a captured request, newest first.
func (g *Gateway) InspectSessions() []InspectSession {
	g.inspect.mu.Lock()
	defer g.inspect.mu.Unlock()
	out := []InspectSession{}
	for sid, c := range g.inspect.reqs {
		var top struct {
			Model string            `json:"model"`
			Input []json.RawMessage `json:"input"`
		}
		_ = json.Unmarshal(c.body, &top)
		out = append(out, InspectSession{Session: sid, Repo: cwdName(c.body), Model: top.Model, At: c.at, Messages: len(top.Input), Bytes: len(c.body)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.After(out[j].At) })
	return out
}

// ContextReport is a Codex session's context, item by item.
type ContextReport struct {
	Session  string         `json:"session"`
	Repo     string         `json:"repo,omitempty"`
	Model    string         `json:"model"`
	At       time.Time      `json:"at"`
	Context  int64          `json:"context"`
	Estimate bool           `json:"estimate"`
	Prompts  int            `json:"prompts"`
	Flagged  int            `json:"flagged"`
	Items    []ctxview.Item `json:"items"`
}

// InspectContext is session sid's latest request as items, nil when none
// was seen since the gateway started. context is the session's size as
// ChatGPT last reported it (0 when unknown).
func (g *Gateway) InspectContext(sid string, context int64) *ContextReport {
	g.inspect.mu.Lock()
	c := g.inspect.reqs[sid]
	g.inspect.mu.Unlock()
	if c == nil {
		return nil
	}
	var top struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(c.body, &top)
	rep := &ContextReport{Session: sid, Repo: cwdName(c.body), Model: top.Model, At: c.at, Context: context}
	rep.Items, rep.Prompts = ContextItems(c.body)
	rep.Estimate = ctxview.Scale(rep.Items, context, rep.Prompts)
	rep.Flagged = ctxview.Flag(rep.Items, cwdOf(c.body), "")
	return rep
}

// InspectItem is the full text of item i.
func (g *Gateway) InspectItem(sid string, i int) (string, bool) {
	rep := g.InspectContext(sid, 0)
	if rep == nil || i < 0 || i >= len(rep.Items) {
		return "", false
	}
	return rep.Items[i].Full, true
}

// Codex's groups beyond the shared ones.
const (
	grpReplies   = "Codex's replies"
	grpReasoning = "Reasoning (encrypted)"
	grpSummary   = "Codex's own summaries"
	grpOther     = "Other"
)

// inputItem is the part of a Responses input item the inspector reads.
type inputItem struct {
	Type      string          `json:"type"`
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	Name      string          `json:"name"`
	Arguments string          `json:"arguments"`
	Input     string          `json:"input"`
	CallID    string          `json:"call_id"`
	Output    json.RawMessage `json:"output"`
	Encrypted string          `json:"encrypted_content"`
	Tools     json.RawMessage `json:"tools"`
	Action    json.RawMessage `json:"action"`
}

// ContextItems splits a Codex request into items, and counts the prompts.
func ContextItems(body []byte) ([]ctxview.Item, int) {
	var top struct {
		Instructions string            `json:"instructions"`
		Tools        []json.RawMessage `json:"tools"`
		Input        []json.RawMessage `json:"input"`
	}
	if json.Unmarshal(body, &top) != nil {
		return nil, 0
	}
	var items []ctxview.Item
	add := func(group, name string, turn int, text string) {
		items = append(items, ctxview.NewItem(group, name, turn, text))
	}
	if top.Instructions != "" {
		add(ctxview.GrpSystem, "Instructions", 0, top.Instructions)
	}
	addTools(&items, top.Tools)

	calls := map[string]string{} // call id -> "shell: go test"
	turn := 0
	for _, raw := range top.Input {
		var it inputItem
		if json.Unmarshal(raw, &it) != nil {
			continue
		}
		switch it.Type {
		case "additional_tools":
			var tools []json.RawMessage
			_ = json.Unmarshal(it.Tools, &tools)
			addTools(&items, tools)
		case "message", "":
			texts := contentTexts(it.Content)
			switch it.Role {
			case "developer", "system":
				for _, t := range texts {
					add(developerGroup(t), developerName(t), turn, t)
				}
			case "user":
				prompt := false
				for _, t := range texts {
					if g, name := userGroup(t); g != "" {
						add(g, name, turn+1, t)
					} else if strings.TrimSpace(t) != "" {
						if !prompt {
							turn++
							prompt = true
						}
						add(ctxview.GrpPrompts, "Prompt", turn, t)
					}
				}
			case "assistant":
				for _, t := range texts {
					add(grpReplies, "Reply", turn, t)
				}
			}
		case "function_call", "custom_tool_call", "local_shell_call":
			desc := describeCall(it)
			calls[it.CallID] = desc
			args := it.Arguments + it.Input
			if args == "" {
				args = string(it.Action)
			}
			add(grpReplies, "Call: "+desc, turn, args)
		case "function_call_output", "custom_tool_call_output", "local_shell_call_output":
			name := calls[it.CallID]
			if name == "" {
				name = "Tool output"
			}
			add(ctxview.GrpResults, name, turn, outputText(it.Output))
		case "reasoning":
			items = append(items, ctxview.Item{Group: grpReasoning, Name: "Reasoning", Turn: turn, Bytes: len(it.Encrypted) + len(it.Content),
				Preview: "Encrypted by OpenAI: only the model can read it.", Full: "Encrypted by OpenAI: only the model can read it."})
		case "compaction", "compaction_summary":
			items = append(items, ctxview.Item{Group: grpSummary, Name: "Summary of the earlier conversation", Turn: turn, Bytes: len(raw),
				Preview: "Codex compacted the history before this point; the summary is encrypted.", Full: "Encrypted."})
		default:
			add(grpOther, it.Type, turn, string(raw))
		}
	}
	return items, turn
}

func addTools(items *[]ctxview.Item, tools []json.RawMessage) {
	for _, t := range tools {
		var tool struct {
			Type  string            `json:"type"`
			Name  string            `json:"name"`
			Tools []json.RawMessage `json:"tools"`
		}
		_ = json.Unmarshal(t, &tool)
		group, name := ctxview.GrpTools, tool.Name
		if strings.HasPrefix(tool.Name, "mcp__") || strings.Contains(tool.Name, "__") {
			group = ctxview.GrpMCP
		}
		var names []string
		for _, n := range tool.Tools {
			var x struct {
				Name string `json:"name"`
			}
			_ = json.Unmarshal(n, &x)
			names = append(names, x.Name)
		}
		if tool.Type == "namespace" {
			name = fmt.Sprintf("%s: %d tools", tool.Name, len(names))
		} else if name == "" {
			name = tool.Type
		}
		*items = append(*items, ctxview.Item{Group: group, Name: name, Bytes: len(t),
			Preview: ctxview.Preview(strings.Join(names, ", ")), Full: strings.Join(names, "\n")})
	}
}

func contentTexts(c json.RawMessage) []string {
	var str string
	if json.Unmarshal(c, &str) == nil {
		return []string{str}
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	_ = json.Unmarshal(c, &blocks)
	var out []string
	for _, b := range blocks {
		if b.Text != "" {
			out = append(out, b.Text)
		}
	}
	return out
}

// outputText is a tool output: a string, or content blocks.
func outputText(o json.RawMessage) string {
	var str string
	if json.Unmarshal(o, &str) == nil {
		return str
	}
	return strings.Join(contentTexts(o), "\n")
}

// developerGroup names a developer instruction block by its tag.
func developerGroup(t string) string {
	s := strings.TrimSpace(t)
	switch {
	case strings.HasPrefix(s, "You are Codex"):
		return ctxview.GrpSystem
	case strings.HasPrefix(s, "<skills_instructions>"):
		return ctxview.GrpSkills
	}
	return ctxview.GrpReminders
}

func developerName(t string) string {
	s := strings.TrimSpace(t)
	if strings.HasPrefix(s, "You are Codex") {
		return "Codex's instructions"
	}
	if strings.HasPrefix(s, "<") {
		if i := strings.IndexAny(s, ">\n"); i > 1 {
			return strings.TrimSuffix(s[1:i], ">")
		}
	}
	return ctxview.FirstLine(s)
}

// userGroup tells Codex's own text in a user message (AGENTS.md, the
// environment) from the user's prompt ("" for a prompt).
func userGroup(t string) (string, string) {
	s := strings.TrimSpace(t)
	switch {
	case strings.HasPrefix(s, "# AGENTS.md instructions"):
		name := "AGENTS.md"
		if line := ctxview.FirstLine(s); strings.Contains(line, " for ") {
			name = strings.TrimSpace(line[strings.Index(line, " for ")+5:]) + "/AGENTS.md"
		}
		return ctxview.GrpInstructions, name
	case strings.HasPrefix(s, "<user_instructions>"):
		return ctxview.GrpInstructions, "Your instructions"
	case strings.HasPrefix(s, "<environment_context>"):
		return ctxview.GrpReminders, "environment_context"
	case strings.HasPrefix(s, "<") && strings.HasSuffix(ctxview.FirstLine(s), ">"):
		// Codex's other tagged notes (<turn_aborted> and the like).
		return ctxview.GrpReminders, developerName(s)
	}
	if _, ok := ctxview.StubID(s); ok {
		return ctxview.GrpReminders, s
	}
	return "", ""
}

func describeCall(it inputItem) string {
	var in map[string]any
	_ = json.Unmarshal([]byte(it.Arguments), &in)
	if cmd, ok := in["command"].([]any); ok && len(cmd) > 0 {
		parts := make([]string, 0, len(cmd))
		for _, c := range cmd {
			parts = append(parts, fmt.Sprint(c))
		}
		return it.Name + ": " + ctxview.Clip(ctxview.FirstLine(strings.Join(parts, " ")), 80)
	}
	for _, k := range []string{"cmd", "command", "path", "file_path", "query", "url"} {
		if v, ok := in[k].(string); ok && v != "" {
			return it.Name + ": " + ctxview.Clip(ctxview.FirstLine(v), 80)
		}
	}
	if it.Input != "" {
		return it.Name + ": " + ctxview.Clip(ctxview.FirstLine(it.Input), 80)
	}
	if it.Name == "" {
		return it.Type
	}
	return it.Name
}

// cwdOf is the working directory Codex states in its environment context.
func cwdOf(body []byte) string {
	i := bytes.Index(body, []byte(`<cwd>`))
	if i < 0 {
		return ""
	}
	rest := body[i+5:]
	j := bytes.Index(rest, []byte(`</cwd>`))
	if j < 0 || j > 4096 {
		return ""
	}
	return string(rest[:j])
}

func cwdName(body []byte) string {
	c := strings.TrimRight(cwdOf(body), "/")
	if i := strings.LastIndexByte(c, '/'); i >= 0 {
		return c[i+1:]
	}
	return c
}

// applyRemovals replaces removed items in a Codex body with their notes:
// a tool output's "output", or the text of an instruction or reminder.
// Every other byte of the item is kept.
func applyRemovals(body []byte, removed map[string]ctxview.Removal) ([]byte, bool) {
	if len(removed) == 0 {
		return body, false
	}
	var top map[string]json.RawMessage
	if json.Unmarshal(body, &top) != nil {
		return body, false
	}
	var input []json.RawMessage
	if json.Unmarshal(top["input"], &input) != nil {
		return body, false
	}
	changed := false
	for i, raw := range input {
		var it map[string]json.RawMessage
		if json.Unmarshal(raw, &it) != nil {
			continue
		}
		var typ, role string
		_ = json.Unmarshal(it["type"], &typ)
		_ = json.Unmarshal(it["role"], &role)
		hit := false
		switch {
		case strings.HasSuffix(typ, "_output"):
			text := outputText(it["output"])
			if r, ok := removed[ctxview.ID(ctxview.GrpResults, text)]; ok {
				it["output"], _ = json.Marshal(ctxview.Stub(r.Name, len(text), r.ID))
				hit = true
			}
		case typ == "message" || typ == "":
			var blocks []map[string]json.RawMessage
			if json.Unmarshal(it["content"], &blocks) != nil {
				continue
			}
			for _, b := range blocks {
				var t string
				if json.Unmarshal(b["text"], &t) != nil || t == "" {
					continue
				}
				var group string
				switch role {
				case "developer", "system":
					group = developerGroup(t)
				case "user":
					group, _ = userGroup(t)
				}
				if !ctxview.Removable(group) {
					continue
				}
				if r, ok := removed[ctxview.ID(group, t)]; ok {
					b["text"], _ = json.Marshal(ctxview.Stub(r.Name, len(t), r.ID))
					hit = true
				}
			}
			if hit {
				it["content"], _ = json.Marshal(blocks)
			}
		}
		if hit {
			if nb, err := json.Marshal(it); err == nil {
				input[i], changed = nb, true
			}
		}
	}
	if !changed {
		return body, false
	}
	ni, err := json.Marshal(input)
	if err != nil {
		return body, false
	}
	top["input"] = ni
	nb, err := json.Marshal(top)
	if err != nil {
		return body, false
	}
	return nb, true
}
