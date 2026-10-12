package integration

// scripts/transparent-root.sh, run for real under zsh against temp copies of
// /etc/hosts and /etc/pf.conf, with pfctl, curl, dscacheutil, killall and
// sleep stubbed. The real ones would rewrite this Mac's firewall and DNS.
//
// What is pinned here: the order of pfctl calls (dry run before load, load
// before the hosts switch), that a rejected or failed pfctl stops the script
// whatever it printed, and that remove gives back the exact hosts file and
// releases the pf token it took.
//
// And which ruleset is loaded. `pfctl -f /etc/pf.conf` replaces the whole
// running main ruleset, dropping what other software added to it, so it may
// only run when the running ruleset has no reference to the anchor, or when
// loading the anchor alone did not produce a working path. These tests can
// say which pfctl commands the script issues. They cannot say what the real
// pfctl does with them: nothing here has been run against real pf.

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// pfctlStub logs its argv and answers from flag files in $STUB_DIR:
//
//	reject_dry         the dry run (-n -f) fails, with a syntax error
//	fail_load          the load (-f) exits 1 while printing nothing alarming
//	pf_disabled        -s info reports pf disabled
//	ref_live           the running main ruleset references the anchor (-s nat)
//	reject_anchor_dry  the anchor-only dry run fails
//	fail_anchor_load   the anchor-only load exits 1
//	flush_ineffective  the anchor still lists its rule after -F all
//	anchor_unreadable  -a claude-burst -s nat exits 1
//
// A load of the main ruleset leaves full_loaded behind, which is what the
// curl stub's needs_full waits for.
const pfctlStub = `#!/bin/sh
d="$STUB_DIR"
echo "pfctl $*" >> "$d/calls"
case "$*" in
  "-a claude-burst -n -f "*)
    if [ -f "$d/reject_anchor_dry" ]; then echo "claude-burst:2: syntax error"; exit 1; fi
    exit 0 ;;
  "-a claude-burst -f "*)
    if [ -f "$d/fail_anchor_load" ]; then exit 1; fi
    exit 0 ;;
  "-a claude-burst -s nat")
    if [ -f "$d/anchor_unreadable" ]; then exit 1; fi
    if [ -f "$d/flush_ineffective" ]; then echo "rdr pass on lo0 inet proto tcp from any to 127.0.0.1 port = 443 -> 127.0.0.1 port 7777"; fi
    exit 0 ;;
  "-s nat")
    echo 'rdr-anchor "com.apple/*" all'
    if [ -f "$d/ref_live" ]; then echo 'rdr-anchor "claude-burst" all'; fi
    exit 0 ;;
  "-n -f "*)
    if [ -f "$d/reject_dry" ]; then echo "/etc/pf.conf:9: syntax error"; exit 1; fi
    exit 0 ;;
  "-f "*)
    if [ -f "$d/fail_load" ]; then echo "pfctl: rules loaded"; exit 1; fi
    : > "$d/full_loaded"
    exit 0 ;;
  "-s info")
    if [ -f "$d/pf_disabled" ]; then echo "Status: Disabled for 0 days"; else echo "Status: Enabled for 0 days"; fi
    exit 0 ;;
  "-E") echo "pf enabled"; echo "Token : 4242"; exit 0 ;;
esac
exit 0
`

// curlStub answers every health check: the gateway is up, and the real
// path's body names the overflow field the script looks for. With needs_full
// the two checks that travel through the redirect (the intercepted host, and
// 127.0.0.1:443) fail until the main ruleset has been loaded: the case where
// loading the anchor alone is not enough. With redirect_dead 127.0.0.1:443
// never answers, whatever is loaded.
const curlStub = `#!/bin/sh
for a; do last="$a"; done
echo "curl $last" >> "$STUB_DIR/calls"
if [ -f "$STUB_DIR/redirect_dead" ]; then
  case "$last" in *://127.0.0.1:443/*) exit 7 ;; esac
fi
if [ -f "$STUB_DIR/needs_full" ] && [ ! -f "$STUB_DIR/full_loaded" ]; then
  case "$last" in
    https://api.anthropic.com/*|*://127.0.0.1:443/*) exit 7 ;;
  esac
fi
echo '{"status":"ok","overflow":false}'
`

