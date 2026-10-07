// Package ctxview is the context inspector's shared part: a session's
// context as items, the flags on them, and the items the user has removed.
//
// Claude Code and Codex each keep their own history and send all of it with
// every request. Removing an item never touches that history: the gateway
// leaves the item out of each request it forwards, putting a one-line note
// in its place, and Restore stops doing so. So a removal is always
// reversible, and survives nothing it should not: a restart keeps it (only
// ids, names and sizes are written, never content), and the history moving
// on (compaction, /clear) simply means the item is no longer there to remove.
package ctxview

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/atomicfile"
	"github.com/andrewbakercloudscale/claude-burst/internal/automask"
)

// Groups, shared by Claude Code and Codex where they mean the same thing.
const (
	GrpInstructions = "Instruction files"
	GrpSkills       = "Skills"
	GrpReminders    = "Other reminders"
	GrpSystem       = "System prompt"
	GrpTools        = "Built-in tools"
	GrpMCP          = "MCP tools"
	GrpPrompts      = "Your prompts"
	GrpResults      = "Tool results"
)

// removable: what can be left out without breaking the conversation. The
// system prompt, tools, prompts and replies cannot: a reply's thinking is
// signed, a tool list is what the model may call, and a prompt is the
// conversation itself. A tool result keeps its place (its call still pairs
// with it); only its content becomes the note.
var removable = map[string]bool{GrpInstructions: true, GrpSkills: true, GrpReminders: true, GrpResults: true}

// Removable reports whether items of group can be removed.
func Removable(group string) bool { return removable[group] }

// Item is one thing in the context.
type Item struct {
	Group     string   `json:"group"`
	Name      string   `json:"name"`
	Turn      int      `json:"turn,omitempty"`      // the prompt it arrived with; 0 for what every request carries
	TurnsAgo  int      `json:"turns_ago,omitempty"` // prompts since
	Bytes     int      `json:"bytes"`
	Tokens    int64    `json:"tokens"`
	Flags     []string `json:"flags,omitempty"`
	Preview   string   `json:"preview"`
	ID        string   `json:"id,omitempty"` // set on removable items
	Removable bool     `json:"removable,omitempty"`
	Removed   bool     `json:"removed,omitempty"` // the request carried the note, not the item
	Full      string   `json:"-"`
	// Pictures is how many images the item carries, and Extra what they
	// cost in tokens, estimated from their size in pixels: a picture's
	// cost is not in its bytes, so Scale adds it to the text's share.
	Pictures int   `json:"pictures,omitempty"`
	Extra    int64 `json:"-"`
}

// NewItem builds an item from its text. A removed item's text is the note,
// which carries the id it was removed under.
func NewItem(group, name string, turn int, text string) Item {
	it := Item{Group: group, Name: name, Turn: turn, Bytes: len(text), Preview: Preview(text), Full: text}
	if removable[group] {
		it.Removable = true
		if id, ok := StubID(text); ok {
			it.ID, it.Removed = id, true
			// A name read from the text is the note's first line: use the
			// name the note carries.
			if m := stubName.FindStringSubmatch(text); m != nil && strings.HasPrefix(name, "[Removed") {
				it.Name = m[1]
			}
			it.Preview = "Removed by you in Claude Burst. Restore puts it back in the next request."
		} else {
			it.ID = ID(group, text)
		}
	}
	return it
}

// ID names an item by its content, so the same item is found again in every
// later request however the history around it has moved.
func ID(group, text string) string {
	h := sha256.Sum256([]byte(group + "\x00" + text))
	return hex.EncodeToString(h[:8])
}

var stubName = regexp.MustCompile(`in Claude Burst: (.*), \d+ bytes\. Ask them`)

var stubRe = regexp.MustCompile(`burst-removed:([0-9a-f]{16})\]$`)

// Stub is the note sent in a removed item's place: the model learns
// something was there and can ask for it.
func Stub(name string, bytes int, id string) string {
	return fmt.Sprintf("[Removed from this context by the user in Claude Burst: %s, %d bytes. Ask them if you need it again. burst-removed:%s]", name, bytes, id)
}

