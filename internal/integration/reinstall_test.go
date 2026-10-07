package integration

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// reinstallRig is GitHub's copy of the repo (install.sh prints its version,
// v2 there), a checkout of it one commit behind, and a home in which
// install-burst-off.sh has put burst-reinstall.
type reinstallRig struct {
	t                      *testing.T
	home, origin, checkout string
}

func newReinstallRig(t *testing.T) *reinstallRig {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("macOS scripts")
	}
	base := t.TempDir()
	r := &reinstallRig{t: t, home: filepath.Join(base, "home"), origin: filepath.Join(base, "origin"), checkout: filepath.Join(base, "claude-burst")}
	must(t, os.MkdirAll(r.home, 0o755))
	must(t, os.MkdirAll(filepath.Join(r.origin, "scripts"), 0o755))
	r.git(r.origin, "init", "-q", "-b", "main")
	for _, f := range []string{"install-burst-off.sh", "reinstall.sh", "rollback.sh", "transparent-root.sh", "untrust-ca-systemwide.sh", "codex-unroute.sh", "audit-add.sh"} {
		b, err := os.ReadFile(filepath.Join(repoRoot(t), "scripts", f))
		must(t, err)
		must(t, os.WriteFile(filepath.Join(r.origin, "scripts", f), b, 0o755))
	}
	r.release("v1")
	r.git(base, "clone", "-q", r.origin, r.checkout)
	r.release("v2")
	if out, err := r.run("zsh", filepath.Join(r.checkout, "scripts", "install-burst-off.sh")); err != nil {
		t.Fatalf("install-burst-off.sh: %v\n%s", err, out)
	}
	return r
}

func (r *reinstallRig) git(dir string, args ...string) {
	r.t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		r.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func (r *reinstallRig) release(version string) {
	r.t.Helper()
	must(r.t, os.WriteFile(filepath.Join(r.origin, "install.sh"), []byte("#!/bin/zsh\necho installed "+version+" from ${0:A:h}\n"), 0o755))
	r.git(r.origin, "add", "-A")
	r.git(r.origin, "commit", "-q", "-m", version)
}

func (r *reinstallRig) run(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), "HOME="+r.home, "CLAUDE_BURST_INSTALL_DIR="+filepath.Join(r.home, ".local", "bin"), "CLAUDE_BURST_REPO=")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (r *reinstallRig) reinstall() string {
	r.t.Helper()
	out, err := r.run(filepath.Join(r.home, ".local", "bin", "burst-reinstall"))
	if err != nil {
		r.t.Fatalf("burst-reinstall: %v\n%s", err, out)
	}
	return out
}

// The command on the PATH runs a copy outside the checkout, finds the
// checkout it was installed from, and installs GitHub's newest.
func TestBurstReinstallFetchesTheNewestAndInstallsIt(t *testing.T) {
	r := newReinstallRig(t)
	if _, err := os.Stat(filepath.Join(r.home, ".local", "share", "claude-burst", "reinstall.sh")); err != nil {
		t.Fatalf("reinstall.sh must be copied out of the checkout: %v", err)
	}
	if out := r.reinstall(); !strings.Contains(out, "installed v2 from") || !strings.Contains(out, filepath.Base(r.checkout)) {
		t.Fatalf("want GitHub's v2 installed from the checkout:\n%s", out)
	}
}

func TestBurstReinstallLeavesWorkInProgressAsItIs(t *testing.T) {
	r := newReinstallRig(t)
	must(t, os.WriteFile(filepath.Join(r.checkout, "wip.txt"), []byte("mine\n"), 0o644))
	if out := r.reinstall(); !strings.Contains(out, "installed v1") || !strings.Contains(out, "uncommitted changes") {
		t.Fatalf("uncommitted work must not be pulled over:\n%s", out)
	}
}

// A checkout that was moved or deleted is no reason to stay broken.
func TestBurstReinstallClonesWhenTheCheckoutIsGone(t *testing.T) {
	r := newReinstallRig(t)
	must(t, os.RemoveAll(r.checkout))
	share := filepath.Join(r.home, ".local", "share", "claude-burst")
	// Point the clone at the test's origin instead of GitHub.
	b, err := os.ReadFile(filepath.Join(share, "reinstall.sh"))
	must(t, err)
	must(t, os.WriteFile(filepath.Join(share, "reinstall.sh"), []byte(strings.Replace(string(b), "https://github.com/andrewbakercloudscale/claude-burst.git", r.origin, 1)), 0o755))
	if out := r.reinstall(); !strings.Contains(out, "installed v2 from") || !strings.Contains(out, "claude-burst/src") {
		t.Fatalf("want a fresh clone installed:\n%s", out)
	}
}
