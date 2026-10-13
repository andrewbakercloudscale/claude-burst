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
	key                    releaseKey
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
	for _, f := range []string{"install-burst-off.sh", "reinstall.sh", "rollback.sh", "repair.sh", "transparent-root.sh", "untrust-ca-systemwide.sh", "codex-unroute.sh", "audit-add.sh"} {
		b, err := os.ReadFile(filepath.Join(repoRoot(t), "scripts", f))
		must(t, err)
		must(t, os.WriteFile(filepath.Join(r.origin, "scripts", f), b, 0o755))
	}
	r.key = newReleaseKey(t)
	r.key.installVerifier(t, filepath.Join(r.origin, "scripts"))
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
	r.key.sign(r.t, r.origin, version)
}

// push puts a new install.sh on GitHub's main without releasing it.
func (r *reinstallRig) push(version string) {
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

// The alerts, diagnose.sh and the docs tell people to run burst-repair,
// burst-off and burst-reinstall. Each is put on the PATH by the installer,
// and each runs a copy kept outside the checkout: until 12 Oct 2026
// burst-repair was a command nothing installed.
func TestEveryCommandTheAlertsNameIsInstalled(t *testing.T) {
	r := newReinstallRig(t)
	for name, script := range map[string]string{"burst-off": "rollback.sh", "burst-reinstall": "reinstall.sh", "burst-repair": "repair.sh"} {
		st, err := os.Stat(filepath.Join(r.home, ".local", "bin", name))
		if err != nil || st.Mode().Perm()&0o111 == 0 {
			t.Errorf("%s is not an installed command: %v", name, err)
			continue
		}
		if !strings.Contains(readFile(filepath.Join(r.home, ".local", "bin", name)), "/.local/share/claude-burst/"+script) {
			t.Errorf("%s does not run the copy of %s kept outside the checkout", name, script)
		}
		if st, err := os.Stat(filepath.Join(r.home, ".local", "share", "claude-burst", script)); err != nil || st.Mode().Perm()&0o111 == 0 {
			t.Errorf("%s was not copied out of the checkout: %v", script, err)
		}
	}
	// And the uninstall takes all three away again.
	uninstall := readFile(filepath.Join(repoRoot(t), "install.sh"))
	for _, name := range []string{"burst-off", "burst-reinstall", "burst-repair"} {
		if !strings.Contains(uninstall, `"$INSTALL_DIR/`+name+`"`) {
			t.Errorf("install.sh uninstall leaves %s behind", name)
		}
	}
}

// burst-reinstall installs only a release the release key signed, checked
// with the verifier the installer copied out of the checkout, not the one
// in what was just fetched. Until 13 Oct 2026 it pulled GitHub's main and
// ran its install.sh, which asks for the password.
func TestBurstReinstallInstallsOnlyASignedRelease(t *testing.T) {
	t.Run("pushed but not released", func(t *testing.T) {
		r := newReinstallRig(t)
		r.push("owned")
		if out := r.reinstall(); strings.Contains(out, "installed owned") || !strings.Contains(out, "installed v2") {
			t.Fatalf("want the signed v2, never the unreleased push:\n%s", out)
		}
	})
	t.Run("newest tag unsigned", func(t *testing.T) {
		r := newReinstallRig(t)
		r.push("owned")
		r.git(r.origin, "tag", "v3")
		out := r.reinstall()
		if strings.Contains(out, "installed owned") || !strings.Contains(out, "installed v1") || !strings.Contains(out, "could not be verified") {
			t.Fatalf("want the copy already here (v1) and the reason:\n%s", out)
		}
	})
	t.Run("fetched code cannot vouch for itself", func(t *testing.T) {
		// The attacker signs with a key of their own and ships a signers
		// file that trusts it. The verifier in use is the installed copy.
		r := newReinstallRig(t)
		theirs := newReleaseKey(t)
		must(t, os.WriteFile(filepath.Join(r.origin, "scripts", "release-signers"), []byte(theirs.signers), 0o644))
		r.push("owned")
		theirs.sign(t, r.origin, "v3")
		out := r.reinstall()
		if strings.Contains(out, "installed owned") || !strings.Contains(out, "installed v1") {
			t.Fatalf("a release signed by a key only the fetched code trusts was installed:\n%s", out)
		}
	})
	t.Run("checkout gone, newest tag unsigned", func(t *testing.T) {
		r := newReinstallRig(t)
		must(t, os.RemoveAll(r.checkout))
		share := filepath.Join(r.home, ".local", "share", "claude-burst")
		b, err := os.ReadFile(filepath.Join(share, "reinstall.sh"))
		must(t, err)
		must(t, os.WriteFile(filepath.Join(share, "reinstall.sh"), []byte(strings.Replace(string(b), "https://github.com/andrewbakercloudscale/claude-burst.git", r.origin, 1)), 0o755))
		r.push("owned")
		r.git(r.origin, "tag", "v3")
		out, err := r.run(filepath.Join(r.home, ".local", "bin", "burst-reinstall"))
		if err == nil || strings.Contains(out, "installed owned") || strings.Contains(out, "installed v") {
			t.Fatalf("a fresh clone that does not verify must install nothing: err %v\n%s", err, out)
		}
		if _, err := os.Stat(filepath.Join(share, "src")); err == nil {
			t.Fatal("the unverified clone was kept")
		}
	})
}

// repairSync runs repair.sh up to the end of its sync with GitHub (the rest
// stops processes and reinstalls) against the rig's checkout.
func (r *reinstallRig) repairSync() string {
	r.t.Helper()
	src := readFile(filepath.Join(repoRoot(r.t), "scripts", "repair.sh"))
	head, _, ok := strings.Cut(src, `say "freeing Burst's ports"`)
	if !ok {
		r.t.Fatal("repair.sh no longer has the step this test cuts at")
	}
	script := filepath.Join(r.t.TempDir(), "repair-sync.sh")
	must(r.t, os.WriteFile(script, []byte(head+"\ngit log -1 --format='HEAD is %s'\n"), 0o755))
	cmd := exec.Command("/bin/bash", script)
	cmd.Env = append(os.Environ(), "HOME="+r.home, "CLAUDE_BURST_REPO="+r.checkout)
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("repair.sh sync: %v\n%s", err, out)
	}
	return string(out)
}

