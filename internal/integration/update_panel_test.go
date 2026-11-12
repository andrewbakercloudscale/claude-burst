package integration

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// panelRig is an installed panel, GitHub's copy of its repo (one commit
// ahead, installer says v2), and a clone of it beside a copy of this repo's
// update-panel.sh. The installer prints the version it is.
type panelRig struct {
	t                   *testing.T
	home, burst, beside string
	origin              string
	key                 releaseKey
}

func newPanelRig(t *testing.T) *panelRig {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("macOS scripts")
	}
	base := t.TempDir()
	r := &panelRig{t: t, home: filepath.Join(base, "home"), burst: filepath.Join(base, "work", "claude-burst"),
		beside: filepath.Join(base, "work", "claude-code-cost-sidebar")}
	must(t, os.MkdirAll(filepath.Join(r.home, ".local", "bin"), 0o755))
	must(t, os.WriteFile(filepath.Join(r.home, ".local", "bin", "ccusage-panel.sh"), []byte("#!/bin/sh\n"), 0o755))
	must(t, os.MkdirAll(filepath.Join(r.burst, "scripts"), 0o755))
	script, err := os.ReadFile(filepath.Join(repoRoot(t), "scripts", "update-panel.sh"))
	must(t, err)
	must(t, os.WriteFile(filepath.Join(r.burst, "scripts", "update-panel.sh"), script, 0o755))
	r.key = newReleaseKey(t)
	r.key.installVerifier(t, filepath.Join(r.burst, "scripts"))

	origin := filepath.Join(base, "origin")
	r.origin = origin
	must(t, os.MkdirAll(origin, 0o755))
	r.git(origin, "init", "-q", "-b", "main")
	r.release(origin, "v1")
	r.git(base, "clone", "-q", origin, r.beside)
	r.release(origin, "v2")
	return r
}

