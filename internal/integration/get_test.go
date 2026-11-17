package integration

// scripts/get.sh, the one-line install, run as the README runs it: piped
// into bash, in a throwaway HOME, against a local "GitHub" with signed
// releases.
//
// Run unguarded it would install Go with sudo, edit /etc/hosts and pf
// through install.sh, trust a CA in the System keychain and quit Claude
// Desktop. So every tool that reaches the Mac is a stub on PATH that logs
// its call, the run is refused unless bash resolves each to its stub, the
// repo's install.sh, diagnose.sh and trust helper are stubs in the local
// origin, and the prompts read a file of answers, never the terminal.

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"testing"
)

var getStubs = map[string]string{
	"uname": `#!/bin/sh
case "$1" in -m) echo arm64 ;; *) if [ -f "$STUB/os" ]; then cat "$STUB/os"; else echo Darwin; fi ;; esac
`,
	"sw_vers": `#!/bin/sh
if [ -f "$STUB/macos" ]; then cat "$STUB/macos"; else echo 26.5; fi
`,
	"xcode-select": `#!/bin/sh
echo "xcode-select $*" >> "$STUB/calls"
[ "$1" = -p ] && [ -f "$STUB/no-clt" ] && exit 2
exit 0
`,
	// Never root: -v and -n true answer yes, and a command is run as this
	// user. Every command it can be handed here is itself a stub.
	"sudo": `#!/bin/sh
echo "sudo $*" >> "$STUB/calls"
[ "$1" = -v ] && exit 0
[ "$1" = -n ] && shift
[ "$*" = true ] && exit 0
exec "$@"
`,
	"installer": `#!/bin/sh
echo "installer $*" >> "$STUB/calls"
mkdir -p "$GOPKG" && printf '#!/bin/sh\necho "go version go1.25.1 darwin/arm64"\n' > "$GOPKG/go" && chmod 755 "$GOPKG/go"
`,
	// go.dev's list and the installer it names come from files the test wrote.
	"curl": `#!/bin/sh
echo "curl $*" >> "$STUB/calls"
out=""; url=""
while [ $# -gt 0 ]; do case "$1" in -o) out="$2"; shift ;; http*) url="$1" ;; esac; shift; done
case "$url" in
  *mode=json*) cp "$STUB/go.json" "$out" ;;
  */dl/go*.pkg) cp "$STUB/go.pkg" "$out" ;;
  *) exit 22 ;;
esac
`,
	"osascript": `#!/bin/sh
echo "osascript $*" >> "$STUB/calls"
case "$*" in *quit*) rm -f "$STUB/desktop-running" ;; esac
`,
	"open":     "#!/bin/sh\necho \"open $*\" >> \"$STUB/calls\"\n",
	"pgrep":    "#!/bin/sh\necho \"pgrep $*\" >> \"$STUB/calls\"\n[ -f \"$STUB/desktop-running\" ]\n",
	"security": "#!/bin/sh\necho \"security $*\" >> \"$STUB/calls\"\n[ -f \"$STUB/ca-trusted\" ] || exit 44\n",
}

// The origin's install.sh: says what it was run with, and leaves behind
// what a real install would (config, CA, binary, the notifier helper).
const getInstallStub = `#!/bin/zsh
echo "install.sh mode=$CLAUDE_BURST_MODE force=$CLAUDE_BURST_FORCE signing=$CLAUDE_BURST_SIGNING from ${0:A:h} at $(<${0:A:h}/VERSION)" >> "$STUB/calls"
[[ -f "$STUB/install-fails" ]] && { echo "install.sh: broke"; exit 1; }
cfg=~/.config/claude-burst
mkdir -p $cfg/ca ~/.local/bin
mode=$CLAUDE_BURST_MODE
[[ -z $mode && -f $cfg/config.json ]] && mode=$(/usr/bin/python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["intercept"]["mode"])' $cfg/config.json)
echo "{\"intercept\":{\"mode\":\"${mode:-base-url}\"}}" > $cfg/config.json
printf -- '-----BEGIN CERTIFICATE-----\nQUJDREVG\n-----END CERTIFICATE-----\n' > $cfg/ca/ca-cert.pem
printf '#!/bin/sh\necho "claude-burst $*" >> "$STUB/calls"\n[ "$1" = version ] && echo "claude-burst $(cat %s/VERSION)"\nexit 0\n' ${0:A:h} > ~/.local/bin/claude-burst
chmod 755 ~/.local/bin/claude-burst
n="$HOME/Applications/Claude Burst.app/Contents/Helpers/Claude Burst Notifier.app/Contents/MacOS"
mkdir -p $n
printf '#!/bin/sh\necho "{\\"status\\":\\"%s\\"}"\n' "$(cat $STUB/notify 2>/dev/null || echo authorized)" > $n/claude-burst-notifier
chmod 755 $n/claude-burst-notifier
echo "installed"
`

