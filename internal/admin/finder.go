package admin

import (
	"bytes"
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

	"github.com/andrewbakercloudscale/claude-burst/internal/atomicfile"
)

// Finder shortcuts: right-click a folder, Services, "Launch Claude Code in
// Ghostty" (or a session of it, OMC, Codex, OpenCode, a plain terminal). Each is an
// Automator Quick Action in ~/Library/Services that runs a launcher script
// in ~/.local/bin. The dashboard ticks which ones to install or remove.
//
// Install only adds what is missing: an existing Quick Action or launcher is
// never overwritten, because the usage panel's setup patches the Claude
// launcher in place (claude-panel-setup.sh), and rewriting it would silently
// undo that. Remove deletes the Quick Actions only, and only ones that run
// our launcher; the launchers stay, so a reinstall keeps the panel's patch.

type finderShortcut struct {
	Key  string `json:"key"`
	Name string `json:"name"` // the menu item, and the .workflow name
	Tool string `json:"tool"` // the command the launcher runs; "" for a plain shell
	// Also is a second command the shortcut is no use without.
	Also string `json:"also,omitempty"`
	Note string `json:"note,omitempty"`
	// BypassFlag is what the launcher adds when the row's bypass tick is on;
	// "" for a shortcut with no such tick.
	BypassFlag string `json:"bypass_flag,omitempty"`
	launcher   string // file name in ~/.local/bin
	script     []byte // launcher written when none exists
	// was is the launcher 0.19.2 wrote, which knew no bypass tick: a file
	// still equal to it is ours and unchanged, so it is brought up to date.
	was []byte
}

//go:embed assets/finder/ghostty-claude-launcher
var claudeLauncher []byte

//go:embed assets/finder/ghostty-opencode-launcher
var opencodeLauncher []byte

const claudeBypass = "--dangerously-skip-permissions"

// The Claude launcher has no bypass tick: the usage panel patches that file
// and gives it the dashboard's "Start with bypass permissions" option.
var finderShortcuts = []finderShortcut{
	{Key: "claude", Name: "Launch Claude Code in Ghostty", Tool: "claude", launcher: "ghostty-claude-launcher", script: claudeLauncher},
	{Key: "claude-continue", Name: "Continue last Claude Code session in Ghostty", Tool: "claude", Note: "claude --continue: the folder's most recent conversation",
		BypassFlag: claudeBypass, launcher: "ghostty-claude-continue-launcher",
		script: plainLauncher("claude-continue", "claude", "Claude Code", "--continue", claudeBypass), was: plainLauncherV1("claude", "Claude Code", "--continue")},
	{Key: "claude-resume", Name: "Resume a Claude Code session in Ghostty", Tool: "claude", Note: "claude --resume: pick from the folder's conversations",
		BypassFlag: claudeBypass, launcher: "ghostty-claude-resume-launcher",
		script: plainLauncher("claude-resume", "claude", "Claude Code", "--resume", claudeBypass)},
	{Key: "omc", Name: "Launch Claude Code with OMC in Ghostty", Tool: "omc", Note: "oh-my-claudecode: Claude Code inside tmux",
		BypassFlag: "--madmax", launcher: "ghostty-omc-launcher",
		script: plainLauncher("omc", "omc", "OMC (oh-my-claudecode)", "", "--madmax"), was: plainLauncherV1("omc", "OMC (oh-my-claudecode)", "")},
	{Key: "omc-interop", Name: "Launch OMC and Codex side by side in Ghostty", Tool: "omc", Also: "codex", Note: "omc interop: Claude Code and Codex in one tmux window",
		launcher: "ghostty-omc-interop-launcher", script: plainLauncher("omc-interop", "omc", "OMC (oh-my-claudecode)", "interop", "")},
	{Key: "codex", Name: "Launch Codex in Ghostty", Tool: "codex",
		BypassFlag: "--dangerously-bypass-approvals-and-sandbox", launcher: "ghostty-codex-launcher",
		script: plainLauncher("codex", "codex", "Codex", "", "--dangerously-bypass-approvals-and-sandbox"), was: plainLauncherV1("codex", "Codex", "")},
	{Key: "opencode", Name: "Launch OpenCode in Ghostty", Tool: "opencode", launcher: "ghostty-opencode-launcher", script: opencodeLauncher},
	{Key: "ghostty", Name: "Open Ghostty here", Note: "a plain terminal window in the folder", launcher: "ghostty-here-launcher", script: shellLauncher},
}

// shellLauncher opens the user's own shell in the folder Finder passed.
var shellLauncher = []byte(`#!/bin/bash
# First argument is the folder Finder passed in.
FOLDER="$1"
if [ -n "$FOLDER" ] && [ -d "$FOLDER" ]; then
  cd "$FOLDER"
fi
exec "${SHELL:-/bin/zsh}" -l
`)

// finderConfName holds the bypass ticks, one "bypass=<key>" line each. The
// launchers read it when they run, so a tick needs no reinstall.
const finderConfName = "finder.conf"

func finderConfPath() string {
	return filepath.Join(os.Getenv("HOME"), ".config", "claude-burst", finderConfName)
}

