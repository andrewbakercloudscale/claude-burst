package router

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/ctxview"
)

// The context inspector: what a session's context is made of, item by item,
// with the things worth a look flagged. It needs the request itself, so the
// latest request of each conversation is kept IN MEMORY ONLY: never written
// to disk, gone on a restart, and served only by the dashboard on this Mac.

type inspectStore struct {
	mu   sync.Mutex
	reqs map[string]*captured // conversation -> its latest request sent whole
	// want: sessions whose whole conversation is wanted, and why. A session
	// on a message thread sends only what is new, so the next request that
	// continues its thread is asked for the history (applyThreadRequest).
	want map[string]string
}

type captured struct {
	session string
	model   string
	body    []byte
	msgs    int
	at      time.Time
	since   int // requests that continued its thread since: not in body
	// cont is when a request last continued its thread. A conversation on a
	// thread is the session's own: body is empty when none of its requests
	// has been sent whole since the gateway started.
	cont time.Time
}

const (
	inspectKeep    = 24 * time.Hour
	inspectMaxReqs = 40
)

func newInspectStore() *inspectStore {
	return &inspectStore{reqs: map[string]*captured{}, want: map[string]string{}}
}

// captureForInspect keeps body as the latest request of its conversation.
// Side requests (the away recap and the like) are not the conversation. A
// request that continues a message thread carries one message, not the
// conversation: it is counted against the conversation it continues, whose
// last whole request stays what the inspector shows.
func (s *Server) captureForInspect(r *http.Request, body []byte) {
	sid := r.Header.Get("x-claude-code-session-id")
	if sid == "" {
		return
	}
	var top struct {
		Model    string            `json:"model"`
		Messages []json.RawMessage `json:"messages"`
		Thread   struct {
			Type string `json:"type"`
		} `json:"thread"`
	}
	if json.Unmarshal(body, &top) != nil || len(top.Messages) == 0 || isSideRequest(top.Messages) {
		return
	}
	// The conversation as compaction names it, which a summary swapped in
	// does not change; its first message otherwise.
	ci := compactInfoFrom(r.Context())
	key := ci.key
	if key == "" {
		key = sid + "|" + conversationID(top.Messages[0])
	}
	st := s.inspect
	st.mu.Lock()
	defer st.mu.Unlock()
	if ci.thread || top.Thread.Type == "continue" {
		// With no whole request on record (the gateway restarted under the
		// session) it is still noted: otherwise a smaller conversation sent
		// whole, one of the session's own helpers, would stand for it.
		c := st.reqs[key]
		if c == nil {
			c = &captured{session: sid, model: top.Model, at: time.Now()}
			st.reqs[key] = c
		}
		c.since++
		c.cont = time.Now()
		return
	}
	st.reqs[key] = &captured{session: sid, model: top.Model, body: body, msgs: len(top.Messages), at: time.Now()}
	// A thread known only by its last response comes back whole under its
	// real name: the note kept for it has done its work. The model tells it
	// from a helper's request, which leaves the note alone.
	for k, c := range st.reqs {
		if k != key && c.session == sid && len(c.body) == 0 && c.model == top.Model {
			delete(st.reqs, k)
		}
	}
	if len(st.reqs) > inspectMaxReqs {
		cut := time.Now().Add(-inspectKeep)
		var oldest string
		for k, c := range st.reqs {
			if c.at.Before(cut) {
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

// WantHistory has session sid's whole conversation asked for with its next
// request, when that request continues a message thread: the inspector then
// shows the session as it is now, and what was removed from its context is
// left out of what the API holds. A session that sends its history with
// every request needs no asking.
func (s *Server) WantHistory(sid, why string) {
	if sid == "" {
		return
	}
	s.inspect.mu.Lock()
	s.inspect.want[sid] = why
	s.inspect.mu.Unlock()
}

func (st *inspectStore) wanted(sid string) string {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.want[sid]
}

func (st *inspectStore) asked(sid string) {
	st.mu.Lock()
	delete(st.want, sid)
	st.mu.Unlock()
}

// standsFor says c, not b, is the conversation a session is shown by: the
// one on a message thread (the latest continued, when several), its largest
// otherwise. A session's subagents and helpers share its id.
func standsFor(c, b *captured) bool {
	if b == nil {
		return true
	}
	if !c.cont.IsZero() || !b.cont.IsZero() {
		return c.cont.After(b.cont)
	}
	return len(c.body) > len(b.body)
}

// InspectSession is one session the inspector can show.
type InspectSession struct {
	Session  string    `json:"session"`
	Repo     string    `json:"repo,omitempty"`
	Model    string    `json:"model"`
	At       time.Time `json:"at"`
	Messages int       `json:"messages"`
	Bytes    int       `json:"bytes"`
}

// InspectSessions lists the sessions with a captured request, newest first.
// A session's subagents share its id; standsFor picks its conversation.
func (s *Server) InspectSessions() []InspectSession {
	s.inspect.mu.Lock()
	best := map[string]*captured{}
	for _, c := range s.inspect.reqs {
		if standsFor(c, best[c.session]) {
			best[c.session] = c
		}
	}
	s.inspect.mu.Unlock()
	var out []InspectSession
	for sid, c := range best {
		name, _ := s.repos.Resolve(sid)
		out = append(out, InspectSession{Session: sid, Repo: name, Model: c.model, At: c.at, Messages: c.msgs, Bytes: len(c.body)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.After(out[j].At) })
	return out
}

// ContextItem is one thing in the context.
type ContextItem = ctxview.Item

// ContextReport is a session's context, item by item.
type ContextReport struct {
	Session  string        `json:"session"`
	Repo     string        `json:"repo,omitempty"`
	RepoRoot string        `json:"repo_root,omitempty"`
	Model    string        `json:"model"`
	At       time.Time     `json:"at"`
	Context  int64         `json:"context"`         // what the API reported; 0 when not known yet
	Estimate bool          `json:"estimate"`        // tokens are bytes/4, not shares of a reported context
	Since    int           `json:"since,omitempty"` // requests that continued the thread since At: what they added is not in Items
	Prompts  int           `json:"prompts"`
	Flagged  int           `json:"flagged"`
	Items    []ContextItem `json:"items"`
}

// Groups, in the order the dashboard shows them.
const (
	grpInstructions = ctxview.GrpInstructions
	grpSkills       = ctxview.GrpSkills
	grpReminders    = ctxview.GrpReminders
	grpSystem       = ctxview.GrpSystem
	grpTools        = ctxview.GrpTools
	grpMCP          = ctxview.GrpMCP
	grpPrompts      = ctxview.GrpPrompts
	grpReplies      = "Claude's replies"
	grpResults      = ctxview.GrpResults
)

var contentsOf = regexp.MustCompile(`(?m)^Contents of (\S+?)(?: \(([^)\n]*)\))?:\s*$`)

// InspectContext builds the report for session sid's main conversation, or
// nil when no request of it has been seen since the gateway started.
func (s *Server) InspectContext(sid string) *ContextReport {
	s.inspect.mu.Lock()
	var c *captured
	for _, x := range s.inspect.reqs {
		if x.session == sid && standsFor(x, c) {
			c = x
		}
	}
	if c != nil {
		cp := *c
		c = &cp
	}
	s.inspect.mu.Unlock()
	if c == nil {
		return nil
	}
	name, root := s.repos.Resolve(sid)
	rep := &ContextReport{Session: sid, Repo: name, RepoRoot: root, Model: c.model, At: c.at, Since: c.since}
	// The conversation in use, not the session's largest on record: after a
	// compaction the largest is the one the summary replaced.
	if main := MainConversation(s.CompactionSessions(), sid); main != nil {
		rep.Context = main.Context
	}
	if len(c.body) == 0 {
		// Nothing sent whole yet: the report says how far behind it is.
		rep.Items = []ContextItem{}
		return rep
	}
	rep.Items, rep.Prompts = contextItems(c.body)
	rep.Estimate = ctxview.Scale(rep.Items, rep.Context, rep.Prompts)
	rep.Flagged = ctxview.Flag(rep.Items, root, "Read ")
	return rep
}

// InspectItem is the full text of item i, for the dashboard's "show all".
func (s *Server) InspectItem(sid string, i int) (string, bool) {
	rep := s.InspectContext(sid)
	if rep == nil || i < 0 || i >= len(rep.Items) {
		return "", false
	}
	return rep.Items[i].Full, true
}

// contextItems splits a request into items, and counts the prompts in it.
func contextItems(body []byte) ([]ContextItem, int) {
	var top struct {
		System   json.RawMessage   `json:"system"`
		Tools    []json.RawMessage `json:"tools"`
		Messages []json.RawMessage `json:"messages"`
	}
	if json.Unmarshal(body, &top) != nil {
		return nil, 0
	}
	var items []ContextItem
	add := func(group, name string, turn int, text string) {
		items = append(items, ctxview.NewItem(group, name, turn, text))
	}

	for i, t := range textsOf(top.System) {
		add(grpSystem, fmt.Sprintf("System prompt, part %d", i+1), 0, t)
	}
	mcp := map[string][]string{}
	mcpBytes := map[string]int{}
	var builtin []string
	builtinBytes := 0
	for _, t := range top.Tools {
		var tool struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(t, &tool)
		if rest, ok := strings.CutPrefix(tool.Name, "mcp__"); ok {
			server, _, _ := strings.Cut(rest, "__")
			mcp[server] = append(mcp[server], tool.Name)
			mcpBytes[server] += len(t)
			continue
		}
		builtin = append(builtin, tool.Name)
		builtinBytes += len(t)
	}
	if len(builtin) > 0 {
		items = append(items, ContextItem{Group: grpTools, Name: fmt.Sprintf("%d tools", len(builtin)), Bytes: builtinBytes,
			Preview: strings.Join(builtin, ", "), Full: strings.Join(builtin, "\n")})
	}
	servers := make([]string, 0, len(mcp))
	for k := range mcp {
		servers = append(servers, k)
	}
	sort.Strings(servers)
	for _, k := range servers {
		items = append(items, ContextItem{Group: grpMCP, Name: fmt.Sprintf("%s: %d tools", k, len(mcp[k])), Bytes: mcpBytes[k],
			Preview: strings.Join(mcp[k], ", "), Full: strings.Join(mcp[k], "\n")})
	}

	calls := map[string]string{} // tool_use id -> "Read /path"
	turn := 0
	for _, raw := range top.Messages {
		var msg struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(raw, &msg) != nil {
			continue
		}
		var str string
		if json.Unmarshal(msg.Content, &str) == nil {
			if msg.Role == "user" {
				turn++
				addUserText(&items, turn, str, add)
			} else {
				add(grpReplies, "Reply", turn, str)
			}
			continue
		}
		var blocks []json.RawMessage
		if json.Unmarshal(msg.Content, &blocks) != nil {
			continue
		}
		// A user message with any text of its own is a prompt; one of only
		// tool results is the turn carrying on.
		if msg.Role == "user" {
			for _, b := range blocks {
				var blk struct {
					Type string `json:"type"`
					Text string `json:"text"`
				}
				_ = json.Unmarshal(b, &blk)
				if blk.Type == "text" && !strings.HasPrefix(strings.TrimSpace(blk.Text), "<system-reminder>") {
					turn++
					break
				}
			}
		}
		for _, b := range blocks {
			var blk struct {
				Type      string          `json:"type"`
				Text      string          `json:"text"`
				ID        string          `json:"id"`
				Name      string          `json:"name"`
				Input     json.RawMessage `json:"input"`
				ToolUseID string          `json:"tool_use_id"`
				Content   json.RawMessage `json:"content"`
			}
			if json.Unmarshal(b, &blk) != nil {
				continue
			}
			switch blk.Type {
			case "text":
				if msg.Role == "user" {
					addUserText(&items, turn, blk.Text, add)
				} else {
					add(grpReplies, "Reply", turn, blk.Text)
				}
			case "tool_use":
				calls[blk.ID] = describeCall(blk.Name, blk.Input)
				add(grpReplies, "Call: "+calls[blk.ID], turn, string(blk.Input))
			case "tool_result":
				name := calls[blk.ToolUseID]
				if name == "" {
					name = "Tool result"
				}
				add(grpResults, name, turn, strings.Join(textsOf(blk.Content), "\n"))
			}
		}
	}
	return items, turn
}

// addUserText splits a user text into its reminders (instruction files,
// skills, the rest) and the prompt itself.
func addUserText(items *[]ContextItem, turn int, text string, add func(group, name string, turn int, text string)) {
	rest := text
	for {
		i := strings.Index(rest, "<system-reminder>")
		if i < 0 {
			break
		}
		j := strings.Index(rest[i:], "</system-reminder>")
		if j < 0 {
			break
		}
		addReminder(rest[i+len("<system-reminder>"):i+j], turn, add)
		rest = rest[:i] + rest[i+j+len("</system-reminder>"):]
	}
	if t := strings.TrimSpace(rest); t != "" {
		add(grpPrompts, "Prompt", turn, t)
	}
}

func addReminder(r string, turn int, add func(group, name string, turn int, text string)) {
	locs := contentsOf.FindAllStringSubmatchIndex(r, -1)
	if len(locs) > 0 {
		if head := strings.TrimSpace(r[:locs[0][0]]); len(head) > 200 || isStub(head) {
			add(grpReminders, firstLine(head), turn, head)
		}
		for k, m := range locs {
			end := len(r)
			if k+1 < len(locs) {
				end = locs[k+1][0]
			}
			add(grpInstructions, r[m[2]:m[3]], turn, strings.TrimSpace(r[m[1]:end]))
		}
		return
	}
	if strings.Contains(r, "skills are available") {
		add(grpSkills, "Skills list", turn, strings.TrimSpace(r))
		return
	}
	add(grpReminders, firstLine(r), turn, strings.TrimSpace(r))
}

// describeCall names a tool call by what it touched: "Read /x/y.go",
// "Bash: go test ./...".
func describeCall(name string, input json.RawMessage) string {
	var in map[string]any
	_ = json.Unmarshal(input, &in)
	str := func(k string) string { v, _ := in[k].(string); return v }
	switch {
	case str("file_path") != "":
		return name + " " + str("file_path")
	case str("command") != "":
		return name + ": " + clip(firstLine(str("command")), 80)
	case str("pattern") != "":
		return name + ": " + clip(str("pattern"), 80)
	case str("url") != "":
		return name + " " + clip(str("url"), 80)
	case str("description") != "":
		return name + ": " + clip(str("description"), 80)
	}
	return name
}

// textsOf is the text of a content value: a string, or the text blocks of
// an array.
func textsOf(c json.RawMessage) []string {
	var str string
	if json.Unmarshal(c, &str) == nil {
		return []string{str}
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(c, &blocks) != nil {
		return nil
	}
	var out []string
	for _, b := range blocks {
		if b.Type == "text" {
			out = append(out, b.Text)
		}
	}
	return out
}

func firstLine(s string) string { return ctxview.FirstLine(s) }

func clip(s string, n int) string { return ctxview.Clip(s, n) }

func isStub(t string) bool { _, ok := ctxview.StubID(t); return ok }
