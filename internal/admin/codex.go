package admin

import (
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/codex"
	"github.com/andrewbakercloudscale/claude-burst/internal/config"
	"github.com/andrewbakercloudscale/claude-burst/internal/metrics"
)

// SetCodex attaches the running Codex gateway; nil leaves the Codex tab
// saying the listener is not running.
// startErr says why it is not running, when it is not.
func (s *Server) SetCodex(g *codex.Gateway, startErr string) { s.codex, s.codexErr = g, startErr }

type codexState struct {
	Status      codex.Status    `json:"status"`
	Listen      string          `json:"listen"`
	Upstream    string          `json:"upstream"`
	Listening   bool            `json:"listening"`
	ListenError string          `json:"listen_error,omitempty"`
	LastRequest *time.Time      `json:"last_request,omitempty"`
	Limits      *codex.Limits   `json:"limits,omitempty"`
	History     metrics.History `json:"history"`
	Recent      []metrics.Event `json:"recent"`
	Sessions    []codexSession  `json:"sessions"`
	Error       string          `json:"error,omitempty"`
}

func (s *Server) handleCodex(w http.ResponseWriter, r *http.Request) {
	cfg, err := config.Load()
	if err != nil {
		writeJSON(w, codexState{Error: err.Error(), Recent: []metrics.Event{}})
		return
	}
	path, _ := codex.ConfigPath()
	st := codexState{
		Status:   codex.ReadStatus(path),
		Listen:   cfg.CodexListen(),
		Upstream: cfg.CodexUpstream(),

		ListenError: s.codexErr,
		Recent:      []metrics.Event{},
		Sessions:    []codexSession{},
	}
	if s.codex != nil {
		st.Listening, st.ListenError = s.codex.ListenState()
		if !st.Listening && st.ListenError == "" {
			st.ListenError = "still starting"
		}
		if t := s.codex.LastRequest(); !t.IsZero() {
			st.LastRequest = &t
		}
		if l := s.codex.Limits(); !l.Seen.IsZero() {
			st.Limits = &l
		}
	}
	if mp, err := config.CodexMetricsPath(); err == nil {
		if h, err := metrics.Daily(mp, 14); err == nil {
			st.History = h
		}
		if evs, err := metrics.Recent(mp, 2000); err == nil && evs != nil {
			if len(evs) > 50 {
				st.Recent = evs[:50]
			} else {
				st.Recent = evs
			}
			st.Sessions = codexSessions(evs, s.codexWindows())
		}
	}
	writeJSON(w, st)
}

func (s *Server) handleCodexRoute(enable bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cfg, err := config.Load()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		path, err := codex.ConfigPath()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		backups := ""
		if d, err := config.ConfigDir(); err == nil {
			backups = filepath.Join(d, "backups")
		}
		if enable {
			if cfg.CodexListen() == "" {
				http.Error(w, `the Codex gateway is off (codex.listen is "off" in config.json)`, http.StatusConflict)
				return
			}
			// Never point Codex at a port nothing answers: every turn
			// would fail. The listener this process started is the one
			// wanted, and it must still accept a connection now.
			bound, why := false, s.codexErr
			if s.codex != nil {
				bound, why = s.codex.ListenState()
			}
			if !bound {
				if why == "" {
					why = "it is not running yet"
				}
				http.Error(w, "the Codex gateway is not listening ("+why+"), so Codex was left going straight to ChatGPT. Fix that first: see Trace the path.", http.StatusConflict)
				return
			}
			if c, derr := net.DialTimeout("tcp", cfg.CodexListen(), 2*time.Second); derr != nil {
				http.Error(w, "nothing answers on "+cfg.CodexListen()+" ("+derr.Error()+"), so Codex was left going straight to ChatGPT.", http.StatusConflict)
				return
			} else {
				c.Close()
			}
			err = codex.Enable(path, cfg.CodexListen(), backups)
		} else {
			err = codex.Disable(path, backups)
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		detail := "Codex goes straight to ChatGPT again for sessions started from now on. Sessions already open keep using Burst until they are restarted."
		if enable {
			detail = "Codex goes through Claude Burst from its next session. Restart Codex (quit the app and open it again, and any codex running in a terminal): each session reads " + path + " when it starts."
		}
		writeJSON(w, map[string]any{"ok": true, "detail": detail, "status": codex.ReadStatus(path)})
	}
}

// codexSession is one Codex session's context: how full its window is after
// its latest turn, which is what decides when Codex compacts or runs out.
type codexSession struct {
	ID    string    `json:"id"`
	Model string    `json:"model"`
	First time.Time `json:"first"`
	Last  time.Time `json:"last"`
	Turns int       `json:"turns"`
	// Context is the latest turn's whole input (cached or not) plus its
	// output: the conversation as the next turn will send it.
	Context int64 `json:"context"`
	// Window is the model's context window, 0 when it is not known.
	Window  int64   `json:"window"`
	Percent float64 `json:"percent"`
	Tokens  int64   `json:"tokens"`
	Errors  int     `json:"errors"`
}

// codexSessionsShown caps the list: the busiest recent ones are the point.
const codexSessionsShown = 12

// codexSessions groups turns (newest first, as metrics.Recent returns them)
// by session, newest session first.
func codexSessions(evs []metrics.Event, windows map[string]int64) []codexSession {
	byID := map[string]*codexSession{}
	var order []string
	for _, e := range evs {
		id := e.SessionID
		if id == "" {
			id = "(no session id)"
		}
		cs := byID[id]
		if cs == nil {
			cs = &codexSession{ID: id, Last: e.Time}
			byID[id] = cs
			order = append(order, id)
		}
		cs.Turns++
		cs.First = e.Time
		cs.Tokens += e.InputTokens + e.CacheReadTokens + e.OutputTokens
		if e.HTTPStatus >= 400 {
			cs.Errors++
		}
		// The newest turn with tokens sets the context: an error carries none.
		if cs.Context == 0 && e.HTTPStatus < 400 && e.InputTokens+e.CacheReadTokens > 0 {
			cs.Context = e.InputTokens + e.CacheReadTokens + e.OutputTokens
			cs.Model = e.Model
		}
	}
	out := make([]codexSession, 0, len(order))
	for _, id := range order {
		cs := byID[id]
		if w := windows[cs.Model]; w > 0 {
			cs.Window = w
			cs.Percent = float64(cs.Context) * 100 / float64(w)
		}
		out = append(out, *cs)
		if len(out) == codexSessionsShown {
			break
		}
	}
	return out
}

// codexWindows is each model's context window: what the gateway learned
// from the model list, else Codex's own cache of that list.
func (s *Server) codexWindows() map[string]int64 {
	if s.codex != nil {
		if w := s.codex.Windows(); len(w) > 0 {
			return w
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	dir := filepath.Join(home, ".codex")
	if h := os.Getenv("CODEX_HOME"); h != "" {
		dir = h
	}
	b, err := os.ReadFile(filepath.Join(dir, "models_cache.json"))
	if err != nil {
		return nil
	}
	return codex.ParseWindows(b)
}
