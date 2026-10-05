package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/notice"
)

// A fake claude for the status check: its version and plugin list come from
// files the test writes. Never the real one: that would read (and an action
// would change) this Mac's own plugins.
func fakeClaudeBin(t *testing.T, version, listJSON string) {
	t.Helper()
	dir := t.TempDir()
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.WriteFile(filepath.Join(dir, "list.json"), []byte(listJSON), 0o600))
	script := "#!/bin/sh\ncase \"$1 $2\" in\n  \"--version \") echo \"" + version + " (Claude Code)\" ;;\n  \"plugin list\") cat \"" + filepath.Join(dir, "list.json") + "\" ;;\nesac\n"
	bin := filepath.Join(dir, "claude")
	must(os.WriteFile(bin, []byte(script), 0o755))
	old := claudeBin
	claudeBin = func() string { return bin }
	t.Cleanup(func() { claudeBin = old })
}

func writeMod(t *testing.T, dir, hook string) {
	t.Helper()
	for p, body := range map[string]string{".claude-plugin/plugin.json": `{"name":"burst-session"}`, "hooks/register.js": hook, "tests/x.test.ts": "ignored"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, p), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func modStatusOf(t *testing.T, s *Server) modStatus {
	t.Helper()
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/mod-status", nil))
	var st modStatus
	if err := json.Unmarshal(rr.Body.Bytes(), &st); err != nil {
		t.Fatalf("%v: %s", err, rr.Body.String())
	}
	return st
}

func TestModStatusSaysInstalledCurrentOrOutOfDate(t *testing.T) {
	s := newTestServer(t)
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	s.rootHelper = filepath.Join(repo, "scripts", "transparent-root.sh")
	src := filepath.Join(repo, "mods", "burst-session")
	installed := t.TempDir()
	writeMod(t, src, "export function register(on) {}\n")
	writeMod(t, installed, "export function register(on) {}\n")
	list := `[{"id":"other@x","installPath":"/nowhere"},{"id":"burst-session@burst","version":"0.2.0","installPath":"` + installed + `"}]`

	fakeClaudeBin(t, "2.1.288", list)
	st := modStatusOf(t, s)
	if !st.Supported || !st.Installed || !st.Current || st.Version != "0.2.0" || st.Claude != "2.1.288" {
		t.Fatalf("current: %+v", st)
	}

	writeMod(t, src, "export function register(on) { /* edited */ }\n")
	if st := modStatusOf(t, s); !st.Installed || st.Current {
		t.Fatalf("edited source must read out of date: %+v", st)
	}

	fakeClaudeBin(t, "2.1.200", `[]`)
	if st := modStatusOf(t, s); st.Supported || st.Installed || !strings.Contains(st.Hint, "2.1.287") {
		t.Fatalf("old Claude Code: %+v", st)
	}
}

func TestModToastsOptionIsSavedAndServed(t *testing.T) {
	s := newTestServer(t)
	if !modOf(t, s, "").Toasts {
		t.Fatal("toasts must default to on")
	}
	if rr := mutate(t, s, "/api/mod-action", `{"action":"toasts","toasts":false}`); rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if modOf(t, s, "").Toasts {
		t.Fatal("/api/mod does not report the saved option")
	}
	if rr := mutate(t, s, "/api/mod-action", `{"action":"toasts","toasts":true}`); rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !modOf(t, s, "").Toasts {
		t.Fatal("turning it back on is not reported")
	}
	for _, body := range []string{`{"action":"toasts"}`, `{"action":"nuke"}`, `nope`} {
		if rr := mutate(t, s, "/api/mod-action", body); rr.Code != http.StatusBadRequest {
			t.Errorf("%s: status=%d, want 400", body, rr.Code)
		}
	}
}

// A problem stands until an event of the same kind follows it; another
// session's problem and a day-old one do not.
func TestStandingProblems(t *testing.T) {
	now := time.Now()
	ev := func(kind, sev, title, sid string, ago time.Duration) notice.Event {
		at := now.Add(-ago)
		return notice.Event{Kind: kind, Severity: sev, Title: title, Session: sid, At: at, TS: at.Unix()}
	}
	evs := []notice.Event{
		ev("network", notice.Error, "Network offline", "", 10*time.Minute),
		ev("network", notice.OK, "Network back", "", 8*time.Minute),
		ev("upstream", notice.Error, "Requests are failing", "", 5*time.Minute),
		ev("failover", notice.Warn, "On the secondary", "", 2*time.Minute),
		ev("context", notice.Warn, "for S2", "S2", time.Minute),
		ev("old", notice.Error, "from yesterday", "", 25*time.Hour),
	}
	var got []string
	for _, e := range standingProblems(evs, "S1", now) {
		got = append(got, e.Title)
	}
	if strings.Join(got, "|") != "On the secondary|Requests are failing" {
		t.Fatalf("problems = %q", got)
	}
}

// Burst upgraded from GitHub's copy leaves the checkout alone. One that
// still has the mod under its old name must not be installed from: it would
// put the old mod beside this one.
func TestModInstallRefusesACheckoutFromBeforeTheRename(t *testing.T) {
	s := newTestServer(t)
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	s.rootHelper = filepath.Join(repo, "scripts", "transparent-root.sh")
	writeMod(t, filepath.Join(repo, "mods", "burst-band"), "// old\n")
	installed := t.TempDir()
	writeMod(t, installed, "export function register(on) {}\n")
	writeMod(t, modInstalledFrom(), "export function register(on) {}\n")
	fakeClaudeBin(t, "2.1.288", `[{"id":"burst-session@burst","version":"0.8.0","installPath":"`+installed+`"}]`)

	if st := modStatusOf(t, s); !st.Installed || !st.Current {
		t.Fatalf("the mod matches what Burst installed, whatever the checkout holds: %+v", st)
	}
	rr := mutate(t, s, "/api/mod-action", `{"action":"install"}`)
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "git pull") {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

// The two options share a file: saving one must keep the other, and the
// file is what the mod reads when the gateway is down.
func TestModHandoffOptionKeepsToasts(t *testing.T) {
	s := newTestServer(t)
	read := func() modSettings { return readModSettings() }
	if !read().Handoff {
		t.Fatal("hand-off must default to on")
	}
	if rr := mutate(t, s, "/api/mod-action", `{"action":"toasts","toasts":false}`); rr.Code != http.StatusOK {
		t.Fatalf("toasts: %d %s", rr.Code, rr.Body)
	}
	if rr := mutate(t, s, "/api/mod-action", `{"action":"handoff","handoff":false}`); rr.Code != http.StatusOK {
		t.Fatalf("handoff: %d %s", rr.Code, rr.Body)
	}
	if got := read(); got.Toasts || got.Handoff {
		t.Fatalf("both off, got %+v", got)
	}
	if rr := mutate(t, s, "/api/mod-action", `{"action":"toasts","toasts":true}`); rr.Code != http.StatusOK {
		t.Fatalf("toasts: %d %s", rr.Code, rr.Body)
	}
	if got := read(); !got.Toasts || got.Handoff {
		t.Fatalf("toasts on must leave hand-off off, got %+v", got)
	}
	b, err := os.ReadFile(modSettingsPath())
	if err != nil || !strings.Contains(string(b), `"handoff": false`) {
		t.Fatalf("the mod reads this file: %s, %v", b, err)
	}
	// In the path it has costs, so it is asked for: off until turned on.
	if read().HandoffInPath {
		t.Fatal("hand-off with Burst in the path must default to off")
	}
	if rr := mutate(t, s, "/api/mod-action", `{"action":"handoff_in_path","handoff_in_path":true}`); rr.Code != http.StatusOK {
		t.Fatalf("handoff_in_path: %d %s", rr.Code, rr.Body)
	}
	if got := read(); !got.HandoffInPath || !got.Toasts || got.Handoff {
		t.Fatalf("in-path on must leave the others, got %+v", got)
	}
	if b, _ := os.ReadFile(modSettingsPath()); !strings.Contains(string(b), `"handoff_in_path": true`) {
		t.Fatalf("the mod reads this file: %s", b)
	}
	if rr := mutate(t, s, "/api/mod-action", `{"action":"handoff"}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("handoff with no value = %d, want 400", rr.Code)
	}
}
