package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// The Upgrade button, against real git repositories in a temp dir: a bare
// "origin" standing in for GitHub and a clone standing in for the user's
// checkout. Nothing leaves the machine.

type upgradeRig struct {
	t        *testing.T
	origin   string // bare repo, "GitHub"
	work     string // a second clone that pushes new commits to origin
	checkout string // the user's checkout the gateway was built from
	s        *Server
	running  string // the commit the "running binary" was built from
	launched []string
	fetches  atomic.Int32
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false", "-c", "init.defaultBranch=main"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func commitFile(t *testing.T, dir, name, body, msg string) {
	t.Helper()
	p := filepath.Join(dir, name)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", name)
	git(t, dir, "commit", "-q", "-m", msg)
}

func mainGo(v string) string { return "package main\n\nconst version = \"" + v + "\"\n" }

func newUpgradeRig(t *testing.T) *upgradeRig {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	r := &upgradeRig{t: t, origin: filepath.Join(root, "origin.git"), work: filepath.Join(root, "work"), checkout: filepath.Join(root, "checkout")}
	git(t, root, "init", "-q", "--bare", "-b", "main", r.origin)
	git(t, root, "clone", "-q", r.origin, r.work)
	git(t, r.work, "checkout", "-q", "-b", "main")
	commitFile(t, r.work, "cmd/claude-burst/main.go", mainGo("0.2.0"), "first")
	commitFile(t, r.work, "scripts/transparent-root.sh", "#!/bin/zsh\n", "scripts")
	// deploy.sh stub: records that it ran, from which commit.
	commitFile(t, r.work, "scripts/deploy.sh", "#!/bin/bash\ncd \"$(dirname \"$0\")/..\" && git rev-parse --short HEAD > \"$DEPLOY_MARKER\"\n", "deploy")
	git(t, r.work, "push", "-q", "origin", "main")
	git(t, root, "clone", "-q", r.origin, r.checkout)

	r.s = newTestServer(t)
	r.s.version = "0.2.0"
	r.s.rootHelper = filepath.Join(r.checkout, "scripts", "transparent-root.sh")
	r.running = git(t, r.checkout, "rev-parse", "HEAD")

	oldB, oldG, oldL := buildCommit, runGit, launchTerminal
	buildCommit = func() (string, bool) { return r.running, false }
	runGit = func(ctx context.Context, dir string, args ...string) (string, error) {
		if len(args) > 0 && args[0] == "fetch" {
			r.fetches.Add(1)
		}
		return oldG(ctx, dir, args...)
	}
	launchTerminal = func(p string) error { r.launched = append(r.launched, p); return nil }
	upgradeMu.Lock()
	upgradeLast = nil
	upgradeMu.Unlock()
	t.Cleanup(func() {
		buildCommit, runGit, launchTerminal = oldB, oldG, oldL
		upgradeMu.Lock()
		upgradeLast = nil
		upgradeMu.Unlock()
	})
	return r
}

// publish pushes a new commit to "GitHub".
func (r *upgradeRig) publish(file, body, msg string) {
	commitFile(r.t, r.work, file, body, msg)
	git(r.t, r.work, "push", "-q", "origin", "main")
}

func (r *upgradeRig) status(refresh bool) upgradeStatus {
	r.t.Helper()
	path := "/api/upgrade-status"
	if refresh {
		path += "?refresh=1"
	}
	req := httptest.NewRequest(http.MethodGet, "http://x"+path, nil)
	req.Host = "127.0.0.1:7788"
	rr := httptest.NewRecorder()
	r.s.Handler().ServeHTTP(rr, req)
	var st upgradeStatus
	if err := json.Unmarshal(rr.Body.Bytes(), &st); err != nil {
		r.t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	return st
}

func (r *upgradeRig) upgrade() *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "http://x/api/upgrade", strings.NewReader("{}"))
	req.Host = "127.0.0.1:7788"
	req.Header.Set("X-Claude-Burst-Admin", "1")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.s.Handler().ServeHTTP(rr, req)
	return rr
}

