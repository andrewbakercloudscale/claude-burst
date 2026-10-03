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
	"errors"
	"io/fs"
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
		_, err := stat(filepath.Join(d, ".git"))
		observe(d, err)
		if err == nil {
			noteRepo(d)
			return filepath.Base(d), d
		}
		if p := filepath.Dir(d); p == d {
			break
		}
	}
	return filepath.Base(dir), dir
}

// stat is os.Stat, a variable so tests can answer as a denied folder would.
var stat = os.Stat

// The folders macOS guards with a privacy prompt ("claude-burst would like
// to access files in your Desktop folder"). Looking for .git under them is
// what raises it, so the lookups here are also the only honest record of
// whether access is granted: nothing else may look, since looking asks.
var protectedFolders = []string{"Desktop", "Documents", "Downloads"}

// Access states for a protected folder.
const (
	Allowed = "allowed"  // a lookup under it got an answer
	Denied  = "denied"   // a lookup under it was refused
	NotSeen = "not seen" // no session has run under it since the gateway started
)

// FolderAccess is what lookups under one protected folder last observed.
type FolderAccess struct {
	Folder string    `json:"folder"` // "Desktop"
	Path   string    `json:"path"`
	State  string    `json:"state"`
	At     time.Time `json:"at,omitempty"` // the last observation
	Repos  []string  `json:"repos"`        // repository roots found under it
}

var access struct {
	mu sync.Mutex
	m  map[string]*FolderAccess // folder name -> observation
}

// protectedFolder names the protected folder path is in, or "".
func protectedFolder(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	for _, f := range protectedFolders {
		base := filepath.Join(home, f)
		if path == base || strings.HasPrefix(path, base+string(filepath.Separator)) {
			return f
		}
	}
	return ""
}

func accessEntry(folder string) *FolderAccess {
	if access.m == nil {
		access.m = map[string]*FolderAccess{}
	}
	e := access.m[folder]
	if e == nil {
		home, _ := os.UserHomeDir()
		e = &FolderAccess{Folder: folder, Path: filepath.Join(home, folder), State: NotSeen}
		access.m[folder] = e
	}
	return e
}

// observe records what a lookup in dir says about its protected folder: a
// refusal is Denied, any other answer (found or not there) is Allowed.
// The latest observation wins, so granting access later shows up.
func observe(dir string, err error) {
	f := protectedFolder(dir)
	if f == "" {
		return
	}
	state := Allowed
	if err != nil && errors.Is(err, fs.ErrPermission) {
		state = Denied
	}
	access.mu.Lock()
	defer access.mu.Unlock()
	e := accessEntry(f)
	e.State, e.At = state, time.Now()
}

// noteRepo records a repository root found under a protected folder.
func noteRepo(root string) {
	f := protectedFolder(root)
	if f == "" {
		return
	}
	access.mu.Lock()
	defer access.mu.Unlock()
	e := accessEntry(f)
	for _, r := range e.Repos {
		if r == root {
			return
		}
	}
	if len(e.Repos) < 50 {
		e.Repos = append(e.Repos, root)
	}
}

// Access reports each protected folder as the lookups so far have seen it,
// in a fixed order. It never touches the folders itself.
func Access() []FolderAccess {
	access.mu.Lock()
	defer access.mu.Unlock()
	out := make([]FolderAccess, 0, len(protectedFolders))
	for _, f := range protectedFolders {
		e := *accessEntry(f)
		e.Repos = append([]string{}, e.Repos...)
		out = append(out, e)
	}
	return out
}

// resetAccess forgets every observation. For tests.
func resetAccess() {
	access.mu.Lock()
	access.m = nil
	access.mu.Unlock()
}