const dscacheutilStub = `#!/bin/sh
echo "dscacheutil $*" >> "$STUB_DIR/calls"
[ "$1" = "-q" ] && echo "ip_address: 160.79.104.10"
exit 0
`

const loggingStub = `#!/bin/sh
echo "$(basename "$0") $*" >> "$STUB_DIR/calls"
`

// forbiddenStub marks a command the script must never reach in these runs.
const forbiddenStub = `#!/bin/sh
echo "FORBIDDEN $(basename "$0") $*" >> "$STUB_DIR/calls"
exit 1
`

const hostsOrig = "##\n# Host Database\n##\n127.0.0.1\tlocalhost\n255.255.255.255\tbroadcasthost\n::1\tlocalhost\n10.0.0.5\tmy-nas.local\n"

const pfConfOrig = "scrub-anchor \"com.apple/*\"\nnat-anchor \"com.apple/*\"\nrdr-anchor \"com.apple/*\"\ndummynet-anchor \"com.apple/*\"\nanchor \"com.apple/*\"\nload anchor \"com.apple\" from \"/etc/pf.anchors/com.apple\"\n"

type rootEnv struct {
	dir, stub        string
	hosts, pfConf    string
	anchor, stateDir string
	env              []string
	script           string
}

func newRootEnv(t *testing.T, flags ...string) *rootEnv {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("pf and /etc/hosts handling is macOS-only")
	}
	if _, err := exec.LookPath("zsh"); err != nil {
		t.Skip("zsh not available")
	}
	if os.Geteuid() == 0 {
		t.Skip("never run this as root: a missed seam would edit the real /etc")
	}
	dir := t.TempDir()
	e := &rootEnv{
		dir:      dir,
		stub:     filepath.Join(dir, "stub"),
		hosts:    filepath.Join(dir, "etc", "hosts"),
		pfConf:   filepath.Join(dir, "etc", "pf.conf"),
		anchor:   filepath.Join(dir, "etc", "pf.anchors", "claude-burst"),
		stateDir: filepath.Join(dir, "etc", "claude-burst"),
		script:   filepath.Join(repoRoot(t), "scripts", "transparent-root.sh"),
	}
	bin := filepath.Join(dir, "bin")
	for _, d := range []string{bin, e.stub, filepath.Dir(e.hosts)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	stubs := map[string]string{
		"pfctl": pfctlStub, "curl": curlStub, "dscacheutil": dscacheutilStub,
		"killall": loggingStub, "sleep": loggingStub,
	}
	for _, name := range []string{"sudo", "launchctl", "networksetup", "security", "defaults", "pmset", "ioreg"} {
		stubs[name] = forbiddenStub
	}
	for name, body := range stubs {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// The script's text edits run in python3. /usr/bin/python3 is an xcrun
	// shim that costs about a second per call under a temp HOME, so link the
	// interpreter the developer's PATH already resolves.
	if py, err := exec.LookPath("python3"); err == nil {
		if err := os.Symlink(py, filepath.Join(bin, "python3")); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range flags {
		if err := os.WriteFile(filepath.Join(e.stub, f), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(t, e.hosts, hostsOrig)
	write(t, e.pfConf, pfConfOrig)

	seams := map[string]string{
		"CLAUDE_BURST_HOSTS_FILE":     e.hosts,
		"CLAUDE_BURST_PF_CONF":        e.pfConf,
		"CLAUDE_BURST_PF_ANCHOR":      e.anchor,
		"CLAUDE_BURST_ROOT_STATE_DIR": e.stateDir,
		"CLAUDE_BURST_PFCTL":          filepath.Join(bin, "pfctl"),
		"CLAUDE_BURST_TEST_EUID":      "0",
	}
	// Every seam the script reads must be set here, or it falls back to a
	// real system path. Checked against the script itself so a new seam
	// cannot be added without this test noticing.
	src, err := os.ReadFile(e.script)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range regexp.MustCompile(`\$\{(CLAUDE_BURST_[A-Z_]+):-`).FindAllStringSubmatch(string(src), -1) {
		if _, ok := seams[m[1]]; !ok {
			t.Fatalf("transparent-root.sh reads %s, which this test does not set: it would use the real default", m[1])
		}
	}
	for k, v := range seams {
		if k != "CLAUDE_BURST_TEST_EUID" && !strings.HasPrefix(v, dir) {
			t.Fatalf("%s=%s is outside the test's temp dir", k, v)
		}
	}
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "CLAUDE_BURST_") && !strings.HasPrefix(kv, "PATH=") &&
			!strings.HasPrefix(kv, "HOME=") && !strings.HasPrefix(kv, "ZDOTDIR=") {
			env = append(env, kv)
		}
	}
	// A temp HOME and ZDOTDIR, and zsh -f below, so no startup file of the
	// developer's can put the real tools back in front of the stubs.
	env = append(env, "PATH="+bin+":/usr/bin:/bin", "STUB_DIR="+e.stub,
		"HOME="+dir, "ZDOTDIR="+dir)
	for k, v := range seams {
		env = append(env, k+"="+v)
	}
	e.env = env
	return e
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

type rootRun struct {
	calls []string // with the temp dir shown as <tmp>
	out   string
	code  int
}

func (e *rootEnv) run(t *testing.T, args ...string) rootRun {
	t.Helper()
	os.Remove(filepath.Join(e.stub, "calls"))
	cmd := exec.Command("zsh", append([]string{"-f", e.script}, args...)...)
	cmd.Env = e.env
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(e.stub, "calls"))
	var calls []string
	for _, l := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if l != "" {
			calls = append(calls, strings.ReplaceAll(l, e.dir, "<tmp>"))
		}
	}
	// Assert the stubs ran: a script that reached the real pfctl would leave
	// no pfctl line here and might still exit 0.
	if !hasPrefix(calls, "pfctl ") {
		t.Fatalf("the stub pfctl never ran: calls %q\noutput:\n%s", calls, out)
	}
	for _, c := range calls {
		if strings.HasPrefix(c, "FORBIDDEN ") {
			t.Fatalf("the script ran %q\noutput:\n%s", c, out)
		}
	}
	return rootRun{calls: calls, out: string(out), code: code}
}

func hasPrefix(calls []string, p string) bool {
	for _, c := range calls {
		if strings.HasPrefix(c, p) {
			return true
		}
	}
	return false
}

func only(calls []string, p string) []string {
	var r []string
	for _, c := range calls {
		if strings.HasPrefix(c, p) {
			r = append(r, c)
		}
	}
	return r
}

func sameCalls(t *testing.T, what string, got, want []string, out string) {
	t.Helper()
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("%s:\n got %q\nwant %q\noutput:\n%s", what, got, want, out)
	}
}