func TestUpgradeUpToDate(t *testing.T) {
	r := newUpgradeRig(t)
	st := r.status(false)
	if st.Error != "" || !st.UpToDate || st.CanUpgrade || st.Behind != 0 || st.LatestVersion != "0.2.0" {
		t.Fatalf("status = %+v", st)
	}
	rr := r.upgrade()
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "already up to date") {
		t.Fatalf("upgrade when up to date: %d %s", rr.Code, rr.Body.String())
	}
	if len(r.launched) != 0 {
		t.Fatal("nothing may be launched when there is nothing to install")
	}
}

// The version check: new commits and a new version number on GitHub.
func TestUpgradeFindsANewerVersion(t *testing.T) {
	r := newUpgradeRig(t)
	r.publish("README.md", "docs\n", "Docs: explain single plan")
	r.publish("cmd/claude-burst/main.go", mainGo("0.3.0"), "Release 0.3.0")

	st := r.status(true)
	if st.Error != "" || st.UpToDate || !st.CanUpgrade || st.Behind != 2 {
		t.Fatalf("status = %+v", st)
	}
	if st.RunningVersion != "0.2.0" || st.LatestVersion != "0.3.0" {
		t.Fatalf("versions: running %q latest %q", st.RunningVersion, st.LatestVersion)
	}
	if len(st.NewCommits) != 2 || st.NewCommits[0] != "Release 0.3.0" {
		t.Fatalf("new commits = %q (newest first)", st.NewCommits)
	}
	if st.RunningCommit != short(r.running) {
		t.Fatalf("running commit = %q", st.RunningCommit)
	}
}

// The running build is what is compared, not the checkout: a checkout that
// was pulled but never deployed still runs the old gateway.
func TestUpgradeComparesTheRunningBuildNotTheCheckout(t *testing.T) {
	r := newUpgradeRig(t)
	r.publish("README.md", "docs\n", "newer")
	git(t, r.checkout, "pull", "-q", "--ff-only")
	st := r.status(true)
	if st.UpToDate || st.Behind != 1 || !st.CanUpgrade {
		t.Fatalf("checkout current but binary old: %+v", st)
	}
}

// A build with commits GitHub lacks (not pushed yet) is up to date, and
// says it is ahead rather than "the latest on GitHub".
func TestUpgradeRunningAheadOfGitHub(t *testing.T) {
	r := newUpgradeRig(t)
	commitFile(t, r.checkout, "local.txt", "mine\n", "not pushed")
	r.running = git(t, r.checkout, "rev-parse", "HEAD")
	st := r.status(true)
	if !st.UpToDate || st.Ahead != 1 || st.Behind != 0 || st.CanUpgrade {
		t.Fatalf("status = %+v", st)
	}
}

func TestUpgradeRefusesADirtyCheckout(t *testing.T) {
	r := newUpgradeRig(t)
	r.publish("README.md", "docs\n", "newer")
	os.WriteFile(filepath.Join(r.checkout, "scratch.txt"), []byte("wip"), 0o644)
	st := r.status(true)
	if st.CanUpgrade || !strings.Contains(st.Reason, "uncommitted changes") {
		t.Fatalf("status = %+v", st)
	}
	if rr := r.upgrade(); rr.Code != http.StatusConflict || len(r.launched) != 0 {
		t.Fatalf("upgrade over uncommitted changes: %d %s", rr.Code, rr.Body.String())
	}
}

func TestUpgradeRefusesLocalCommits(t *testing.T) {
	r := newUpgradeRig(t)
	r.publish("README.md", "docs\n", "newer")
	commitFile(t, r.checkout, "local.txt", "mine\n", "local work")
	st := r.status(true)
	if st.CanUpgrade || !strings.Contains(st.Reason, "local commits") {
		t.Fatalf("status = %+v", st)
	}
}

