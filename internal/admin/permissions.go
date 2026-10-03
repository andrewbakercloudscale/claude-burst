package admin

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/repo"
)

// Permissions: what macOS lets claude-burst do, and how to fix what it
// does not.
//
// Go signs every build ad hoc, and an ad hoc signature's identity is the
// binary's own hash, so macOS's privacy grants were lost on every update
// and "claude-burst would like to access files in your Desktop folder"
// came back. scripts/signing-setup.sh makes a local certificate once and
// the installers sign with it, so the identity, and the Allow, survive.
//
// Folder access is never probed on a GET: looking under Desktop is itself
// what asks. The page shows what session lookups (internal/repo) have
// already seen, and probes only when the user clicks Check.

// The signing identity and identifier scripts/codesign.sh signs with.
const (
	signingName        = "Claude Burst Local Signing"
	signingIdentifier  = "ninja.andrewbaker.claude-burst"
	privacySettingsURL = "x-apple.systempreferences:com.apple.preference.security?Privacy_FilesAndFolders"
)

// Variables so tests never run security, codesign or open, or touch the
// real folders.
var (
	permExec = func(name string, args ...string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.WaitDelay = time.Second
		return cmd.CombinedOutput()
	}
	permReadDir = os.ReadDir
	// permCheckWait bounds a folder check: while macOS shows its prompt the
	// read waits for the answer.
	permCheckWait = 60 * time.Second
)

type signingStatus struct {
	State         string `json:"state"` // ok, warn
	IdentityReady bool   `json:"identity_ready"`
	Signed        bool   `json:"signed"` // the running binary carries the identity
	Authority     string `json:"authority"`
	Identifier    string `json:"identifier"`
	Binary        string `json:"binary"`
	ScriptFound   bool   `json:"script_found"`
	Meaning       string `json:"meaning"`
	Fix           string `json:"fix"`
}

type folderStatus struct {
	repo.FolderAccess
	Meaning string `json:"meaning"`
	Fix     string `json:"fix"`
}

type permissionsResponse struct {
	Signing signingStatus  `json:"signing"`
	Folders []folderStatus `json:"folders"`
}

func (s *Server) signingScript() (string, bool) {
	dir, ok := s.scriptsDir()
	if !ok {
		return "", false
	}
	p := filepath.Join(dir, "signing-setup.sh")
	if st, err := os.Stat(p); err != nil || st.IsDir() {
		return "", false
	}
	return p, true
}

func (s *Server) signing() signingStatus {
	st := signingStatus{State: "warn"}
	_, st.ScriptFound = s.signingScript()
	if out, err := permExec("security", "find-identity", "-v", "-p", "codesigning"); err == nil {
		st.IdentityReady = strings.Contains(string(out), `"`+signingName+`"`)
	}
	if bin, err := executablePath(); err == nil {
		st.Binary = bin
		out, _ := permExec("codesign", "-dvv", bin)
		for _, line := range strings.Split(string(out), "\n") {
			if v, ok := strings.CutPrefix(line, "Identifier="); ok {
				st.Identifier = v
			}
			if v, ok := strings.CutPrefix(line, "Authority="); ok && st.Authority == "" {
				st.Authority = v
			}
			if line == "Signature=adhoc" && st.Authority == "" {
				st.Authority = "ad hoc"
			}
		}
	}
	st.Signed = st.Authority == signingName
	switch {
	case st.Signed:
		st.State = "ok"
		st.Meaning = "This build is signed with the local certificate, so macOS recognises it across updates and remembers your Allow."
	case st.IdentityReady:
		st.Meaning = "The certificate is set up, but this build is not signed with it, so macOS treats it as a new program."
		st.Fix = "Set up signing signs the installed binary and restarts the gateway; the next deploy or install signs it too."
	default:
		st.Meaning = "Each update is signed ad hoc, so macOS sees a new program every time and asks for folder access again."
		st.Fix = "Set up signing (once): it adds a code-signing certificate to your login Keychain. macOS asks for your password or Touch ID."
	}
	if !st.ScriptFound && !st.Signed {
		st.Fix += " scripts/signing-setup.sh was not found next to this install, so run it from a claude-burst checkout."
	}
	return st
}