// installedState writes the files an install leaves, by running install.
func (e *rootEnv) install(t *testing.T) rootRun {
	t.Helper()
	r := e.run(t, "install")
	if r.code != 0 {
		t.Fatalf("install exit %d:\n%s", r.code, r.out)
	}
	return r
}

func TestRootScriptRefusesWithoutRoot(t *testing.T) {
	t.Parallel()
	e := newRootEnv(t)
	for i, kv := range e.env {
		if kv == "CLAUDE_BURST_TEST_EUID=0" {
			e.env[i] = "CLAUDE_BURST_TEST_EUID=501"
		}
	}
	cmd := exec.Command("zsh", "-f", e.script, "remove")
	cmd.Env = e.env
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "must run as root") {
		t.Fatalf("remove as uid 501: err %v, output %s", err, out)
	}
	if read(t, e.hosts) != hostsOrig {
		t.Fatal("refused, but the hosts file changed")
	}
}

// Install: the gateway is proven, pf.conf references the anchor and is
// loaded, the redirect is proven, and only then is /etc/hosts switched.
func TestRootInstallCallOrderAndHostsLast(t *testing.T) {
	t.Parallel()
	e := newRootEnv(t)
	r := e.install(t)
	want := []string{
		"curl https://127.0.0.1:7777/healthz",
		"dscacheutil -q host -a name api.anthropic.com",
		"pfctl -s nat", // no reference to the anchor yet: the full load is needed
		"pfctl -f <tmp>/etc/pf.conf",
		"pfctl -s info",
		"curl https://127.0.0.1:443/healthz",
		"dscacheutil -flushcache",
		"killall -HUP mDNSResponder",
	}
	sameCalls(t, "install calls", r.calls, want, r.out)
	hosts := read(t, e.hosts)
	if !strings.Contains(hosts, "\n127.0.0.1 api.anthropic.com\n") || !strings.HasPrefix(hosts, hostsOrig) {
		t.Fatalf("hosts after install:\n%s", hosts)
	}
	pf := read(t, e.pfConf)
	if !strings.Contains(pf, `rdr-anchor "claude-burst"`) || !strings.Contains(pf, `load anchor "claude-burst" from "`+e.anchor+`"`) {
		t.Fatalf("pf.conf after install:\n%s", pf)
	}
	if a := read(t, e.anchor); !strings.Contains(a, "port 443 -> 127.0.0.1 port 7777") {
		t.Fatalf("anchor:\n%s", a)
	}
	if read(t, filepath.Join(e.stateDir, "hosts.pre-install.bak")) != hostsOrig {
		t.Fatal("no backup of the original hosts file")
	}
}

