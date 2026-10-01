// Package coord lets Claude Code sessions on one Mac edit the same files
// without clobbering each other or sweeping up each other's work, and
// without anyone waiting. It runs as Claude Code hooks (`claude-burst coord
// <hook>`, installed in ~/.claude/settings.json while the dashboard switch is
// on), so every session takes part without being asked to.
//
// Model: each file being edited has a MASTER, the first session to edit it.
//
//   - Another session's Edit of that file goes straight through. Claude
//     Code's Edit replaces an exact piece of text and refuses when the file
//     changed since it was read, so two sessions' edits cannot overwrite each
//     other. That session becomes a contributor and is told not to commit
//     the file; the master is sent what changed and commits it, contribution
//     included.
//   - A whole-file Write of a shared file by anyone but its master is
//     refused: it would wipe the master's work.
//   - `git add -A`, `git add .` and `git commit -a` are refused while another
//     session has work in the same repository, and a contributor's git add or
//     commit that names a file someone else masters is refused.
//   - A file stops being shared when it is committed, when its master's turn
//     ends and git keeps no record of it (outside a repository, or ignored),
//     or with `coord release`. A master that ends hands the file to the
//     session that asked for it (`coord take`), else its most recently
//     active contributor, or lets it go.
//   - A master that has gone idle keeps its files until another session
//     needs one: that session's Edit or Write takes it over on the spot, and
//     the old master is told. `coord take` asks a busy master for a file.
//   - A master sitting on others' uncommitted changes too long is nudged.
//   - A turn cannot end with files its session masters uncommitted: the
//     Stop hook holds the session, once per turn, to commit them (locally,
//     "WIP:" if unfinished), so a file that passes on passes on clean.
//
// It fails open. Any error, including a lock it cannot get within a few
// seconds, lets the tool call through: a broken coordinator must never stop
// work. State is one JSON file under a flock; nothing here needs the gateway.
package coord

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
)

// Settings are the timings. Zero values take the defaults in Resolved.
type Settings struct {
	MasterIdle  time.Duration // master idle this long: the file passes on
	Nudge       time.Duration // others' changes uncommitted this long: nudge the master
	SessionDead time.Duration // no sign of life this long: the session is gone
}

const (
	DefaultMasterIdle  = 15 * time.Minute
	DefaultNudge       = 10 * time.Minute
	DefaultSessionDead = 2 * time.Hour
)

// lockWait is how long a hook waits for the state lock before letting the
// tool call through. A variable so a test can shorten it.
var lockWait = 3 * time.Second

func (s Settings) Resolved() Settings {
	if s.MasterIdle <= 0 {
		s.MasterIdle = DefaultMasterIdle
	}
	if s.Nudge <= 0 {
		s.Nudge = DefaultNudge
	}
	if s.SessionDead <= 0 {
		s.SessionDead = DefaultSessionDead
	}
	return s
}

// Coordinator is one hook or CLI invocation.
type Coordinator struct {
	Dir      string           // state directory
	Exe      string           // this binary, for the commands sessions are told to run
	Settings Settings         // timings
	Now      func() time.Time // the clock; tests move it
}

func (c *Coordinator) now() float64 {
	t := time.Now()
	if c.Now != nil {
		t = c.Now()
	}
	return float64(t.UnixNano()) / 1e9
}

// ---------------------------------------------------------------- state

type session struct {
	Started    float64 `json:"started"`
	Seen       float64 `json:"seen"`
	Transcript string  `json:"transcript,omitempty"`
	Cwd        string  `json:"cwd,omitempty"`
	Label      string  `json:"label,omitempty"`
	Ended      bool    `json:"ended,omitempty"`
}

// shared is one file being edited: its master, and who else changed it
// (session id to the time of their last edit).
type shared struct {
	Master       string             `json:"master"`
	Since        float64            `json:"since"`
	Touched      float64            `json:"touched"`
	Contributors map[string]float64 `json:"contributors,omitempty"`
	Nudged       float64            `json:"nudged,omitempty"`
	Wanted       string             `json:"wanted,omitempty"` // a session that asked to take it over
}

type message struct {
	From string  `json:"from"`
	Text string  `json:"text"`
	TS   float64 `json:"ts"`
}

type state struct {
	Sessions map[string]*session  `json:"sessions"`
	Files    map[string]*shared   `json:"files"`
	Inbox    map[string][]message `json:"inbox"`
}

// tx is the state under the lock, plus what this invocation needs.
type tx struct {
	*Coordinator
	d   *state
	now float64
}

