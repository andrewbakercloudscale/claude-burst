package router

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/automask"
)

// The context inspector: what a session's context is made of, item by item,
// with the things worth a look flagged. It needs the request itself, so the
// latest request of each conversation is kept IN MEMORY ONLY: never written
// to disk, gone on a restart, and served only by the dashboard on this Mac.

type inspectStore struct {
	mu   sync.Mutex
	reqs map[string]*captured // session|conversation -> latest request
}

type captured struct {
	session string
	model   string
	body    []byte
	msgs    int
	at      time.Time
}

const (
	inspectKeep    = 24 * time.Hour
	inspectMaxReqs = 40
)

func newInspectStore() *inspectStore { return &inspectStore{reqs: map[string]*captured{}} }

// captureForInspect keeps body as the latest request of its conversation.
// Side requests (the away recap and the like) are not the conversation.
func (s *Server) captureForInspect(sid string, body []byte) {
	if sid == "" {
		return
	}
	var top struct {
		Model    string            `json:"model"`
		Messages []json.RawMessage `json:"messages"`
	}
	if json.Unmarshal(body, &top) != nil || len(top.Messages) == 0 || isSideRequest(top.Messages) {
		return
	}
	key := sid + "|" + conversationID(top.Messages[0])
	st := s.inspect
	st.mu.Lock()
	defer st.mu.Unlock()
	st.reqs[key] = &captured{session: sid, model: top.Model, body: body, msgs: len(top.Messages), at: time.Now()}
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
// A session's subagents share its id; its largest conversation stands for it.
func (s *Server) InspectSessions() []InspectSession {
	s.inspect.mu.Lock()
	best := map[string]*captured{}
	for _, c := range s.inspect.reqs {
		if b := best[c.session]; b == nil || len(c.body) > len(b.body) {
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
type ContextItem struct {
	Group    string   `json:"group"`
	Name     string   `json:"name"`
	Turn     int      `json:"turn,omitempty"`      // the prompt it arrived with; 0 for what every request carries
	TurnsAgo int      `json:"turns_ago,omitempty"` // prompts since
	Bytes    int      `json:"bytes"`
	Tokens   int64    `json:"tokens"`
	Flags    []string `json:"flags,omitempty"`
	Preview  string   `json:"preview"`
	full     string
}

// ContextReport is a session's context, item by item.
type ContextReport struct {
	Session  string        `json:"session"`
	Repo     string        `json:"repo,omitempty"`
	RepoRoot string        `json:"repo_root,omitempty"`
	Model    string        `json:"model"`
	At       time.Time     `json:"at"`
	Context  int64         `json:"context"`  // what the API reported; 0 when not known yet
	Estimate bool          `json:"estimate"` // tokens are bytes/4, not shares of a reported context
	Prompts  int           `json:"prompts"`
	Flagged  int           `json:"flagged"`
	Items    []ContextItem `json:"items"`
}

// Groups, in the order the dashboard shows them.
const (
	grpInstructions = "Instruction files"
	grpSkills       = "Skills"
	grpReminders    = "Other reminders"
	grpSystem       = "System prompt"
	grpTools        = "Built-in tools"
	grpMCP          = "MCP tools"
	grpPrompts      = "Your prompts"
	grpReplies      = "Claude's replies"
	grpResults      = "Tool results"
)

var contentsOf = regexp.MustCompile(`(?m)^Contents of (\S+?)(?: \(([^)\n]*)\))?:\s*$`)

// InspectContext builds the report for session sid's main conversation, or
// nil when no request of it has been seen since the gateway started.
func (s *Server) InspectContext(sid string) *ContextReport {
	s.inspect.mu.Lock()
	var c *captured
	for _, x := range s.inspect.reqs {
		if x.session == sid && (c == nil || len(x.body) > len(c.body)) {
			c = x
		}
	}
	s.inspect.mu.Unlock()
	if c == nil {
		return nil
	}
	name, root := s.repos.Resolve(sid)
	rep := &ContextReport{Session: sid, Repo: name, RepoRoot: root, Model: c.model, At: c.at}
	for _, cs := range s.CompactionSessions() {
		if cs.Session == sid && cs.Context > rep.Context {
			rep.Context = cs.Context
		}
	}
	rep.Items, rep.Prompts = contextItems(c.body)

	total := 0
	for _, it := range rep.Items {
		total += it.Bytes
	}
	rep.Estimate = rep.Context <= 0
	for i := range rep.Items {
		it := &rep.Items[i]
		if rep.Estimate || total == 0 {
			it.Tokens = int64(it.Bytes / 4)
		} else {
			it.Tokens = rep.Context * int64(it.Bytes) / int64(total)
		}
		if it.Turn > 0 {
			it.TurnsAgo = rep.Prompts - it.Turn
		}
	}
	flagItems(rep.Items, root)
	for _, it := range rep.Items {
		if len(it.Flags) > 0 {
			rep.Flagged++
		}
	}
	return rep
}

// InspectItem is the full text of item i, for the dashboard's "show all".
func (s *Server) InspectItem(sid string, i int) (string, bool) {
	rep := s.InspectContext(sid)
	if rep == nil || i < 0 || i >= len(rep.Items) {
		return "", false
	}
	return rep.Items[i].full, true
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
		items = append(items, ContextItem{Group: group, Name: name, Turn: turn, Bytes: len(text), Preview: preview(text), full: text})
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
			Preview: strings.Join(builtin, ", "), full: strings.Join(builtin, "\n")})
	}
	servers := make([]string, 0, len(mcp))
	for k := range mcp {
		servers = append(servers, k)
	}
	sort.Strings(servers)
	for _, k := range servers {
		items = append(items, ContextItem{Group: grpMCP, Name: fmt.Sprintf("%s: %d tools", k, len(mcp[k])), Bytes: mcpBytes[k],
			Preview: strings.Join(mcp[k], ", "), full: strings.Join(mcp[k], "\n")})
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
		if head := strings.TrimSpace(r[:locs[0][0]]); len(head) > 200 {
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

// flagItems marks what is worth a look. Each flag says why in a few words.
func flagItems(items []ContextItem, root string) {
	home, _ := os.UserHomeDir()
	lastRead := map[string]int{} // path -> index of its newest read
	for i, it := range items {
		if p, ok := strings.CutPrefix(it.Name, "Read "); ok && it.Group == grpResults {
			lastRead[p] = i
		}
	}
	scan := automask.NewSession()
	defaults := func(r *automask.Rule) bool { return r.Default }
	for i := range items {
		it := &items[i]
		if it.Group == grpResults && it.Bytes > 20_000 && it.TurnsAgo >= 10 {
			it.Flags = append(it.Flags, fmt.Sprintf("large and %d prompts old", it.TurnsAgo))
		}
		if p, ok := strings.CutPrefix(it.Name, "Read "); ok && it.Group == grpResults {
			if lastRead[p] != i {
				it.Flags = append(it.Flags, "read again later: this copy is out of date")
			}
			if _, err := os.Stat(p); err != nil && filepath.IsAbs(p) {
				it.Flags = append(it.Flags, "file no longer exists")
			}
		}
		if it.Group == grpInstructions {
			p := it.Name
			inRepo := root != "" && strings.HasPrefix(p, root+string(os.PathSeparator))
			inClaude := home != "" && strings.HasPrefix(p, filepath.Join(home, ".claude")+string(os.PathSeparator))
			if !inRepo && !inClaude {
				it.Flags = append(it.Flags, "from outside this repository")
			}
			if _, err := os.Stat(p); err != nil && filepath.IsAbs(p) {
				it.Flags = append(it.Flags, "file no longer exists")
			}
		}
		if _, hits, _ := scan.Mask(it.full, "", defaults); len(hits) > 0 {
			it.Flags = append(it.Flags, "personal data: "+automask.Summary(hits))
		}
	}
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

func preview(s string) string { return clip(strings.Join(strings.Fields(s), " "), 240) }

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return clip(s, 100)
}

func clip(s string, n int) string {
	if len([]rune(s)) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "..."
}