// StubID returns the id in a note, if text is one.
func StubID(text string) (string, bool) {
	t := strings.TrimSpace(text)
	if !strings.HasPrefix(t, "[Removed from this context by the user in Claude Burst") {
		return "", false
	}
	m := stubRe.FindStringSubmatch(t)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// Scale gives each item its share of a reported context, or bytes/4 when
// none is known yet (estimate).
func Scale(items []Item, context int64, prompts int) (estimate bool) {
	total := 0
	var extra int64
	for _, it := range items {
		total += it.Bytes
		extra += it.Extra
	}
	estimate = context <= 0
	// The text shares what the pictures leave. Should the pictures be put
	// at more than the context, their estimate is what is wrong: all of it
	// is then shared by size, a token of picture as four bytes.
	text := context - extra
	for i := range items {
		it := &items[i]
		switch {
		case estimate || total == 0:
			it.Tokens = int64(it.Bytes/4) + it.Extra
		case text <= 0:
			it.Tokens = context * (int64(it.Bytes) + 4*it.Extra) / (int64(total) + 4*extra)
		default:
			it.Tokens = text*int64(it.Bytes)/int64(total) + it.Extra
		}
		if it.Turn > 0 {
			it.TurnsAgo = prompts - it.Turn
		}
	}
	return estimate
}

// What makes an item worth a look by its size alone: an older output of a
// call made again from this many bytes, and any removable item from this
// share of the context.
const (
	rerunMinBytes   = 2000
	bigSharePercent = 5
)

// Flag marks what is worth a look; it returns how many items it flagged.
// Each flag says why in a few words. root is the repository the session
// runs in ("" when unknown); readPrefix is how a file read is named ("Read "
// for Claude Code).
func Flag(items []Item, root, readPrefix string) int {
	home, _ := os.UserHomeDir()
	lastRead := map[string]int{} // path -> index of its newest read
	lastRun := map[string]int{}  // a call named with what it was given -> index of its newest result
	var context int64
	for i, it := range items {
		context += it.Tokens
		if it.Group != GrpResults {
			continue
		}
		if p, ok := strings.CutPrefix(it.Name, readPrefix); ok && readPrefix != "" {
			lastRead[p] = i
		} else if strings.ContainsAny(it.Name, " :") {
			lastRun[it.Name] = i
		}
	}
	scan := automask.NewSession()
	defaults := func(r *automask.Rule) bool { return r.Default }
	n := 0
	for i := range items {
		it := &items[i]
		if it.Removed {
			continue
		}
		if it.Group == GrpResults && it.Bytes > 20_000 && it.TurnsAgo >= 10 {
			it.Flags = append(it.Flags, fmt.Sprintf("large and %d prompts old", it.TurnsAgo))
		}
		if p, ok := strings.CutPrefix(it.Name, readPrefix); ok && readPrefix != "" && it.Group == GrpResults {
			if j := lastRead[p]; j != i {
				it.Flags = append(it.Flags, fmt.Sprintf("read again at prompt %d: this is the older copy", items[j].Turn))
			}
			if _, err := os.Stat(p); err != nil && filepath.IsAbs(p) {
				it.Flags = append(it.Flags, "file no longer exists")
			}
		}
		// The same command or search made again: the older output is
		// rarely what is wanted. Small ones are not worth the look.
		if j, ok := lastRun[it.Name]; ok && j != i && it.Group == GrpResults && it.Bytes >= rerunMinBytes {
			it.Flags = append(it.Flags, fmt.Sprintf("run again at prompt %d: this is the older output", items[j].Turn))
		}
		if it.Pictures > 0 && it.TurnsAgo >= 1 {
			it.Flags = append(it.Flags, fmt.Sprintf("picture from %d prompt%s ago", it.TurnsAgo, map[bool]string{true: "", false: "s"}[it.TurnsAgo == 1]))
		}
		if it.Removable && context > 0 && it.Tokens*100/context >= bigSharePercent {
			it.Flags = append(it.Flags, fmt.Sprintf("big: %d%% of the context", it.Tokens*100/context))
		}
		if it.Group == GrpInstructions && filepath.IsAbs(it.Name) {
			p := it.Name
			inRepo := root != "" && strings.HasPrefix(p, root+string(os.PathSeparator))
			inClaude := home != "" && strings.HasPrefix(p, filepath.Join(home, ".claude")+string(os.PathSeparator))
			if !inRepo && !inClaude {
				it.Flags = append(it.Flags, "from outside this repository")
			}
			if _, err := os.Stat(p); err != nil {
				it.Flags = append(it.Flags, "file no longer exists")
			}
		}
		if _, hits, _ := scan.Mask(it.Full, "", defaults); len(hits) > 0 {
			it.Flags = append(it.Flags, "personal data: "+automask.Summary(hits))
		}
		if len(it.Flags) > 0 {
			n++
		}
	}
	return n
}

// Preview is the first 240 characters, whitespace folded.
func Preview(s string) string { return Clip(strings.Join(strings.Fields(s), " "), 240) }

// FirstLine is s's first line, clipped to 100 characters.
func FirstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return Clip(s, 100)
}