type getRig struct {
	t                     *testing.T
	base, home, stub, bin string
	origin, gopkg, apps   string
	key                   releaseKey
	env                   []string
}

func newGetRig(t *testing.T) *getRig {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("get.sh is macOS-only")
	}
	if os.Geteuid() == 0 {
		t.Skip("get.sh refuses to run as root")
	}
	// The real path: get.sh and install.sh print it, and TempDir's is a
	// symlink on macOS (/var is /private/var).
	base, err := filepath.EvalSymlinks(t.TempDir())
	must(t, err)
	r := &getRig{
		t: t, base: base,
		home:   filepath.Join(base, "home"),
		stub:   filepath.Join(base, "stub"),
		bin:    filepath.Join(base, "bin"),
		origin: filepath.Join(base, "origin"),
		gopkg:  filepath.Join(base, "gopkg"),
		apps:   filepath.Join(base, "apps"),
	}
	for _, d := range []string{r.home, r.stub, r.bin, r.apps, filepath.Join(r.origin, "scripts")} {
		must(t, os.MkdirAll(d, 0o755))
	}
	for name, body := range getStubs {
		must(t, os.WriteFile(filepath.Join(r.bin, name), []byte(body), 0o755))
	}
	r.stubFile("ca-trusted", "")
	r.goInstalled("1.25.1")

	// GitHub: the real verifier and audit script, stub installer, diagnose
	// and trust helper, and a go.mod asking for Go 1.24.
	r.git(r.origin, "init", "-q", "-b", "main")
	b, err := os.ReadFile(filepath.Join(repoRoot(t), "scripts", "audit-add.sh"))
	must(t, err)
	r.write("scripts/audit-add.sh", string(b), 0o755)
	r.write("install.sh", getInstallStub, 0o755)
	r.write("go.mod", "module example\n\ngo 1.24\n\ntoolchain go1.27.2\n", 0o644)
	r.write("scripts/diagnose.sh", "#!/bin/bash\necho \"PASS the gateway answers\"\nif [ -f \"$STUB/diagnose-fails\" ]; then echo \"FAIL something\"; echo \"1 checks, 1 failed\"; exit 1; fi\necho \"1 checks, 0 failed\"\n", 0o755)
	r.write("scripts/trust-ca-systemwide.sh", "#!/bin/sh\necho \"trust-ca-systemwide.sh\" >> \"$STUB/calls\"\n[ -f \"$STUB/trust-works\" ] && : > \"$STUB/ca-trusted\"\nexit 0\n", 0o755)
	r.key = newReleaseKey(t)
	r.key.installVerifier(t, filepath.Join(r.origin, "scripts"))
	r.release("v1")

	// Built from nothing, so none of the developer's environment (their Go,
	// Homebrew, CLAUDE_BURST_* settings) reaches the script.
	r.env = []string{
		"HOME=" + r.home,
		// Python's byte-code cache would land in HOME/Library/Caches and
		// read as a change in the dry run.
		"PYTHONDONTWRITEBYTECODE=1",
		"PATH=" + r.bin + ":/usr/bin:/bin",
		"STUB=" + r.stub,
		"GOPKG=" + r.gopkg,
		"CLAUDE_BURST_GIT_URL=" + r.origin,
		"CLAUDE_BURST_TTY=" + filepath.Join(r.stub, "tty"),
		"CLAUDE_BURST_GET_EXTRA_PATH=",
		"CLAUDE_BURST_GO_PKG_BIN=" + r.gopkg,
		"CLAUDE_BURST_APPLICATIONS_DIR=" + r.apps,
		"CLAUDE_BURST_AUDIT_FILE=" + filepath.Join(r.base, "audit.jsonl"),
	}
	r.answer("")
	return r
}

