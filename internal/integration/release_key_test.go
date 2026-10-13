package integration

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// releaseKey is a throwaway signing key standing in for the release key, so
// the scripts' real verifier (scripts/verify-release.sh) runs for real in
// these tests and nothing of the real key is needed or touched.
type releaseKey struct {
	path    string // the private key; the public half is path + ".pub"
	signers string // the release-signers file that trusts it
}

func newReleaseKey(t *testing.T) releaseKey {
	t.Helper()
	k := releaseKey{path: filepath.Join(t.TempDir(), "release_key")}
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", k.path).CombinedOutput(); err != nil {
		t.Skipf("ssh-keygen: %v\n%s", err, out)
	}
	pub, err := os.ReadFile(k.path + ".pub")
	must(t, err)
	f := strings.Fields(string(pub))
	k.signers = "release@test namespaces=\"git\" " + f[0] + " " + f[1] + "\n"
	return k
}

// sign makes tag a signed release of dir's HEAD.
func (k releaseKey) sign(t *testing.T, dir, tag string) {
	t.Helper()
	cmd := exec.Command("git", "-C", dir, "-c", "user.name=t", "-c", "user.email=t@t",
		"-c", "gpg.format=ssh", "-c", "user.signingkey="+k.path+".pub", "tag", "-s", "-m", tag, tag)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("signing %s: %v\n%s", tag, err, out)
	}
}

// installVerifier puts the repo's real verifier and fetch-panel.sh into
// scriptsDir, trusting this key and no other.
func (k releaseKey) installVerifier(t *testing.T, scriptsDir string) {
	t.Helper()
	must(t, os.MkdirAll(scriptsDir, 0o755))
	for _, f := range []string{"verify-release.sh", "fetch-panel.sh"} {
		b, err := os.ReadFile(filepath.Join(repoRoot(t), "scripts", f))
		must(t, err)
		must(t, os.WriteFile(filepath.Join(scriptsDir, f), b, 0o755))
	}
	must(t, os.WriteFile(filepath.Join(scriptsDir, "release-signers"), []byte(k.signers), 0o644))
}

// The verifier itself, against real git and real signatures.
func TestVerifyReleasePrintsOnlyASignedNewestRelease(t *testing.T) {
	key, other := newReleaseKey(t), newReleaseKey(t)
	base := t.TempDir()
	repo, scripts := filepath.Join(base, "repo"), filepath.Join(base, "scripts")
	key.installVerifier(t, scripts)
	git := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	must(t, os.MkdirAll(repo, 0o755))
	git("init", "-q", "-b", "main")
	commit := func(msg string) string {
		git("commit", "-q", "--allow-empty", "-m", msg)
		return git("rev-parse", "HEAD")
	}
	verify := func() (string, string, error) {
		cmd := exec.Command("/bin/bash", filepath.Join(scripts, "verify-release.sh"), repo, "main")
		var stderr strings.Builder
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		return strings.TrimSpace(string(out)), stderr.String(), err
	}

	commit("first")
	if out, _, err := verify(); err == nil || out != "" {
		t.Fatalf("no release at all must not verify: %q", out)
	}
	c1 := commit("Version 0.1.0")
	key.sign(t, repo, "v0.1.0")
	if out, stderr, err := verify(); err != nil || out != "v0.1.0 "+c1 {
		t.Fatalf("a signed release must print its tag and commit, got %q, %v\n%s", out, err, stderr)
	}
	// Commits after the release are not the release.
	commit("pushed, not released")
	if out, _, err := verify(); err != nil || out != "v0.1.0 "+c1 {
		t.Fatalf("unreleased commits changed the answer: %q, %v", out, err)
	}
	// 0.10 is newer than 0.9: the order is by version, not by letters.
	c9 := commit("Version 0.9.0")
	key.sign(t, repo, "v0.9.0")
	c10 := commit("Version 0.10.0")
	key.sign(t, repo, "v0.10.0")
	if out, _, err := verify(); err != nil || out != "v0.10.0 "+c10 {
		t.Fatalf("want v0.10.0 as the newest (v0.9.0 is %s): %q, %v", c9, out, err)
	}
	// A newer tag that is not signed by the release key stops everything:
	// nothing is printed, so nothing older is installed in its place by a
	// caller that only reads stdout, and the reason names the tag.
	for name, tag := range map[string]func(){
		"a plain tag":            func() { git("tag", "v1.0.0") },
		"an annotated tag":       func() { git("tag", "-a", "-m", "x", "v1.0.0") },
		"a tag another key made": func() { other.sign(t, repo, "v1.0.0") },
	} {
		tag()
		out, stderr, err := verify()
		if err == nil || out != "" || !strings.Contains(stderr, "v1.0.0") || !strings.Contains(stderr, "not signed by the Claude Burst release key") {
			t.Errorf("%s was accepted or not named: out %q err %v\n%s", name, out, err, stderr)
		}
		git("tag", "-d", "v1.0.0")
	}
	// Without its signers file the verifier refuses, it does not pass.
	must(t, os.Remove(filepath.Join(scripts, "release-signers")))
	if out, _, err := verify(); err == nil || out != "" {
		t.Fatalf("no signers file must not verify: %q", out)
	}
}

// The signers file that ships is one line a verifier can use, and the
// scripts that sync with GitHub all go through the verifier.
func TestEveryPathThatFetchesCodeVerifiesTheRelease(t *testing.T) {
	root := repoRoot(t)
	signers := readFile(filepath.Join(root, "scripts", "release-signers"))
	if !strings.Contains(signers, `namespaces="git" ssh-ed25519 `) {
		t.Fatalf("scripts/release-signers is not an allowed-signers line for git: %q", signers)
	}
	for file, banned := range map[string][]string{
		"scripts/repair.sh":              {"git pull"},
		"scripts/reinstall.sh":           {"git pull", "pull --ff-only", "origin/main"},
		"scripts/update-panel.sh":        {"pull --ff-only", "git clone"},
		"install.sh":                     {"pull --ff-only", "git clone"},
		"internal/admin/mac.go":          {`"clone"`},
		"internal/admin/upgrade.go":      {"merge --ff-only origin/main", `--detach "$tmp" origin/main`},
		"scripts/fetch-panel.sh":         nil,
		"scripts/install-burst-off.sh":   nil,
		"internal/admin/upgrade_test.go": nil,
	} {
		src := readFile(filepath.Join(root, file))
		if !strings.Contains(src, "verify-release.sh") && !strings.Contains(src, "fetch-panel.sh") && !strings.Contains(src, "fetchPanel(") {
			t.Errorf("%s does not go through the release verifier", file)
		}
		for _, b := range banned {
			if strings.Contains(src, b) {
				t.Errorf("%s still has %q: code fetched that way is not verified", file, b)
			}
		}
	}
}
