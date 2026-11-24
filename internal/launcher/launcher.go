// Package launcher is the two apps in ~/Applications that open Burst's pages
// with no address to remember: Spotlight (Cmd-Space, "burst"), Launchpad or
// the Dock.
//
//   - "Claude Burst" runs `claude-burst open`: the dashboard, or the support
//     console when the gateway is down.
//   - "Claude Burst Support" runs `claude-burst open --console`: always the
//     support console (its own port, up when the gateway is not), where Burst
//     is restarted, repaired, turned off and back on.
//
// Each is a shell script in a bundle. Nothing in them needs root, a
// certificate or a hosts entry.
package launcher

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Name is what Spotlight and the Dock show for the dashboard's app, and
// ConsoleName for the support console's.
const (
	Name        = "Claude Burst"
	ConsoleName = "Claude Burst Support"
)

// bundleID marks the bundle as ours: Install never writes over, and Remove
// never deletes, an app of the same name that does not carry it. The console's
// app has its own, so Launch Services keeps the two apart.
const (
	bundleID        = "ninja.andrewbaker.claude-burst.launcher"
	consoleBundleID = "ninja.andrewbaker.claude-burst.launcher.console"
)

// spec is one of the two apps.
type spec struct {
	name, id, args, what string
}

var (
	dashboardApp = spec{Name, bundleID, "open", "opens the dashboard, or the support console when the gateway is down"}
	consoleApp   = spec{ConsoleName, consoleBundleID, "open --console", "opens the support console: restart, repair, turn Burst off or back on"}
)

//go:embed assets/ClaudeBurst.icns
var icon []byte

// Variables so tests never touch the real Launch Services or browser: both
// ignore a test's temporary HOME.
var (
	register = func(app string) error {
		return exec.Command("/System/Library/Frameworks/CoreServices.framework/Frameworks/LaunchServices.framework/Support/lsregister", "-f", app).Run()
	}
	openURL = func(url string) error { return exec.Command("/usr/bin/open", url).Run() }
)

// Path is where the app goes: the user's own Applications folder, which
// Spotlight indexes and which needs no admin rights.
func Path(home string) string { return appPath(home, dashboardApp) }

// ConsolePath is where the support console's app goes.
func ConsolePath(home string) string { return appPath(home, consoleApp) }

func appPath(home string, a spec) string { return filepath.Join(home, "Applications", a.name+".app") }

// ours is whether the app at path is one Install wrote with bundle id id.
// The closing tag keeps the dashboard's id from matching the console's.
func ours(path, id string) bool {
	b, err := os.ReadFile(filepath.Join(path, "Contents", "Info.plist"))
	return err == nil && bytes.Contains(b, []byte("<string>"+id+"</string>"))
}

// Install writes both apps, or brings ours up to date, and returns their
// paths, the dashboard's first. bin is the claude-burst binary they run. An
// app of either name that is not ours is left alone; the other is still
// written, and the error names the one that was not.
func Install(home, bin, version string) ([]string, error) {
	if strings.ContainsAny(bin, "\"$`\\\n") {
		return nil, fmt.Errorf("binary path %q has characters that cannot go inside the launcher script", bin)
	}
	var apps []string
	var errs []error
	for _, a := range []spec{dashboardApp, consoleApp} {
		app, err := install(home, bin, version, a)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		apps = append(apps, app)
	}
	return apps, errors.Join(errs...)
}

func install(home, bin, version string, a spec) (string, error) {
	app := appPath(home, a)
	if _, err := os.Stat(app); err == nil && !ours(app, a.id) {
		return "", fmt.Errorf("%s is there and was not made by Claude Burst; move it away and run this again", app)
	}
	files := []struct {
		rel  string
		data []byte
		mode os.FileMode
	}{
		{"Contents/Info.plist", []byte(fmt.Sprintf(infoPlistTmpl, version, a.name, a.id)), 0o644},
		{"Contents/MacOS/claude-burst-open", []byte(fmt.Sprintf(scriptTmpl, bin, a.name, a.what, a.args)), 0o755},
		{"Contents/Resources/ClaudeBurst.icns", icon, 0o644},
	}
	for _, f := range files {
		p := filepath.Join(app, f.rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(p, f.data, f.mode); err != nil {
			return "", err
		}
		// WriteFile keeps the mode of a file that was already there.
		if err := os.Chmod(p, f.mode); err != nil {
			return "", err
		}
	}
	// Tell Launch Services now; without it Spotlight finds the app within
	// a minute or so anyway, so a failure here is not one of Install.
	_ = register(app)
	return app, nil
}

// Remove deletes each of the two apps that is ours, and returns the paths
// it deleted.
func Remove(home string) ([]string, error) {
	var removed []string
	for _, a := range []spec{dashboardApp, consoleApp} {
		app := appPath(home, a)
		if !ours(app, a.id) {
			continue
		}
		if err := os.RemoveAll(app); err != nil {
			return removed, err
		}
		removed = append(removed, app)
	}
	return removed, nil
}

// Open shows the first of urls that answers, in the default browser, and
// returns it. The dashboard goes first and the support console second, so a
// gateway that is down still opens a page that says why.
func Open(urls ...string) (string, error) {
	client := &http.Client{Timeout: 2 * time.Second}
	for _, u := range urls {
		resp, err := client.Get(u)
		if err != nil {
			continue
		}
		resp.Body.Close()
		return u, openURL(u)
	}
	return "", fmt.Errorf("nothing answered on %s", strings.Join(urls, " or "))
}

// URL is the address a browser on this Mac reaches a listener at. One with
// no host, or a wildcard one, is reached on loopback: the dashboard's own
// Host check accepts only loopback names.
func URL(listen string) (string, error) {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "", fmt.Errorf("%q is not a host:port address: %w", listen, err)
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/", nil
}

// The app's executable. It is a script, so there is nothing to sign and an
// update of the binary needs no new app.
const scriptTmpl = `#!/bin/bash
# %[2]s.app: %[3]s.
# Written by "claude-burst app install".
BIN="%[1]s"
[ -x "$BIN" ] || BIN="$HOME/.local/bin/claude-burst"
if [ ! -x "$BIN" ]; then
  osascript -e 'display alert "Claude Burst is not installed" message "The claude-burst command is gone. Run install.sh again, or move this app to the Bin."'
  exit 1
fi
if ! "$BIN" %[4]s >/dev/null 2>&1; then
  osascript -e 'display alert "Claude Burst is not running" message "The page did not answer. Start it from Terminal with: launchctl kickstart -k gui/$UID/ninja.andrewbaker.claude-burst"'
  exit 1
fi
`

// LSUIElement: the app is gone as soon as the browser has the page, so it
// shows no Dock icon of its own while it runs.
const infoPlistTmpl = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>CFBundleName</key>
	<string>%[2]s</string>
	<key>CFBundleDisplayName</key>
	<string>%[2]s</string>
	<key>CFBundleIdentifier</key>
	<string>%[3]s</string>
	<key>CFBundleExecutable</key>
	<string>claude-burst-open</string>
	<key>CFBundleIconFile</key>
	<string>ClaudeBurst</string>
	<key>CFBundlePackageType</key>
	<string>APPL</string>
	<key>CFBundleInfoDictionaryVersion</key>
	<string>6.0</string>
	<key>CFBundleShortVersionString</key>
	<string>%[1]s</string>
	<key>CFBundleVersion</key>
	<string>%[1]s</string>
	<key>LSUIElement</key>
	<true/>
</dict>
</plist>
`