// with runs fn on the state under an exclusive lock and saves it if fn
// returns nil. It gives up after lockWait rather than stall a session.
func (c *Coordinator) with(fn func(t *tx) error) error {
	if err := os.MkdirAll(c.Dir, 0o755); err != nil {
		return err
	}
	lf, err := os.OpenFile(filepath.Join(c.Dir, "state.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer lf.Close()
	deadline := time.Now().Add(lockWait)
	for {
		err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) || time.Now().After(deadline) {
			return fmt.Errorf("state lock: %w", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	defer syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)

	d := &state{}
	path := filepath.Join(c.Dir, "state.json")
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, d) // a corrupt file starts over, never blocks
	}
	if d.Sessions == nil {
		d.Sessions = map[string]*session{}
	}
	if d.Files == nil {
		d.Files = map[string]*shared{}
	}
	if d.Inbox == nil {
		d.Inbox = map[string][]message{}
	}
	t := &tx{Coordinator: c, d: d, now: c.now()}
	if err := fn(t); err != nil {
		return err
	}
	b, err := json.MarshalIndent(d, "", " ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Log appends one line to coord.log, in local time.
func (c *Coordinator) Log(format string, a ...any) {
	f, err := os.OpenFile(filepath.Join(c.Dir, "coord.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %s\n", time.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(format, a...))
}

func short(sid string) string {
	if len(sid) > 8 {
		return sid[:8]
	}
	return sid
}

// lastActive is liveness: the transcript's mtime (Claude Code appends to it
// every turn), else the last hook seen.
func lastActive(s *session) float64 {
	if s == nil {
		return 0
	}
	t := s.Seen
	if s.Transcript != "" {
		if fi, err := os.Stat(s.Transcript); err == nil {
			if m := float64(fi.ModTime().UnixNano()) / 1e9; m > t {
				t = m
			}
		}
	}
	return t
}

func (t *tx) alive(sid string) bool {
	s := t.d.Sessions[sid]
	return s != nil && !s.Ended && t.now-lastActive(s) < t.Settings.SessionDead.Seconds()
}

func (t *tx) label(sid string) string {
	l := "?"
	if s := t.d.Sessions[sid]; s != nil && s.Label != "" {
		l = s.Label
	}
	return fmt.Sprintf("%s [%s]", l, short(sid))
}

func (t *tx) ago(ts float64) string {
	s := int(t.now - ts)
	if s < 90 {
		return fmt.Sprintf("%ds", s)
	}
	return fmt.Sprintf("%dm", s/60)
}

func (t *tx) notify(sid, from, text string) {
	t.d.Inbox[sid] = append(t.d.Inbox[sid], message{From: from, Text: text, TS: t.now})
}

func (t *tx) fmtMsgs(msgs []message) string {
	var out []string
	for _, m := range msgs {
		who := "the coordinator"
		switch {
		case m.From == "user":
			who = "the user"
		case m.From != "system":
			who = "session " + t.label(m.From)
		}
		out = append(out, fmt.Sprintf("- from %s (%s ago): %s", who, t.ago(m.TS), m.Text))
	}
	return strings.Join(out, "\n")
}

// valid returns the file's entry if its master still stands, handing it on
// (or letting it go) when the master has ended. An idle master keeps the
// file: it is taken over only when another session needs it (takeOver).
func (t *tx) valid(path string) *shared {
	f := t.d.Files[path]
	if f == nil {
		return nil
	}
	if t.alive(f.Master) {
		return f
	}
	t.handOn(path, "its session ended")
	return t.d.Files[path]
}

// idleFor is how long the file's master has done nothing, and whether that
// is long enough for another session to take the file over.
func (t *tx) idleFor(f *shared) (float64, bool) {
	d := t.now - max(f.Touched, lastActive(t.d.Sessions[f.Master]))
	return d, !t.alive(f.Master) || d > t.Settings.MasterIdle.Seconds()
}

// takeOver makes sid the master of path in place of an idle or ended one,
// tells the old master, and returns what sid must be told. The old master's
// uncommitted changes, if any, stay in the file: it becomes a contributor,
// so it may not commit the file, and the new master commits them.
func (t *tx) takeOver(path, sid string) (ctx string, dirty bool) {
	f := t.d.Files[path]
	old := f.Master
	idle, _ := t.idleFor(f)
	dirty = gitState(path) == fileDirty
	delete(f.Contributors, sid)
	if dirty {
		if f.Contributors == nil {
			f.Contributors = map[string]float64{}
		}
		f.Contributors[old] = t.now
	}
	f.Master, f.Since, f.Touched, f.Nudged, f.Wanted = sid, t.now, t.now, t.now, ""
	was := fmt.Sprintf("%s was, and had been idle for %s", t.label(old), t.ago(t.now-idle))
	if !t.alive(old) {
		was = t.label(old) + " was, and has ended"
	}
	ctx = fmt.Sprintf("COORDINATION: you are now the master of %s (%s). Commit it when your work in it is done.", path, was)
	if dirty {
		ctx += fmt.Sprintf(" It holds uncommitted changes that are not yours: read `git diff -- %s` and keep them, they are committed with your work.", path)
	}
	if t.alive(old) {
		t.notify(old, "system", fmt.Sprintf("You were idle for %s, so %s has taken over %s and now commits it. Do not git add or commit it yourself; any changes of yours still in it are committed by the new master. If you need to change it again, use Edit: it goes through and the new master is told.",
			t.ago(t.now-idle), t.label(sid), path))
	}
	t.Log("master of %s taken over by %s from %s (idle %s)", path, short(sid), short(old), t.ago(t.now-idle))
	return ctx, dirty
}

// handOn passes the file to the session that asked for it, else its most
// recently active live contributor, who is told it now commits the file,
// or lets it go when there is none.
func (t *tx) handOn(path, why string) {
	f := t.d.Files[path]
	if f == nil {
		return
	}
	old := f.Master
	next, best := "", -1.0
	for sid := range f.Contributors {
		if sid != old && t.alive(sid) {
			if a := lastActive(t.d.Sessions[sid]); a > best {
				next, best = sid, a
			}
		}
	}
	if f.Wanted != "" && f.Wanted != old && t.alive(f.Wanted) {
		next = f.Wanted
	}
	if next == "" {
		delete(t.d.Files, path)
		t.Log("free %s from %s (%s)", path, short(old), why)
		return
	}
	delete(f.Contributors, next)
	f.Master, f.Since, f.Touched, f.Nudged, f.Wanted = next, t.now, t.now, 0, ""
	t.notify(next, "system", fmt.Sprintf("You are now the master of %s (%s was, %s). It may hold uncommitted changes from other sessions as well as yours: look at `git diff -- %s`, keep them, and commit the file when your work in it is done.",
		path, t.label(old), why, path))
	t.Log("master of %s passes from %s to %s (%s)", path, short(old), short(next), why)
}

// hookInput is the part of Claude Code's hook JSON this reads.
type hookInput struct {
	SessionID      string         `json:"session_id"`
	TranscriptPath string         `json:"transcript_path"`
	Cwd            string         `json:"cwd"`
	ToolName       string         `json:"tool_name"`
	ToolInput      map[string]any `json:"tool_input"`
	StopHookActive bool           `json:"stop_hook_active"`
}

func (t *tx) touch(h hookInput) string {
	sid := h.SessionID
	if sid == "" {
		return ""
	}
	s := t.d.Sessions[sid]
	if s == nil {
		s = &session{Started: t.now}
		t.d.Sessions[sid] = s
	}
	s.Seen = t.now
	if h.TranscriptPath != "" {
		s.Transcript = h.TranscriptPath
	}
	if h.Cwd != "" {
		s.Cwd = h.Cwd
	}
	if s.Label == "" {
		s.Label = filepath.Base(strings.TrimRight(s.Cwd, "/"))
		if s.Label == "" || s.Label == "." || s.Label == "/" {
			s.Label = "session"
		}
	}
	s.Ended = false
	return sid
}

func (t *tx) gc() {
	for sid := range t.d.Sessions {
		if !t.alive(sid) {
			t.endSession(sid, "stale")
			delete(t.d.Sessions, sid)
			delete(t.d.Inbox, sid)
		}
	}
	for p := range t.d.Files {
		t.valid(p)
	}
}

func (t *tx) endSession(sid, why string) {
	for p, f := range t.d.Files {
		delete(f.Contributors, sid)
		if f.Master == sid {
			t.handOn(p, why)
		}
	}
	if s := t.d.Sessions[sid]; s != nil {
		s.Ended = true
	}
}

func (t *tx) masterOf(sid string) []string {
	var out []string
	for p, f := range t.d.Files {
		if f.Master == sid {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// contributedTo is the files sid changed that another session masters.
func (t *tx) contributedTo(sid string) []string {
	var out []string
	for p, f := range t.d.Files {
		if _, ok := f.Contributors[sid]; ok && f.Master != sid {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

var editTools = map[string]bool{"Edit": true, "Write": true, "MultiEdit": true, "NotebookEdit": true}

func targetPath(h hookInput) string {
	p, _ := h.ToolInput["file_path"].(string)
	if p == "" {
		p, _ = h.ToolInput["notebook_path"].(string)
	}
	if p == "" {
		return ""
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(h.Cwd, p)
	}
	return realPath(p)
}

// realPath resolves symlinks in the longest existing prefix, so a file
// being created (which does not exist yet) still gets a canonical name.
func realPath(p string) string {
	p = filepath.Clean(p)
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	dir, base := filepath.Split(p)
	if dir == "" || dir == p {
		return p
	}
	return filepath.Join(realPath(strings.TrimSuffix(dir, "/")), base)
}

// fileState is what git says about a file.
type fileState int

const (
	fileDirty   fileState = iota // uncommitted changes: someone's work in progress
	fileClean                    // tracked and committed
	fileOutside                  // not in a repository, or ignored: git keeps no record
)

// existingDir is the nearest directory of p that exists.
func existingDir(p string) string {
	dir := filepath.Dir(p)
	for {
		if _, err := os.Stat(dir); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return dir
		}
		dir = parent
	}
}

func gitState(path string) fileState {
	out, err := exec.Command("git", "-C", existingDir(path), "status", "--porcelain", "--ignored", "--", path).Output()
	if err != nil {
		return fileOutside // not a repository (exit 128), or git missing
	}
	s := strings.TrimSpace(string(out))
	switch {
	case s == "":
		return fileClean
	case strings.HasPrefix(s, "!!"):
		return fileOutside
	}
	return fileDirty
}

// repoRoot is the git top level holding path, or "".
func repoRoot(path string) string {
	out, err := exec.Command("git", "-C", existingDir(path), "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return ""
	}
	return realPath(strings.TrimSpace(string(out)))
}

// ---------------------------------------------------------------- hooks

func emit(w io.Writer, event string, kv map[string]any) {
	o := map[string]any{"hookEventName": event}
	for k, v := range kv {
		o[k] = v
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(map[string]any{"hookSpecificOutput": o})
}

func deny(w io.Writer, reason string) {
	emit(w, "PreToolUse", map[string]any{"permissionDecision": "deny", "permissionDecisionReason": reason})
}

func (c *Coordinator) cmdLine(args string) string {
	exe := c.Exe
	if exe == "" {
		exe = "claude-burst"
	}
	return fmt.Sprintf("%q coord %s", exe, args)
}

// Hook runs one hook: name is the subcommand, in is Claude Code's JSON.
func (c *Coordinator) Hook(name string, in io.Reader, out io.Writer) error {
	var h hookInput
	if err := json.NewDecoder(in).Decode(&h); err != nil {
		return err
	}
	switch name {
	case "session-start":
		return c.sessionStart(h, out)
	case "pre-tool":
		return c.preTool(h, out)
	case "post-tool":
		return c.postTool(h, out)
	case "prompt":
		return c.prompt(h, out)
	case "stop":
		return c.stop(h, out)
	case "session-end":
		return c.with(func(t *tx) error {
			if h.SessionID != "" {
				t.endSession(h.SessionID, "its session ended")
			}
			return nil
		})
	}
	return fmt.Errorf("unknown hook %q", name)
}

func (c *Coordinator) sessionStart(h hookInput, out io.Writer) error {
	return c.with(func(t *tx) error {
		t.gc()
		sid := t.touch(h)
		if sid == "" {
			return nil
		}
		me := t.d.Sessions[sid]
		lines := []string{
			"SESSION COORDINATION is on for this Mac (Claude Burst). Other Claude Code sessions may be editing the same files. Nobody waits; the rules keep work from being lost or swept up:",
			"1. The first session to edit a file is its MASTER and commits it. You may still Edit a file another session masters: your change is applied, the master is told, and the master commits it with its own. Do not `git add` or commit a file another session masters; you will be told which ones those are.",
			"2. Use Edit, not Write, on a file another session masters: a whole-file Write would wipe its work, and is refused.",
			"3. `git add` only files you changed, by name. `git add -A`, `git add .` and `git commit -a` are refused while another session has work in the same repository.",
			"4. Never run git stash, git reset --hard, git checkout -- <file> or switch branches in a working tree another session is using.",
			"5. Commit the files you master before you stop: a session that has gone idle hands its files to whoever needs them next, so they must not be left uncommitted. Commit locally only; unfinished work is committed with \"WIP:\" in the message. You will be reminded at the end of a turn that leaves any uncommitted.",
			fmt.Sprintf("6. If a file's master has been idle for %d minutes or has ended, your Edit of it makes you its master and you commit it. To ask a busy master for a file: `%s`.",
				int(t.Settings.MasterIdle.Minutes()), c.cmdLine(fmt.Sprintf(`take <path> --session %s`, short(sid)))),
			fmt.Sprintf("Your session id is %s. To message another session: `%s`.", sid, c.cmdLine(fmt.Sprintf(`send <session-id-prefix> "message" --from %s`, short(sid)))),
		}
		var others []string
		for o := range t.d.Sessions {
			if o != sid && t.alive(o) {
				others = append(others, o)
			}
		}
		sort.Strings(others)
		if len(others) == 0 {
			lines = append(lines, "", "No other sessions are active right now.")
		} else {
			lines = append(lines, "", "Other active sessions right now:")
			for _, o := range others {
				s := t.d.Sessions[o]
				same := ""
				if s.Cwd == me.Cwd {
					same = " (the SAME working directory as you)"
				}
				line := fmt.Sprintf("- %s in %s%s, active %s ago", t.label(o), s.Cwd, same, t.ago(lastActive(s)))
				if m := t.masterOf(o); len(m) > 0 {
					line += ", master of: " + strings.Join(m, ", ")
				}
				lines = append(lines, line)
			}
		}
		emit(out, "SessionStart", map[string]any{"additionalContext": strings.Join(lines, "\n")})
		return nil
	})
}

func snippet(v any) string {
	s, _ := v.(string)
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 160 {
		s = s[:160] + "..."
	}
	return s
}

// describeEdit says in one line what an edit changes, for the master.
func describeEdit(h hookInput) string {
	switch h.ToolName {
	case "Edit":
		return fmt.Sprintf("replaced %q with %q", snippet(h.ToolInput["old_string"]), snippet(h.ToolInput["new_string"]))
	case "MultiEdit":
		n := 0
		if e, ok := h.ToolInput["edits"].([]any); ok {
			n = len(e)
		}
		return fmt.Sprintf("made %d edits", n)
	case "NotebookEdit":
		return "edited a notebook cell"
	}
	return "rewrote it"
}

func (c *Coordinator) preTool(h hookInput, out io.Writer) error {
	if h.ToolName == "Bash" {
		return c.preBash(h, out)
	}
	if !editTools[h.ToolName] {
		return nil
	}
	path := targetPath(h)
	if path == "" {
		return nil
	}
	return c.with(func(t *tx) error {
		sid := t.touch(h)
		if sid == "" {
			return nil
		}
		f := t.valid(path)
		if f == nil {
			t.d.Files[path] = &shared{Master: sid, Since: t.now, Touched: t.now}
			if gitState(path) == fileDirty {
				// Work no session is recorded as making: by hand, by a shell
				// command, or by a session that ended without committing.
				t.Log("%s masters %s, which already had uncommitted changes", short(sid), path)
				emit(out, "PreToolUse", map[string]any{"additionalContext": fmt.Sprintf("COORDINATION: you are now the master of %s, and it already had uncommitted changes before this edit that no running session is recorded as making (made by hand, by a shell command, or by a session that has ended). Read `git diff -- %s` and keep them: commit them with your work, or ask the user if they look unintended. Never discard them yourself.",
					path, path)})
			}
			return nil // the first to edit it: master
		}
		if f.Master == sid {
			f.Touched = t.now
			return nil
		}
		if _, can := t.idleFor(f); can {
			ctx, dirty := t.takeOver(path, sid)
			if h.ToolName == "Write" && dirty {
				deny(out, ctx+" A whole-file Write would wipe those changes, so it was NOT applied. Re-read the file and use Edit for the parts you need to change.")
				return nil
			}
			emit(out, "PreToolUse", map[string]any{"additionalContext": ctx})
			return nil
		}
		if h.ToolName == "Write" {
			if _, err := os.Stat(path); err == nil {
				t.Log("refuse Write by %s on %s (master %s)", short(sid), path, short(f.Master))
				deny(out, fmt.Sprintf("COORDINATION: %s is being edited by another Claude session, %s, which has not committed it yet. A whole-file Write would wipe its changes, so it was NOT applied. Re-read the file and use Edit for the parts you need to change; that goes through, and %s commits it with your change.",
					path, t.label(f.Master), t.label(f.Master)))
				return nil
			}
		}
		if f.Contributors == nil {
			f.Contributors = map[string]float64{}
		}
		_, again := f.Contributors[sid]
		f.Contributors[sid] = t.now
		t.notify(f.Master, sid, fmt.Sprintf("I also changed %s, which you are master of: %s. Keep my change and commit it with yours. If it gets in the way of what you are doing, message me: `%s`.",
			path, describeEdit(h), c.cmdLine(fmt.Sprintf(`send %s "..." --from %s`, short(sid), short(f.Master)))))
		t.Log("share %s: %s edits, master %s", path, short(sid), short(f.Master))
		if !again {
			emit(out, "PreToolUse", map[string]any{"additionalContext": fmt.Sprintf("COORDINATION: %s is shared. Its master is %s, which is also changing it and has not committed it yet. Your edit goes through and the master has been told; it commits the file, your change included. Do NOT git add or commit %s yourself. If your work needs it committed by a certain point, tell the master: `%s`.",
				path, t.label(f.Master), path, c.cmdLine(fmt.Sprintf(`send %s "..." --from %s`, short(f.Master), short(sid))))})
		}
		return nil
	})
}

var (
	gitAddAll    = regexp.MustCompile(`\bgit\s+(-C\s+\S+\s+)?add\s+(.*\s)?(-A|--all|\.|-u|--update)(\s|$|;|&)`)
	gitCommitAll = regexp.MustCompile(`\bgit\s+(-C\s+\S+\s+)?commit\s+(.*\s)?(-a|-am|--all)(\s|$|;|&)`)
	gitStaging   = regexp.MustCompile(`\bgit\s+(-C\s+\S+\s+)?(add|commit)\b`)
)

// preBash refuses git commands that would sweep up another session's work.
func (c *Coordinator) preBash(h hookInput, out io.Writer) error {
	cmd, _ := h.ToolInput["command"].(string)
	if !gitStaging.MatchString(cmd) {
		return nil
	}
	return c.with(func(t *tx) error {
		sid := t.touch(h)
		if sid == "" {
			return nil
		}
		for p := range t.d.Files {
			t.valid(p)
		}
		// Files another session masters that this one must not stage.
		for _, p := range t.contributedTo(sid) {
			rel := p
			if root := repoRoot(p); root != "" {
				if r, err := filepath.Rel(root, p); err == nil {
					rel = r
				}
			}
			if strings.Contains(cmd, rel) || strings.Contains(cmd, filepath.Base(p)) {
				f := t.d.Files[p]
				t.Log("refuse %s staging %s (master %s)", short(sid), p, short(f.Master))
				deny(out, fmt.Sprintf("COORDINATION: %s is mastered by %s, which commits it with your change in it. Nothing was run. Leave it out: stage only files you master or edited alone, by name.",
					p, t.label(f.Master)))
				return nil
			}
		}
		if !gitAddAll.MatchString(cmd) && !gitCommitAll.MatchString(cmd) {
			return nil
		}
		root := repoRoot(filepath.Join(h.Cwd, "x"))
		if root == "" {
			return nil
		}
		var others []string
		seen := map[string]bool{}
		for p, f := range t.d.Files {
			if !strings.HasPrefix(p, root+string(filepath.Separator)) {
				continue
			}
			for who := range f.Contributors {
				if who != sid && !seen[who] {
					seen[who] = true
					others = append(others, t.label(who))
				}
			}
			if f.Master != sid && !seen[f.Master] {
				seen[f.Master] = true
				others = append(others, t.label(f.Master))
			}
		}
		if len(others) == 0 {
			return nil
		}
		sort.Strings(others)
		t.Log("refuse sweeping git by %s in %s", short(sid), root)
		deny(out, fmt.Sprintf("COORDINATION: %s also has uncommitted work in %s, so `git add -A`, `git add .`, `git add -u` and `git commit -a` would sweep it into your commit. Nothing was run. Stage the files you changed by name instead.",
			strings.Join(others, " and "), root))
		return nil
	})
}

// settle ends sharing for files that need it no more: committed ones
// always (whoever committed them), and at the end of this session's turn
// also the ones it masters that git keeps no record of, which no commit
// would ever settle.
func (t *tx) settle(sid string, turnOver bool) {
	for p, f := range t.d.Files {
		switch gitState(p) {
		case fileClean:
			delete(t.d.Files, p)
			t.Log("free %s (committed)", p)
		case fileOutside:
			if turnOver && f.Master == sid {
				delete(t.d.Files, p)
				t.Log("free %s (turn ended, not tracked by git)", p)
			}
		}
	}
}

func (t *tx) masterNudges(sid string) []string {
	var notes []string
	n := t.Settings.Nudge.Seconds()
	for _, p := range t.masterOf(sid) {
		f := t.d.Files[p]
		var who []string
		first := 0.0
		for c, ts := range f.Contributors {
			if first == 0 || ts < first {
				first = ts
			}
			who = append(who, t.label(c))
		}
		if len(who) == 0 || t.now-first <= n || t.now-f.Nudged <= n {
			continue
		}
		sort.Strings(who)
		f.Nudged = t.now
		notes = append(notes, fmt.Sprintf("- %s: changes from %s have waited %s to be committed. Finish your part and commit the file (by name), or hand it over: `%s`.",
			p, strings.Join(who, ", "), t.ago(first), t.cmdLine(fmt.Sprintf("release %q --session %s", p, short(sid)))))
	}
	return notes
}

func (t *tx) deliver(sid string) string {
	msgs := t.d.Inbox[sid]
	delete(t.d.Inbox, sid)
	var parts []string
	if len(msgs) > 0 {
		parts = append(parts, "Messages from the session coordinator or other sessions:\n"+t.fmtMsgs(msgs))
	}
	if n := t.masterNudges(sid); len(n) > 0 {
		parts = append(parts, "Other sessions' changes are waiting for your commit:\n"+strings.Join(n, "\n"))
	}
	return strings.Join(parts, "\n\n")
}

func (c *Coordinator) postTool(h hookInput, out io.Writer) error {
	return c.with(func(t *tx) error {
		sid := t.touch(h)
		if sid == "" {
			return nil
		}
		if h.ToolName == "Bash" {
			if cmd, _ := h.ToolInput["command"].(string); strings.Contains(cmd, "git") && strings.Contains(cmd, "commit") {
				t.settle(sid, false)
			}
		}
		if ctx := t.deliver(sid); ctx != "" {
			emit(out, "PostToolUse", map[string]any{"additionalContext": ctx})
		}
		return nil
	})
}

func (c *Coordinator) prompt(h hookInput, out io.Writer) error {
	return c.with(func(t *tx) error {
		t.gc()
		sid := t.touch(h)
		if sid == "" {
			return nil
		}
		if ctx := t.deliver(sid); ctx != "" {
			emit(out, "UserPromptSubmit", map[string]any{"additionalContext": ctx})
		}
		return nil
	})
}

// stop ends a turn. Files git keeps no record of are let go; files this
// session masters that still have uncommitted changes must be committed
// first, so that whoever needs them next takes over a clean file. The
// session is held once per turn with the instruction to commit; when it
// stops again in the same turn (stop_hook_active) it is let go, so a
// commit that cannot be made never keeps a session looping.
func (c *Coordinator) stop(h hookInput, out io.Writer) error {
	return c.with(func(t *tx) error {
		sid := t.touch(h)
		if sid == "" {
			return nil
		}
		t.settle(sid, true)
		var dirty []string
		for _, p := range t.masterOf(sid) {
			if gitState(p) == fileDirty {
				dirty = append(dirty, p)
			}
		}
		if len(dirty) == 0 {
			return nil
		}
		if h.StopHookActive {
			t.Log("%s stopped with uncommitted %s", short(sid), strings.Join(dirty, ", "))
			return nil
		}
		sort.Strings(dirty)
		t.Log("hold %s to commit %s", short(sid), strings.Join(dirty, ", "))
		var list []string
		for _, p := range dirty {
			line := "- " + p
			if f := t.d.Files[p]; len(f.Contributors) > 0 {
				var who []string
				for o := range f.Contributors {
					who = append(who, t.label(o))
				}
				sort.Strings(who)
				line += " (also holds changes from " + strings.Join(who, ", ") + ": keep them, they are committed with yours)"
			}
			list = append(list, line)
		}
		enc := json.NewEncoder(out)
		enc.SetEscapeHTML(false)
		return enc.Encode(map[string]any{"decision": "block", "reason": "COORDINATION: before you stop, commit the files you master. Other sessions take over files from a session that has gone idle, so they must not be left uncommitted. These have uncommitted changes:\n" +
			strings.Join(list, "\n") +
			"\nStage them by name and commit them now, with a message saying what the change is. Commit locally only: do not push. If the work is unfinished, commit it anyway with \"WIP:\" at the start of the message, so the next session can see it and carry on. Then stop. If a commit cannot be made, say why in one line and stop."})
	})
}

// ---------------------------------------------------------------- CLI and dashboard

// Status is the dashboard's and `coord status`'s view.
type Status struct {
	Sessions []SessionStatus `json:"sessions"`
	Files    []FileStatus    `json:"files"`
}

type SessionStatus struct {
	ID         string   `json:"id"`
	Label      string   `json:"label"`
	Name       string   `json:"name,omitempty"`
	Cwd        string   `json:"cwd"`
	ActiveAgoS int      `json:"active_ago_s"`
	MasterOf   []string `json:"master_of,omitempty"`
}

type FileStatus struct {
	Path        string `json:"path"`
	Master      string `json:"master"`
	MasterLabel string `json:"master_label"`
	MasterName  string `json:"master_name,omitempty"`
	SinceS      int    `json:"since_s"`
	// IdleS is how long the master has done nothing; past Master idle,
	// TakeOver is set and another session's edit takes the file over.
	IdleS        int      `json:"idle_s"`
	TakeOver     bool     `json:"take_over,omitempty"`
	Wanted       string   `json:"wanted,omitempty"` // the label of a session that asked for it
	Contributors []string `json:"contributors,omitempty"`
	// ContributorNames is Contributors' session names, in the same order.
	ContributorNames []string `json:"contributor_names,omitempty"`
}

var titleRe = regexp.MustCompile(`"type":"(custom-title|ai-title)","(?:customTitle|aiTitle)":"((?:[^"\\]|\\.)*)"`)

// sessionName is what Claude Code calls the session: the title it was
// given (/rename, or the usage panel's folder title) unless that is just
// the folder's name, else the title Claude Code generated for it. It reads
// the transcript's last megabyte, where the latest titles are.
func sessionName(transcript, folder string) string {
	f, err := os.Open(transcript)
	if err != nil {
		return ""
	}
	defer f.Close()
	const tail = 1 << 20
	if fi, err := f.Stat(); err == nil && fi.Size() > tail {
		f.Seek(fi.Size()-tail, io.SeekStart)
	}
	b, _ := io.ReadAll(f)
	custom, ai := "", ""
	for _, m := range titleRe.FindAllSubmatch(b, -1) {
		var v string
		if json.Unmarshal([]byte(`"`+string(m[2])+`"`), &v) != nil {
			continue
		}
		if string(m[1]) == "custom-title" {
			custom = v
		} else {
			ai = v
		}
	}
	if custom != "" && custom != folder {
		return custom
	}
	return ai
}

// Status tidies the state (idle masters, dead sessions, committed files)
// and reports it.
func (c *Coordinator) Status() (Status, error) {
	var st Status
	err := c.with(func(t *tx) error {
		t.gc()
		for p := range t.d.Files {
			if gitState(p) == fileClean {
				delete(t.d.Files, p)
			}
		}
		names := map[string]string{}
		for sid, s := range t.d.Sessions {
			if !t.alive(sid) {
				continue
			}
			names[sid] = sessionName(s.Transcript, s.Label)
			st.Sessions = append(st.Sessions, SessionStatus{ID: sid, Label: s.Label, Name: names[sid], Cwd: s.Cwd,
				ActiveAgoS: int(t.now - lastActive(s)), MasterOf: t.masterOf(sid)})
		}
		sort.Slice(st.Sessions, func(i, j int) bool { return st.Sessions[i].ActiveAgoS < st.Sessions[j].ActiveAgoS })
		for p, f := range t.d.Files {
			idle, can := t.idleFor(f)
			fs := FileStatus{Path: p, Master: f.Master, MasterLabel: t.label(f.Master), MasterName: names[f.Master], SinceS: int(t.now - f.Since),
				IdleS: int(idle), TakeOver: can}
			if f.Wanted != "" {
				fs.Wanted = t.label(f.Wanted)
			}
			var cs []string
			for c := range f.Contributors {
				cs = append(cs, c)
			}
			sort.Slice(cs, func(i, j int) bool { return t.label(cs[i]) < t.label(cs[j]) })
			for _, c := range cs {
				fs.Contributors = append(fs.Contributors, t.label(c))
				fs.ContributorNames = append(fs.ContributorNames, names[c])
			}
			st.Files = append(st.Files, fs)
		}
		sort.Slice(st.Files, func(i, j int) bool { return st.Files[i].Path < st.Files[j].Path })
		return nil
	})
	return st, err
}

func (t *tx) resolve(prefix string) (string, error) {
	var hits []string
	for sid := range t.d.Sessions {
		if strings.HasPrefix(sid, prefix) {
			hits = append(hits, sid)
		}
	}
	if len(hits) != 1 {
		return "", fmt.Errorf("session prefix %q matches %d sessions", prefix, len(hits))
	}
	return hits[0], nil
}

// Send queues a message for the session whose id starts with to. from is a
// session id prefix, or "" for the user.
func (c *Coordinator) Send(to, from, text string) (string, error) {
	var target string
	err := c.with(func(t *tx) error {
		tsid, err := t.resolve(to)
		if err != nil {
			return err
		}
		fsid := "user"
		if from != "" {
			if fsid, err = t.resolve(from); err != nil {
				return err
			}
		}
		t.notify(tsid, fsid, text)
		target = tsid
		return nil
	})
	return target, err
}

// Take makes the session whose id starts with sessionPrefix the master of
// path. An idle or ended master loses it now; a busy one is asked, and the
// file passes to the asker when the master releases it or ends (a commit
// frees it, so the asker's next edit makes it master). It returns what
// happened, in a sentence for the asker.
func (c *Coordinator) Take(path, sessionPrefix string) (string, error) {
	path = realPath(path)
	var res string
	err := c.with(func(t *tx) error {
		sid, err := t.resolve(sessionPrefix)
		if err != nil {
			return err
		}
		f := t.valid(path)
		switch {
		case f == nil:
			t.d.Files[path] = &shared{Master: sid, Since: t.now, Touched: t.now}
			res = "nobody was editing it: you are now its master"
		case f.Master == sid:
			res = "you are already its master"
		default:
			if _, can := t.idleFor(f); can {
				res, _ = t.takeOver(path, sid)
				return nil
			}
			f.Wanted = sid
			t.notify(f.Master, sid, fmt.Sprintf("I need %s. When your work in it is done, commit it (by name), or hand it to me uncommitted: `%s`.",
				path, c.cmdLine(fmt.Sprintf("release %q --session %s", path, short(f.Master)))))
			t.Log("%s asks %s for %s", short(sid), short(f.Master), path)
			res = fmt.Sprintf("%s is busy in it, so it has been asked. The file is yours when it commits or releases it, or after it has been idle %d minutes; meanwhile your Edits go through and it commits them.",
				t.label(f.Master), int(t.Settings.MasterIdle.Minutes()))
		}
		return nil
	})
	return res, err
}

// Release hands path on, as if its master had ended. With a session
// prefix, only if that session is the master.
func (c *Coordinator) Release(path, sessionPrefix string) error {
	path = realPath(path)
	return c.with(func(t *tx) error {
		f := t.d.Files[path]
		if f == nil {
			return fmt.Errorf("no session is editing %s", path)
		}
		if sessionPrefix != "" && !strings.HasPrefix(f.Master, sessionPrefix) {
			return fmt.Errorf("%s is mastered by another session", path)
		}
		t.handOn(path, "released by hand")
		return nil
	})
}
