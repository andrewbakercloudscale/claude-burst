package admin

import (
	"encoding/json"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/andrewbakercloudscale/claude-burst/internal/claudesettings"
)

// The cost and model line under Claude Code's prompt: Claude Code's own
// statusLine setting, pointed at ccusage. Off by default. The setting belongs
// to the user, so Burst only ever adds its own ccusage line where there is no
// status line at all, and only ever removes one that runs ccusage statusline;
// a status line of anyone else's is reported and left alone.

const statusLineMarker = "ccusage statusline"

// ccusageCommand is the command written: the installed ccusage when it is on
// the PATH (fast), else npx, which fetches it on first use. -B text adds the
// burn rate as words rather than an emoji meter. --no-offline fetches
// current prices: ccusage's built-in list did not know Opus 5.5, and the
// line read $0.00 for the block and the burn rate. A variable for tests.
var ccusageCommand = func() string {
	if _, err := exec.LookPath("ccusage"); err == nil {
		return "ccusage statusline -B text --no-offline"
	}
	for _, dir := range []string{"/opt/homebrew/bin", "/usr/local/bin", filepath.Join(homeDir(), ".local", "bin")} {
		if p := filepath.Join(dir, "ccusage"); isExecutable(p) {
			return p + " statusline -B text --no-offline"
		}
	}
	return "npx -y ccusage@latest statusline -B text --no-offline"
}

func isExecutable(p string) bool {
	_, err := exec.LookPath(p)
	return err == nil
}

type statusLineView struct {
	On    bool   `json:"on"`
	Other string `json:"other,omitempty"` // a status line that is not ours, left alone
}

// statusLineCommand reads the current statusLine command, "" when none.
func statusLineCommand(root map[string]any) string {
	sl, _ := root["statusLine"].(map[string]any)
	cmd, _ := sl["command"].(string)
	return cmd
}

func readStatusLine() (statusLineView, error) {
	p, err := claudesettings.Path()
	if err != nil {
		return statusLineView{}, err
	}
	root, err := claudesettings.Read(p)
	if err != nil {
		return statusLineView{}, err
	}
	cmd := statusLineCommand(root)
	if strings.Contains(cmd, statusLineMarker) {
		return statusLineView{On: true}, nil
	}
	return statusLineView{Other: cmd}, nil
}

func (s *Server) handleStatusLineGet(w http.ResponseWriter, r *http.Request) {
	v, err := readStatusLine()
	if err != nil {
		http.Error(w, "reading ~/.claude/settings.json: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, v)
}

func (s *Server) handleStatusLinePost(w http.ResponseWriter, r *http.Request) {
	var req struct {
		On *bool `json:"on"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.On == nil {
		http.Error(w, `bad request body: want {"on": true|false}`, http.StatusBadRequest)
		return
	}
	p, err := claudesettings.Path()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	root, err := claudesettings.Read(p)
	if err != nil {
		http.Error(w, "reading "+p+": "+err.Error(), http.StatusInternalServerError)
		return
	}
	cur := statusLineCommand(root)
	ours := strings.Contains(cur, statusLineMarker)
	switch {
	case *req.On && ours, !*req.On && !ours:
		// Off never touches a status line that is not ours.
		writeJSON(w, map[string]string{"detail": "already as asked; nothing changed"})
		return
	case *req.On && cur != "":
		http.Error(w, "you already have a status line of your own ("+cur+"), so Burst leaves it alone. Remove statusLine from ~/.claude/settings.json first if you want the ccusage one.", http.StatusConflict)
		return
	case *req.On:
		root["statusLine"] = map[string]any{"type": "command", "command": ccusageCommand()}
	default:
		delete(root, "statusLine")
	}
	if err := claudesettings.Write(p, root); err != nil {
		http.Error(w, "writing "+p+": "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"detail": "saved; Claude Code picks it up at its next refresh"})
}
