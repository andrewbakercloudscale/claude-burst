package admin

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/claudesettings"
	"github.com/andrewbakercloudscale/claude-burst/internal/config"
	"github.com/andrewbakercloudscale/claude-burst/internal/keepawake"
)

// The "This Mac" section: settings people change often enough that they
// should not live only in config files. Keeping the Mac awake with the lid
// shut, the usage panel (install and remove), and the panel's session
// options (Remote Control, caffeinate, session names).

// keepAwakeModes are the only values /api/keep-awake accepts; the mode is
// matched against this map, never interpolated, before it reaches a script.
var keepAwakeModes = map[string]bool{"off": true, config.KeepAwakeOnAC: true, config.KeepAwakeAlways: true}

type keepAwakeView struct {
	Mode    string           `json:"mode"` // off, ac, always: what config.json asks for
	Idle    int              `json:"idle_minutes"`
	Live    keepawake.Status `json:"live"`
	Problem string           `json:"problem,omitempty"`
}

// installedLidScript is the root-owned copy the keep-awake daemon runs
// (lid-awake-root.sh's $INSTALLED). A variable for tests.
var installedLidScript = "/usr/local/libexec/claude-burst/lid-awake-root.sh"

func (s *Server) readKeepAwake() keepAwakeView {
	cfg, _ := config.Load()
	v := keepAwakeView{Mode: "off", Live: keepawake.Read(), Idle: cfg.KeepAwakeIdleMinutes}
	if cfg.KeepAwakeLidClosed {
		v.Mode = cfg.KeepAwakeLidClosedPower
	}
	v.Problem = v.Live.Problem(cfg.KeepAwakeLidClosed, cfg.KeepAwakeLidClosedPower, cfg.KeepAwakeIdleMinutes)
	if v.Problem == "" && cfg.KeepAwakeLidClosed {
		v.Problem = s.staleLidDaemon()
	}
	return v
}

// staleLidDaemon reports when the daemon runs an older copy of
// lid-awake-root.sh than the checkout's. The daemon never rereads the repo,
// so a fix to the script (the screen going off behind a shut lid, 30 Sep)
// did nothing until something reran apply, and with the settings unchanged
// the dashboard's Apply button stayed disabled. A problem enables it.
func (s *Server) staleLidDaemon() string {
	dir, ok := s.scriptsDir()
	if !ok {
		return ""
	}
	repo, err := os.ReadFile(filepath.Join(dir, "lid-awake-root.sh"))
	if err != nil {
		return ""
	}
	live, err := os.ReadFile(installedLidScript)
	if err != nil || bytes.Equal(repo, live) {
		return ""
	}
	return "the keep-awake daemon runs an older copy of scripts/lid-awake-root.sh"
}

// setGhosttyAppNap is a variable so tests never change the real Ghostty
// preference: `defaults` ignores HOME, so a test's temporary HOME does not
// protect it, and one did turn this machine's setting off.
var setGhosttyAppNap = keepawake.SetGhosttyAppNap

// sudoNonInteractive runs a root command only if sudo has cached
// credentials. Never prompts: the gateway has no terminal. A variable for
// tests.
var sudoNonInteractive = func(args ...string) ([]byte, error) {
	return exec.Command("sudo", append([]string{"-n"}, args...)...).CombinedOutput()
}