// Repair merges only a signed release, and carries on from the checkout as
// it is when the newest one does not verify: a Mac being repaired must not
// be left unrepaired over it. A checkout from before releases were signed
// has no verifier and is synced the old way, once.
func TestRepairSyncsOnlyToASignedRelease(t *testing.T) {
	t.Run("signed release", func(t *testing.T) {
		r := newReinstallRig(t)
		r.push("owned")
		if out := r.repairSync(); !strings.Contains(out, "HEAD is v2") || !strings.Contains(out, "on release v2") {
			t.Fatalf("want the signed v2, not the unreleased push:\n%s", out)
		}
	})
	t.Run("newest tag unsigned", func(t *testing.T) {
		r := newReinstallRig(t)
		r.push("owned")
		r.git(r.origin, "tag", "v3")
		if out := r.repairSync(); !strings.Contains(out, "HEAD is v1") || !strings.Contains(out, "could not be verified") {
			t.Fatalf("want the checkout left on v1 and the reason:\n%s", out)
		}
	})
	t.Run("checkout from before releases were signed", func(t *testing.T) {
		r := newReinstallRig(t)
		share := filepath.Join(r.home, ".local", "share", "claude-burst")
		must(t, os.Remove(filepath.Join(share, "verify-release.sh")))
		r.git(r.checkout, "rm", "-q", "scripts/verify-release.sh", "scripts/release-signers")
		r.git(r.checkout, "commit", "-q", "-m", "as it was before signing")
		r.git(r.checkout, "push", "-q", "origin", "HEAD:refs/heads/legacy")
		r.git(r.origin, "merge", "-q", "legacy")
		r.push("v3 unsigned")
		if out := r.repairSync(); !strings.Contains(out, "HEAD is v3 unsigned") || !strings.Contains(out, "before releases were signed") {
			t.Fatalf("want one unverified sync, said plainly:\n%s", out)
		}
	})
}
