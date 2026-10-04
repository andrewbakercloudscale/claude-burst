package admin

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/notice"
)

// Burst's on-screen alerts are drawn by the usage panel, one per Ghostty
// window, and only while that Ghostty is in front. Someone working in Codex
// (the ChatGPT app) never had one in front, so a failover, a dead network or
// a Codex usage limit went unseen there. CodexAlerts, run by the support
// console (always up), shows the same alerts over the Codex window with the
// panel's own overlay helper, and claims each one the way the panels do, so
// it is shown once, wherever the person is.
//
// Only alerts about Burst as a whole. An alert about one Claude Code
// session (its context, its handover) stays with that session's panel:
// over Codex it is noise.
type CodexAlerts struct {
	NoticesPath string
	Overlay     string // the panel's claude-panel-overlay
	ClaimsDir   string // the panels' claims, one directory per event id
	OptionsPath string // the panel's options file (CLAUDE_PANEL_ALERTS)

	// Front returns the frontmost app's pid; IsCodex says whether a pid is
	// the Codex app; Show draws one alert and waits for it. Replaced in tests.
	Front   func() int
	IsCodex func(pid int) bool
	Show    func(pid int, secs int, severity, title, detail string)
	Now     func() time.Time
}

// codexAlertHold is how long an alert waits for Codex to come to the front,
// as long as the panels hold one for a Ghostty.
const codexAlertHold = 10 * time.Minute

// NewCodexAlerts returns the presenter for this Mac, or nil when the usage
// panel's overlay helper is not installed (nothing to draw with).
func NewCodexAlerts(configDir string) *CodexAlerts {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	ov := filepath.Join(home, ".local", "bin", "claude-panel-overlay")
	if !fileExists(ov) {
		return nil
	}
	c := &CodexAlerts{
		NoticesPath: notice.Path(configDir),
		Overlay:     ov,
		ClaimsDir:   filepath.Join(home, ".config", "claude-panel", "alerts-claimed"),
		OptionsPath: filepath.Join(home, ".config", "claude-panel", "options"),
		Now:         time.Now,
	}
	c.Front = func() int {
		out, err := exec.Command(ov, "--front-pid").Output()
		if err != nil {
			return 0
		}
		n, _ := strconv.Atoi(strings.TrimSpace(string(out)))
		return n
	}
	c.IsCodex = isCodexApp
	c.Show = func(pid, secs int, sev, title, detail string) {
		_ = exec.Command(ov, strconv.Itoa(pid), strconv.Itoa(secs), title, "", sev, detail).Run()
	}
	return c
}

// isCodexApp: the ChatGPT desktop app (Codex lives in it) or a standalone
// Codex app.
func isCodexApp(pid int) bool {
	if pid <= 0 {
		return false
	}
	out, err := exec.Command("ps", "-o", "comm=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return false
	}
	p := string(out)
	return strings.Contains(p, "/ChatGPT.app/Contents/MacOS/") || strings.Contains(p, "/Codex.app/Contents/MacOS/")
}

// alertSeconds matches the panel: information briefly, warnings longer,
// errors until read (bounded here, since there is no resolve to wait for).
func alertSeconds(sev string) int {
	switch sev {
	case notice.Error:
		return 30
	case notice.Warn:
		return 8
	}
	return 4
}

// Run shows alerts over Codex until ctx ends. Alerts already in
// notices.json when it starts are old news and never shown.
func (c *CodexAlerts) Run(ctx context.Context) {
	seen := map[string]bool{}
	if evs, err := notice.Read(c.NoticesPath); err == nil {
		for _, e := range evs {
			seen[e.ID] = true
		}
	}
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.tick(seen)
		}
	}
}

// tick shows the oldest waiting alert if Codex is in front. One at a time:
// Show returns when the alert has gone.
func (c *CodexAlerts) tick(seen map[string]bool) {
	if c.alertsOff() {
		return
	}
	evs, err := notice.Read(c.NoticesPath)
	if err != nil {
		return
	}
	var waiting []notice.Event
	for _, e := range evs {
		if seen[e.ID] {
			continue
		}
		if e.Session != "" || c.Now().Sub(e.At) > codexAlertHold || c.claimed(e.ID) {
			seen[e.ID] = true
			continue
		}
		waiting = append(waiting, e)
	}
	if len(waiting) == 0 {
		return
	}
	pid := c.Front()
	if !c.IsCodex(pid) {
		return // held: a panel may take it, or Codex may come to the front
	}
	e := waiting[0]
	seen[e.ID] = true
	if !c.claim(e.ID) {
		return // a panel took it first
	}
	c.Show(pid, alertSeconds(e.Severity), e.Severity, e.Title, e.Detail)
}

func (c *CodexAlerts) claimed(id string) bool {
	st, err := os.Stat(filepath.Join(c.ClaimsDir, id))
	return err == nil && st.IsDir()
}

// claim is the panels' own: mkdir is atomic, so exactly one shows it.
func (c *CodexAlerts) claim(id string) bool {
	_ = os.MkdirAll(c.ClaimsDir, 0o700)
	return os.Mkdir(filepath.Join(c.ClaimsDir, id), 0o700) == nil
}

// alertsOff honours the panel's "Show gateway alerts on screen" switch.
func (c *CodexAlerts) alertsOff() bool {
	b, err := os.ReadFile(c.OptionsPath)
	if err != nil {
		return false
	}
	v := ""
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "CLAUDE_PANEL_ALERTS=") {
			v = strings.TrimPrefix(l, "CLAUDE_PANEL_ALERTS=")
		}
	}
	switch strings.ToLower(strings.Trim(strings.TrimSpace(v), `"'`)) {
	case "false", "0", "no", "off":
		return true
	}
	return false
}