func folderStatuses() []folderStatus {
	var out []folderStatus
	for _, a := range repo.Access() {
		f := folderStatus{FolderAccess: a}
		switch a.State {
		case repo.Allowed:
			f.Meaning = "Sessions in repositories here are named, and their own Compact at applies."
		case repo.Denied:
			f.Meaning = "macOS refused, so sessions here show their folder name instead of the repository, and a repository's own Compact at cannot match them."
			f.Fix = "Open Privacy settings, Files and Folders, claude-burst, and turn on " + a.Folder + ". Then restart the gateway."
		default:
			f.Meaning = "No session has run under this folder since the gateway started, so there is nothing to go on yet."
			f.Fix = "Check folder access looks now; macOS may ask."
		}
		out = append(out, f)
	}
	return out
}

// handlePermissions reports signing and folder access. It never looks
// inside the folders.
func (s *Server) handlePermissions(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, permissionsResponse{Signing: s.signing(), Folders: folderStatuses()})
}

// handlePermissionsCheck looks inside each protected folder, which may
// raise macOS's prompt; the dashboard says so before the click.
func (s *Server) handlePermissionsCheck(w http.ResponseWriter, r *http.Request) {
	home, err := os.UserHomeDir()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	type result struct {
		Folder string `json:"folder"`
		State  string `json:"state"` // allowed, denied, missing, waiting
		Detail string `json:"detail,omitempty"`
	}
	var results []result
	for _, a := range repo.Access() {
		dir := filepath.Join(home, a.Folder)
		done := make(chan error, 1)
		go func() { _, err := permReadDir(dir); done <- err }()
		res := result{Folder: a.Folder}
		select {
		case err := <-done:
			switch {
			case err == nil:
				res.State = repo.Allowed
				repo.Observe(dir, nil)
			case errors.Is(err, fs.ErrPermission):
				res.State = repo.Denied
				repo.Observe(dir, err)
			case errors.Is(err, fs.ErrNotExist):
				res.State = "missing"
			default:
				res.State, res.Detail = "error", err.Error()
			}
		case <-time.After(permCheckWait):
			res.State, res.Detail = "waiting", "macOS is still asking: answer its prompt, then check again"
		}
		results = append(results, res)
	}
	writeJSON(w, map[string]any{"results": results, "permissions": permissionsResponse{Signing: s.signing(), Folders: folderStatuses()}})
}

// handleSigningSetup opens Terminal running scripts/signing-setup.sh: it
// asks for a password, so it cannot run from here.
func (s *Server) handleSigningSetup(w http.ResponseWriter, r *http.Request) {
	script, ok := s.signingScript()
	if !ok {
		http.Error(w, "scripts/signing-setup.sh was not found next to this install; run it from a claude-burst checkout", http.StatusConflict)
		return
	}
	body := "#!/bin/zsh\n" +
		"cd " + shellQuote(filepath.Dir(filepath.Dir(script))) + " || exit 1\n" +
		"zsh " + shellQuote(script) + "\n" +
		"echo\nread -k 1 \"?Press any key to close this window...\"\n"
	path, err := writeGeneratedScript("signing-setup", body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := launchTerminal(path); err != nil {
		writeJSON(w, map[string]string{"script": path,
			"detail": "Terminal did not open (" + err.Error() + "). Run it yourself: " + path})
		return
	}
	writeJSON(w, map[string]string{"script": path,
		"detail": "A Terminal window is setting up signing. macOS asks for your password or Touch ID, and the Keychain may ask for your login password. Click Refresh here when it finishes."})
}

// handleOpenPrivacySettings opens System Settings at Files and Folders.
func (s *Server) handleOpenPrivacySettings(w http.ResponseWriter, r *http.Request) {
	if out, err := permExec("open", privacySettingsURL); err != nil {
		http.Error(w, "could not open System Settings: "+strings.TrimSpace(string(out)+" "+err.Error()), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"detail": "System Settings is open at Privacy & Security, Files and Folders. Find claude-burst and turn on the folders it needs."})
}
