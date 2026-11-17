package launcher

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// The notifier is a second, tiny app nested in Claude Burst.app, at
// Contents/Helpers/Claude Burst Notifier.app, that posts alerts to macOS
// Notification Center (internal/alerting runs it). It is an app of its own,
// with its own bundle id, because Notification Center files notifications
// and asks permission per bundle: posted from claude-burst, a bare binary,
// there is no bundle to file them under, and posted through osascript they
// went under Script Editor and were dropped (removed on 3 Oct 2026). Its
// display name is "Claude Burst", so that is what the prompt and the banners
// say.
//
// Nested rather than a second app in ~/Applications: one app for the person
// to see, Spotlight shows only the outer one, Remove takes both, and the
// outer app's ownership check (ours) covers the helper too. The outer app
// stays a script that needs no signature; the helper is compiled Swift and
// is signed on its own.

// NotifierBundleID is the identity macOS files the notifications and the
// permission under. Never change it: a new id asks for permission again.
const NotifierBundleID = "ninja.andrewbaker.claude-burst.notifier"

// notifierVersion is the helper's own version, not Burst's: the helper is
// rebuilt only when its source, its Info.plist or its signing changes, so
// a deploy that changed none of them leaves it, and its signature, alone.
const notifierVersion = "1"

//go:embed assets/notifier.swift
var notifierSource []byte

// The signing identity scripts/codesign.sh uses for claude-burst.
const signingName = "Claude Burst Local Signing"

// Variables so tests never run swiftc, codesign or security.
var (
	// compileNotifier builds src into the executable out.
	compileNotifier = func(src, out string) error {
		cmd := exec.Command("/usr/bin/swiftc", "-O", "-swift-version", "5", "-o", out, src)
		if b, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("swiftc: %v: %s", err, strings.TrimSpace(string(b)))
		}
		return nil
	}
	// signingIdentity is the identity to sign with, "-" (ad hoc) when the
	// local one is missing or macOS no longer trusts it.
	signingIdentity = func() string {
		out, err := exec.Command("security", "find-identity", "-v", "-p", "codesigning").Output()
		if err == nil && bytes.Contains(out, []byte(`"`+signingName+`"`)) {
			return signingName
		}
		return "-"
	}
	signApp = func(app, identity string) error {
		if b, err := exec.Command("codesign", "-f", "-s", identity, app).CombinedOutput(); err != nil {
			return fmt.Errorf("codesign: %v: %s", err, strings.TrimSpace(string(b)))
		}
		return nil
	}
)

// NotifierPath is the helper app inside the Claude Burst app.
func NotifierPath(home string) string {
	return filepath.Join(Path(home), "Contents", "Helpers", "Claude Burst Notifier.app")
}

// NotifierExecutable is the helper's executable, which the gateway runs.
func NotifierExecutable(home string) string {
	return filepath.Join(NotifierPath(home), "Contents", "MacOS", "claude-burst-notifier")
}

func notifierStamp(identity string) string {
	h := sha256.New()
	h.Write(notifierSource)
	h.Write([]byte(fmt.Sprintf(notifierPlistTmpl, notifierVersion)))
	h.Write(icon)
	h.Write([]byte(identity))
	return hex.EncodeToString(h.Sum(nil))
}

// InstallNotifier builds the helper into the Claude Burst app, or leaves it
// when it is already this build: the source, the Info.plist and the signing
// identity it was made with are recorded in a stamp beside it. It reports
// what it did, for the install's output. The app must be installed first.
func InstallNotifier(home string) (string, error) {
	app := Path(home)
	if !ours(app) {
		return "", fmt.Errorf("%s is not installed; run claude-burst app install", app)
	}
	identity := signingIdentity()
	dest := NotifierPath(home)
	// Outside the helper's bundle, which its signature seals. It records the
	// identity the helper was actually signed with, so a helper left ad hoc
	// because the local identity failed is tried again next time.
	stampFile := filepath.Join(filepath.Dir(dest), ".notifier-stamp")
	if b, err := os.ReadFile(stampFile); err == nil && strings.TrimSpace(string(b)) == notifierStamp(identity) {
		if _, err := os.Stat(NotifierExecutable(home)); err == nil {
			return "up to date", nil
		}
	}

	// Built beside the destination and moved in whole, so a failed build
	// leaves the helper that was there.
	tmp, err := os.MkdirTemp(filepath.Dir(app), ".notifier-build-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	src := filepath.Join(tmp, "notifier.swift")
	if err := os.WriteFile(src, notifierSource, 0o644); err != nil {
		return "", err
	}
	staged := filepath.Join(tmp, "Claude Burst Notifier.app")
	exe := filepath.Join(staged, "Contents", "MacOS", "claude-burst-notifier")
	if err := os.MkdirAll(filepath.Dir(exe), 0o755); err != nil {
		return "", err
	}
	if err := compileNotifier(src, exe); err != nil {
		return "", err
	}
	files := []struct {
		rel  string
		data []byte
	}{
		{"Contents/Info.plist", []byte(fmt.Sprintf(notifierPlistTmpl, notifierVersion))},
		{"Contents/Resources/ClaudeBurst.icns", icon},
	}
	for _, f := range files {
		p := filepath.Join(staged, f.rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(p, f.data, 0o644); err != nil {
			return "", err
		}
	}
	signed, used := "signed with "+signingName, identity
	if err := signApp(staged, identity); err != nil {
		if identity == "-" {
			return "", err
		}
		// The local identity can be listed and still refuse (its key not
		// yet allowed for codesign): ad hoc still posts notifications.
		if err2 := signApp(staged, "-"); err2 != nil {
			return "", err2
		}
		signed, used = "signed ad hoc ("+signingName+" failed: "+err.Error()+")", "-"
	} else if identity == "-" {
		signed = "signed ad hoc"
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", err
	}
	if err := os.RemoveAll(dest); err != nil {
		return "", err
	}
	if err := os.Rename(staged, dest); err != nil {
		return "", err
	}
	if err := os.WriteFile(stampFile, []byte(notifierStamp(used)+"\n"), 0o644); err != nil {
		return "", err
	}
	_ = register(dest)
	return "built, " + signed, nil
}

// LSUIElement: no Dock icon and no menu bar while it posts or while a click
// on a notification opens it.
const notifierPlistTmpl = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>CFBundleName</key>
	<string>Claude Burst</string>
	<key>CFBundleDisplayName</key>
	<string>Claude Burst</string>
	<key>CFBundleIdentifier</key>
	<string>` + NotifierBundleID + `</string>
	<key>CFBundleExecutable</key>
	<string>claude-burst-notifier</string>
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
	<key>LSMinimumSystemVersion</key>
	<string>12.0</string>
	<key>LSUIElement</key>
	<true/>
</dict>
</plist>
`
