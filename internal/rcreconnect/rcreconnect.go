// Package rcreconnect brings Remote Control back to a Claude Code session
// that gave up on it.
//
// Claude Code stops reconnecting Remote Control after a run of drops and
// says "Remote Control disconnected ... run /remote-control to reconnect".
// It never tries again by itself, so a session the network dropped stays
// off the phone until someone types that at the Mac. On 2026-11-05 a
// hotspot on and off for three hours left two sessions like that
// (aws-regulatory-risk-score from 15:44, wordpress-cyber-devtools from
// 16:17) while the phone said "Remote control session is offline".
//
// Every minute this finds such a session and, once it is safe, types
// /remote-control into it through Ghostty Dock's request folder. Safe means
// all of:
//   - Claude Code has dropped the link itself: no bridge in its
//     ~/.claude/sessions/<pid>.json and the disconnect line on its screen
//     (a session whose user turned Remote Control off says neither);
//   - the session is idle, and has been for a minute;
//   - its prompt is empty, so nothing half typed is sent;
//   - nobody has touched the Mac's keyboard or mouse for two minutes, so
//     this never types under someone's hands;
//   - Anthropic answered Burst in the last two minutes.
//
// Attempts back off (now, then 2, 5, 10, then every 15 minutes) and start
// again once the session has its bridge back.
package rcreconnect

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// DisconnectLine is what Claude Code prints when it gives up.
const DisconnectLine = "Remote Control disconnected"

// Backoff is the wait before each attempt after the first; the last repeats.
var Backoff = []time.Duration{2 * time.Minute, 5 * time.Minute, 10 * time.Minute, 15 * time.Minute}

const (
	settleIdle  = time.Minute     // the session idle at least this long
	handsOff    = 2 * time.Minute // the Mac untouched at least this long
	screenLines = 60              // how much of the screen to read
)

// Session is the part of ~/.claude/sessions/<pid>.json used here.
type Session struct {
	Pid             int    `json:"pid"`
	ID              string `json:"sessionId"`
	Name            string `json:"name"`
	Status          string `json:"status"`
	StatusUpdatedAt int64  `json:"statusUpdatedAt"` // ms
	Bridge          string `json:"bridgeSessionId"`
}

// Deps are the outside world, replaced in tests.
type Deps struct {
	SessionsDir string
	Alive       func(pid int) bool
	NetworkUp   func() bool
	HandsOff    func() (time.Duration, error)
	Screen      func(sessionID string, lines int) (string, error)
	Type        func(sessionID, text string) error
	Now         func() time.Time
	Logf        func(format string, args ...any)
}

type state struct {
	tries int
	last  time.Time
	sent  bool // typed, waiting to see the bridge come back
}

// Watcher keeps the per-session attempt state between ticks.
type Watcher struct {
	d    Deps
	seen map[int]*state
}

// New returns a Watcher over d.
func New(d Deps) *Watcher { return &Watcher{d: d, seen: map[int]*state{}} }

// Tick looks once, and types into at most one session per call so a
// failure of the Dock's channel shows on the first session, not all.
func (w *Watcher) Tick() {
	now := w.d.Now()
	live := map[int]bool{}
	for _, s := range w.sessions() {
		live[s.Pid] = true
		st := w.seen[s.Pid]
		if s.Bridge != "" {
			if st != nil && st.sent {
				w.d.Logf("remote control: %s is connected again", label(s))
			}
			delete(w.seen, s.Pid)
			continue
		}
		if st == nil {
			st = &state{}
			w.seen[s.Pid] = st
		}
		if st.tries > 0 {
			i := st.tries - 1
			if i >= len(Backoff) {
				i = len(Backoff) - 1
			}
			if now.Sub(st.last) < Backoff[i] {
				continue
			}
		}
		if why := w.blocked(s, now); why != "" {
			continue
		}
		st.tries++
		st.last = now
		if err := w.d.Type(s.ID, "/remote-control"); err != nil {
			w.d.Logf("remote control: could not type /remote-control into %s (attempt %d): %v", label(s), st.tries, err)
			return
		}
		st.sent = true
		w.d.Logf("remote control: %s had given up on Remote Control; typed /remote-control into it (attempt %d)", label(s), st.tries)
		return
	}
	for pid := range w.seen {
		if !live[pid] {
			delete(w.seen, pid)
		}
	}
}

