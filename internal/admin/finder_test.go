package admin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// finderRig gives each test a temporary HOME and stubs everything that
// ignores HOME: pbs refreshes the real Services menu whatever HOME says.
type finderRig struct {
	home      string
	refreshes int
}

func newFinderRig(t *testing.T, ghostty bool) *finderRig {
	t.Helper()
	r := &finderRig{home: t.TempDir()}
	t.Setenv("HOME", r.home)
	oldR, oldG, oldT := refreshServices, ghosttyInstalled, toolOnPath
	refreshServices = func() error { r.refreshes++; return nil }
	ghosttyInstalled = func() bool { return ghostty }
	toolOnPath = func(tool string) bool { return tool == "claude" }
	t.Cleanup(func() { refreshServices, ghosttyInstalled, toolOnPath = oldR, oldG, oldT })
	return r
}

func (r *finderRig) workflow(name string) string {
	return filepath.Join(r.home, "Library", "Services", name+".workflow", "Contents", "document.wflow")
}

func (r *finderRig) launcher(name string) string {
	return filepath.Join(r.home, ".local", "bin", name)
}

func TestFinderInstallWritesBothShortcutsAndTheirLaunchers(t *testing.T) {
	r := newFinderRig(t, true)
	done, err := installFinderShortcuts(false)
	if err != nil {
		t.Fatal(err)
	}
	if r.refreshes != 1 {
		t.Fatalf("Services menu refreshed %d times, want 1 (stub not called means the real pbs was)", r.refreshes)
	}
	for _, f := range finderShortcuts {
		doc, err := os.ReadFile(r.workflow(f.Name))
		if err != nil {
			t.Fatalf("%s: %v (done: %q)", f.Name, err, done)
		}
		if !strings.Contains(string(doc), `-e "`+r.launcher(f.launcher)+`" "$folder" &amp;`) {
			t.Errorf("%s does not run its launcher, XML-escaped:\n%s", f.Name, doc)
		}
		info, _ := os.ReadFile(filepath.Join(filepath.Dir(r.workflow(f.Name)), "Info.plist"))
		if !strings.Contains(string(info), "<string>"+f.Name+"</string>") || !strings.Contains(string(info), "public.folder") {
			t.Errorf("%s Info.plist lacks its menu item or folder type:\n%s", f.Name, info)
		}
		st, err := os.Stat(r.launcher(f.launcher))
		if err != nil || st.Mode()&0o111 == 0 {
			t.Errorf("%s launcher missing or not executable: %v", f.Name, err)
		}
		if runtime.GOOS == "darwin" {
			for _, p := range []string{r.workflow(f.Name), filepath.Join(filepath.Dir(r.workflow(f.Name)), "Info.plist")} {
				if out, err := exec.Command("plutil", "-lint", p).CombinedOutput(); err != nil {
					t.Errorf("plutil rejects %s: %s", p, out)
				}
			}
		}
	}
	if joined := strings.Join(done, "\n"); !strings.Contains(joined, "opencode is not installed") {
		t.Errorf("a missing tool must be mentioned: %q", done)
	}
	v := readFinder()
	for _, s := range v.Shortcuts {
		if !s.Installed || !s.Ours || !s.Launcher {
			t.Errorf("status after install: %+v", s)
		}
	}
}

// The check the user asked for: a second Install finds both there and
// changes nothing, not even the Services menu.
func TestFinderInstallIsANoOpWhenAlreadyThere(t *testing.T) {
	r := newFinderRig(t, true)
	if _, err := installFinderShortcuts(false); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(r.workflow(finderShortcuts[0].Name))
	r.refreshes = 0
	done, err := installFinderShortcuts(false)
	if err != nil {
		t.Fatal(err)
	}
	if r.refreshes != 0 {
		t.Error("nothing changed, so the Services menu must not be refreshed")
	}
	after, _ := os.ReadFile(r.workflow(finderShortcuts[0].Name))
	if !bytes.Equal(before, after) {
		t.Error("an installed shortcut was rewritten")
	}
	for _, d := range done {
		if !strings.Contains(d, "already installed") {
			t.Errorf("want every line to say already installed, got %q", done)
		}
	}
}