func readFinderBypass() map[string]bool {
	out := map[string]bool{}
	b, err := os.ReadFile(finderConfPath())
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(b), "\n") {
		if k, ok := strings.CutPrefix(strings.TrimSpace(line), "bypass="); ok && k != "" {
			out[k] = true
		}
	}
	return out
}

func writeFinderBypass(on map[string]bool) error {
	var sb strings.Builder
	for _, f := range finderShortcuts {
		if on[f.Key] && f.BypassFlag != "" {
			sb.WriteString("bypass=" + f.Key + "\n")
		}
	}
	if err := os.MkdirAll(filepath.Dir(finderConfPath()), 0o700); err != nil {
		return err
	}
	return atomicfile.Write(finderConfPath(), []byte(sb.String()), 0o600)
}

// plainLauncher is the Claude launcher's shape for another tool: find it
// however we were started, go to the folder Finder passed, run it awake.
func plainLauncher(key, tool, label, args, bypass string) []byte {
	run := `ARGS=(` + args + `)
`
	if bypass != "" {
		run += `# The dashboard's bypass tick for this shortcut (Finder shortcuts).
grep -qx 'bypass=` + key + `' "$HOME/.config/claude-burst/` + finderConfName + `" 2>/dev/null && ARGS+=(` + bypass + `)
`
	}
	return []byte(`#!/bin/bash
# Source login files so ` + "`" + tool + "`" + ` is on PATH no matter how we were launched.
for rc in "$HOME/.zshrc" "$HOME/.bashrc" "$HOME/.bash_profile" "$HOME/.profile"; do
  [ -f "$rc" ] && source "$rc" 2>/dev/null || true
done

TOOL="$(command -v ` + tool + ` 2>/dev/null || true)"
case "$TOOL" in /*) ;; *) TOOL="" ;; esac # a shell function or alias is no use to caffeinate
if [ -z "$TOOL" ]; then
  for candidate in \
    "$HOME/.local/bin/` + tool + `" \
    "$HOME/.npm-global/bin/` + tool + `" \
    "/opt/homebrew/bin/` + tool + `" \
    "/usr/local/bin/` + tool + `" \
    "/usr/bin/` + tool + `"; do
    if [ -x "$candidate" ]; then TOOL="$candidate"; break; fi
  done
fi
if [ -z "$TOOL" ]; then
  osascript -e 'display alert "` + label + ` not found" message "Install it, or make sure the ` + tool + ` command is on your PATH."'
  exit 1
fi

# First argument is the folder Finder passed in.
FOLDER="$1"
if [ -n "$FOLDER" ] && [ -d "$FOLDER" ]; then
  cd "$FOLDER"
fi

# caffeinate -i keeps the Mac awake while a session is running.
` + run + `caffeinate -i "$TOOL" "${ARGS[@]}"
`)
}

// plainLauncherV1 is what 0.19.2 wrote, kept to recognise its files.
func plainLauncherV1(tool, label, args string) []byte {
	if args != "" {
		args = " " + args
	}
	return []byte(`#!/bin/bash
# Source login files so ` + "`" + tool + "`" + ` is on PATH no matter how we were launched.
for rc in "$HOME/.zshrc" "$HOME/.bashrc" "$HOME/.bash_profile" "$HOME/.profile"; do
  [ -f "$rc" ] && source "$rc" 2>/dev/null || true
done

TOOL="$(command -v ` + tool + ` 2>/dev/null || true)"
case "$TOOL" in /*) ;; *) TOOL="" ;; esac # a shell function or alias is no use to caffeinate
if [ -z "$TOOL" ]; then
  for candidate in \
    "$HOME/.local/bin/` + tool + `" \
    "$HOME/.npm-global/bin/` + tool + `" \
    "/opt/homebrew/bin/` + tool + `" \
    "/usr/local/bin/` + tool + `" \
    "/usr/bin/` + tool + `"; do
    if [ -x "$candidate" ]; then TOOL="$candidate"; break; fi
  done
fi
if [ -z "$TOOL" ]; then
  osascript -e 'display alert "` + label + ` not found" message "Install it, or make sure the ` + tool + ` command is on your PATH."'
  exit 1
fi

# First argument is the folder Finder passed in.
FOLDER="$1"
if [ -n "$FOLDER" ] && [ -d "$FOLDER" ]; then
  cd "$FOLDER"
fi

# caffeinate -i keeps the Mac awake while a session is running.
caffeinate -i "$TOOL"` + args + `
`)
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
	// Missing is the command that is not installed, when ToolFound is false.
	Missing string `json:"missing,omitempty"`
	// BypassOn is the row's bypass tick. BypassOK is false when a launcher
	// is there that does not read the tick (changed by hand, or not ours).
	BypassOn bool `json:"bypass_on"`
	BypassOK bool `json:"bypass_ok"`
}

type finderView struct {
	Ghostty   bool           `json:"ghostty"`
	Shortcuts []finderStatus `json:"shortcuts"`
}

