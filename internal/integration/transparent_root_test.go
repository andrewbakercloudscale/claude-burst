package integration

// scripts/transparent-root.sh, run for real under zsh against temp copies of
// /etc/hosts and /etc/pf.conf, with pfctl, curl, dscacheutil, killall and
// sleep stubbed. The real ones would rewrite this Mac's firewall and DNS.
//
// What is pinned here: the order of pfctl calls (dry run before load, load
// before the hosts switch), that a rejected or failed pfctl stops the script
// whatever it printed, and that remove gives back the exact hosts file and
// releases the pf token it took.

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
//	reject_dry   the dry run (-n -f) fails, with a syntax error
//	fail_load    the load (-f) exits 1 while printing nothing alarming
//	pf_disabled  -s info reports pf disabled
const pfctlStub = `#!/bin/sh
d="$STUB_DIR"
echo "pfctl $*" >> "$d/calls"
case "$*" in
  "-n -f "*)
    if [ -f "$d/reject_dry" ]; then echo "/etc/pf.conf:9: syntax error"; exit 1; fi
    exit 0 ;;
  "-f "*)
    if [ -f "$d/fail_load" ]; then echo "pfctl: rules loaded"; exit 1; fi
    exit 0 ;;
  "-s info")
    if [ -f "$d/pf_disabled" ]; then echo "Status: Disabled for 0 days"; else echo "Status: Enabled for 0 days"; fi
    exit 0 ;;
  "-E") echo "pf enabled"; echo "Token : 4242"; exit 0 ;;
esac
exit 0
`

// curlStub answers every health check: the gateway is up, and the real
// path's body names the overflow field the script looks for.
const curlStub = `#!/bin/sh
for a; do last="$a"; done
echo "curl $last" >> "$STUB_DIR/calls"
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
	sameCalls(t, "remove pfctl calls", only(r.calls, "pfctl "), []string{
		"pfctl -a claude-burst -F all",
		"pfctl -f <tmp>/etc/pf.conf",
		"pfctl -X 4242",
		"pfctl -s info",
	}, r.out)

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
		"pfctl -n -f <tmp>/etc/pf.conf",
		"pfctl -f <tmp>/etc/pf.conf",
		"pfctl -s info",
		"pfctl -a claude-burst -F states",
	}, r.out)
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
	sameCalls(t, "pfctl calls", only(r.calls, "pfctl "), []string{"pfctl -n -f <tmp>/etc/pf.conf"}, r.out)
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
