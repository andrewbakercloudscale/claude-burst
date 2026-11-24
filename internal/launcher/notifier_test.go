package launcher

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// stubNotifierBuild keeps swiftc, codesign and security out of a test: the
// "compiler" writes a placeholder executable, and each step is recorded.
func stubNotifierBuild(t *testing.T, identity string) (compiled, signed *[]string) {
	t.Helper()
	compiled, signed = &[]string{}, &[]string{}
	oldC, oldI, oldS := compileNotifier, signingIdentity, signApp
	compileNotifier = func(src, out string) error {
		b, err := os.ReadFile(src)
		if err != nil || !strings.Contains(string(b), "UNUserNotificationCenter") {
			t.Errorf("compiled %s, which is not the helper's source (%v)", src, err)
		}
		*compiled = append(*compiled, out)
		return os.WriteFile(out, []byte("#!/bin/sh\n"), 0o755)
	}
	signingIdentity = func() string { return identity }
	signApp = func(app, id string) error { *signed = append(*signed, id); return nil }
	t.Cleanup(func() { compileNotifier, signingIdentity, signApp = oldC, oldI, oldS })
	return compiled, signed
}

func TestInstallNotifierBuildsOnceAndAgainWhenSigningChanges(t *testing.T) {
	stub(t)
	compiled, signed := stubNotifierBuild(t, "-")
	home := t.TempDir()
	if _, err := Install(home, "/usr/local/bin/claude-burst", "1.0"); err != nil {
		t.Fatal(err)
	}
	what, err := InstallNotifier(home)
	if err != nil || !strings.Contains(what, "built") {
		t.Fatalf("InstallNotifier = %q, %v", what, err)
	}
	if _, err := os.Stat(NotifierExecutable(home)); err != nil {
		t.Fatalf("no executable: %v", err)
	}
	plist := filepath.Join(NotifierPath(home), "Contents", "Info.plist")
	b, err := os.ReadFile(plist)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{NotifierBundleID, "<string>Claude Burst</string>", "<string>claude-burst-notifier</string>"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("Info.plist lacks %s", want)
		}
	}
	if out, err := exec.Command("/usr/bin/plutil", "-lint", plist).CombinedOutput(); err != nil {
		t.Fatalf("Info.plist is not a property list: %s", out)
	}
	// The same build again: nothing compiled, nothing signed.
	if what, err := InstallNotifier(home); err != nil || what != "up to date" {
		t.Fatalf("second InstallNotifier = %q, %v", what, err)
	}
	if len(*compiled) != 1 || len(*signed) != 1 || (*signed)[0] != "-" {
		t.Fatalf("compiled %v, signed %v", *compiled, *signed)
	}
	// The local identity becomes trusted: rebuilt and signed with it.
	signingIdentity = func() string { return signingName }
	if what, err := InstallNotifier(home); err != nil || !strings.Contains(what, signingName) {
		t.Fatalf("InstallNotifier after the identity = %q, %v", what, err)
	}
	if len(*compiled) != 2 || (*signed)[1] != signingName {
		t.Fatalf("compiled %v, signed %v", *compiled, *signed)
	}
	// The outer app's update leaves the helper alone, and Remove takes both.
	if _, err := Install(home, "/usr/local/bin/claude-burst", "1.1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(NotifierExecutable(home)); err != nil {
		t.Fatalf("updating the app removed the helper: %v", err)
	}
	if removed, err := Remove(home); err != nil || len(removed) == 0 {
		t.Fatalf("Remove = %v, %v", removed, err)
	}
	if _, err := os.Stat(NotifierPath(home)); !os.IsNotExist(err) {
		t.Fatalf("the helper outlived the app: %v", err)
	}
}

// A local identity that is listed but refuses to sign leaves the helper
// signed ad hoc, which still posts, and it is tried again next time.
func TestInstallNotifierFallsBackToAdHoc(t *testing.T) {
	stub(t)
	compiled, signed := stubNotifierBuild(t, signingName)
	signApp = func(app, id string) error {
		*signed = append(*signed, id)
		if id != "-" {
			return os.ErrPermission
		}
		return nil
	}
	home := t.TempDir()
	if _, err := Install(home, "/usr/local/bin/claude-burst", "1.0"); err != nil {
		t.Fatal(err)
	}
	what, err := InstallNotifier(home)
	if err != nil || !strings.Contains(what, "ad hoc") {
		t.Fatalf("InstallNotifier = %q, %v", what, err)
	}
	if _, err := InstallNotifier(home); err != nil || len(*compiled) != 2 {
		t.Fatalf("not tried again: compiled %v, err %v", *compiled, err)
	}
}

func TestInstallNotifierNeedsTheApp(t *testing.T) {
	stub(t)
	compiled, _ := stubNotifierBuild(t, "-")
	if _, err := InstallNotifier(t.TempDir()); err == nil || len(*compiled) != 0 {
		t.Fatalf("built into a missing app: %v %v", err, *compiled)
	}
}

func TestInstallNotifierKeepsTheOldHelperWhenTheBuildFails(t *testing.T) {
	stub(t)
	stubNotifierBuild(t, "-")
	home := t.TempDir()
	if _, err := Install(home, "/usr/local/bin/claude-burst", "1.0"); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallNotifier(home); err != nil {
		t.Fatal(err)
	}
	signingIdentity = func() string { return signingName } // forces a rebuild
	compileNotifier = func(src, out string) error { return os.ErrInvalid }
	if _, err := InstallNotifier(home); err == nil {
		t.Fatal("a failed build reported success")
	}
	if _, err := os.Stat(NotifierExecutable(home)); err != nil {
		t.Fatalf("a failed build removed the working helper: %v", err)
	}
	if left, _ := filepath.Glob(filepath.Join(home, "Applications", ".notifier-build-*")); len(left) != 0 {
		t.Fatalf("build directory left behind: %v", left)
	}
}
