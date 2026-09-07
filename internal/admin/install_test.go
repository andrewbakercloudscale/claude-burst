package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
)

// newInstallServer builds an admin server whose rootHelper points into a
// fake scripts/ directory, since installScript refuses to generate a script
// full of paths that do not exist.
func newInstallServer(t *testing.T) (*Server, string) {
	t.Helper()
	s := newTestServer(t)
	scripts := t.TempDir()
	for _, name := range []string{"transparent-root.sh", "install-proxy.sh", "backup-config.sh", "health-diagnostics.sh"} {
		if err := os.WriteFile(filepath.Join(scripts, name), []byte("#!/bin/zsh\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	s.rootHelper = filepath.Join(scripts, "transparent-root.sh")
	return s, scripts
}

// TestInstallScriptOrdering is the test this whole feature exists for.
//
// `claude-burst enable` rewrites settings.json the instant it runs. If the
// gateway is not already listening, the next Claude Code request -- from a
// session in progress, not just the next one someone starts -- gets
// connection refused; that happened on 2026-08-30 and had to be undone by
// hand-editing settings.json. So the health wait must come BEFORE enable,
// and a button that quietly reordered those would reintroduce exactly that
// failure with no other signal.
func TestInstallScriptOrdering(t *testing.T) {
	s, _ := newInstallServer(t)
	script, err := s.installScript("base-url", config.Default())
	if err != nil {
		t.Fatal(err)
	}
	wait := strings.Index(script, "wait_healthy")
	// The call, not the function definition at the top of the script.
	call := strings.Index(script, "if ! wait_healthy")
	enable := strings.Index(script, `"$BIN" enable`)
	backup := strings.Index(script, "backup-config.sh")
	if wait < 0 || call < 0 || enable < 0 || backup < 0 {
		t.Fatalf("script is missing a required step:\n%s", script)
	}
	if !(backup < call && call < enable) {
		t.Errorf("wrong order: backup at %d, health wait at %d, enable at %d -- enable must come last\n%s",
			backup, call, enable, script)
	}
}

// TestInstallScriptTransparentDelegatesAndPrimesSudo checks the two things
// that distinguish transparent mode: it must ask for the password while a
// human is present (install-proxy.sh bails out rather than hanging on a
// prompt when credentials aren't cached), and it must delegate the ordering
// to install-proxy.sh rather than carrying a second copy of it.
func TestInstallScriptTransparentDelegatesAndPrimesSudo(t *testing.T) {
	s, scripts := newInstallServer(t)
	script, err := s.installScript("transparent", config.Default())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(script, "sudo -v") {
		t.Errorf("transparent install never primes sudo credentials:\n%s", script)
	}
	// The exec line specifically, not the comment above it that also names
	// the script -- the ordering assertion below depends on finding the
	// place install-proxy.sh actually runs.
	run := strings.Index(script, `"$SCRIPTS/install-proxy.sh"`+"\n")
	if run < 0 {
		t.Fatalf("transparent install does not delegate to install-proxy.sh:\n%s", script)
	}
	if strings.Index(script, "sudo -v") > run {
		t.Error("sudo is primed after install-proxy.sh runs, which is too late to help")
	}
	// exec would replace the shell, so everything after it -- including the
	// pause that keeps the window open -- would never run, and the output of
	// a failed install would vanish with the window.
	if strings.Contains(script, "exec \"$SCRIPTS/install-proxy.sh\"") {
		t.Error("install-proxy.sh is exec'd, which discards the rest of the script")
	}
	if !strings.Contains(script, "Press any key") {
		t.Error("the window is not held open at the end")
	}
	if !strings.Contains(script, scripts) {
		t.Errorf("script does not reference the resolved scripts dir %q", scripts)
	}
}

// TestInstallRejectsUnknownMode: the mode is the only caller-supplied value
// that reaches script generation, so it is matched against a fixed set
// rather than interpolated. A mode that fell through to the script would be
// shell injection into a file that then runs with sudo primed.
func TestInstallRejectsUnknownMode(t *testing.T) {
	s, _ := newInstallServer(t)
	for _, mode := range []string{"", "wat", "base-url; rm -rf /", "transparent\nrm -rf /"} {
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/install",
			strings.NewReader(`{"mode":`+strconv.Quote(mode)+`}`))
		req.Host = "127.0.0.1"
		req.Header.Set(mutationHeader, "1")
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("mode %q: got %d, want 400", mode, rr.Code)
		}
	}
}

// TestInstallWritesScriptAndLaunches checks the handler end to end with the
// GUI launch stubbed: a real one opens a Terminal window, which a test can
// neither do nor assert on.
func TestInstallWritesScriptAndLaunches(t *testing.T) {
	s, _ := newInstallServer(t)
	var launched string
	orig := launchTerminal
	launchTerminal = func(p string) error { launched = p; return nil }
	t.Cleanup(func() { launchTerminal = orig })

	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/install", strings.NewReader(`{"mode":"base-url"}`))
	req.Host = "127.0.0.1"
	req.Header.Set(mutationHeader, "1")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rr.Code, rr.Body.String())
	}
	var resp installResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Script == "" || resp.Script != launched {
		t.Fatalf("response script %q != launched %q", resp.Script, launched)
	}
	st, err := os.Stat(resp.Script)
	if err != nil {
		t.Fatal(err)
	}
	// `open -a Terminal` opens a non-executable file as a document instead
	// of running it, so the mode is load-bearing, not hygiene.
	if st.Mode().Perm()&0o100 == 0 {
		t.Errorf("script is not executable (%v) -- Terminal will open it as a document", st.Mode().Perm())
	}
	if resp.NeedsSudo {
		t.Error("base-url mode reported as needing sudo; it changes nothing machine-wide")
	}
}

