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

	origin := filepath.Join(base, "origin")
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
}

func (r *panelRig) run(script string, env ...string) string {
	r.t.Helper()
	cmd := exec.Command("zsh", script)
	cmd.Env = append(append(os.Environ(), "HOME="+r.home), env...)
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
	if out := r.run(filepath.Join(tmp, "update-panel.sh")); !strings.Contains(out, "no copy of its repo was found") {
		t.Fatalf("without the real checkout's path there is nothing to find:\n%s", out)
	}
	if out := r.run(filepath.Join(tmp, "update-panel.sh"), "CLAUDE_BURST_REPO="+r.burst); !strings.Contains(out, "installed v2") {
		t.Fatalf("with it, the panel beside the real checkout is updated:\n%s", out)
	}
}
