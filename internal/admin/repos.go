package admin

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/andrewbakercloudscale/claude-burst/internal/metrics"
)

// Spend per repository. The gateway sees a session id on every request but
// not where the session runs; Claude Code's transcript for that session
// (~/.claude/projects/<dir>/<session>.jsonl) records its working directory
// as "cwd", and the repository is the nearest directory above that holding
// .git. The project directory name alone cannot be decoded back into a
// path: Claude Code replaces every character outside [A-Za-z0-9] with "-".

const (
	unknownRepo = "(no transcript)"
	tempRepo    = "(temporary directories)"
)

// repoResolver caches session id to repository. Only found sessions are
// cached: a transcript can appear after the first request that names it.
type repoResolver struct {
	mu    sync.Mutex
	cache map[string][2]string // session -> {name, root}
	dir   string               // Claude Code's projects directory
	temp  []string             // scratch roots, grouped as one row
}

func newRepoResolver() *repoResolver {
	base := os.Getenv("CLAUDE_CONFIG_DIR")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".claude")
	}
	return &repoResolver{cache: map[string][2]string{}, dir: filepath.Join(base, "projects"),
		temp: []string{"/private/tmp/", "/tmp/", strings.TrimSuffix(os.TempDir(), "/") + "/"}}
}

func (r *repoResolver) resolve(session string) (name, root string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if v, ok := r.cache[session]; ok {
		return v[0], v[1]
	}
	matches, _ := filepath.Glob(filepath.Join(r.dir, "*", session+".jsonl"))
	if len(matches) == 0 {
		return unknownRepo, ""
	}
	cwd := transcriptCwd(matches[0])
	if cwd == "" {
		return unknownRepo, ""
	}
	name, root = r.repoOf(cwd)
	r.cache[session] = [2]string{name, root}
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
// (a directory, or a file in a worktree). Scratch sessions under the temp
// directory are grouped, since each has its own directory and none is a
// project anyone would look for.
func (r *repoResolver) repoOf(dir string) (name, root string) {
	for _, t := range r.temp {
		if strings.HasPrefix(dir, t) {
			return tempRepo, ""
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

// repoSpend groups per-session spend by repository, largest spend first.
func (r *repoResolver) repoSpend(sessions map[string]metrics.SessionUse) []metrics.RepoUse {
	by := map[string]*metrics.RepoUse{}
	for sid, su := range sessions {
		name, root := r.resolve(sid)
		u := by[name]
		if u == nil {
			u = &metrics.RepoUse{Repo: name, Path: root}
			by[name] = u
		}
		u.Sessions++
		u.Requests += su.Requests
		u.USD += su.USD
		u.Unpriced = u.Unpriced || su.Unpriced
	}
	out := make([]metrics.RepoUse, 0, len(by))
	for _, u := range by {
		out = append(out, *u)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].USD != out[j].USD {
			return out[i].USD > out[j].USD
		}
		return out[i].Repo < out[j].Repo
	})
	return out
}
