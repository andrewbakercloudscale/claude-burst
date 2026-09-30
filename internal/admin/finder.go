package admin

import (
	"crypto/rand"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// Finder shortcuts: right-click a folder, Services, "Launch Claude Code in
// Ghostty" (or OpenCode). Each is an Automator Quick Action in
// ~/Library/Services that runs a launcher script in ~/.local/bin.
//
// Install only adds what is missing: an existing Quick Action or launcher is
// never overwritten, because the usage panel's setup patches the Claude
// launcher in place (claude-panel-setup.sh), and rewriting it would silently
// undo that. Remove deletes the Quick Actions only, and only ones that run
// our launcher; the launchers stay, so a reinstall keeps the panel's patch.

type finderShortcut struct {
	Key      string `json:"key"`
	Name     string `json:"name"` // the menu item, and the .workflow name
	Tool     string `json:"tool"` // the command the launcher runs
	launcher string // file name in ~/.local/bin
	script   []byte // launcher written when none exists
}

//go:embed assets/finder/ghostty-claude-launcher
var claudeLauncher []byte

//go:embed assets/finder/ghostty-opencode-launcher
var opencodeLauncher []byte

var finderShortcuts = []finderShortcut{
	{Key: "claude", Name: "Launch Claude Code in Ghostty", Tool: "claude", launcher: "ghostty-claude-launcher", script: claudeLauncher},
	{Key: "opencode", Name: "Launch OpenCode in Ghostty", Tool: "opencode", launcher: "ghostty-opencode-launcher", script: opencodeLauncher},
}

// Variables so tests never touch the real Services menu, Ghostty or PATH:
// pbs and mdfind ignore a test's temporary HOME.
var (
	refreshServices = func() error {
		return exec.Command("/System/Library/CoreServices/pbs", "-update").Run()
	}
	ghosttyInstalled = func() bool {
		for _, p := range []string{"/Applications/Ghostty.app", filepath.Join(os.Getenv("HOME"), "Applications", "Ghostty.app")} {
			if st, err := os.Stat(p); err == nil && st.IsDir() {
				return true
			}
		}
		return false
	}
	toolOnPath = func(tool string) bool {
		if _, err := exec.LookPath(tool); err == nil {
			return true
		}
		home := os.Getenv("HOME")
		for _, d := range []string{home + "/.local/bin", home + "/.npm-global/bin", "/opt/homebrew/bin", "/usr/local/bin"} {
			if st, err := os.Stat(filepath.Join(d, tool)); err == nil && !st.IsDir() {
				return true
			}
		}
		return false
	}
)

var finderMu sync.Mutex

func servicesDir() string { return filepath.Join(os.Getenv("HOME"), "Library", "Services") }
func launcherDir() string { return filepath.Join(os.Getenv("HOME"), ".local", "bin") }

func (f finderShortcut) workflowPath() string {
	return filepath.Join(servicesDir(), f.Name+".workflow")
}
func (f finderShortcut) launcherPath() string { return filepath.Join(launcherDir(), f.launcher) }

type finderStatus struct {
	finderShortcut
	Installed bool `json:"installed"` // the Quick Action is in ~/Library/Services
	Ours      bool `json:"ours"`      // and it runs our launcher, so Remove may delete it
	Launcher  bool `json:"launcher"`  // the launcher script exists
	ToolFound bool `json:"tool_found"`
}

type finderView struct {
	Ghostty   bool           `json:"ghostty"`
	Shortcuts []finderStatus `json:"shortcuts"`
}

func readFinder() finderView {
	v := finderView{Ghostty: ghosttyInstalled()}
	for _, f := range finderShortcuts {
		st := finderStatus{finderShortcut: f, ToolFound: toolOnPath(f.Tool)}
		if b, err := os.ReadFile(filepath.Join(f.workflowPath(), "Contents", "document.wflow")); err == nil {
			st.Installed = true
			st.Ours = strings.Contains(string(b), f.launcherPath())
		} else if _, err := os.Stat(f.workflowPath()); err == nil {
			st.Installed = true // there, but not in a shape we recognise: leave it alone
		}
		if fi, err := os.Stat(f.launcherPath()); err == nil && !fi.IsDir() {
			st.Launcher = true
		}
		v.Shortcuts = append(v.Shortcuts, st)
	}
	return v
}

func (s *Server) handleFinder(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, readFinder())
}