func (r *getRig) git(dir string, args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (r *getRig) write(rel, body string, mode os.FileMode) {
	r.t.Helper()
	p := filepath.Join(r.origin, rel)
	must(r.t, os.MkdirAll(filepath.Dir(p), 0o755))
	must(r.t, os.WriteFile(p, []byte(body), mode))
}

// release makes version the newest signed release on the origin's main.
func (r *getRig) release(version string) {
	r.t.Helper()
	r.write("VERSION", version+"\n", 0o644)
	r.git(r.origin, "add", "-A")
	r.git(r.origin, "commit", "-q", "-m", version)
	r.key.sign(r.t, r.origin, version)
}

func (r *getRig) stubFile(name, body string) {
	r.t.Helper()
	must(r.t, os.WriteFile(filepath.Join(r.stub, name), []byte(body), 0o644))
}

// answer is what the person types at get.sh's one question.
func (r *getRig) answer(a string) { r.stubFile("tty", a+"\n") }

func (r *getRig) goInstalled(version string) {
	r.t.Helper()
	must(r.t, os.WriteFile(filepath.Join(r.bin, "go"), []byte("#!/bin/sh\necho \"go version go"+version+" darwin/arm64\"\n"), 0o755))
}

func (r *getRig) noGo() { must(r.t, os.Remove(filepath.Join(r.bin, "go"))) }

// withBrew adds a Homebrew whose `brew install go` puts a Go in its prefix,
// whose bin get.sh adds to the PATH as it does /opt/homebrew/bin.
func (r *getRig) withBrew() {
	r.t.Helper()
	prefix := filepath.Join(r.base, "brew")
	r.env = append(r.env, "CLAUDE_BURST_GET_EXTRA_PATH="+filepath.Join(prefix, "bin"))
	must(r.t, os.WriteFile(filepath.Join(r.bin, "brew"), []byte(`#!/bin/sh
echo "brew $*" >> "$STUB/calls"
case "$1" in
  --prefix) echo "`+prefix+`" ;;
  list) exit 1 ;;
  install|upgrade) mkdir -p "`+prefix+`/bin" && printf '#!/bin/sh\necho "go version go1.27.2 darwin/arm64"\n' > "`+prefix+`/bin/go" && chmod 755 "`+prefix+`/bin/go" ;;
esac
`), 0o755))
}

// clone puts a checkout of the origin at dir, as an earlier install would.
func (r *getRig) clone(dir string) {
	r.t.Helper()
	r.git(r.base, "clone", "-q", r.origin, dir)
}

func (r *getRig) desktopOpen() {
	r.t.Helper()
	must(r.t, os.MkdirAll(filepath.Join(r.apps, "Claude.app", "Contents"), 0o755))
	r.stubFile("desktop-running", "")
}

// run pipes get.sh into bash, as the README's line does.
func (r *getRig) run(args ...string) (out, calls string, code int) {
	t := r.t
	t.Helper()
	// Refuse to start unless bash, in this environment, finds every tool
	// that reaches the Mac in the stub directory, and finds no Homebrew or
	// Go but the test's own.
	names := make([]string, 0, len(getStubs))
	for n := range getStubs {
		names = append(names, n)
	}
	check := exec.Command("/bin/bash", "-c", "for n in "+strings.Join(names, " ")+" brew go; do command -v $n || echo none:$n; done")
	check.Env = r.env
	resolved, err := check.Output()
	must(t, err)
	for _, l := range strings.Fields(string(resolved)) {
		if strings.HasPrefix(l, "none:") {
			if n := strings.TrimPrefix(l, "none:"); n != "brew" && n != "go" {
				t.Fatalf("%s did not resolve at all", n)
			}
			continue
		}
		if filepath.Dir(l) != r.bin {
			t.Fatalf("%s is not a stub: get.sh would run the real one", l)
		}
	}

	script, err := os.ReadFile(filepath.Join(repoRoot(t), "scripts", "get.sh"))
	must(t, err)
	cmd := exec.Command("/bin/bash", append([]string{"-s", "--"}, args...)...)
	cmd.Stdin = strings.NewReader(string(script))
	cmd.Dir = r.home
	cmd.Env = r.env
	// No controlling terminal: /dev/tty cannot be opened, so a prompt that
	// missed CLAUDE_BURST_TTY fails instead of waiting on the developer.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	b, err := cmd.CombinedOutput()
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return string(b), readFile(filepath.Join(r.stub, "calls")), code
}

func wantIn(t *testing.T, what, got string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("%s lacks %q:\n%s", what, w, got)
		}
	}
}