func TestUpgradeReportsAFailedCheck(t *testing.T) {
	r := newUpgradeRig(t)
	os.RemoveAll(r.origin) // GitHub unreachable
	st := r.status(true)
	if st.Error == "" || st.CanUpgrade || st.UpToDate {
		t.Fatalf("status = %+v", st)
	}
	if rr := r.upgrade(); rr.Code != http.StatusBadGateway || !strings.Contains(rr.Body.String(), "version check failed") {
		t.Fatalf("upgrade with a failed check: %d %s", rr.Code, rr.Body.String())
	}
}

// The check is cached for the header; the click always checks afresh.
func TestUpgradeCheckIsCachedButTheClickIsFresh(t *testing.T) {
	r := newUpgradeRig(t)
	r.status(false)
	r.status(false)
	if n := r.fetches.Load(); n != 1 {
		t.Fatalf("fetched %d times for two status reads, want 1", n)
	}
	r.publish("README.md", "docs\n", "newer")
	if rr := r.upgrade(); rr.Code != http.StatusOK {
		t.Fatalf("upgrade: %d %s (a stale cached up-to-date must not refuse it)", rr.Code, rr.Body.String())
	}
	if n := r.fetches.Load(); n != 2 {
		t.Fatalf("the click must fetch again, fetches = %d", n)
	}
}

// End to end: the click writes the script and opens it; running that
// script fast-forwards the checkout and then deploys the new commit.
func TestUpgradeScriptFastForwardsThenDeploys(t *testing.T) {
	if _, err := exec.LookPath("zsh"); err != nil {
		t.Skip("zsh not available")
	}
	r := newUpgradeRig(t)
	r.publish("cmd/claude-burst/main.go", mainGo("0.3.0"), "Release 0.3.0")
	latest := git(t, r.work, "rev-parse", "--short", "HEAD")

	rr := r.upgrade()
	if rr.Code != http.StatusOK || len(r.launched) != 1 {
		t.Fatalf("upgrade: %d %s launched=%v", rr.Code, rr.Body.String(), r.launched)
	}
	if !strings.Contains(rr.Body.String(), "0.2.0") || !strings.Contains(rr.Body.String(), "0.3.0") {
		t.Errorf("the reply should name both versions: %s", rr.Body.String())
	}
	script, _ := os.ReadFile(r.launched[0])
	if i, j := strings.Index(string(script), "merge --ff-only"), strings.Index(string(script), "scripts/deploy.sh"); i < 0 || j < i {
		t.Fatalf("the script must fast-forward before it deploys:\n%s", script)
	}

	marker := filepath.Join(t.TempDir(), "deployed")
	cmd := exec.Command("zsh", r.launched[0])
	cmd.Env = append(os.Environ(), "DEPLOY_MARKER="+marker)
	cmd.Stdin = strings.NewReader("")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("script: %v\n%s", err, out)
	}
	got, _ := os.ReadFile(marker)
	if strings.TrimSpace(string(got)) != latest {
		t.Fatalf("deploy ran on %q, want the new commit %q\n%s", got, latest, out)
	}
	if head := git(t, r.checkout, "rev-parse", "--short", "HEAD"); head != latest {
		t.Fatalf("checkout at %s, want %s", head, latest)
	}
}

// The script re-checks: uncommitted changes made after the click stop it
// before anything changes.
func TestUpgradeScriptStopsOnChangesMadeAfterTheClick(t *testing.T) {
	if _, err := exec.LookPath("zsh"); err != nil {
		t.Skip("zsh not available")
	}
	r := newUpgradeRig(t)
	r.publish("README.md", "docs\n", "newer")
	if rr := r.upgrade(); rr.Code != http.StatusOK {
		t.Fatalf("upgrade: %d %s", rr.Code, rr.Body.String())
	}
	before := git(t, r.checkout, "rev-parse", "HEAD")
	os.WriteFile(filepath.Join(r.checkout, "wip.txt"), []byte("wip"), 0o644)
	marker := filepath.Join(t.TempDir(), "deployed")
	cmd := exec.Command("zsh", r.launched[0])
	cmd.Env = append(os.Environ(), "DEPLOY_MARKER="+marker)
	cmd.Stdin = strings.NewReader("")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("script must fail on a dirty tree:\n%s", out)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("deploy ran over uncommitted changes")
	}
	if git(t, r.checkout, "rev-parse", "HEAD") != before {
		t.Fatal("the checkout moved")
	}
}

