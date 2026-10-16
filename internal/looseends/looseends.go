// Package looseends finds work left lying in the repositories Claude Code
// sessions have worked in: uncommitted files, commits nobody pushed, and
// stashes. A session that is closed before it commits leaves its changes in
// the working tree with nobody responsible for them; the next session in
// that repository finds them in its way (a deploy refused for work in no
// commit, an edit no running session will say is its own), or sweeps them
// into a commit of its own.
//
// It only reads. Nothing here commits, pushes, stashes or deletes: whether
// half-finished work is worth keeping is not something a scan can know.
// Session coordination (internal/coord) records which session left which
// file, so a report can say whose work it was.
package looseends

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Owner is who left a file uncommitted, as session coordination recorded it.
type Owner struct {
	Session string  `json:"session"`
	Label   string  `json:"label"`          // the session's folder, or its name
	Since   float64 `json:"since"`          // unix seconds: when it was left, or became master
	Live    bool    `json:"live,omitempty"` // the session is still running and masters it
}

// File is one uncommitted path.
type File struct {
	Path   string  `json:"path"`   // absolute
	Status string  `json:"status"` // git's two-letter porcelain code: " M", "??", "A ", ...
	MTime  float64 `json:"mtime"`  // unix seconds, 0 when the file is gone
	Owner  *Owner  `json:"owner,omitempty"`
}

// Commit is one commit not on the branch's upstream.
type Commit struct {
	Hash    string  `json:"hash"`
	When    float64 `json:"when"`
	Subject string  `json:"subject"`
}

// Stash is one stash entry.
type Stash struct {
	Ref     string  `json:"ref"`
	When    float64 `json:"when"`
	Subject string  `json:"subject"`
}

// Repo is what one repository has lying about.
type Repo struct {
	Root        string   `json:"root"`
	Branch      string   `json:"branch"`
	Upstream    string   `json:"upstream,omitempty"` // "" when the branch tracks nothing
	NoRemote    bool     `json:"no_remote,omitempty"`
	Uncommitted []File   `json:"uncommitted,omitempty"`
	Unpushed    []Commit `json:"unpushed,omitempty"`
	Ahead       int      `json:"ahead,omitempty"` // all of them; Unpushed lists the newest few
	Stashes     []Stash  `json:"stashes,omitempty"`
	Error       string   `json:"error,omitempty"`
}

// Loose reports whether anything is lying about.
func (r Repo) Loose() bool {
	return len(r.Uncommitted) > 0 || r.Ahead > 0 || len(r.Stashes) > 0
}

// Oldest is the time of the oldest loose thing in r, or 0.
func (r Repo) Oldest() float64 {
	var o float64
	take := func(t float64) {
		if t > 0 && (o == 0 || t < o) {
			o = t
		}
	}
	for _, f := range r.Uncommitted {
		if f.Owner != nil && f.Owner.Since > 0 {
			take(f.Owner.Since)
		} else {
			take(f.MTime)
		}
	}
	for _, c := range r.Unpushed {
		take(c.When)
	}
	for _, s := range r.Stashes {
		take(s.When)
	}
	return o
}

// Report is a scan of several repositories.
type Report struct {
	At      float64 `json:"at"`
	Scanned int     `json:"scanned"` // repositories looked at, loose or not
	Repos   []Repo  `json:"repos"`   // only those with something loose, oldest first
}

// Counts are a report's totals.
func (rp Report) Counts() (files, commits, stashes int) {
	for _, r := range rp.Repos {
		files += len(r.Uncommitted)
		commits += r.Ahead
		stashes += len(r.Stashes)
	}
	return
}

// maxCommits is how many unpushed commits a repository lists; Ahead counts all.
const maxCommits = 10

// Scan looks at each repository root, eight at a time. owners names who
// left a path uncommitted; it may be nil.
func Scan(roots []string, owners map[string]Owner, now time.Time) Report {
	rp := Report{At: float64(now.Unix()), Scanned: len(roots)}
	out := make([]Repo, len(roots))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for i, root := range roots {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, root string) {
			defer wg.Done()
			out[i] = ScanRepo(root, owners)
			<-sem
		}(i, root)
	}
	wg.Wait()
	for _, r := range out {
		if r.Loose() || r.Error != "" {
			rp.Repos = append(rp.Repos, r)
		}
	}
	sort.SliceStable(rp.Repos, func(i, j int) bool {
		a, b := rp.Repos[i].Oldest(), rp.Repos[j].Oldest()
		if a != b {
			return a < b
		}
		return rp.Repos[i].Root < rp.Repos[j].Root
	})
	return rp
}

// git runs git in dir. A variable so tests can count calls.
var git = func(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	// Never prompt, never wait on a credential helper or a lock.
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0")
	return cmd.Output()
}

