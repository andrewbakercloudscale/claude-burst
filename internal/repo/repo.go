// Package repo finds the repository a Claude Code session runs in.
//
// The gateway sees a session id on every request but not where the session
// runs; Claude Code's transcript for that session
// (~/.claude/projects/<dir>/<session>.jsonl) records its working directory
// as "cwd", and the repository is the nearest directory above that holding
// .git. The project directory name alone cannot be decoded back into a
// path: Claude Code replaces every character outside [A-Za-z0-9] with "-".
//
// Used by the dashboard's spend and savings by repository, and by
// compaction's per-repository Compact at.
package repo

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// Unknown is a session with no transcript (yet), or none naming a cwd.
	Unknown = "(no transcript)"
	// Temp groups scratch sessions under the temp directory: each has its
	// own directory and none is a project anyone would look for.
	Temp = "(temporary directories)"
)

// missTTL is how long a session with no transcript is remembered as
// unknown. A transcript can appear after the first request that names it,
// so a miss is retried, but not on every request.
const missTTL = time.Minute

// Resolver caches session id to repository.
type Resolver struct {
	mu   sync.Mutex
	hit  map[string][2]string // session -> {name, root}
	miss map[string]time.Time // session -> when it was last not found
	dir  string               // Claude Code's projects directory
	temp []string             // scratch roots
	now  func() time.Time
}

// New resolves against Claude Code's projects directory
// ($CLAUDE_CONFIG_DIR or ~/.claude).
func New() *Resolver {
	base := os.Getenv("CLAUDE_CONFIG_DIR")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".claude")
	}
	return NewWith(filepath.Join(base, "projects"),
		[]string{"/private/tmp/", "/tmp/", strings.TrimSuffix(os.TempDir(), "/") + "/"})
}

// NewWith resolves against projectsDir, grouping directories under any of
// temp as Temp. For tests.
func NewWith(projectsDir string, temp []string) *Resolver {
	return &Resolver{hit: map[string][2]string{}, miss: map[string]time.Time{},
		dir: projectsDir, temp: temp, now: time.Now}
}

// Resolve names the repository session runs in and its root, or Unknown
// and "" when its transcript cannot say. Temp sessions have no root.
func (r *Resolver) Resolve(session string) (name, root string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if v, ok := r.hit[session]; ok {
		return v[0], v[1]
	}
	if t, ok := r.miss[session]; ok && r.now().Sub(t) < missTTL {
		return Unknown, ""
	}
	matches, _ := filepath.Glob(filepath.Join(r.dir, "*", session+".jsonl"))
	cwd := ""
	if len(matches) > 0 {
		cwd = transcriptCwd(matches[0])
	}
	if cwd == "" {
		if len(r.miss) > 10000 {
			r.miss = map[string]time.Time{}
		}
		r.miss[session] = r.now()
		return Unknown, ""
	}
	delete(r.miss, session)
	name, root = r.repoOf(cwd)
	r.hit[session] = [2]string{name, root}
	return name, root
}

// transcriptCwd returns the first "cwd" recorded in a transcript. It sits on
// the first message line, after a few metadata lines, so the scan is short.
func transcriptCwd(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for i := 0; i < 500 && sc.Scan(); i++ {
		if !strings.Contains(sc.Text(), `"cwd"`) {
			continue
		}
		var line struct {
			Cwd string `json:"cwd"`
		}
		if json.Unmarshal(sc.Bytes(), &line) == nil && line.Cwd != "" {
			return line.Cwd
		}
	}
	return ""
}

// repoOf names the repository holding dir: the nearest ancestor with .git
// (a directory, or a file in a worktree).
func (r *Resolver) repoOf(dir string) (name, root string) {
	for _, t := range r.temp {
		if strings.HasPrefix(dir, t) {
			return Temp, ""
		}
	}
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			return filepath.Base(d), d
		}
		if p := filepath.Dir(d); p == d {
			break
		}
	}
	return filepath.Base(dir), dir
}