// The usage panel patches the Claude launcher in place. Installing must
// keep an existing launcher byte for byte, or it silently undoes that.
func TestFinderInstallKeepsAnExistingLauncher(t *testing.T) {
	r := newFinderRig(t, true)
	patched := []byte("#!/bin/bash\n# patched by claude-panel-setup.sh\ncaffeinate -i \"$CLAUDE\" --session-id \"$PIN_SID\"\n")
	os.MkdirAll(filepath.Dir(r.launcher("ghostty-claude-launcher")), 0o755)
	if err := os.WriteFile(r.launcher("ghostty-claude-launcher"), patched, 0o755); err != nil {
		t.Fatal(err)
	}
	done, err := installFinderShortcuts(true)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(r.launcher("ghostty-claude-launcher"))
	if !bytes.Equal(got, patched) {
		t.Fatalf("existing launcher overwritten:\n%s", got)
	}
	if !strings.Contains(done[0], "kept the existing launcher") {
		t.Errorf("done = %q", done)
	}
}

// A freshly written Claude launcher has no panel patch; with the panel
// installed, say how to add it.
func TestFinderInstallSaysToReinstallThePanelForANewLauncher(t *testing.T) {
	newFinderRig(t, true)
	done, err := installFinderShortcuts(true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(done[0], "Reinstall / update on the usage panel") {
		t.Errorf("done = %q", done)
	}
}

// The embedded Claude launcher must keep the shape the panel's patch
// expects: a final line running "$CLAUDE", with its comment above it.
func TestEmbeddedClaudeLauncherMatchesThePanelPatch(t *testing.T) {
	lines := strings.Split(strings.TrimRight(string(claudeLauncher), "\n"), "\n")
	last := lines[len(lines)-1]
	if !strings.Contains(last, `"$CLAUDE"`) {
		t.Fatalf("last line %q does not run \"$CLAUDE\"; claude-panel-setup.sh could not pin --session-id", last)
	}
	for _, b := range [][]byte{claudeLauncher, opencodeLauncher} {
		if !bytes.HasPrefix(b, []byte("#!/bin/bash\n")) {
			t.Error("launcher must start with #!/bin/bash")
		}
	}
	if runtime.GOOS == "darwin" {
		for name, b := range map[string][]byte{"claude": claudeLauncher, "opencode": opencodeLauncher} {
			cmd := exec.Command("bash", "-n")
			cmd.Stdin = bytes.NewReader(b)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("%s launcher does not parse: %s", name, out)
			}
		}
	}
}

func TestFinderInstallRefusesWithoutGhostty(t *testing.T) {
	r := newFinderRig(t, false)
	if _, err := installFinderShortcuts(false); err == nil || !strings.Contains(err.Error(), "Ghostty") {
		t.Fatalf("err = %v, want a Ghostty error", err)
	}
	if _, err := os.Stat(filepath.Join(r.home, "Library", "Services")); err == nil {
		t.Fatal("nothing may be written without Ghostty")
	}
}

// Remove deletes our shortcuts, leaves the launchers, and never touches a
// shortcut of the same name that runs something else.
func TestFinderRemove(t *testing.T) {
	r := newFinderRig(t, true)
	// Someone else's "Launch OpenCode in Ghostty".
	foreign := r.workflow("Launch OpenCode in Ghostty")
	os.MkdirAll(filepath.Dir(foreign), 0o755)
	os.WriteFile(foreign, []byte("<plist>runs /somewhere/else</plist>"), 0o644)

	if _, err := installFinderShortcuts(false); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(foreign); string(got) != "<plist>runs /somewhere/else</plist>" {
		t.Fatal("install overwrote a shortcut it did not make")
	}
	r.refreshes = 0
	done, err := removeFinderShortcuts()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(filepath.Dir(r.workflow("Launch Claude Code in Ghostty")))); !os.IsNotExist(err) {
		t.Error("our Claude shortcut was not removed")
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Error("a shortcut not made here was removed")
	}
	if _, err := os.Stat(r.launcher("ghostty-claude-launcher")); err != nil {
		t.Error("the launcher must stay")
	}
	if r.refreshes != 1 {
		t.Errorf("refreshes = %d, want 1", r.refreshes)
	}
	if j := strings.Join(done, "\n"); !strings.Contains(j, "removed") || !strings.Contains(j, "left in place") {
		t.Errorf("done = %q", done)
	}
	v := readFinder()
	if last := v.Shortcuts[len(v.Shortcuts)-1]; v.Shortcuts[0].Installed || !last.Installed || last.Ours {
		t.Errorf("status after remove: %+v", v.Shortcuts)
	}
}