// TestInstallReportsScriptPathWhenTerminalFails: the script is written
// before the window is opened, so a launch failure still leaves something
// runnable. Reporting only "could not open Terminal" would read as "nothing
// happened" and send the user looking for a command that already exists.
func TestInstallReportsScriptPathWhenTerminalFails(t *testing.T) {
	s, _ := newInstallServer(t)
	orig := launchTerminal
	launchTerminal = func(string) error { return os.ErrPermission }
	t.Cleanup(func() { launchTerminal = orig })

	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/install", strings.NewReader(`{"mode":"transparent"}`))
	req.Host = "127.0.0.1"
	req.Header.Set(mutationHeader, "1")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	var resp installResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Script == "" {
		t.Fatal("no script path reported")
	}
	if _, err := os.Stat(resp.Script); err != nil {
		t.Fatalf("script was not written: %v", err)
	}
	if !strings.Contains(resp.Detail, resp.Script) {
		t.Errorf("detail does not name the runnable script: %q", resp.Detail)
	}
}

// TestInstallScriptRefusesWithoutScriptsDir: rootHelperPath returns a
// descriptive placeholder when it finds nothing, and generating a script
// full of nonexistent paths from that would produce a Terminal window that
// fails several commands in -- after the user has typed their password.
func TestInstallScriptRefusesWithoutScriptsDir(t *testing.T) {
	s := newTestServer(t)
	s.rootHelper = "scripts/transparent-root.sh (in the claude-burst repo)"
	if _, err := s.installScript("transparent", config.Default()); err == nil {
		t.Fatal("expected a refusal when the scripts directory cannot be located")
	}
}

// TestInstallScriptBaseURLRefusesOverTransparentRedirect: switching modes
// with the machine-wide redirect still installed does not merely bypass
// burst. The redirect sends :443 to a gateway that, in base-url mode,
// serves plain HTTP and does not listen on :443 -- so api.anthropic.com
// stops working for EVERY process on this Mac, and removing the redirect
// needs a root the base-url path never asks for. The script must stop
// before touching anything.
func TestInstallScriptBaseURLRefusesOverTransparentRedirect(t *testing.T) {
	s, _ := newInstallServer(t)
	script, err := s.installScript("base-url", config.Default())
	if err != nil {
		t.Fatal(err)
	}
	guard := strings.Index(script, "/etc/hosts")
	configure := strings.Index(script, "configure --intercept-mode base-url")
	if guard < 0 {
		t.Fatalf("base-url install never checks for an existing /etc/hosts redirect:\n%s", script)
	}
	if guard > configure {
		t.Error("the redirect check runs after config.json has already been rewritten")
	}
	if !strings.Contains(script, "transparent-root.sh remove") {
		t.Error("the refusal does not name the command that clears the redirect")
	}
}
