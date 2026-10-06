package launcher

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// stub keeps Launch Services and the browser out of a test, and says what
// each was asked for.
func stub(t *testing.T) (registered, opened *[]string) {
	t.Helper()
	registered, opened = &[]string{}, &[]string{}
	oldReg, oldOpen := register, openURL
	register = func(app string) error { *registered = append(*registered, app); return nil }
	openURL = func(u string) error { *opened = append(*opened, u); return nil }
	t.Cleanup(func() { register, openURL = oldReg, oldOpen })
	return registered, opened
}

func TestInstallWritesAnAppThatRunsOpen(t *testing.T) {
	registered, _ := stub(t)
	home := t.TempDir()
	// A stand-in claude-burst that records how it was called.
	called := filepath.Join(home, "called")
	bin := filepath.Join(home, "claude-burst")
	if err := os.WriteFile(bin, []byte("#!/bin/bash\necho \"$@\" > '"+called+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	app, err := Install(home, bin, "1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, "Applications", "Claude Burst.app"); app != want {
		t.Fatalf("app = %s, want %s", app, want)
	}
	if len(*registered) != 1 || (*registered)[0] != app {
		t.Fatalf("Launch Services was told %v, want the app once", *registered)
	}
	plist, err := os.ReadFile(filepath.Join(app, "Contents", "Info.plist"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"<string>claude-burst-open</string>", "<string>ClaudeBurst</string>", "<string>1.2.3</string>", bundleID} {
		if !strings.Contains(string(plist), want) {
			t.Errorf("Info.plist lacks %s", want)
		}
	}
	if out, err := exec.Command("/usr/bin/plutil", "-lint", filepath.Join(app, "Contents", "Info.plist")).CombinedOutput(); err != nil {
		t.Fatalf("Info.plist is not a property list: %s", out)
	}
	if fi, err := os.Stat(filepath.Join(app, "Contents", "Resources", "ClaudeBurst.icns")); err != nil || fi.Size() == 0 {
		t.Fatalf("the icon is missing: %v", err)
	}

	// The executable is what macOS runs when the app is opened. It is run
	// here from a copy beside the bundle: on this Mac a script inside an
	// .app that starts a program out of a temporary folder is killed
	// (SIGKILL, by something outside the test), and the stand-in can be
	// nowhere else.
	exe := filepath.Join(app, "Contents", "MacOS", "claude-burst-open")
	fi, err := os.Stat(exe)
	if err != nil || fi.Mode().Perm() != 0o755 {
		t.Fatalf("the app's executable: %v, mode %v, want 0755", err, fi.Mode())
	}
	script, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	plain := filepath.Join(home, "claude-burst-open")
	if err := os.WriteFile(plain, script, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(plain)
	cmd.Env = append(os.Environ(), "HOME="+home) // its fallback is $HOME/.local/bin
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the app's executable failed: %v: %s", err, out)
	}
	got, err := os.ReadFile(called)
	if err != nil || strings.TrimSpace(string(got)) != "open" {
		t.Fatalf("the app ran claude-burst with %q (%v), want open", got, err)
	}
}

func TestInstallBringsItsOwnAppUpToDate(t *testing.T) {
	stub(t)
	home := t.TempDir()
	if _, err := Install(home, "/old/claude-burst", "1.0.0"); err != nil {
		t.Fatal(err)
	}
	app, err := Install(home, "/new/claude-burst", "2.0.0")
	if err != nil {
		t.Fatal(err)
	}
	script, _ := os.ReadFile(filepath.Join(app, "Contents", "MacOS", "claude-burst-open"))
	if !strings.Contains(string(script), `BIN="/new/claude-burst"`) || strings.Contains(string(script), "/old/") {
		t.Fatalf("the script still runs the old binary:\n%s", script)
	}
}

func TestAnAppThatIsNotOursIsLeftAlone(t *testing.T) {
	registered, _ := stub(t)
	home := t.TempDir()
	theirs := filepath.Join(Path(home), "Contents", "Info.plist")
	if err := os.MkdirAll(filepath.Dir(theirs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(theirs, []byte("<plist>somebody else's</plist>"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Install(home, "/bin/claude-burst", "1.0.0"); err == nil {
		t.Fatal("Install wrote over an app it did not make")
	}
	if removed, err := Remove(home); err != nil || removed {
		t.Fatalf("Remove = %v, %v: it deleted an app it did not make", removed, err)
	}
	if b, _ := os.ReadFile(theirs); string(b) != "<plist>somebody else's</plist>" {
		t.Fatalf("their Info.plist was changed: %s", b)
	}
	if len(*registered) != 0 {
		t.Fatalf("Launch Services was told %v about an app that was not written", *registered)
	}
}

func TestRemoveDeletesOurApp(t *testing.T) {
	stub(t)
	home := t.TempDir()
	if removed, err := Remove(home); err != nil || removed {
		t.Fatalf("Remove with no app = %v, %v", removed, err)
	}
	app, err := Install(home, "/bin/claude-burst", "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if removed, err := Remove(home); err != nil || !removed {
		t.Fatalf("Remove = %v, %v", removed, err)
	}
	if _, err := os.Stat(app); !os.IsNotExist(err) {
		t.Fatalf("the app is still there: %v", err)
	}
}

func TestOpenGoesToTheConsoleWhenTheDashboardIsDown(t *testing.T) {
	_, opened := stub(t)
	up := func() *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	}
	dashboard, console := up(), up()
	defer console.Close()

	got, err := Open(dashboard.URL+"/", console.URL+"/")
	if err != nil || got != dashboard.URL+"/" {
		t.Fatalf("with both up, Open = %q, %v: want the dashboard", got, err)
	}

	dashboard.Close()
	got, err = Open(dashboard.URL+"/", console.URL+"/")
	if err != nil || got != console.URL+"/" {
		t.Fatalf("with the dashboard down, Open = %q, %v: want the console", got, err)
	}

	console.Close()
	if got, err = Open(dashboard.URL+"/", console.URL+"/"); err == nil {
		t.Fatalf("with both down, Open = %q and no error", got)
	}
	if want := []string{dashboard.URL + "/", console.URL + "/"}; strings.Join(*opened, " ") != strings.Join(want, " ") {
		t.Fatalf("the browser was sent to %v, want %v", *opened, want)
	}
}

func TestURLReachesAWildcardListenerOnLoopback(t *testing.T) {
	for listen, want := range map[string]string{
		"127.0.0.1:7788": "http://127.0.0.1:7788/",
		"0.0.0.0:7788":   "http://127.0.0.1:7788/",
		":7788":          "http://127.0.0.1:7788/",
	} {
		if got, err := URL(listen); err != nil || got != want {
			t.Errorf("URL(%q) = %q, %v: want %q", listen, got, err, want)
		}
	}
	if _, err := URL("7788"); err == nil {
		t.Error("URL took an address with no port")
	}
}
