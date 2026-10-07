package admin

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
)

// The support console: a small page served by its own process
// (`claude-burst console`, its own LaunchAgent), so it is up when the
// gateway and its dashboard are not. That is the whole point: when Burst is
// broken, the person needs to see what happened (the audit, the log) and to
// fix it (restart, repair, turn Burst off), and the dashboard that would
// normally do that went down with the gateway.
//
// It reads files and probes ports, nothing more. It never loads the router
// or a provider, so a config that stops the gateway starting cannot stop
// the console.

//go:embed console.html
var consoleHTML []byte

// GatewayLabel is the gateway's LaunchAgent.
const GatewayLabel = "ninja.andrewbaker.claude-burst"

// Console serves the support console.
type Console struct {
	Version    string
	RootHelper string // the installed transparent-root.sh, beside the repo's scripts
	// Run and Terminal are replaced in tests: nothing here may touch the
	// real launchd or open a real Terminal from a test.
	Run      func(ctx context.Context, name string, args ...string) ([]byte, error)
	Terminal func(scriptPath string) error
}

// NewConsole returns a console for the installed gateway.
func NewConsole(version, rootHelper string) *Console {
	return &Console{
		Version: version, RootHelper: rootHelper,
		Run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, name, args...).CombinedOutput()
		},
		Terminal: func(p string) error { return launchTerminal(p) },
	}
}

// Handler is the console's HTTP handler, behind the same loopback Host
// check and mutation header as the dashboard.
func (c *Console) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", c.handleIndex)
	mux.HandleFunc("/audit.js", readOnly(handleAuditJS))
	mux.HandleFunc("/api/audit", readOnly(handleAudit))
	mux.HandleFunc("/api/audit/context", readOnly(handleAuditContext))
	mux.HandleFunc("/api/console", readOnly(c.handleStatus))
	mux.HandleFunc("/api/console/restart", mutating(audited("console", c.handleRestart)))
	mux.HandleFunc("/api/console/restore-config", mutating(audited("console", c.handleRestoreConfig)))
	mux.HandleFunc("/api/console/repair", mutating(audited("console", c.handleTerminal("repair"))))
	mux.HandleFunc("/api/console/diagnose", mutating(audited("console", c.handleTerminal("diagnose"))))
	mux.HandleFunc("/api/console/off", mutating(audited("console", c.handleTerminal("off"))))
	return guard("", mux)
}

func (c *Console) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(consoleHTML)
}

type consoleCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

type consoleStatus struct {
	Version     string         `json:"version"`
	Dashboard   string         `json:"dashboard"` // URL, "" when off
	DashboardUp bool           `json:"dashboard_up"`
	Checks      []consoleCheck `json:"checks"`
	ConfigError string         `json:"config_error,omitempty"`
	LogTail     []string       `json:"log_tail"`
	Now         time.Time      `json:"now"`
}

