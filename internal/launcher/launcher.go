// Package launcher is the "Claude Burst" app in ~/Applications: Spotlight
// (Cmd-Space, "burst"), Launchpad or the Dock opens the dashboard with no
// address to remember. The app is a shell script in a bundle that runs
// `claude-burst open`, which goes to the dashboard, or to the support console
// when the gateway is down. Nothing in it needs root, a certificate or a
// hosts entry.
package launcher

import (
	"bytes"
	_ "embed"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Name is what Spotlight and the Dock show.
const Name = "Claude Burst"

// bundleID marks the bundle as ours: Install never writes over, and Remove
// never deletes, an app of the same name that does not carry it.
const bundleID = "ninja.andrewbaker.claude-burst.launcher"

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
func Path(home string) string { return filepath.Join(home, "Applications", Name+".app") }

// ours is whether the app at path is one Install wrote.
func ours(path string) bool {
	b, err := os.ReadFile(filepath.Join(path, "Contents", "Info.plist"))
	return err == nil && bytes.Contains(b, []byte(bundleID))
}

// Install writes the app, or brings ours up to date. bin is the claude-burst
// binary it runs. An app of that name that is not ours is left alone.
func Install(home, bin, version string) (string, error) {
	app := Path(home)
	if _, err := os.Stat(app); err == nil && !ours(app) {
		return "", fmt.Errorf("%s is there and was not made by Claude Burst; move it away and run this again", app)
	}
	if strings.ContainsAny(bin, "\"$`\\\n") {
		return "", fmt.Errorf("binary path %q has characters that cannot go inside the launcher script", bin)
	}
	files := []struct {
		rel  string
		data []byte
		mode os.FileMode
	}{
		{"Contents/Info.plist", []byte(fmt.Sprintf(infoPlistTmpl, version)), 0o644},
		{"Contents/MacOS/claude-burst-open", []byte(fmt.Sprintf(scriptTmpl, bin)), 0o755},
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

// Remove deletes the app if it is ours. It reports whether it did.
func Remove(home string) (bool, error) {
	app := Path(home)
	if !ours(app) {
		return false, nil
	}
	return true, os.RemoveAll(app)
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
# Claude Burst.app: opens the dashboard, or the support console when the
# gateway is down. Written by "claude-burst app install".
BIN="%s"
[ -x "$BIN" ] || BIN="$HOME/.local/bin/claude-burst"
if [ ! -x "$BIN" ]; then
  osascript -e 'display alert "Claude Burst is not installed" message "The claude-burst command is gone. Run install.sh again, or move this app to the Bin."'
  exit 1
fi
if ! "$BIN" open >/dev/null 2>&1; then
  osascript -e 'display alert "Claude Burst is not running" message "Neither the dashboard nor the support console answered. Start it from Terminal with: launchctl kickstart -k gui/$UID/ninja.andrewbaker.claude-burst"'
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
	<string>Claude Burst</string>
	<key>CFBundleDisplayName</key>
	<string>Claude Burst</string>
	<key>CFBundleIdentifier</key>
	<string>` + bundleID + `</string>
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