func wantNotIn(t *testing.T, what, got string, nots ...string) {
	t.Helper()
	for _, n := range nots {
		if strings.Contains(got, n) {
			t.Errorf("%s has %q:\n%s", what, n, got)
		}
	}
}

func TestGetStopsOnAMacItCannotRunOn(t *testing.T) {
	for name, set := range map[string]func(r *getRig){
		"linux":     func(r *getRig) { r.stubFile("os", "Linux\n") },
		"old macOS": func(r *getRig) { r.stubFile("macos", "13.6\n") },
	} {
		t.Run(name, func(t *testing.T) {
			r := newGetRig(t)
			set(r)
			out, calls, code := r.run()
			if code == 0 {
				t.Fatalf("want a stop:\n%s", out)
			}
			wantIn(t, "output", out, "Nothing was changed")
			wantNotIn(t, "output", out, "SETUP DID NOT FINISH")
			wantNotIn(t, "calls", calls, "sudo", "install.sh")
			if _, err := os.Stat(filepath.Join(r.home, "claude-burst")); err == nil {
				t.Error("cloned on a Mac it stopped on")
			}
		})
	}
}

// A fresh Mac with Homebrew and no Go: brew installs Go, and nothing goes
// near Go's own installer. The clone lands on the newest signed release,
// not on main beyond it. A rerun reuses it and asks nothing new.
func TestGetFreshMacInstallsGoWithHomebrewAndRerunsCleanly(t *testing.T) {
	r := newGetRig(t)
	r.noGo()
	r.withBrew()
	r.release("v2")
	r.write("VERSION", "unreleased\n", 0o644)
	r.git(r.origin, "commit", "-qam", "unreleased")

	out, calls, code := r.run()
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	wantIn(t, "calls", calls, "brew install go", "install.sh mode=transparent force=1 signing=no from "+filepath.Join(r.home, "claude-burst")+" at v2")
	wantNotIn(t, "calls", calls, "installer -pkg", "curl")
	wantIn(t, "output", out, "on release v2", "DONE: Claude Burst is installed and working", "./install.sh uninstall", "burst-off")
	if n := strings.Count(calls, "sudo -v"); n != 1 {
		t.Errorf("want the password asked for once, got %d:\n%s", n, calls)
	}

	must(t, os.Remove(filepath.Join(r.stub, "calls")))
	out, calls, code = r.run()
	if code != 0 {
		t.Fatalf("rerun exit %d:\n%s", code, out)
	}
	wantIn(t, "rerun output", out, "Go 1.27.2: fine", "DONE")
	wantNotIn(t, "rerun output", out, "cloning")
	wantNotIn(t, "rerun calls", calls, "brew install")
	// The mode the first run chose is kept by install.sh, not forced again.
	wantIn(t, "rerun calls", calls, "install.sh mode= force=1")
}