func (s *Server) handleKeepAwake(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Mode string `json:"mode"`
		Idle int    `json:"idle_minutes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || !keepAwakeModes[req.Mode] {
		http.Error(w, "mode must be off, ac or always", http.StatusBadRequest)
		return
	}
	if req.Idle < 0 || req.Idle > config.MaxKeepAwakeIdleMinutes {
		http.Error(w, fmt.Sprintf("idle_minutes must be 0 to %d", config.MaxKeepAwakeIdleMinutes), http.StatusBadRequest)
		return
	}
	dir, ok := s.scriptsDir()
	if !ok {
		http.Error(w, "could not locate the claude-burst repo's scripts/ directory", http.StatusBadRequest)
		return
	}
	on := req.Mode != "off"
	if _, ok := updateConfig(w, func(c *config.Config) error {
		c.KeepAwakeLidClosed = on
		if on {
			c.KeepAwakeLidClosedPower = req.Mode
			c.KeepAwakeIdleMinutes = req.Idle
		}
		return nil
	}); !ok {
		return
	}
	// The user half needs no root.
	napNote := ""
	if err := setGhosttyAppNap(on); err != nil {
		napNote = " Could not change Ghostty's App Nap setting: " + err.Error() + "."
	}

	script := filepath.Join(dir, "lid-awake-root.sh")
	args := []string{script, "remove"}
	if on {
		args = []string{script, "apply", req.Mode, strconv.Itoa(req.Idle), keepawake.ActivityPath()}
		// The window starts now, not at the next Claude turn.
		keepawake.MarkNow()
	}
	if out, err := sudoNonInteractive(args...); err == nil {
		writeJSON(w, map[string]string{"detail": strings.TrimSpace(string(out)) + napNote})
		return
	}
	path, err := writeGeneratedScript("keep-awake", keepAwakeScript(args))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	resp := map[string]string{"script": path}
	if err := launchTerminal(path); err != nil {
		resp["detail"] = fmt.Sprintf("Saved. The machine-wide half needs your password, and Terminal did not open (%v). Run: %s", err, path)
	} else {
		resp["detail"] = "Saved. A Terminal window on this Mac is asking for your password to apply the machine-wide half; click Refresh here when it finishes." + napNote
	}
	writeJSON(w, resp)
}

func keepAwakeScript(args []string) string {
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = shellQuote(a)
	}
	return fmt.Sprintf(`#!/bin/zsh
# Generated by the Claude Burst dashboard. Rewritten on every click.
echo "== Claude Burst: keep the Mac awake with the lid shut =="
echo
echo "This changes pmset SleepDisabled, which is machine-wide and needs root."
echo "It asks for your password."
echo
sudo %s
echo
read -k 1 "?Done. Press any key to close this window..."
`, strings.Join(q, " "))
}

// --- usage panel -------------------------------------------------------

const panelRepoURL = "https://github.com/andrewbakercloudscale/claudecode-cost-usage-panel.git"

// panelOptions maps the dashboard's names to keys in the panel's options
// file. Only these keys are ever written.
var panelOptions = map[string]string{
	"remote_control": "CLAUDE_PANEL_REMOTE_CONTROL",
	"caffeinate":     "CLAUDE_PANEL_CAFFEINATE",
	"session_title":  "CLAUDE_PANEL_SESSION_TITLE",
	"cost_alerts":    "CLAUDE_PANEL_COST_ALERTS",
}

// panelNumbers are the panel's numeric options, with their defaults and the
// range the dashboard accepts.
var panelNumbers = map[string]struct {
	Key     string
	Default float64
	Min     float64
	Max     float64
}{
	"restart_tokens": {"CLAUDE_PANEL_RESTART_TOKENS", 400000, 0, 2000000},
	"alert_min_usd":  {"CLAUDE_PANEL_ALERT_MIN_USD", 5, 0, 10000},
}

// panelDefaults are what the panel does when a key is absent.
var panelDefaults = map[string]bool{"remote_control": false, "caffeinate": false, "session_title": true, "cost_alerts": true}

type panelView struct {
	Installed bool               `json:"installed"`
	RepoDir   string             `json:"repo_dir,omitempty"`
	Options   map[string]bool    `json:"options"`
	Numbers   map[string]float64 `json:"numbers"`
	Running   bool               `json:"running"` // an install or removal is in progress
	LastRun   *panelRun          `json:"last_run,omitempty"`
}

type panelRun struct {
	Action   string    `json:"action"`
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished,omitempty"`
	OK       bool      `json:"ok"`
	Output   string    `json:"output"`
}

var panelMu sync.Mutex
var panelLast *panelRun

func homeDir() string { h, _ := os.UserHomeDir(); return h }

func panelOptionsPath() string {
	return filepath.Join(homeDir(), ".config", "claude-panel", "options")
}

// panelRepoDir finds a checkout: beside claude-burst's own, else the copy
// install.sh clones. "" when there is none.
func (s *Server) panelRepoDir() string {
	var cands []string
	if dir, ok := s.scriptsDir(); ok {
		cands = append(cands, filepath.Join(filepath.Dir(filepath.Dir(dir)), "claudecode-cost-usage-panel"))
	}
	cands = append(cands, filepath.Join(homeDir(), ".local", "share", "claude-burst", "claudecode-cost-usage-panel"))
	for _, c := range cands {
		if _, err := os.Stat(filepath.Join(c, "claude-panel-setup.sh")); err == nil {
			return c
		}
	}
	return ""
}

