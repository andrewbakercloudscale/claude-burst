package admin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/atomicfile"
)

// Burst's Claude Code mod (burst@burst) on the dashboard: whether it is installed and current,
// install, update and remove, and whether Burst's alerts and compaction
// lines show in the session as toasts. scripts/update-mod.sh does the installing, the same
// script install.sh and deploy.sh run.

const modPlugin = "burst@burst"

// modSettings are the mod's own options, read by /api/mod. Toasts is on by
// default: a session with the mod shows Burst's alerts and its compaction
// lines as toasts, and the mod claims each alert the way the panels do, so
// the Ghostty pop-up for it stands aside. Off, the pop-ups and the line
// under the prompt are as they were without the mod.
//
// Handoff is on by default too: Claude Code's own compaction is answered
// with the summary Burst already wrote (internal/router/handoff.go), and a
// session that has left Burst is compacted with it. The mod reads this file
// itself, since the option matters most when the gateway is not running.
//
// HandoffInPath is off by default: from 800k held, a session is compacted
// that way with Burst still in the path. It has costs Burst working
// normally does not: the turn after it is read uncached, and the replaced
// messages are gone from Claude Code's copy.
type modSettings struct {
	Toasts        bool `json:"toasts"`
	Handoff       bool `json:"handoff"`
	HandoffInPath bool `json:"handoff_in_path"`
}

func modSettingsPath() string {
	return filepath.Join(homeDir(), ".config", "claude-burst", "mod.json")
}

func readModSettings() modSettings {
	m := modSettings{Toasts: true, Handoff: true}
	if b, err := os.ReadFile(modSettingsPath()); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	return m
}

// claudeBin finds Claude Code. The LaunchAgent's PATH has no ~/.local/bin,
// where the native installer puts it. A variable for tests.
var claudeBin = func() string {
	if p, err := exec.LookPath("claude"); err == nil {
		return p
	}
	p := filepath.Join(homeDir(), ".local", "bin", "claude")
	if st, err := os.Stat(p); err == nil && !st.IsDir() {
		return p
	}
	return ""
}

type modStatus struct {
	Claude    string `json:"claude"` // Claude Code's version, "" when not found
	Supported bool   `json:"supported"`
	Installed bool   `json:"installed"`
	Version   string `json:"version,omitempty"`
	// Current is whether the installed copy matches mods/burst.
	Current bool   `json:"current"`
	Source  string `json:"source,omitempty"`
	Toasts  bool   `json:"toasts"`
	Handoff bool   `json:"handoff"`
	// HandoffInPath: see modSettings.
	HandoffInPath bool      `json:"handoff_in_path"`
	Last          *panelRun `json:"last,omitempty"`
	Error         string    `json:"error,omitempty"`
	Hint          string    `json:"hint,omitempty"`
}

var (
	modMu   sync.Mutex
	modLast *panelRun
)