// blocked says why s may not be typed into now, "" when it may. The cheap
// checks come first: the screen read and the HID query run only for a
// session that passed the rest.
func (w *Watcher) blocked(s Session, now time.Time) string {
	if s.Status != "idle" {
		return "busy"
	}
	if now.Sub(time.UnixMilli(s.StatusUpdatedAt)) < settleIdle {
		return "only just idle"
	}
	if !w.d.NetworkUp() {
		return "network down"
	}
	idle, err := w.d.HandsOff()
	if err != nil || idle < handsOff {
		return "someone is at the Mac"
	}
	screen, err := w.d.Screen(s.ID, screenLines)
	if err != nil {
		return "screen unreadable"
	}
	if !GaveUp(screen) {
		return "no disconnect on screen"
	}
	if !PromptEmpty(screen) {
		return "prompt not empty"
	}
	return ""
}

// GaveUp reports whether the screen's latest Remote Control line is Claude
// Code's giving up, not a later "/remote-control is active".
func GaveUp(screen string) bool {
	d := strings.LastIndex(screen, DisconnectLine)
	if d < 0 {
		return false
	}
	return !strings.Contains(screen[d:], "/remote-control is active")
}

var promptLine = regexp.MustCompile(`^\s*❯(.*)$`)

// PromptEmpty reports whether the input line (the last line starting with
// the prompt mark) has nothing typed on it.
func PromptEmpty(screen string) bool {
	lines := strings.Split(screen, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if m := promptLine.FindStringSubmatch(lines[i]); m != nil {
			// The box's right border, when it has one, is not input.
			return strings.Trim(m[1], "  │|") == ""
		}
	}
	return false
}

func label(s Session) string {
	if s.Name != "" {
		return s.Name + " [" + short(s.ID) + "]"
	}
	return short(s.ID)
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func (w *Watcher) sessions() []Session {
	entries, err := os.ReadDir(w.d.SessionsDir)
	if err != nil {
		return nil
	}
	var out []Session
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(w.d.SessionsDir, e.Name()))
		if err != nil {
			continue
		}
		var s Session
		if json.Unmarshal(b, &s) != nil || s.Pid <= 0 || s.ID == "" || !w.d.Alive(s.Pid) {
			continue
		}
		out = append(out, s)
	}
	return out
}

// ---- the real Mac --------------------------------------------------------

// Live returns Deps for this Mac. networkUp is Burst's own view.
func Live(home string, networkUp func() bool, logf func(string, ...any)) Deps {
	api := filepath.Join(home, ".local/state/ghostty-dock/api")
	return Deps{
		SessionsDir: filepath.Join(home, ".claude/sessions"),
		Alive:       alive,
		NetworkUp:   networkUp,
		HandsOff:    hidIdle,
		Screen: func(id string, lines int) (string, error) {
			var r struct {
				Text string `json:"text"`
			}
			err := dockCall(api, map[string]any{"op": "read", "target": id, "lines": lines, "scrollback": false}, &r)
			return r.Text, err
		},
		Type: func(id, text string) error {
			return dockCall(api, map[string]any{"op": "send", "target": id, "text": text, "enter": true}, nil)
		},
		Now:  time.Now,
		Logf: logf,
	}
}

func alive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// hidIdle is how long since the Mac last had keyboard or mouse input.
func hidIdle() (time.Duration, error) {
	out, err := exec.Command("/usr/sbin/ioreg", "-c", "IOHIDSystem", "-d", "4").Output()
	if err != nil {
		return 0, err
	}
	for _, l := range strings.Split(string(out), "\n") {
		if !strings.Contains(l, `"HIDIdleTime"`) {
			continue
		}
		f := strings.Fields(l)
		ns, err := strconv.ParseInt(f[len(f)-1], 10, 64)
		if err != nil {
			return 0, err
		}
		return time.Duration(ns), nil
	}
	return 0, errors.New("no HIDIdleTime")
}

// dockCall is bin/ghostty-dock's call(): a request file in the app's api
// folder, answered by a res- file next to it within five seconds.
func dockCall(dir string, req map[string]any, out any) error {
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return errors.New("Ghostty Dock is not running (no " + dir + ")")
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return err
	}
	id := hex.EncodeToString(b[:])
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, ".req-"+id+".json")
	reqPath := filepath.Join(dir, "req-"+id+".json")
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, reqPath); err != nil {
		return err
	}
	res := filepath.Join(dir, "res-"+id+".json")
	deadline := time.Now().Add(5 * time.Second)
	for {
		if data, err := os.ReadFile(res); err == nil {
			_ = os.Remove(res)
			var ans struct {
				OK    bool   `json:"ok"`
				Error string `json:"error"`
			}
			if err := json.Unmarshal(data, &ans); err != nil {
				return fmt.Errorf("unreadable answer: %w", err)
			}
			if !ans.OK {
				if ans.Error == "" {
					ans.Error = "failed"
				}
				return errors.New(ans.Error)
			}
			if out != nil {
				return json.Unmarshal(data, out)
			}
			return nil
		}
		if time.Now().After(deadline) {
			_ = os.Remove(reqPath)
			return errors.New("no answer from Ghostty Dock in 5s")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