func readPanelOptions(path string) map[string]bool {
	out := map[string]bool{}
	for k, v := range panelDefaults {
		out[k] = v
	}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		key, val, ok := strings.Cut(strings.TrimSpace(sc.Text()), "=")
		if !ok || strings.HasPrefix(key, "#") {
			continue
		}
		for name, k := range panelOptions {
			if k == key {
				switch strings.ToLower(strings.Trim(val, `"' `)) {
				case "true", "1", "yes", "on":
					out[name] = true
				default:
					out[name] = false
				}
			}
		}
	}
	return out
}

func readPanelNumbers(path string) map[string]float64 {
	out := map[string]float64{}
	for name, n := range panelNumbers {
		out[name] = n.Default
	}
	b, _ := os.ReadFile(path)
	for _, l := range strings.Split(string(b), "\n") {
		key, val, ok := strings.Cut(strings.TrimSpace(l), "=")
		if !ok {
			continue
		}
		for name, n := range panelNumbers {
			if n.Key == key {
				if f, err := strconv.ParseFloat(strings.Trim(val, `"' `), 64); err == nil {
					out[name] = f
				}
			}
		}
	}
	return out
}

// setPanelOption rewrites KEY=value in place, keeping every other line and
// comment, or appends it.
func setPanelOption(path, key string, on bool) error {
	return setPanelValue(path, key, strconv.FormatBool(on))
}