// modHash is update-mod.sh's mod_hash: the manifest, then the hooks
// directory's files in name order. Tests and local state are not hashed.
func modHash(dir string) string {
	h := sha256.New()
	b, err := os.ReadFile(filepath.Join(dir, ".claude-plugin", "plugin.json"))
	if err != nil {
		return ""
	}
	h.Write(b)
	ents, err := os.ReadDir(filepath.Join(dir, "hooks"))
	if err != nil {
		return ""
	}
	names := []string{}
	for _, e := range ents {
		if e.Type().IsRegular() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, n := range names {
		b, err := os.ReadFile(filepath.Join(dir, "hooks", n))
		if err != nil {
			return ""
		}
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (s *Server) modSource() string {
	dir, ok := s.scriptsDir()
	if !ok {
		return ""
	}
	src := filepath.Join(filepath.Dir(dir), "mods", "burst")
	if _, err := os.Stat(filepath.Join(src, ".claude-plugin", "plugin.json")); err != nil {
		return ""
	}
	return src
}

// modStaleCheckout is true when the checkout predates one of the mod's
// renames (burst-band until 0.19.1, claude-burst until 0.20.2): Burst was upgraded from GitHub's copy and the
// checkout left alone. Its update-mod.sh would install the old mod beside
// this one, so a session would draw the band twice.
func (s *Server) modStaleCheckout() bool {
	dir, ok := s.scriptsDir()
	if !ok {
		return false
	}
	if s.modSource() != "" {
		return false
	}
	for _, old := range []string{"burst-band", "claude-burst"} {
		if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "mods", old, ".claude-plugin", "plugin.json")); err == nil {
			return true
		}
	}
	return false
}

// modInstalledFrom is the copy update-mod.sh last installed from.
func modInstalledFrom() string {
	return filepath.Join(homeDir(), ".local", "share", "claude-burst", "marketplace", "mods", "claude-burst")
}

func (s *Server) readModStatus(ctx context.Context) modStatus {
	set := readModSettings()
	st := modStatus{Toasts: set.Toasts, Handoff: set.Handoff, HandoffInPath: set.HandoffInPath, Source: s.modSource()}
	if s.modStaleCheckout() {
		st.Source = modInstalledFrom()
	}
	modMu.Lock()
	if modLast != nil {
		c := *modLast
		st.Last = &c
	}
	modMu.Unlock()
	bin := claudeBin()
	if bin == "" {
		st.Hint = "Claude Code was not found on this Mac."
		return st
	}
	out, err := exec.CommandContext(ctx, bin, "--version").Output()
	if err != nil {
		st.Error = "claude --version failed: " + err.Error()
		return st
	}
	st.Claude = strings.Fields(string(out) + " ")[0]
	st.Supported = versionAtLeast(st.Claude, "2.1.287")
	if !st.Supported {
		st.Hint = "Mods need Claude Code 2.1.287 or later; this Mac has " + st.Claude + "."
	}
	out, err = exec.CommandContext(ctx, bin, "plugin", "list", "--json").Output()
	if err != nil {
		st.Error = "claude plugin list failed: " + err.Error()
		return st
	}
	var rows []struct {
		ID          string `json:"id"`
		Version     string `json:"version"`
		InstallPath string `json:"installPath"`
	}
	_ = json.Unmarshal(out, &rows)
	for _, r := range rows {
		if r.ID == modPlugin {
			st.Installed, st.Version = true, r.Version
			st.Current = st.Source != "" && modHash(r.InstallPath) != "" && modHash(r.InstallPath) == modHash(st.Source)
		}
	}
	return st
}

// versionAtLeast compares dotted numeric versions.
func versionAtLeast(v, min string) bool {
	a, b := strings.Split(v, "."), strings.Split(min, ".")
	for i := 0; i < len(b); i++ {
		var x, y int
		if i < len(a) {
			for _, c := range a[i] {
				if c < '0' || c > '9' {
					break
				}
				x = x*10 + int(c-'0')
			}
		}
		for _, c := range b[i] {
			y = y*10 + int(c-'0')
		}
		if x != y {
			return x > y
		}
	}
	return true
}

func (s *Server) handleModStatus(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	writeJSON(w, s.readModStatus(ctx))
}

// handleModAction: {"action":"install"} installs or updates, "remove"
// removes, and "toasts", "handoff" or "handoff_in_path" with that key set
// to a bool saves that option.
func (s *Server) handleModAction(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Action  string `json:"action"`
		Toasts  *bool  `json:"toasts"`
		Handoff *bool  `json:"handoff"`
		InPath  *bool  `json:"handoff_in_path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request body", http.StatusBadRequest)
		return
	}
	switch req.Action {
	case "toasts", "handoff", "handoff_in_path":
		on := req.Toasts
		switch req.Action {
		case "handoff":
			on = req.Handoff
		case "handoff_in_path":
			on = req.InPath
		}
		if on == nil {
			http.Error(w, req.Action+" must be true or false", http.StatusBadRequest)
			return
		}
		// Read, change one, write: each option keeps the others' values.
		set := readModSettings()
		switch req.Action {
		case "handoff":
			set.Handoff = *on
		case "handoff_in_path":
			set.HandoffInPath = *on
		default:
			set.Toasts = *on
		}
		b, _ := json.MarshalIndent(set, "", "  ")
		if err := os.MkdirAll(filepath.Dir(modSettingsPath()), 0o700); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := atomicfile.Write(modSettingsPath(), append(b, '\n'), 0o600); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"ok": true, "toasts": set.Toasts, "handoff": set.Handoff, "handoff_in_path": set.HandoffInPath})
		return
	case "install", "remove":
	default:
		http.Error(w, "action must be install, remove, toasts, handoff or handoff_in_path", http.StatusBadRequest)
		return
	}
	dir, ok := s.scriptsDir()
	if !ok {
		http.Error(w, "the scripts directory was not found, so update-mod.sh cannot run", http.StatusInternalServerError)
		return
	}
	if req.Action == "install" && s.modStaleCheckout() {
		http.Error(w, "the claude-burst checkout in "+filepath.Dir(dir)+" is older than the Burst that is running and would install the mod under its old name. Run git pull there, or press Upgrade", http.StatusConflict)
		return
	}
	modMu.Lock()
	if modLast != nil && modLast.Finished.IsZero() {
		modMu.Unlock()
		http.Error(w, "an install or removal is already running", http.StatusConflict)
		return
	}
	run := &panelRun{Action: req.Action, Started: time.Now()}
	modLast = run
	modMu.Unlock()

	arg := "install"
	if req.Action == "remove" {
		arg = "uninstall"
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, "zsh", filepath.Join(dir, "update-mod.sh"), arg)
		cmd.Env = append(os.Environ(), "CLAUDE_BIN="+claudeBin())
		out, err := cmd.CombinedOutput()
		modMu.Lock()
		run.Finished, run.OK, run.Output = time.Now(), err == nil && !strings.Contains(string(out), "WARNING"), strings.TrimSpace(string(out))
		if err != nil {
			run.Output += "\n" + err.Error()
		}
		modMu.Unlock()
	}()
	writeJSON(w, map[string]any{"ok": true, "started": true})
}