// handleStatus answers "is Burst working, and if not, where does it stop".
func (c *Console) handleStatus(w http.ResponseWriter, r *http.Request) {
	st := consoleStatus{Version: c.Version, Checks: []consoleCheck{}, LogTail: []string{}, Now: time.Now()}
	cfg, err := config.Load()
	if err != nil {
		// Defaults, so the checks still say something useful; the error is
		// itself the most likely cause of what the person is seeing.
		st.ConfigError = err.Error()
		cfg = config.Default()
	}
	ctx, cancel := context.WithTimeout(r.Context(), 6*time.Second)
	defer cancel()

	loaded := false
	if out, err := c.Run(ctx, "launchctl", "print", fmt.Sprintf("gui/%d/%s", os.Getuid(), GatewayLabel)); err == nil {
		loaded = true
		state := "loaded"
		for _, l := range strings.Split(string(out), "\n") {
			if t := strings.TrimSpace(l); strings.HasPrefix(t, "state = ") {
				state = strings.TrimPrefix(t, "state = ")
				break
			}
		}
		st.Checks = append(st.Checks, consoleCheck{"Gateway service", state == "running", "launchd says: " + state})
	} else {
		st.Checks = append(st.Checks, consoleCheck{"Gateway service", false, "not loaded in launchd: Restart starts it"})
	}
	if dir, err := config.ConfigDir(); err == nil {
		if b, err := os.ReadFile(filepath.Join(dir, "rolled-back")); err == nil {
			st.Checks = append(st.Checks, consoleCheck{"Turned off", false,
				"Burst was turned off on purpose (" + strings.TrimSpace(string(b)) + "). Restart turns it back on."})
		}
	}
	if cfg.Intercept.Mode == config.InterceptTransparent {
		st.Checks = append(st.Checks, redirectCheck(ctx, cfg.Intercept.Host))
	} else {
		st.Checks = append(st.Checks, dialCheck("Gateway port", cfg.Listen))
	}
	if a := cfg.AdminListen; a != "" && a != "off" {
		st.Dashboard = "http://" + a + "/"
		chk := httpCheck(ctx, "Dashboard", st.Dashboard+"api/state")
		st.DashboardUp = chk.OK
		st.Checks = append(st.Checks, chk)
	}
	if a := cfg.CodexListen(); a != "" {
		st.Checks = append(st.Checks, dialCheck("Codex port", a))
	}
	if !loaded && st.ConfigError == "" {
		st.Checks = append(st.Checks, consoleCheck{"Hint", false, "Restart below starts the gateway again."})
	}
	if p, err := config.LogPath(); err == nil {
		if lines, err := tailLines(p, 40); err == nil {
			for _, l := range lines {
				if !logNoise([]byte(l)) {
					st.LogTail = append(st.LogTail, redactLine(l))
				}
			}
		}
	}
	writeJSON(w, st)
}

func dialCheck(name, addr string) consoleCheck {
	if addr == "" {
		return consoleCheck{name, false, "no address configured"}
	}
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return consoleCheck{name, false, addr + " is not answering: " + err.Error()}
	}
	conn.Close()
	return consoleCheck{name, true, addr + " is answering"}
}

// redirectCheck follows the path Claude Code takes in transparent mode:
// the intercepted host resolves here (/etc/hosts), pf sends it to the
// gateway, and TLS completes against Burst's CA. A direct dial of the
// gateway's own port proves nothing here: pf answers for that port
// differently than for the redirect. The gateway stamps "overflow" into its
// /healthz, which the real Anthropic never sends, so the body says whose
// answer it is.
func redirectCheck(ctx context.Context, host string) consoleCheck {
	if host == "" {
		host = "api.anthropic.com"
	}
	const name = "Redirect to the gateway"
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+host+"/healthz", nil)
	resp, err := (&http.Client{Timeout: 4 * time.Second}).Do(req)
	if err != nil {
		return consoleCheck{name, false, host + " did not reach Burst: " + err.Error()}
	}
	defer resp.Body.Close()
	b := make([]byte, 4096)
	n, _ := io.ReadFull(resp.Body, b)
	if !strings.Contains(string(b[:n]), `"overflow"`) {
		return consoleCheck{name, false, fmt.Sprintf("%s answered HTTP %d, but not as Burst: the redirect is missing and Claude Code goes straight to Anthropic", host, resp.StatusCode)}
	}
	return consoleCheck{name, true, host + " reaches the gateway"}
}

func httpCheck(ctx context.Context, name, url string) consoleCheck {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
	if err != nil {
		return consoleCheck{name, false, "not answering: " + err.Error()}
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return consoleCheck{name, false, fmt.Sprintf("answered HTTP %d", resp.StatusCode)}
	}
	return consoleCheck{name, true, "answering"}
}

// tailLines returns up to n whole lines from the end of path.
func tailLines(path string, n int) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	const chunk = 256 << 10
	off := st.Size() - chunk
	if off < 0 {
		off = 0
	}
	buf := make([]byte, st.Size()-off)
	if _, err := f.ReadAt(buf, off); err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimRight(string(buf), "\n"), "\n")
	if off > 0 && len(lines) > 0 {
		lines = lines[1:] // the first may be cut
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines, nil
}