func TestFinderEndpoints(t *testing.T) {
	newFinderRig(t, true)
	s := newTestServer(t)
	// newTestServer sets its own HOME; point it back at the rig's stubs'
	// view by reading whatever HOME is now.
	h := s.Handler()
	do := func(method, path, body string, hdr bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "http://x"+path, strings.NewReader(body))
		req.Host = "127.0.0.1:7788"
		if hdr {
			req.Header.Set("X-Claude-Burst-Admin", "1")
			req.Header.Set("Content-Type", "application/json")
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}
	if rr := do("POST", "/api/finder-install", `{"action":"install"}`, false); rr.Code == http.StatusOK {
		t.Fatal("install without the admin header must be refused")
	}
	if rr := do("POST", "/api/finder-install", `{"action":"nuke"}`, true); rr.Code != http.StatusBadRequest {
		t.Fatalf("unknown action: %d", rr.Code)
	}
	rr := do("POST", "/api/finder-install", `{"action":"install"}`, true)
	if rr.Code != http.StatusOK {
		t.Fatalf("install: %d %s", rr.Code, rr.Body.String())
	}
	rr = do("GET", "/api/finder", "", false)
	var v finderView
	if err := json.Unmarshal(rr.Body.Bytes(), &v); err != nil || len(v.Shortcuts) != len(finderShortcuts) || !v.Shortcuts[0].Installed {
		t.Fatalf("GET /api/finder = %s (%v)", rr.Body.String(), err)
	}
}

// The buttons reflect what is there: Install disabled once everything is
// installed, labelled for a partial install, Remove disabled with nothing
// of ours to remove.
func TestFinderButtonsReflectWhatIsInstalled(t *testing.T) {
	type btn struct {
		Disabled bool   `json:"disabled"`
		Text     string `json:"textContent"`
	}
	var got map[string]map[string]btn
	sc := func(inst, ours bool) string {
		return fmt.Sprintf(`{name:"n",tool:"claude",tool_found:true,installed:%v,ours:%v}`, inst, ours)
	}
	runPageJS(t, []string{"renderFinder"}, fmt.Sprintf(`
const els = {}; const $ = id => (els[id] = els[id] || {});
let finderState; const setDot = () => {};
const finderPick = {}; const finderTicked = x => finderPick[x.key] ?? (x.installed || x.tool_found);
const run = st => { for (const k in els) delete els[k]; finderState = st; renderFinder();
  return {install: els.finderInstall, remove: els.finderRemove}; };
out({
  none: run({ghostty: true, shortcuts: [%s, %s]}),
  some: run({ghostty: true, shortcuts: [%s, %s]}),
  all: run({ghostty: true, shortcuts: [%s, %s]}),
  foreign: run({ghostty: true, shortcuts: [%s, %s]}),
  noGhostty: run({ghostty: false, shortcuts: [%s, %s]}),
});`, sc(false, false), sc(false, false), sc(true, true), sc(false, false), sc(true, true), sc(true, true),
		sc(true, false), sc(true, false), sc(false, false), sc(false, false)), &got)

	if b := got["none"]; b["install"].Disabled || !strings.Contains(b["install"].Text, "Install Finder shortcuts") || !b["remove"].Disabled {
		t.Errorf("none installed: %+v", b)
	}
	if b := got["some"]; b["install"].Disabled || !strings.Contains(b["install"].Text, "missing one (1)") || b["remove"].Disabled {
		t.Errorf("one installed: %+v", b)
	}
	// Install is never disabled: running it again only adds what is missing,
	// and without Ghostty the server answers with why it cannot.
	if b := got["all"]; b["install"].Disabled || !strings.Contains(b["install"].Text, "Installed: run again") || b["remove"].Disabled {
		t.Errorf("all installed: %+v", b)
	}
	if b := got["foreign"]; b["install"].Disabled || !b["remove"].Disabled {
		t.Errorf("installed but not ours: install can run again, nothing to remove: %+v", b)
	}
	if b := got["noGhostty"]; b["install"].Disabled {
		t.Errorf("no Ghostty: install still runs and the server says why it cannot: %+v", b)
	}
}