// Install GitHub version: a rollback from a build with unpushed commits
// and uncommitted changes. It deploys origin/main from a temporary
// worktree and leaves the checkout exactly as it was.
func TestInstallGitHubVersionRollsBackWithoutTouchingTheCheckout(t *testing.T) {
	if _, err := exec.LookPath("zsh"); err != nil {
		t.Skip("zsh not available")
	}
	r := newUpgradeRig(t)
	github := git(t, r.work, "rev-parse", "--short", "HEAD")
	commitFile(t, r.checkout, "local.txt", "mine\n", "not pushed")
	r.running = git(t, r.checkout, "rev-parse", "HEAD")
	os.WriteFile(filepath.Join(r.checkout, "wip.txt"), []byte("wip"), 0o644)

	req := httptest.NewRequest(http.MethodPost, "http://x/api/upgrade", strings.NewReader(`{"mode":"github"}`))
	req.Host = "127.0.0.1:7788"
	req.Header.Set("X-Claude-Burst-Admin", "1")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || len(r.launched) != 1 || !strings.Contains(rr.Body.String(), "Rolling back 1 commit") {
		t.Fatalf("install github: %d %s launched=%v", rr.Code, rr.Body.String(), r.launched)
	}

	marker := filepath.Join(t.TempDir(), "deployed")
	cmd := exec.Command("zsh", r.launched[0])
	cmd.Env = append(os.Environ(), "DEPLOY_MARKER="+marker)
	cmd.Stdin = strings.NewReader("")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("script: %v\n%s", err, out)
	}
	if got, _ := os.ReadFile(marker); strings.TrimSpace(string(got)) != github {
		t.Fatalf("deployed %q, want GitHub's %q\n%s", got, github, out)
	}
	if git(t, r.checkout, "rev-parse", "HEAD") != r.running {
		t.Fatal("the checkout's HEAD moved")
	}
	if b, _ := os.ReadFile(filepath.Join(r.checkout, "wip.txt")); string(b) != "wip" {
		t.Fatal("uncommitted work was touched")
	}
	if wt := git(t, r.checkout, "worktree", "list"); strings.Count(wt, "\n") != 0 {
		t.Fatalf("the temporary worktree was left behind:\n%s", wt)
	}
}