// handleRestart starts the gateway, or restarts it when it is running, and
// clears the turned-off marker: pressing Restart is choosing Burst again.
func (c *Console) handleRestart(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	if dir, err := config.ConfigDir(); err == nil {
		os.Remove(filepath.Join(dir, "rolled-back"))
		os.Remove(filepath.Join(dir, "rolled-back.noted"))
	}
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	target := domain + "/" + GatewayLabel
	if _, err := c.Run(ctx, "launchctl", "print", target); err == nil {
		if out, err := c.Run(ctx, "launchctl", "kickstart", "-k", target); err != nil {
			http.Error(w, "launchctl kickstart failed: "+strings.TrimSpace(string(out)+" "+err.Error()), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]string{"detail": "Restarted the gateway. Checks refresh in a few seconds."})
		return
	}
	home, _ := os.UserHomeDir()
	plist := filepath.Join(home, "Library", "LaunchAgents", GatewayLabel+".plist")
	if _, err := os.Stat(plist); err != nil {
		http.Error(w, "the gateway's LaunchAgent is not installed ("+plist+"): use Repair, which reinstalls it", http.StatusConflict)
		return
	}
	if out, err := c.Run(ctx, "launchctl", "bootstrap", domain, plist); err != nil {
		http.Error(w, "launchctl bootstrap failed: "+strings.TrimSpace(string(out)+" "+err.Error()), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"detail": "Started the gateway. Checks refresh in a few seconds."})
}

// handleRestoreConfig puts back the newest backup of config.json that
// loads. With a config it cannot read the gateway exits, launchd starts it
// again, and Restart and Repair both stop on the same file: this is the
// one button that gets out of that.
func (c *Console) handleRestoreConfig(w http.ResponseWriter, r *http.Request) {
	res, err := config.RestoreLastGood()
	if errors.Is(err, config.ErrConfigLoads) {
		writeJSON(w, map[string]string{"detail": "config.json loads as it is, so nothing was restored."})
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, map[string]string{"detail": "Restored config.json from " + filepath.Base(res.From) +
		". The file that would not load is kept as " + res.Kept + ". Press Restart gateway if the checks are not green in a few seconds."})
}

// consoleScripts are the fixes that need a password or show a report:
// they run in a Terminal window, where macOS can ask for it.
var consoleScripts = map[string]struct{ file, says string }{
	"repair":   {"repair.sh", "A Terminal window is repairing Burst. It asks for your password for the parts that need it."},
	"diagnose": {"diagnose.sh", "A Terminal window is writing a diagnostic report (secrets redacted) and copying it to the clipboard."},
	"off":      {"rollback.sh", "A Terminal window is turning Burst off: Claude Code goes straight to Anthropic. Restart here turns it back on."},
}

func (c *Console) handleTerminal(which string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sc := consoleScripts[which]
		script := ""
		// Not stat'd: the checkout is usually under ~/Desktop, which macOS
		// may hide from a background process, while the Terminal window that
		// runs the script can read it.
		if filepath.IsAbs(c.RootHelper) {
			script = filepath.Join(filepath.Dir(c.RootHelper), sc.file)
		}
		if script == "" && which == "off" {
			if home, err := os.UserHomeDir(); err == nil && fileExists(filepath.Join(home, ".local", "bin", "burst-off")) {
				script = filepath.Join(home, ".local", "bin", "burst-off")
			}
		}
		if script == "" {
			http.Error(w, "could not find "+sc.file+" (the claude-burst checkout's scripts/ folder): run it from the checkout", http.StatusConflict)
			return
		}
		body := "#!/bin/zsh\n" +
			"cd " + shellQuote(filepath.Dir(script)) + "/.. 2>/dev/null || cd " + shellQuote(filepath.Dir(script)) + "\n" +
			shellQuote(script) + "\n" +
			"echo\nread -k 1 \"?Press any key to close this window...\"\n"
		path, err := writeGeneratedScript("console-"+which, body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := c.Terminal(path); err != nil {
			http.Error(w, "Terminal did not open ("+err.Error()+"). Run it yourself: "+path, http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]string{"detail": sc.says})
	}
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}