func setPanelValue(path, key, value string) error {
	b, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	line := key + "=" + value
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(b) == 0 {
		lines = []string{"# claude-panel options -- true/false. Written by claude-panel-setup.sh and the Claude Burst dashboard."}
	}
	found := false
	for i, l := range lines {
		if k, _, ok := strings.Cut(strings.TrimSpace(l), "="); ok && k == key {
			lines[i], found = line, true
		}
	}
	if !found {
		lines = append(lines, line)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// bypassOption is the one session option that is not only a panel key.
// Its truth is permissions.defaultMode in ~/.claude/settings.json, which
// Claude Code honours however it is started (a typed claude, the Finder
// launcher, an IDE). CLAUDE_PANEL_BYPASS_PERMISSIONS is written beside it
// for the Finder launcher, which passes --dangerously-skip-permissions
// unless that key says false.
const (
	bypassOption   = "bypass_permissions"
	bypassPanelKey = "CLAUDE_PANEL_BYPASS_PERMISSIONS"
	bypassMode     = "bypassPermissions"
)

func readBypass() bool {
	p, err := claudesettings.Path()
	if err != nil {
		return false
	}
	root, err := claudesettings.Read(p)
	if err != nil {
		return false
	}
	perms, _ := root["permissions"].(map[string]any)
	mode, _ := perms["defaultMode"].(string)
	return mode == bypassMode
}

// setBypass sets or clears permissions.defaultMode, touching nothing else.
// Turning it off removes the key only when it is bypassPermissions, so a
// mode the user chose themselves (plan, acceptEdits) is left alone.
func setBypass(on bool) error {
	p, err := claudesettings.Path()
	if err != nil {
		return err
	}
	root, err := claudesettings.Read(p)
	if err != nil {
		return err
	}
	perms, _ := root["permissions"].(map[string]any)
	if perms == nil {
		if !on {
			return nil
		}
		perms = map[string]any{}
		root["permissions"] = perms
	}
	mode, _ := perms["defaultMode"].(string)
	switch {
	case on && mode != bypassMode:
		perms["defaultMode"] = bypassMode
	case !on && mode == bypassMode:
		delete(perms, "defaultMode")
	default:
		return nil
	}
	return claudesettings.Write(p, root)
}

func (s *Server) readPanel() panelView {
	_, err := os.Stat(filepath.Join(homeDir(), ".local", "bin", "ccusage-panel.sh"))
	v := panelView{Installed: err == nil, RepoDir: s.panelRepoDir(), Options: readPanelOptions(panelOptionsPath()), Numbers: readPanelNumbers(panelOptionsPath())}
	v.Options[bypassOption] = readBypass()
	panelMu.Lock()
	if panelLast != nil {
		c := *panelLast
		v.LastRun, v.Running = &c, c.Finished.IsZero()
	}
	panelMu.Unlock()
	return v
}

func (s *Server) handlePanelOptions(w http.ResponseWriter, r *http.Request) {
	var req map[string]any
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req) == 0 {
		http.Error(w, "bad request body", http.StatusBadRequest)
		return
	}
	writes := map[string]string{}
	bypass := -1
	for name, v := range req {
		if name == bypassOption {
			b, isBool := v.(bool)
			if !isBool {
				http.Error(w, name+" must be true or false", http.StatusBadRequest)
				return
			}
			bypass = 0
			if b {
				bypass = 1
			}
			writes[bypassPanelKey] = strconv.FormatBool(b)
			continue
		}
		if key, ok := panelOptions[name]; ok {
			b, isBool := v.(bool)
			if !isBool {
				http.Error(w, name+" must be true or false", http.StatusBadRequest)
				return
			}
			writes[key] = strconv.FormatBool(b)
			continue
		}
		n, ok := panelNumbers[name]
		if !ok {
			http.Error(w, "unknown option "+name, http.StatusBadRequest)
			return
		}
		f, isNum := v.(float64)
		if !isNum || f < n.Min || f > n.Max {
			http.Error(w, fmt.Sprintf("%s must be a number from %g to %g", name, n.Min, n.Max), http.StatusBadRequest)
			return
		}
		writes[n.Key] = strconv.FormatFloat(f, 'f', -1, 64)
	}
	if bypass >= 0 {
		if err := setBypass(bypass == 1); err != nil {
			http.Error(w, "saving permissions.defaultMode in settings.json: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	for key, val := range writes {
		if err := setPanelValue(panelOptionsPath(), key, val); err != nil {
			http.Error(w, "saving panel options: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	writeJSON(w, map[string]string{"detail": "saved; applies to the next Claude Code session you start"})
}

// runPanelCommand is a variable for tests: the real one runs the panel's
// installer or uninstaller, which rewrites ~/.zshrc and settings.json.
var runPanelCommand = func(ctx context.Context, dir, script string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "bash", append([]string{filepath.Join(dir, script)}, args...)...)
	cmd.Dir = dir
	return cmd.CombinedOutput()
}

func (s *Server) handlePanelInstall(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Action string `json:"action"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || (req.Action != "install" && req.Action != "remove") {
		http.Error(w, "action must be install or remove", http.StatusBadRequest)
		return
	}
	panelMu.Lock()
	if panelLast != nil && panelLast.Finished.IsZero() {
		panelMu.Unlock()
		http.Error(w, "an install or removal is already running", http.StatusConflict)
		return
	}
	run := &panelRun{Action: req.Action, Started: time.Now()}
	panelLast = run
	panelMu.Unlock()

	dir := s.panelRepoDir()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		var out []byte
		var err error
		switch {
		case req.Action == "remove" && dir == "":
			err = fmt.Errorf("no checkout of the panel repo found, so no uninstaller to run")
		case req.Action == "remove":
			out, err = runPanelCommand(ctx, dir, "claude-panel-uninstall.sh")
		default:
			if dir == "" {
				dir = filepath.Join(homeDir(), ".local", "share", "claude-burst", "claudecode-cost-usage-panel")
				_ = os.MkdirAll(filepath.Dir(dir), 0o755)
				var o []byte
				o, err = exec.CommandContext(ctx, "git", "clone", "--quiet", panelRepoURL, dir).CombinedOutput()
				out = append(out, o...)
			}
			if err == nil {
				var o []byte
				o, err = runPanelCommand(ctx, dir, "claude-panel-setup.sh")
				out = append(out, o...)
			}
		}
		panelMu.Lock()
		defer panelMu.Unlock()
		run.Finished, run.OK, run.Output = time.Now(), err == nil, tail(string(out), 60)
		if err != nil {
			run.Output += "\nerror: " + err.Error()
		}
	}()
	writeJSON(w, map[string]string{"detail": req.Action + " started"})
}

func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// handleMac is the section's state, read on demand: pmset and defaults
// calls are too slow for the 2s /api/state poll.
func (s *Server) handleMac(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"keep_awake": s.readKeepAwake(), "panel": s.readPanel()})
}

// A masked frame of the panel (dollar figures scaled, ids replaced).
//
//go:embed assets/usage-panel.png
var panelShot []byte

func (s *Server) handlePanelShot(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "max-age=3600")
	w.Write(panelShot)
}