// Only the ticked shortcuts are installed or removed, and each generated
// launcher is a script bash accepts that runs its own tool.
func TestFinderInstallsAndRemovesOnlyTheTickedShortcuts(t *testing.T) {
	r := newFinderRig(t, true)
	if _, err := installFinderShortcuts(false, "omc", "claude-continue"); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"omc": true, "claude-continue": true}
	for _, st := range readFinder().Shortcuts {
		if st.Installed != want[st.Key] {
			t.Errorf("%s installed = %v, want %v", st.Key, st.Installed, want[st.Key])
		}
	}
	for name, ends := range map[string]string{"ghostty-omc-launcher": `caffeinate -i "$TOOL"`, "ghostty-claude-continue-launcher": `caffeinate -i "$TOOL" --continue`} {
		b, err := os.ReadFile(r.launcher(name))
		if err != nil || !strings.HasSuffix(strings.TrimSpace(string(b)), ends) {
			t.Errorf("%s must end with %q: %v\n%s", name, ends, err, b)
		}
		if out, err := exec.Command("bash", "-n", r.launcher(name)).CombinedOutput(); err != nil {
			t.Errorf("bash rejects %s: %s", name, out)
		}
	}
	if _, err := removeFinderShortcuts("omc"); err != nil {
		t.Fatal(err)
	}
	for _, st := range readFinder().Shortcuts {
		if st.Installed != (st.Key == "claude-continue") {
			t.Errorf("after removing omc alone, %s installed = %v", st.Key, st.Installed)
		}
	}

	s := newTestServer(t)
	for body, code := range map[string]int{`{"action":"install","keys":["nope"]}`: http.StatusBadRequest, `{"action":"install","keys":[]}`: http.StatusBadRequest} {
		if rr := mutate(t, s, "/api/finder-install", body); rr.Code != code {
			t.Errorf("%s: status=%d, want %d", body, rr.Code, code)
		}
	}
}

// A row starts ticked when it is installed or its tool is here, and the
// buttons count the ticked rows only.
func TestFinderTicksDecideWhatTheButtonsDo(t *testing.T) {
	var got struct {
		HTML    string   `json:"html"`
		Install string   `json:"install"`
		Keys    []string `json:"keys"`
		After   []string `json:"after"`
	}
	runPageJS(t, []string{"renderFinder"}, `
const els = {}; const $ = id => (els[id] = els[id] || {});
const setDot = () => {};
const finderPick = {};
const finderTicked = x => finderPick[x.key] ?? (x.installed || x.tool_found);
const finderKeys = () => finderState.shortcuts.filter(finderTicked).map(x => x.key);
let finderState = {ghostty: true, shortcuts: [
  {key:"claude", name:"A", tool:"claude", tool_found:true, installed:true, ours:true},
  {key:"omc", name:"B", tool:"omc", tool_found:true, installed:false},
  {key:"codex", name:"C", tool:"codex", tool_found:false, installed:false}]};
renderFinder();
const first = {html: els.finderStatus.innerHTML, install: els.finderInstall.textContent, keys: finderKeys()};
finderPick.codex = true; finderPick.claude = false; renderFinder();
out({...first, after: finderKeys()});`, &got)
	if strings.Count(got.HTML, `type="checkbox"`) != 3 || strings.Count(got.HTML, " checked") != 2 {
		t.Errorf("want three ticks, two of them on: %s", got.HTML)
	}
	if !strings.Contains(got.Install, "missing one (1)") || strings.Join(got.Keys, ",") != "claude,omc" {
		t.Errorf("install=%q keys=%v", got.Install, got.Keys)
	}
	if strings.Join(got.After, ",") != "omc,codex" {
		t.Errorf("after ticking codex and unticking claude: %v", got.After)
	}
}