func (s *Server) handleFinderInstall(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Action string `json:"action"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || (req.Action != "install" && req.Action != "remove") {
		http.Error(w, "action must be install or remove", http.StatusBadRequest)
		return
	}
	finderMu.Lock()
	defer finderMu.Unlock()
	var done []string
	var err error
	if req.Action == "install" {
		done, err = installFinderShortcuts(s.readPanel().Installed)
	} else {
		done, err = removeFinderShortcuts()
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"done": done, "finder": readFinder()})
}

func installFinderShortcuts(panelInstalled bool) ([]string, error) {
	if !ghosttyInstalled() {
		return nil, fmt.Errorf("Ghostty is not installed (looked in /Applications and ~/Applications); get it from https://ghostty.org")
	}
	var done []string
	changed := false
	for _, st := range readFinder().Shortcuts {
		f := st.finderShortcut
		if st.Installed {
			done = append(done, f.Name+": already installed, left as it is")
			continue
		}
		if !st.Launcher {
			if err := os.MkdirAll(launcherDir(), 0o755); err != nil {
				return done, err
			}
			if err := os.WriteFile(f.launcherPath(), f.script, 0o755); err != nil {
				return done, fmt.Errorf("writing %s: %w", f.launcherPath(), err)
			}
		}
		if err := writeWorkflow(f); err != nil {
			os.RemoveAll(f.workflowPath())
			return done, fmt.Errorf("%s: %w", f.Name, err)
		}
		changed = true
		msg := f.Name + ": installed"
		if st.Launcher {
			msg += " (kept the existing launcher)"
		} else if f.Key == "claude" && panelInstalled {
			// The panel patches this launcher at setup time, and it did not
			// exist then.
			msg += "; click Reinstall / update on the usage panel so it opens beside sessions started from Finder"
		}
		if !st.ToolFound {
			msg += "; " + f.Tool + " is not installed, so it will say so when used"
		}
		done = append(done, msg)
	}
	if changed {
		if err := refreshServices(); err != nil {
			done = append(done, "the Services menu did not refresh ("+err.Error()+"); log out and in, or it appears within a few minutes")
		}
	}
	return done, nil
}

func removeFinderShortcuts() ([]string, error) {
	var done []string
	changed := false
	for _, st := range readFinder().Shortcuts {
		f := st.finderShortcut
		switch {
		case !st.Installed:
			done = append(done, f.Name+": not installed")
		case !st.Ours:
			done = append(done, f.Name+": left in place, it does not run "+f.launcherPath()+" so it was not made here")
		default:
			if err := os.RemoveAll(f.workflowPath()); err != nil {
				return done, err
			}
			changed = true
			done = append(done, f.Name+": removed")
		}
	}
	if changed {
		_ = refreshServices()
	}
	return done, nil
}

// xmlEscape escapes what plist text needs, and no more: Automator writes
// quotes and newlines literally, and so do we, so the file reads the same.
var xmlEscape = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return strings.ToUpper(fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]))
}

// writeWorkflow writes the Quick Action bundle: the same two files
// Automator writes for a "Run Shell Script" service on folders in Finder.
func writeWorkflow(f finderShortcut) error {
	if strings.ContainsAny(f.launcherPath(), "\"$`\\") {
		return fmt.Errorf("launcher path %q has characters that cannot go inside the shell command", f.launcherPath())
	}
	contents := filepath.Join(f.workflowPath(), "Contents")
	if err := os.MkdirAll(contents, 0o755); err != nil {
		return err
	}
	info := fmt.Sprintf(infoPlistTmpl, xmlEscape(f.Name))
	// macOS cannot start a Ghostty window from its binary; open -na with -e
	// is the supported way. A clean window that quits with its last tab.
	cmd := `for folder in "$@"; do
  [ -d "$folder" ] || continue
  open -na Ghostty.app --args --window-save-state=never --quit-after-last-window-closed=true -e "` + f.launcherPath() + `" "$folder" &
done
`
	doc := fmt.Sprintf(documentWflowTmpl, xmlEscape(cmd), newUUID(), newUUID(), newUUID())
	if err := os.WriteFile(filepath.Join(contents, "Info.plist"), []byte(info), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(contents, "document.wflow"), []byte(doc), 0o644)
}