func TestInstallGitHubVersionRejectsAnUnknownMode(t *testing.T) {
	r := newUpgradeRig(t)
	req := httptest.NewRequest(http.MethodPost, "http://x/api/upgrade", strings.NewReader(`{"mode":"nuke"}`))
	req.Host = "127.0.0.1:7788"
	req.Header.Set("X-Claude-Burst-Admin", "1")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest || len(r.launched) != 0 {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
}

// The Install GitHub version button is offered whenever the check worked,
// labelled for what it will do.
func TestGitHubInstallButton(t *testing.T) {
	type b struct {
		Text  string `json:"text"`
		Title string `json:"title"`
	}
	var got map[string]*b
	runPageJS(t, []string{"githubInstallButton"}, `
const base = {running_version: "0.2.0", running_commit: "dc3de51", latest_version: "0.2.0", latest_commit: "98f965b"};
out({
  current: githubInstallButton({...base, up_to_date: true}),
  ahead: githubInstallButton({...base, up_to_date: true, ahead: 2}),
  behind: githubInstallButton({...base, behind: 3, can_upgrade: true}),
  blocked: githubInstallButton({...base, behind: 3, can_upgrade: false, reason: "the checkout has uncommitted changes"}),
  failed: githubInstallButton({error: "no network"}),
});`, &got)
	if c := got["current"]; c == nil || !strings.Contains(c.Text, "Reinstall GitHub version") {
		t.Errorf("current: %+v", c)
	}
	if c := got["ahead"]; c == nil || !strings.Contains(c.Text, "Roll back to GitHub version") || !strings.Contains(c.Title, "2 commit") {
		t.Errorf("ahead: %+v", c)
	}
	if c := got["behind"]; c == nil || !strings.Contains(c.Text, "without updating my checkout") {
		t.Errorf("behind: %+v", c)
	}
	if c := got["blocked"]; c == nil || !strings.Contains(c.Text, "Install GitHub version") || !strings.Contains(c.Title, "uncommitted changes") {
		t.Errorf("blocked: %+v", c)
	}
	if got["failed"] != nil {
		t.Errorf("no button when the check failed: %+v", got["failed"])
	}
}

// What the Check version dialog says, from each status.
func TestVersionMessage(t *testing.T) {
	type note struct {
		Cls  string `json:"cls"`
		Text string `json:"text"`
	}
	type m struct {
		Tone       string `json:"tone"`
		Mark       string `json:"mark"`
		Title      string `json:"title"`
		Sub        string `json:"sub"`
		Note       *note  `json:"note"`
		CanUpgrade bool   `json:"canUpgrade"`
	}
	var got map[string]m
	runPageJS(t, []string{"versionMessage"}, `
const base = {running_version: "0.2.0", running_commit: "98f965b"};
out({
  current: versionMessage({...base, up_to_date: true}),
  ahead: versionMessage({...base, up_to_date: true, ahead: 2, latest_commit: "66bdcb1"}),
  newer: versionMessage({...base, behind: 12, can_upgrade: true, latest_version: "0.3.0", latest_commit: "abc1234", new_commits: ["Release 0.3.0"]}),
  blocked: versionMessage({...base, behind: 1, can_upgrade: false, reason: "the checkout has uncommitted changes", latest_commit: "abc1234", new_commits: ["x"]}),
  failed: versionMessage({...base, error: "could not reach GitHub"}),
});`, &got)

	if c := got["current"]; c.Tone != "ok" || c.Title != "Up to date" || !strings.Contains(c.Sub, "latest version on GitHub") || c.CanUpgrade {
		t.Errorf("current: %+v", c)
	}
	if c := got["ahead"]; c.Tone != "ok" || !strings.Contains(c.Sub, "Ahead of GitHub by 2 commits") || strings.Contains(c.Sub, "latest version") {
		t.Errorf("ahead: %+v", c)
	}
	if c := got["newer"]; c.Tone != "new" || c.Title != "Upgrade available: 0.3.0" || c.Sub != "12 new commits on GitHub" || !c.CanUpgrade || c.Note == nil {
		t.Errorf("newer: %+v", c)
	}
	if c := got["blocked"]; c.Tone != "warn" || c.CanUpgrade || c.Note == nil || c.Note.Cls != "warn" || !strings.Contains(c.Note.Text, "uncommitted changes") {
		t.Errorf("blocked: %+v", c)
	}
	if c := got["failed"]; c.Tone != "bad" || c.CanUpgrade || c.Note == nil || !strings.Contains(c.Note.Text, "could not reach GitHub") {
		t.Errorf("failed: %+v", c)
	}
}

// The header button always reads Check version and is never disabled by a
// status, so it can always be asked.
func TestCheckVersionButtonIsAlwaysAvailable(t *testing.T) {
	src := string(indexHTML)
	if !strings.Contains(src, `<button id="upgradeTop" style="margin-left:10px">&#x27F3; Check version</button>`) {
		t.Fatal("header button must read Check version and start enabled")
	}
	if strings.Contains(src, `$("upgradeTop").disabled = b.disabled`) {
		t.Fatal("a status must not disable the button")
	}
}