// No Homebrew: Go's installer from go.dev, installed only when its SHA256
// matches go.dev's list.
func TestGetInstallsGoFromGoDevOnlyWhenTheSHAMatches(t *testing.T) {
	pkg := []byte("pretend this is Go's installer\n")
	sum := sha256.Sum256(pkg)
	goJSON := func(sha string) string {
		return `[{"version":"go1.99.0","stable":false,"files":[{"filename":"go1.99.0.darwin-arm64.pkg","os":"darwin","arch":"arm64","kind":"installer","sha256":"` + strings.Repeat("0", 64) + `"}]},
{"version":"go1.25.1","stable":true,"files":[
 {"filename":"go1.25.1.darwin-amd64.pkg","os":"darwin","arch":"amd64","kind":"installer","sha256":"` + strings.Repeat("1", 64) + `"},
 {"filename":"go1.25.1.darwin-arm64.tar.gz","os":"darwin","arch":"arm64","kind":"archive","sha256":"` + strings.Repeat("2", 64) + `"},
 {"filename":"go1.25.1.darwin-arm64.pkg","os":"darwin","arch":"arm64","kind":"installer","sha256":"` + sha + `"}]}]`
	}

	t.Run("matches", func(t *testing.T) {
		r := newGetRig(t)
		r.noGo()
		r.stubFile("go.json", goJSON(hex.EncodeToString(sum[:])))
		r.stubFile("go.pkg", string(pkg))
		out, calls, code := r.run()
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, out)
		}
		wantIn(t, "calls", calls, "curl -fsSL --max-time 600 https://go.dev/dl/go1.25.1.darwin-arm64.pkg", "sudo -n installer -pkg ", "go1.25.1.darwin-arm64.pkg -target /", "install.sh mode=transparent")
		wantIn(t, "output", out, "SHA256 matches", "Go 1.25.1 installed", "to install Go")
		if n := strings.Count(calls, "sudo -v"); n != 1 {
			t.Errorf("want the password asked for once, got %d:\n%s", n, calls)
		}
	})

	t.Run("does not match", func(t *testing.T) {
		r := newGetRig(t)
		r.noGo()
		r.stubFile("go.json", goJSON(strings.Repeat("a", 64)))
		r.stubFile("go.pkg", string(pkg))
		out, calls, code := r.run()
		if code == 0 {
			t.Fatalf("want a failure:\n%s", out)
		}
		wantNotIn(t, "calls", calls, "installer -pkg", "install.sh")
		wantIn(t, "output", out, "does not match go.dev's", "SETUP DID NOT FINISH", "When asking for help, paste this line:",
			"claude-burst get.sh failed while installing Go from go.dev, exit 1, macOS 26.5 arm64")
	})
}

// The checkout install.sh recorded is used wherever it is, and brought up
// to the newest release; no second clone appears in ~/claude-burst.
func TestGetReusesTheRecordedCheckout(t *testing.T) {
	r := newGetRig(t)
	elsewhere := filepath.Join(r.base, "projects", "claude-burst")
	r.clone(elsewhere)
	share := filepath.Join(r.home, ".local", "share", "claude-burst")
	must(t, os.MkdirAll(share, 0o755))
	must(t, os.WriteFile(filepath.Join(share, "repo"), []byte(elsewhere+"\n"), 0o644))
	r.release("v2")

	out, calls, code := r.run()
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	wantIn(t, "calls", calls, "from "+elsewhere+" at v2")
	wantIn(t, "output", out, "on release v2")
	if _, err := os.Stat(filepath.Join(r.home, "claude-burst")); err == nil {
		t.Error("cloned again although a checkout was recorded")
	}
}

// Someone's edits in the checkout are theirs: not updated, not touched.
func TestGetLeavesLocalEditsAlone(t *testing.T) {
	r := newGetRig(t)
	co := filepath.Join(r.home, "claude-burst")
	r.clone(co)
	r.release("v2")
	edited := filepath.Join(co, "go.mod")
	must(t, os.WriteFile(edited, []byte("module example\n\ngo 1.24\n// mine\n"), 0o644))

	out, calls, code := r.run()
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	wantIn(t, "output", out, "local edits: left exactly as it is")
	wantIn(t, "calls", calls, "from "+co+" at v1")
	if !strings.Contains(readFile(edited), "// mine") {
		t.Error("the local edit was lost")
	}
}