const infoPlistTmpl = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>NSServices</key>
	<array>
		<dict>
			<key>NSMenuItem</key>
			<dict>
				<key>default</key>
				<string>%s</string>
			</dict>
			<key>NSMessage</key>
			<string>runWorkflowAsService</string>
			<key>NSRequiredContext</key>
			<dict>
				<key>NSApplicationIdentifier</key>
				<string>com.apple.finder</string>
			</dict>
			<key>NSSendFileTypes</key>
			<array>
				<string>public.folder</string>
			</array>
		</dict>
	</array>
</dict>
</plist>
`

const documentWflowTmpl = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>AMApplicationBuild</key>
	<string>521.1</string>
	<key>AMApplicationVersion</key>
	<string>2.10</string>
	<key>AMDocumentVersion</key>
	<string>2</string>
	<key>actions</key>
	<array>
		<dict>
			<key>action</key>
			<dict>
				<key>AMAccepts</key>
				<dict>
					<key>Container</key>
					<string>List</string>
					<key>Optional</key>
					<true/>
					<key>Types</key>
					<array>
						<string>com.apple.cocoa.path</string>
					</array>
				</dict>
				<key>AMActionVersion</key>
				<string>2.0.3</string>
				<key>AMApplication</key>
				<array>
					<string>Automator</string>
				</array>
				<key>ActionBundlePath</key>
				<string>/System/Library/Automator/Run Shell Script.action</string>
				<key>ActionName</key>
				<string>Run Shell Script</string>
				<key>ActionParameters</key>
				<dict>
					<key>COMMAND_STRING</key>
					<string>%s</string>
					<key>CheckedForUserDefaultShell</key>
					<true/>
					<key>inputMethod</key>
					<integer>1</integer>
					<key>shell</key>
					<string>/bin/bash</string>
					<key>source</key>
					<string></string>
				</dict>
				<key>BundleIdentifier</key>
				<string>com.apple.RunShellScript</string>
				<key>CFBundleVersion</key>
				<string>2.0.3</string>
				<key>CanShowSelectedItemsWhenRun</key>
				<false/>
				<key>CanShowWhenRun</key>
				<true/>
				<key>Category</key>
				<array>
					<string>AMCategoryUtilities</string>
				</array>
				<key>Class Name</key>
				<string>RunShellScriptAction</string>
				<key>InputUUID</key>
				<string>%s</string>
				<key>OutputUUID</key>
				<string>%s</string>
				<key>UUID</key>
				<string>%s</string>
				<key>UnlockPlugin</key>
				<false/>
				<key>arguments</key>
				<dict/>
				<key>isViewVisible</key>
				<integer>1</integer>
				<key>location</key>
				<string>309.500000:253.000000</string>
				<key>nibPath</key>
				<string>/System/Library/Automator/Run Shell Script.action/Contents/Resources/English.lproj/main.nib</string>
			</dict>
			<key>isViewVisible</key>
			<integer>1</integer>
		</dict>
	</array>
	<key>connectors</key>
	<dict/>
	<key>workflowMetaData</key>
	<dict>
		<key>serviceApplicationBundleID</key>
		<string>com.apple.finder</string>
		<key>serviceApplicationPath</key>
		<string>/System/Library/CoreServices/Finder.app</string>
		<key>serviceInputTypeIdentifier</key>
		<string>com.apple.Automator.fileSystemObject.folder</string>
		<key>serviceOutputTypeIdentifier</key>
		<string>com.apple.Automator.nothing</string>
		<key>serviceProcessesInput</key>
		<integer>0</integer>
		<key>workflowTypeIdentifier</key>
		<string>com.apple.Automator.servicesMenu</string>
	</dict>
</dict>
</plist>
`