func readFinder() finderView {
	v := finderView{Ghostty: ghosttyInstalled()}
	bypass := readFinderBypass()
	for _, f := range finderShortcuts {
		st := finderStatus{finderShortcut: f, ToolFound: true, BypassOn: bypass[f.Key] && f.BypassFlag != ""}
		for _, tool := range []string{f.Tool, f.Also} {
			if tool != "" && st.ToolFound && !toolOnPath(tool) {
				st.ToolFound, st.Missing = false, tool
			}
		}
		if b, err := os.ReadFile(filepath.Join(f.workflowPath(), "Contents", "document.wflow")); err == nil {
			st.Installed = true
			st.Ours = strings.Contains(string(b), f.launcherPath())
		} else if _, err := os.Stat(f.workflowPath()); err == nil {
			st.Installed = true // there, but not in a shape we recognise: leave it alone
		}
		if fi, err := os.Stat(f.launcherPath()); err == nil && !fi.IsDir() {
			st.Launcher = true
		}
		if f.BypassFlag != "" {
			b, err := os.ReadFile(f.launcherPath())
			st.BypassOK = err != nil || bytes.Contains(b, []byte("bypass="+f.Key)) || (f.was != nil && bytes.Equal(b, f.was))
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
		// Keys are the ticked shortcuts. Left out, it is every one.
		Keys []string `json:"keys"`
		// On is the tick, for "bypass": Keys is then the one row it is on.
		On *bool `json:"on"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || (req.Action != "install" && req.Action != "remove" && req.Action != "bypass") {
		http.Error(w, "action must be install, remove or bypass", http.StatusBadRequest)
		return
	}
	for _, k := range req.Keys {
		known := false
		for _, f := range finderShortcuts {
			known = known || f.Key == k
		}
		if !known {
			http.Error(w, "no Finder shortcut is called "+k, http.StatusBadRequest)
			return
		}
	}
	if req.Keys != nil && len(req.Keys) == 0 {
		http.Error(w, "tick at least one shortcut", http.StatusBadRequest)
		return
	}
	finderMu.Lock()
	defer finderMu.Unlock()
	var done []string
	var err error
	if req.Action == "bypass" {
		if len(req.Keys) != 1 || req.On == nil {
			http.Error(w, "bypass needs one shortcut in keys, and on true or false", http.StatusBadRequest)
			return
		}
		msg, err := setFinderBypass(req.Keys[0], *req.On)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]any{"done": []string{msg}, "finder": readFinder()})
		return
	}
	if req.Action == "install" {
		done, err = installFinderShortcuts(s.readPanel().Installed, req.Keys...)
	} else {
		done, err = removeFinderShortcuts(req.Keys...)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"done": done, "finder": readFinder()})
}

// finderPicked is the shortcuts named by keys, every one when none is named.
func finderPicked(keys []string) []finderStatus {
	all := readFinder().Shortcuts
	if len(keys) == 0 {
		return all
	}
	var out []finderStatus
	for _, st := range all {
		for _, k := range keys {
			if st.Key == k {
				out = append(out, st)
			}
		}
	}
	return out
}

// refreshLauncher brings a launcher 0.19.2 wrote up to date. Any other
// file is somebody's and is left alone.
func refreshLauncher(f finderShortcut) error {
	if f.was == nil {
		return nil
	}
	if b, err := os.ReadFile(f.launcherPath()); err != nil || !bytes.Equal(b, f.was) {
		return nil
	}
	return os.WriteFile(f.launcherPath(), f.script, 0o755)
}

// setFinderBypass saves one row's bypass tick. It takes effect the next
// time the shortcut is used: the launcher reads the tick as it starts.
func setFinderBypass(key string, on bool) (string, error) {
	for _, st := range readFinder().Shortcuts {
		if st.Key != key {
			continue
		}
		if st.BypassFlag == "" {
			return "", fmt.Errorf("%s has no bypass option", st.Name)
		}
		if on && !st.BypassOK {
			return "", fmt.Errorf("%s was changed by hand or was not written here, so it does not read this option; delete it and press Install to get one that does", st.launcherPath())
		}
		if err := refreshLauncher(st.finderShortcut); err != nil {
			return "", err
		}
		ticks := readFinderBypass()
		ticks[key] = on
		if err := writeFinderBypass(ticks); err != nil {
			return "", err
		}
		if on {
			return st.Name + ": starts with " + st.BypassFlag + " from the next time it is used", nil
		}
		return st.Name + ": starts without " + st.BypassFlag + " again", nil
	}
	return "", fmt.Errorf("no Finder shortcut is called %s", key)
}

func installFinderShortcuts(panelInstalled bool, keys ...string) ([]string, error) {
	if !ghosttyInstalled() {
		return nil, fmt.Errorf("Ghostty is not installed (looked in /Applications and ~/Applications); get it from https://ghostty.org")
	}
	var done []string
	changed := false
	for _, st := range finderPicked(keys) {
		f := st.finderShortcut
		if err := refreshLauncher(f); err != nil {
			return done, fmt.Errorf("updating %s: %w", f.launcherPath(), err)
		}
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
			msg += "; " + st.Missing + " is not installed, so it will say so when used"
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

func removeFinderShortcuts(keys ...string) ([]string, error) {
	var done []string
	changed := false
	for _, st := range finderPicked(keys) {
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