// pf disabled at install: install takes a token, and remove releases that
// token rather than turning pf off for everything else on the Mac.
func TestRootRemoveRestoresHostsAndReleasesTheToken(t *testing.T) {
	t.Parallel()
	e := newRootEnv(t, "pf_disabled")
	ins := e.install(t)
	if !hasPrefix(ins.calls, "pfctl -E") {
		t.Fatalf("pf was disabled, install did not enable it: %q", ins.calls)
	}

	r := e.run(t, "remove")
	if r.code != 0 {
		t.Fatalf("remove exit %d:\n%s", r.code, r.out)
	}
	if got := read(t, e.hosts); got != hostsOrig {
		t.Fatalf("hosts not restored byte for byte:\n got %q\nwant %q", got, hostsOrig)
	}
	if got := read(t, e.pfConf); got != pfConfOrig {
		t.Fatalf("pf.conf not restored:\n got %q\nwant %q", got, pfConfOrig)
	}
	if _, err := os.Stat(e.anchor); !os.IsNotExist(err) {
		t.Fatal("anchor file left behind")
	}
	// The anchor is flushed and checked empty; the main ruleset is not
	// reloaded, so nothing other software added to it is dropped.
	sameCalls(t, "remove pfctl calls", only(r.calls, "pfctl "), []string{
		"pfctl -a claude-burst -F all",
		"pfctl -a claude-burst -s nat",
		"pfctl -X 4242",
		"pfctl -s info",
	}, r.out)
	if !strings.Contains(r.out, "the running main ruleset was left as it is") {
		t.Fatalf("remove did not say the main ruleset was left alone:\n%s", r.out)
	}

	// Idempotent: a second remove on a clean machine still unloads the
	// anchor (a half install can leave a live rule) and changes nothing.
	r = e.run(t, "remove")
	if r.code != 0 || read(t, e.hosts) != hostsOrig || !strings.Contains(r.out, "machine already clean") {
		t.Fatalf("second remove: exit %d\n%s", r.code, r.out)
	}
	if !hasPrefix(r.calls, "pfctl -a claude-burst -F all") || hasPrefix(r.calls, "pfctl -X") || hasPrefix(r.calls, "pfctl -d") {
		t.Fatalf("second remove calls %q", r.calls)
	}
}