// Clip cuts s to n runes, marking the cut.
func Clip(s string, n int) string {
	if len([]rune(s)) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "..."
}

// Removal is one item a user removed from a session's context.
type Removal struct {
	ID    string    `json:"id"`
	Group string    `json:"group"`
	Name  string    `json:"name"`
	Bytes int       `json:"bytes"`
	At    time.Time `json:"at"`
}

// removalKeep: a session not seen for this long has ended.
const removalKeep = 14 * 24 * time.Hour

// Store holds the removals of every session, keyed "claude:<id>" or
// "codex:<id>", in a file beside the config. Ids, names and sizes only.
type Store struct {
	path string
	mu   sync.Mutex
	m    map[string]map[string]Removal
}

var (
	sharedMu sync.Mutex
	shared   = map[string]*Store{}
)

// Shared is the one Store for path in this process: the Claude Code and
// Codex gateways write the same file, and two stores would overwrite each
// other's removals.
func Shared(path string) *Store {
	sharedMu.Lock()
	defer sharedMu.Unlock()
	if s := shared[path]; s != nil {
		return s
	}
	s := Open(path)
	shared[path] = s
	return s
}

// Open loads the store at path; a missing or unreadable file is empty. An
// empty path keeps it in memory only (tests).
func Open(path string) *Store {
	s := &Store{path: path, m: map[string]map[string]Removal{}}
	if b, err := os.ReadFile(path); path != "" && err == nil {
		_ = json.Unmarshal(b, &s.m)
	}
	if s.m == nil {
		s.m = map[string]map[string]Removal{}
	}
	return s
}

// For returns key's removals by id, nil when there are none. The map is a
// copy.
func (s *Store) For(key string) map[string]Removal {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.m[key]) == 0 {
		return nil
	}
	out := make(map[string]Removal, len(s.m[key]))
	for k, v := range s.m[key] {
		out[k] = v
	}
	return out
}

// Add removes an item from key's context, from the next request on.
func (s *Store) Add(key string, r Removal) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m[key] == nil {
		s.m[key] = map[string]Removal{}
	}
	r.At = time.Now()
	s.m[key][r.ID] = r
	return s.save()
}

// Restore puts a removed item back, from the next request on.
func (s *Store) Restore(key, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m[key], id)
	if len(s.m[key]) == 0 {
		delete(s.m, key)
	}
	return s.save()
}

// save writes the store, dropping sessions untouched for removalKeep.
// Called with mu held.
func (s *Store) save() error {
	cut := time.Now().Add(-removalKeep)
	for k, rs := range s.m {
		latest := time.Time{}
		for _, r := range rs {
			if r.At.After(latest) {
				latest = r.At
			}
		}
		if latest.Before(cut) {
			delete(s.m, k)
		}
	}
	if s.path == "" {
		return nil
	}
	b, err := json.MarshalIndent(s.m, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.Write(s.path, b, 0o600)
}

// RemovalsPath is the store's file in the config directory.
func RemovalsPath(configDir string) string { return filepath.Join(configDir, "context-removals.json") }