func (r *panelRig) git(dir string, args ...string) {
	r.t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		r.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func (r *panelRig) release(dir, version string) {
	r.t.Helper()
	must(r.t, os.WriteFile(filepath.Join(dir, "claude-panel-setup.sh"), []byte("#!/bin/bash\necho installed "+version+"\n"), 0o755))
	r.git(dir, "add", "claude-panel-setup.sh")
	r.git(dir, "commit", "-q", "-m", version)
	r.key.sign(r.t, dir, version)
}

// push puts a new installer on GitHub's main without releasing it.
func (r *panelRig) push(dir, version string) {
	r.t.Helper()
	must(r.t, os.WriteFile(filepath.Join(dir, "claude-panel-setup.sh"), []byte("#!/bin/bash\necho installed "+version+"\n"), 0o755))
	r.git(dir, "add", "claude-panel-setup.sh")
	r.git(dir, "commit", "-q", "-m", version)
}

func (r *panelRig) run(script string, env ...string) string {
	r.t.Helper()
	cmd := exec.Command("zsh", script)
	// Never GitHub: a panel with no copy of its repo is fetched from here.
	cmd.Env = append(append(os.Environ(), "HOME="+r.home, "CLAUDE_BURST_PANEL_REPO="+r.origin, "CLAUDE_BURST_REPO="), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("update-panel.sh must never fail its caller: %v\n%s", err, out)
	}
	return string(out)
}

func (r *panelRig) script() string { return filepath.Join(r.burst, "scripts", "update-panel.sh") }

func TestUpdatePanelPullsACleanCheckoutBesideTheRepo(t *testing.T) {
	r := newPanelRig(t)
	if out := r.run(r.script()); !strings.Contains(out, "installed v2") {
		t.Fatalf("a clean checkout on main must be pulled first:\n%s", out)
	}
}

func TestUpdatePanelLeavesWorkInProgressAlone(t *testing.T) {
	r := newPanelRig(t)
	must(t, os.WriteFile(filepath.Join(r.beside, "notes.txt"), []byte("mine\n"), 0o644))
	out := r.run(r.script())
	if !strings.Contains(out, "installed v1") || !strings.Contains(out, "uncommitted changes") {
		t.Fatalf("uncommitted work must not be pulled over, and the output must say so:\n%s", out)
	}
	must(t, os.Remove(filepath.Join(r.beside, "notes.txt")))
	r.git(r.beside, "checkout", "-q", "-b", "feature")
	out = r.run(r.script())
	if !strings.Contains(out, "installed v1") || !strings.Contains(out, "not on main") {
		t.Fatalf("a branch other than main must not be pulled:\n%s", out)
	}
}

// The dashboard's "Install GitHub version" runs a temporary copy of the
// repo: the panel is beside the real checkout, which CLAUDE_BURST_REPO names.
func TestUpdatePanelFindsThePanelFromATemporaryCopy(t *testing.T) {
	r := newPanelRig(t)
	tmp := filepath.Join(t.TempDir(), "claude-burst-github.abc", "scripts")
	must(t, os.MkdirAll(tmp, 0o755))
	script, err := os.ReadFile(r.script())
	must(t, err)
	must(t, os.WriteFile(filepath.Join(tmp, "update-panel.sh"), script, 0o755))
	r.key.installVerifier(t, tmp)
	out := r.run(filepath.Join(tmp, "update-panel.sh"), "CLAUDE_BURST_REPO="+r.burst)
	if !strings.Contains(out, "updating from "+r.beside) || !strings.Contains(out, "installed v2") {
		t.Fatalf("the panel beside the real checkout is the one updated:\n%s", out)
	}
}

// A panel installed from a repo that has since moved or gone used to be left
// as it was for good, with a warning. A copy is fetched and installed.
func TestUpdatePanelFetchesACopyWhenNoneIsFound(t *testing.T) {
	r := newPanelRig(t)
	must(t, os.RemoveAll(r.beside))
	out := r.run(r.script())
	if !strings.Contains(out, "installed v2") {
		t.Fatalf("want the newest panel fetched and installed:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(r.home, ".local", "share", "claude-burst", "claudecode-cost-usage-panel", ".git")); err != nil {
		t.Fatalf("the fetched copy is kept for the next update: %v\n%s", err, out)
	}
	r.release(r.origin, "v3")
	if out := r.run(r.script()); !strings.Contains(out, "installed v3") {
		t.Fatalf("the kept copy is pulled next time:\n%s", out)
	}
}

// Only a signed release of the panel is fetched in. An installer pushed to
// its main, or tagged by anyone but the release key, is not run: until
// 13 Oct 2026 every update ran whatever main was.
func TestUpdatePanelRunsOnlyASignedRelease(t *testing.T) {
	t.Run("pushed but not released, checkout beside the repo", func(t *testing.T) {
		r := newPanelRig(t)
		r.push(r.origin, "owned")
		if out := r.run(r.script()); strings.Contains(out, "installed owned") || !strings.Contains(out, "installed v2") {
			t.Fatalf("want the signed v2, never the unreleased push:\n%s", out)
		}
	})
	t.Run("newest tag unsigned, checkout beside the repo", func(t *testing.T) {
		r := newPanelRig(t)
		r.push(r.origin, "owned")
		r.git(r.origin, "tag", "v3")
		out := r.run(r.script())
		if strings.Contains(out, "installed owned") || !strings.Contains(out, "installed v1") || !strings.Contains(out, "could not be verified") {
			t.Fatalf("want the copy already there (v1) and the reason:\n%s", out)
		}
	})
	t.Run("newest tag unsigned, no copy here", func(t *testing.T) {
		r := newPanelRig(t)
		must(t, os.RemoveAll(r.beside))
		r.push(r.origin, "owned")
		r.git(r.origin, "tag", "v3")
		out := r.run(r.script())
		if strings.Contains(out, "installed owned") || strings.Contains(out, "installed v") || !strings.Contains(out, "could not be verified") {
			t.Fatalf("nothing may be installed from an unverified fetch:\n%s", out)
		}
		if _, err := os.Stat(filepath.Join(r.home, ".local", "share", "claude-burst", "claudecode-cost-usage-panel")); err == nil {
			t.Fatal("the unverified copy was kept, where the next run would take it for a verified one")
		}
	})
	t.Run("newest tag unsigned, kept copy", func(t *testing.T) {
		r := newPanelRig(t)
		must(t, os.RemoveAll(r.beside))
		if out := r.run(r.script()); !strings.Contains(out, "installed v2") {
			t.Fatalf("setup: %s", out)
		}
		r.push(r.origin, "owned")
		r.git(r.origin, "tag", "v3")
		out := r.run(r.script())
		if strings.Contains(out, "installed owned") || !strings.Contains(out, "installed v2") || !strings.Contains(out, "could not be verified") {
			t.Fatalf("want the kept copy (v2) reinstalled and the reason:\n%s", out)
		}
	})
}

// installOfferPanel runs install.sh's offer_panel on its own, with nobody
// at the keyboard, for a Mac with no panel installed yet.
func (r *panelRig) installOfferPanel(env ...string) string {
	r.t.Helper()
	must(r.t, os.Remove(filepath.Join(r.home, ".local", "bin", "ccusage-panel.sh")))
	src := readFile(filepath.Join(repoRoot(r.t), "install.sh"))
	i := strings.Index(src, "PANEL_REPO=")
	j := strings.Index(src[i:], "\n}\n")
	if i < 0 || j < 0 {
		r.t.Fatal("offer_panel not found in install.sh")
	}
	script := filepath.Join(r.t.TempDir(), "offer.zsh")
	must(r.t, os.WriteFile(script, []byte("ROOT="+r.burst+"\n"+src[i:i+j+3]+"offer_panel\n"), 0o755))
	cmd := exec.Command("zsh", script)
	cmd.Env = append(append(os.Environ(), "HOME="+r.home, "CLAUDE_BURST_PANEL_REPO="+r.origin), env...)
	cmd.Stdin = strings.NewReader("")
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("offer_panel: %v\n%s", err, out)
	}
	return string(out)
}

// Every install includes the usage panel, asked or not; until 14 Oct 2026
// an install with nobody to answer the question skipped it.
func TestInstallIncludesTheUsagePanel(t *testing.T) {
	r := newPanelRig(t)
	if out := r.installOfferPanel(); !strings.Contains(out, "installed v1") {
		t.Fatalf("the install must include the usage panel:\n%s", out)
	}
}

func TestInstallSkipsTheUsagePanelWhenToldTo(t *testing.T) {
	r := newPanelRig(t)
	if out := r.installOfferPanel("CLAUDE_BURST_PANEL=no"); strings.Contains(out, "installed v") || !strings.Contains(out, "skipped") {
		t.Fatalf("CLAUDE_BURST_PANEL=no must skip the usage panel:\n%s", out)
	}
}