// reload-anchor with a passing pfctl: dry run, load, enabled check, then
// this anchor's states only (never the machine-wide table).
func TestRootReloadAnchorCallOrder(t *testing.T) {
	t.Parallel()
	e := newRootEnv(t)
	e.install(t)
	r := e.run(t, "reload-anchor")
	if r.code != 0 {
		t.Fatalf("reload-anchor exit %d:\n%s", r.code, r.out)
	}
	sameCalls(t, "reload-anchor pfctl calls", only(r.calls, "pfctl "), []string{
		"pfctl -s nat",
		"pfctl -n -f <tmp>/etc/pf.conf",
		"pfctl -f <tmp>/etc/pf.conf",
		"pfctl -s info",
		"pfctl -a claude-burst -F states",
	}, r.out)
	// The full reload is the costly one, so the output says it ran and why.
	if !strings.Contains(r.out, "reloading the MAIN ruleset") || !strings.Contains(r.out, `has no rdr-anchor "claude-burst" reference`) {
		t.Fatalf("the full reload was not announced with its reason:\n%s", r.out)
	}
}

// The dry run is rejected: non-zero exit, nothing loaded, and the previous
// anchor put back.
func TestRootReloadAnchorRejectedDryRunNeverLoads(t *testing.T) {
	t.Parallel()
	e := newRootEnv(t)
	e.install(t)
	write(t, e.anchor, "# the anchor that was working\n")
	if err := os.WriteFile(filepath.Join(e.stub, "reject_dry"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	r := e.run(t, "reload-anchor")
	if r.code == 0 {
		t.Fatalf("a rejected dry run must fail the script:\n%s", r.out)
	}
	sameCalls(t, "pfctl calls", only(r.calls, "pfctl "), []string{"pfctl -s nat", "pfctl -n -f <tmp>/etc/pf.conf"}, r.out)
	if got := read(t, e.anchor); got != "# the anchor that was working\n" {
		t.Fatalf("previous anchor not restored:\n%s", got)
	}
	if !strings.Contains(r.out, "pf rejected the ruleset") {
		t.Fatalf("output:\n%s", r.out)
	}
}

// The load exits 1 after a clean dry run while printing something that reads
// like success: the exit status decides, not the text (the pipefail trap the
// script documents).
func TestRootReloadAnchorFailedLoadFailsWhateverItPrints(t *testing.T) {
	t.Parallel()
	e := newRootEnv(t)
	e.install(t)
	write(t, e.anchor, "# the anchor that was working\n")
	if err := os.WriteFile(filepath.Join(e.stub, "fail_load"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	r := e.run(t, "reload-anchor")
	if r.code == 0 {
		t.Fatalf("pfctl -f exited 1: the script must too:\n%s", r.out)
	}
	sameCalls(t, "pfctl calls", only(r.calls, "pfctl "), []string{
		"pfctl -s nat",
		"pfctl -n -f <tmp>/etc/pf.conf",
		"pfctl -f <tmp>/etc/pf.conf",
		"pfctl -f <tmp>/etc/pf.conf", // reloading the restored anchor
	}, r.out)
	if got := read(t, e.anchor); got != "# the anchor that was working\n" {
		t.Fatalf("previous anchor not restored:\n%s", got)
	}
	if hasPrefix(r.calls, "pfctl -a claude-burst -F states") {
		t.Fatal("went on to flush states after a failed load")
	}
}

func (e *rootEnv) flag(t *testing.T, names ...string) {
	t.Helper()
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(e.stub, n), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// loadsMainRuleset reports whether any call loaded (or dry-ran) /etc/pf.conf.
func loadsMainRuleset(calls []string) bool {
	for _, c := range calls {
		if strings.HasPrefix(c, "pfctl -f ") || strings.HasPrefix(c, "pfctl -n -f ") {
			return true
		}
	}
	return false
}

const (
	anchorDry  = "pfctl -a claude-burst -n -f <tmp>/etc/pf.anchors/claude-burst"
	anchorLoad = "pfctl -a claude-burst -f <tmp>/etc/pf.anchors/claude-burst"
)

// The running main ruleset already references the anchor: the anchor alone is
// dry-run and loaded, and /etc/pf.conf is never loaded, so whatever other
// software added to the running ruleset stays.
func TestRootReloadAnchorLoadsOnlyTheAnchorWhenReferenced(t *testing.T) {
	t.Parallel()
	e := newRootEnv(t)
	e.install(t)
	e.flag(t, "ref_live")
	r := e.run(t, "reload-anchor")
	if r.code != 0 {
		t.Fatalf("reload-anchor exit %d:\n%s", r.code, r.out)
	}
	sameCalls(t, "reload-anchor pfctl calls", only(r.calls, "pfctl "), []string{
		"pfctl -s nat",
		anchorDry,
		anchorLoad,
		"pfctl -s info",
		"pfctl -a claude-burst -F states",
	}, r.out)
	if loadsMainRuleset(r.calls) {
		t.Fatalf("the main ruleset was reloaded with the reference already live: %q", r.calls)
	}
	if !strings.Contains(r.out, "the main ruleset was not reloaded") {
		t.Fatalf("output does not say the main ruleset was left alone:\n%s", r.out)
	}
}

// The anchor-only reload goes in but the real path still does not answer:
// the full reload is tried before giving up, so the narrow path can never do
// worse than the reload it replaced.
func TestRootReloadAnchorFallsBackToTheMainRulesetWhenThePathStaysBroken(t *testing.T) {
	t.Parallel()
	e := newRootEnv(t)
	e.install(t)
	os.Remove(filepath.Join(e.stub, "full_loaded")) // left by install's own load
	e.flag(t, "ref_live", "needs_full")
	r := e.run(t, "reload-anchor")
	if r.code != 0 {
		t.Fatalf("reload-anchor exit %d:\n%s", r.code, r.out)
	}
	sameCalls(t, "reload-anchor pfctl calls", only(r.calls, "pfctl "), []string{
		"pfctl -s nat",
		anchorDry,
		anchorLoad,
		"pfctl -s info",
		"pfctl -a claude-burst -F states",
		"pfctl -n -f <tmp>/etc/pf.conf",
		"pfctl -f <tmp>/etc/pf.conf",
		"pfctl -s info",
		"pfctl -a claude-burst -F states",
	}, r.out)
	if !strings.Contains(r.out, "the anchor-only reload did not bring the real path back") {
		t.Fatalf("the fallback did not say why it ran:\n%s", r.out)
	}
}

// The anchor-only step itself fails, at the dry run or at the load: the full
// reload runs, as it would have before the anchor-only step existed.
func TestRootReloadAnchorFallsBackWhenTheAnchorOnlyLoadFails(t *testing.T) {
	t.Parallel()
	for flag, want := range map[string][]string{
		"reject_anchor_dry": {"pfctl -s nat", anchorDry, "pfctl -n -f <tmp>/etc/pf.conf", "pfctl -f <tmp>/etc/pf.conf", "pfctl -s info", "pfctl -a claude-burst -F states"},
		"fail_anchor_load":  {"pfctl -s nat", anchorDry, anchorLoad, "pfctl -n -f <tmp>/etc/pf.conf", "pfctl -f <tmp>/etc/pf.conf", "pfctl -s info", "pfctl -a claude-burst -F states"},
	} {
		e := newRootEnv(t)
		e.install(t)
		e.flag(t, "ref_live", flag)
		r := e.run(t, "reload-anchor")
		if r.code != 0 {
			t.Fatalf("%s: reload-anchor exit %d:\n%s", flag, r.code, r.out)
		}
		sameCalls(t, flag+": pfctl calls", only(r.calls, "pfctl "), want, r.out)
	}
}

// A reinstall over a ruleset that already references the anchor loads the
// anchor alone, and falls back to the main ruleset only when 127.0.0.1:443
// does not reach the gateway afterwards.
func TestRootInstallOverALiveReferenceLoadsOnlyTheAnchor(t *testing.T) {
	t.Parallel()
	e := newRootEnv(t, "ref_live")
	r := e.install(t)
	sameCalls(t, "install pfctl calls", only(r.calls, "pfctl "), []string{
		"pfctl -s nat", anchorDry, anchorLoad, "pfctl -s info",
	}, r.out)

	e = newRootEnv(t, "ref_live", "needs_full")
	r = e.install(t)
	sameCalls(t, "install pfctl calls when the anchor alone is not enough", only(r.calls, "pfctl "), []string{
		"pfctl -s nat", anchorDry, anchorLoad, "pfctl -s info", "pfctl -f <tmp>/etc/pf.conf",
	}, r.out)
	if !strings.Contains(read(t, e.hosts), "127.0.0.1 api.anthropic.com") {
		t.Fatalf("install did not finish after the fallback:\n%s", r.out)
	}

	// Neither load produces a working redirect: the fallback's own result is
	// checked too, and the install is undone with /etc/hosts never switched.
	e = newRootEnv(t, "ref_live", "redirect_dead")
	r = e.run(t, "install")
	if r.code == 0 || !strings.Contains(r.out, "redirect verification FAILED") {
		t.Fatalf("install must fail when 127.0.0.1:443 never reaches the gateway: exit %d\n%s", r.code, r.out)
	}
	if !hasPrefix(r.calls, "pfctl -f <tmp>/etc/pf.conf") {
		t.Fatalf("the full reload was never tried before giving up: %q", r.calls)
	}
	if read(t, e.hosts) != hostsOrig {
		t.Fatalf("hosts changed by an install that failed:\n%s", read(t, e.hosts))
	}
}

// remove checks the outcome of its flush. A rule still listed, or an anchor
// it cannot read, sends it to the full reload it used to run every time.
func TestRootRemoveReloadsTheMainRulesetWhenTheFlushDidNotEmptyTheAnchor(t *testing.T) {
	t.Parallel()
	for _, flag := range []string{"flush_ineffective", "anchor_unreadable"} {
		e := newRootEnv(t)
		e.install(t)
		e.flag(t, flag)
		r := e.run(t, "remove")
		if r.code != 0 {
			t.Fatalf("%s: remove exit %d:\n%s", flag, r.code, r.out)
		}
		sameCalls(t, flag+": remove pfctl calls", only(r.calls, "pfctl "), []string{
			"pfctl -a claude-burst -F all",
			"pfctl -a claude-burst -s nat",
			"pfctl -f <tmp>/etc/pf.conf",
			"pfctl -s info",
		}, r.out)
		if read(t, e.hosts) != hostsOrig {
			t.Fatalf("%s: hosts not restored", flag)
		}
	}
}

// What remove leaves behind. The anchor's .bak goes with the anchor. The
// copies of /etc/hosts and /etc/pf.conf taken at install stay after a plain
// remove (a rollback runs the same command) and are named; purge, which an
// uninstall should use, deletes them and the state directory once empty.
func TestRootRemoveNamesWhatItKeepsAndPurgeDeletesIt(t *testing.T) {
	t.Parallel()
	e := newRootEnv(t)
	e.install(t)
	if r := e.run(t, "reload-anchor"); r.code != 0 {
		t.Fatalf("reload-anchor exit %d:\n%s", r.code, r.out)
	}
	bak := e.anchor + ".bak"
	if _, err := os.Stat(bak); err != nil {
		t.Fatalf("precondition: reload-anchor should leave %s: %v", bak, err)
	}
	hostsBak := filepath.Join(e.stateDir, "hosts.pre-install.bak")
	pfBak := filepath.Join(e.stateDir, "pf.conf.pre-install.bak")

	r := e.run(t, "remove")
	if r.code != 0 {
		t.Fatalf("remove exit %d:\n%s", r.code, r.out)
	}
	if _, err := os.Stat(bak); !os.IsNotExist(err) {
		t.Fatalf("%s left behind by remove", bak)
	}
	for _, f := range []string{hostsBak, pfBak} {
		if _, err := os.Stat(f); err != nil {
			t.Fatalf("a plain remove must keep %s: %v", f, err)
		}
		if !strings.Contains(r.out, f) {
			t.Fatalf("remove kept %s without naming it:\n%s", f, r.out)
		}
	}

	r = e.run(t, "purge")
	if r.code != 0 {
		t.Fatalf("purge exit %d:\n%s", r.code, r.out)
	}
	if _, err := os.Stat(e.stateDir); !os.IsNotExist(err) {
		entries, _ := os.ReadDir(e.stateDir)
		t.Fatalf("purge left %s behind (%d entries):\n%s", e.stateDir, len(entries), r.out)
	}

	// Another daemon's file in the state directory survives a purge.
	e = newRootEnv(t)
	e.install(t)
	other := filepath.Join(e.stateDir, "lid-awake.state")
	write(t, other, "mode=ac\n")
	if r := e.run(t, "purge"); r.code != 0 {
		t.Fatalf("purge exit %d:\n%s", r.code, r.out)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("purge deleted a file that is not transparent mode's: %v", err)
	}
}

// As root python3 is /usr/bin/python3, which runs nothing without the Command
// Line Tools. The script says so before it opens a file, and says how to take
// the redirect out by hand.
func TestRootRemoveSaysWhatToDoWhenPythonDoesNotRun(t *testing.T) {
	t.Parallel()
	e := newRootEnv(t)
	e.install(t)
	withBlock := read(t, e.hosts)
	py := filepath.Join(e.dir, "bin", "python3")
	os.Remove(py)
	if err := os.WriteFile(py, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("zsh", "-f", e.script, "remove")
	cmd.Env = e.env
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("remove succeeded without a working python3:\n%s", out)
	}
	for _, want := range []string{"python3 does not run", "xcode-select --install", "# BEGIN claude-burst hosts"} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}
	if read(t, e.hosts) != withBlock {
		t.Fatal("the hosts file changed although nothing could be edited")
	}
}

// Every script root runs fixes its PATH and drops its seams when it really is
// root, before it does anything else. This reads the scripts: no test here
// runs as root, so the branch itself is never executed by the suite.
func TestRootScriptsFixThePathWhenRoot(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"transparent-root.sh", "pf-heal.sh", "install-pf-heal.sh"} {
		src := readFile(filepath.Join(repoRoot(t), "scripts", name))
		guard := "if [[ $EUID -eq 0 ]]; then\n  PATH=/usr/bin:/bin:/usr/sbin:/sbin\n  export PATH\n  unset CLAUDE_BURST_"
		i := strings.Index(src, guard)
		if i < 0 {
			t.Errorf("%s: no fixed PATH for a root run", name)
			continue
		}
		// Nothing but comments, blank lines, `set` and plain assignments
		// may come before it: a command run earlier is run on the caller's PATH.
		for _, line := range strings.Split(src[:i], "\n") {
			l := strings.TrimSpace(line)
			if l == "" || strings.HasPrefix(l, "#") || strings.HasPrefix(l, "set ") ||
				regexp.MustCompile(`^[A-Z_]+="\$0"$`).MatchString(l) || l == `DIR="$(cd "$(dirname "$0")" && pwd)"` {
				continue
			}
			t.Errorf("%s: %q runs before the PATH is fixed", name, l)
		}
	}
	// rollback.sh is refused as root outright, before its audit trap is set.
	src := readFile(filepath.Join(repoRoot(t), "scripts", "rollback.sh"))
	refuse, trap := strings.Index(src, "if [[ $EUID -eq 0 ]]; then\n  echo \"run this without sudo"), strings.Index(src, "\ntrap ")
	if refuse < 0 || trap < 0 || refuse > trap {
		t.Errorf("rollback.sh must refuse a root run before it sets its trap (refusal at %d, trap at %d)", refuse, trap)
	}
}