// Claude Desktop open and not yet trusting Burst: one question, and only a
// yes quits and reopens it.
func TestGetAsksBeforeRestartingClaudeDesktop(t *testing.T) {
	t.Run("yes", func(t *testing.T) {
		r := newGetRig(t)
		r.desktopOpen()
		r.answer("y")
		out, calls, code := r.run()
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, out)
		}
		wantIn(t, "output", out, "Claude Desktop is open", "Quit and reopen Claude Desktop automatically at the end? [y/N]", "reopened")
		wantIn(t, "calls", calls, `osascript -e tell application "Claude" to quit`, "open -a Claude")
		if i, j := strings.Index(calls, "install.sh"), strings.Index(calls, "osascript"); i < 0 || j < i {
			t.Errorf("Desktop must restart after the install, not before:\n%s", calls)
		}
	})
	t.Run("no", func(t *testing.T) {
		r := newGetRig(t)
		r.desktopOpen()
		r.answer("n")
		out, calls, code := r.run()
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, out)
		}
		wantNotIn(t, "calls", calls, "osascript", "open -a Claude")
		wantIn(t, "output", out, "To do:      Quit Claude Desktop")
	})
}

// --dry-run says what it would do and leaves HOME, the stubs' log of
// anything that changes the Mac, and GitHub's copy alone.
func TestGetDryRunChangesNothing(t *testing.T) {
	r := newGetRig(t)
	r.noGo()
	r.desktopOpen()
	r.answer("y")
	snapshot := func() string {
		var paths []string
		_ = filepath.WalkDir(r.home, func(p string, d fs.DirEntry, err error) error {
			paths = append(paths, p)
			return nil
		})
		sort.Strings(paths)
		return strings.Join(paths, "\n")
	}
	before := snapshot()
	out, calls, code := r.run("--dry-run")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if after := snapshot(); after != before {
		t.Errorf("HOME changed:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	wantNotIn(t, "calls", calls, "sudo", "installer", "curl", "osascript", "open ", "install.sh", "trust-ca", "xcode-select --install")
	wantIn(t, "output", out, "dry run: nothing was changed", "would: clone", "go.dev", "ask whether to quit and reopen Claude Desktop")
	wantNotIn(t, "output", out, "[y/N]")
}

// A failed install, or a failed check after it, ends with the one line to
// paste when asking for help.
func TestGetFailurePrintsTheLineToPaste(t *testing.T) {
	t.Run("install fails", func(t *testing.T) {
		r := newGetRig(t)
		r.stubFile("install-fails", "")
		out, _, code := r.run()
		if code == 0 {
			t.Fatalf("want a failure:\n%s", out)
		}
		wantIn(t, "output", out, "SETUP DID NOT FINISH", "When asking for help, paste this line:",
			"claude-burst get.sh failed while installing Claude Burst (transparent mode)", filepath.Join(r.home, "claude-burst")+"/scripts/diagnose.sh")
	})
	t.Run("check fails", func(t *testing.T) {
		r := newGetRig(t)
		r.stubFile("diagnose-fails", "")
		out, _, code := r.run()
		if code == 0 {
			t.Fatalf("want a failure:\n%s", out)
		}
		wantIn(t, "output", out, "FAIL something", "INSTALLED, BUT A CHECK FAILED", "claude-burst get.sh failed while checking that it works")
	})
	// macOS refused the system-wide trust: asked again at the keyboard, and
	// still refused is a red ending naming the command.
	t.Run("trust refused", func(t *testing.T) {
		r := newGetRig(t)
		must(t, os.Remove(filepath.Join(r.stub, "ca-trusted")))
		out, calls, code := r.run()
		if code == 0 {
			t.Fatalf("want a failure:\n%s", out)
		}
		wantIn(t, "calls", calls, "trust-ca-systemwide.sh")
		wantIn(t, "output", out, "still not trusted", "sudo "+filepath.Join(r.home, "claude-burst")+"/scripts/trust-ca-systemwide.sh")
	})
	t.Run("trust allowed on the second ask", func(t *testing.T) {
		r := newGetRig(t)
		must(t, os.Remove(filepath.Join(r.stub, "ca-trusted")))
		r.stubFile("trust-works", "")
		out, _, code := r.run()
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, out)
		}
		wantIn(t, "output", out, "macOS may show a window", "DONE")
	})
}

// A "denied" notification setting is never asked about again by macOS, so
// get.sh opens System Settings at it.
func TestGetOpensNotificationSettingsWhenDenied(t *testing.T) {
	r := newGetRig(t)
	r.stubFile("notify", "denied")
	out, calls, code := r.run()
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	wantIn(t, "calls", calls, "claude-burst app notifications")
	wantIn(t, "output", out, "turn on Allow notifications")
}