// ScanRepo looks at one repository. It never fetches: "unpushed" is against
// the upstream as this clone last saw it.
func ScanRepo(root string, owners map[string]Owner) Repo {
	r := Repo{Root: root}
	if !filepath.IsAbs(root) {
		r.Error = "not a repository path"
		return r
	}
	b, err := git(root, "status", "--porcelain=v1", "-z", "--branch", "--untracked-files=all")
	if err != nil {
		r.Error = "git status failed: " + strings.TrimSpace(errText(err))
		return r
	}
	ents := splitZ(b)
	for i := 0; i < len(ents); i++ {
		ent := ents[i]
		if i == 0 && strings.HasPrefix(ent, "## ") {
			r.Branch, r.Upstream = parseBranch(ent[3:])
			continue
		}
		if len(ent) < 4 {
			continue
		}
		code, rel := ent[:2], ent[3:]
		if code[0] == 'R' || code[0] == 'C' {
			i++ // a rename or copy is followed by its source path
		}
		p := filepath.Join(root, rel)
		f := File{Path: p, Status: code}
		if st, err := os.Lstat(p); err == nil {
			f.MTime = float64(st.ModTime().Unix())
		}
		if o, ok := owners[p]; ok {
			o := o
			f.Owner = &o
		}
		r.Uncommitted = append(r.Uncommitted, f)
	}

	remotes, _ := git(root, "remote")
	r.NoRemote = len(bytes.TrimSpace(remotes)) == 0
	var rng []string
	switch {
	case r.Upstream != "":
		rng = []string{"@{upstream}..HEAD"}
	case !r.NoRemote && r.Branch != "":
		// A branch that tracks nothing: whatever no remote branch has.
		rng = []string{"HEAD", "--not", "--remotes"}
	}
	if rng != nil {
		if n, err := git(root, append([]string{"rev-list", "--count"}, rng...)...); err == nil {
			r.Ahead, _ = strconv.Atoi(strings.TrimSpace(string(n)))
		}
		if r.Ahead > 0 {
			args := append([]string{"log", "--format=%h%x09%ct%x09%s", "-n", strconv.Itoa(maxCommits)}, rng...)
			if lb, err := git(root, args...); err == nil {
				for _, l := range lines(lb) {
					p := strings.SplitN(l, "\t", 3)
					if len(p) == 3 {
						r.Unpushed = append(r.Unpushed, Commit{Hash: p[0], When: atof(p[1]), Subject: p[2]})
					}
				}
			}
		}
	}
	if sb, err := git(root, "stash", "list", "--format=%gd%x09%ct%x09%gs"); err == nil {
		for _, l := range lines(sb) {
			p := strings.SplitN(l, "\t", 3)
			if len(p) == 3 {
				r.Stashes = append(r.Stashes, Stash{Ref: p[0], When: atof(p[1]), Subject: p[2]})
			}
		}
	}
	return r
}

// parseBranch reads porcelain's "## main...origin/main [ahead 2]" header.
func parseBranch(s string) (branch, upstream string) {
	if i := strings.Index(s, " ["); i >= 0 {
		s = s[:i]
	}
	if strings.HasPrefix(s, "No commits yet on ") {
		return strings.TrimPrefix(s, "No commits yet on "), ""
	}
	if strings.HasPrefix(s, "HEAD (no branch)") {
		return "", ""
	}
	if i := strings.Index(s, "..."); i >= 0 {
		return s[:i], s[i+3:]
	}
	return s, ""
}

func splitZ(b []byte) []string {
	var out []string
	for _, p := range bytes.Split(b, []byte{0}) {
		if len(p) > 0 {
			out = append(out, string(p))
		}
	}
	return out
}

func lines(b []byte) []string {
	var out []string
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		if l := strings.TrimRight(sc.Text(), "\r"); l != "" {
			out = append(out, l)
		}
	}
	return out
}

func atof(s string) float64 { f, _ := strconv.ParseFloat(strings.TrimSpace(s), 64); return f }

func errText(err error) string {
	if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
		return string(ee.Stderr)
	}
	return err.Error()
}

// Roots are the repositories Claude Code sessions have worked in over the
// last `within`, read from the working directory each transcript under
// projectsDir records, plus extra. Only folders that are inside a git
// repository count; each is given as its top level, once.
func Roots(projectsDir string, within time.Duration, extra []string, now time.Time) []string {
	seen := map[string]bool{}
	var dirs []string
	add := func(d string) {
		if d != "" && !seen[d] {
			seen[d] = true
			dirs = append(dirs, d)
		}
	}
	for _, d := range extra {
		add(d)
	}
	matches, _ := filepath.Glob(filepath.Join(projectsDir, "*", "*.jsonl"))
	for _, m := range matches {
		st, err := os.Stat(m)
		if err != nil || now.Sub(st.ModTime()) > within {
			continue
		}
		add(transcriptCwd(m))
	}
	tops := map[string]bool{}
	var out []string
	for _, d := range dirs {
		top := TopLevel(d)
		if top != "" && !tops[top] && !scratch(top) {
			tops[top] = true
			out = append(out, top)
		}
	}
	sort.Strings(out)
	return out
}

// scratch reports whether dir is under a temporary directory: test
// repositories there come and go, and nobody's work is left in them. A
// variable because this package's own tests keep theirs there.
var scratch = func(dir string) bool {
	for _, t := range []string{os.TempDir(), "/tmp", "/private/tmp", "/var/folders", "/private/var/folders"} {
		t = strings.TrimSuffix(t, "/")
		if r, err := filepath.EvalSymlinks(t); err == nil && strings.HasPrefix(dir, strings.TrimSuffix(r, "/")+"/") {
			return true
		}
		if strings.HasPrefix(dir, t+"/") {
			return true
		}
	}
	return false
}

// TopLevel is the repository holding dir: the nearest ancestor with .git.
// Found by looking, not by asking git, so a folder that is gone or a
// hundred transcripts in one folder cost a few stats each.
func TopLevel(dir string) string {
	dir = filepath.Clean(dir)
	if !filepath.IsAbs(dir) {
		return ""
	}
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			if r, err := filepath.EvalSymlinks(d); err == nil {
				return r
			}
			return d
		}
		if p := filepath.Dir(d); p == d {
			return ""
		}
	}
}

// transcriptCwd is the first "cwd" a transcript records.
func transcriptCwd(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for i := 0; i < 500 && sc.Scan(); i++ {
		if !bytes.Contains(sc.Bytes(), []byte(`"cwd"`)) {
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
