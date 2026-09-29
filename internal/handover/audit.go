package handover

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// The audit: every HANDOFF.md the automation has written, read back from
// handover.log, so what was done on the user's behalf, where, and when is
// visible in one place and can be undone in one place.

// AuditEntry is one repository whose HANDOFF.md the writer has changed.
type AuditEntry struct {
	Root string `json:"root"`
	File string `json:"file"`
	// Exists, Bytes and Modified describe the file as it is now.
	Exists   bool      `json:"exists"`
	Bytes    int64     `json:"bytes"`
	Modified time.Time `json:"modified,omitempty"`
	// Writes is how many times the writer changed it, Commits the commits it
	// made (newest last), and FirstWrite and LastWrite when.
	Writes     int       `json:"writes"`
	Commits    []string  `json:"commits"`
	FirstWrite time.Time `json:"first_write"`
	LastWrite  time.Time `json:"last_write"`
	// Runs counts every writer run, including ones that changed nothing,
	// and Failures the ones that failed.
	Runs     int `json:"runs"`
	Failures int `json:"failures"`
}

var logLine = regexp.MustCompile(`^(\d{4}-\d\d-\d\d \d\d:\d\d:\d\d) (skip|queue|write|wrote|none|FAILED)\s+(/[^\s:,]+)`)
var commitRef = regexp.MustCompile(`committed ([0-9a-f]{7,40})`)

// Audit lists the files the writer has changed, most recently written first.
func Audit() ([]AuditEntry, error) {
	lp, err := LogPath()
	if err != nil {
		return nil, err
	}
	f, err := os.Open(lp)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	by := map[string]*AuditEntry{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		m := logLine.FindStringSubmatch(sc.Text())
		if m == nil {
			continue
		}
		at, _ := time.ParseInLocation("2006-01-02 15:04:05", m[1], time.Local)
		root := m[3]
		e := by[root]
		if e == nil {
			e = &AuditEntry{Root: root, File: filepath.Join(root, "HANDOFF.md"), Commits: []string{}}
			by[root] = e
		}
		switch m[2] {
		case "write":
			e.Runs++
		case "FAILED":
			e.Failures++
		case "wrote":
			e.Writes++
			if e.FirstWrite.IsZero() {
				e.FirstWrite = at
			}
			e.LastWrite = at
			if c := commitRef.FindStringSubmatch(sc.Text()); c != nil {
				e.Commits = append(e.Commits, c[1])
			}
		}
	}
	var out []AuditEntry
	for _, e := range by {
		if e.Writes == 0 {
			continue // opted in, but the writer never changed anything there
		}
		if st, err := os.Stat(e.File); err == nil {
			e.Exists, e.Bytes, e.Modified = true, st.Size(), st.ModTime()
		}
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastWrite.After(out[j].LastWrite) })
	return out, sc.Err()
}

// ErrNotAudited is returned for a file the writer has never written.
var ErrNotAudited = errors.New("not a handover file the writer has written")

// ReadAudited returns an audited HANDOFF.md, and nothing else: the dashboard
// passes a repository root, and only a root in the audit is read.
func ReadAudited(root string) (string, error) {
	entries, err := Audit()
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if e.Root == root {
			b, err := os.ReadFile(e.File)
			return string(b), err
		}
	}
	return "", ErrNotAudited
}

// DeleteResult says what DeleteAudited did with one file.
type DeleteResult struct {
	File   string `json:"file"`
	Backup string `json:"backup,omitempty"`
	Error  string `json:"error,omitempty"`
	// Tracked means git still has the file, so the deletion shows as a
	// change in that repository until it is committed.
	Tracked bool `json:"tracked,omitempty"`
}

// DeleteAudited removes every audited HANDOFF.md that still exists. Each is
// moved into a timestamped folder beside the log first, so a deletion can be
// undone by moving it back. Removing the file also opts the repository out:
// the hooks only act where HANDOFF.md exists.
func DeleteAudited() (backupDir string, results []DeleteResult, err error) {
	entries, err := Audit()
	if err != nil {
		return "", nil, err
	}
	dir, err := Dir()
	if err != nil {
		return "", nil, err
	}
	backupDir = filepath.Join(dir, "deleted", time.Now().Format("2006-01-02-150405"))
	var manifest []string
	for i, e := range entries {
		if !e.Exists {
			continue
		}
		if err := os.MkdirAll(backupDir, 0o700); err != nil {
			return "", results, err
		}
		r := DeleteResult{File: e.File}
		dst := filepath.Join(backupDir, fmt.Sprintf("%02d-%s-HANDOFF.md", i+1, filepath.Base(e.Root)))
		if err := copyFile(e.File, dst); err != nil {
			r.Error = "backup failed, file left in place: " + err.Error()
		} else if err := os.Remove(e.File); err != nil {
			r.Error = err.Error()
		} else {
			r.Backup = dst
			r.Tracked = exec.Command("git", "-C", e.Root, "ls-files", "--error-unmatch", "HANDOFF.md").Run() == nil
			manifest = append(manifest, dst+"\t"+e.File)
		}
		results = append(results, r)
	}
	if len(manifest) > 0 {
		_ = os.WriteFile(filepath.Join(backupDir, "MANIFEST.tsv"), []byte("backup\toriginal\n"+strings.Join(manifest, "\n")+"\n"), 0o600)
	}
	return backupDir, results, nil
}

func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o600)
}
