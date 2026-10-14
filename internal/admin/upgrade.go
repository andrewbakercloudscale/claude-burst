package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Upgrade: the header's button that installs the newest release from
// GitHub. It checks the version first: the running binary's own commit
// (Go stamps it into every build made in the repo) against the newest
// release after a fetch. Only when the running build is behind, and the
// checkout can fast-forward cleanly, does it offer the upgrade.
//
// A release is a version tag signed by the release key, and nothing else
// is offered or installed (scripts/verify-release.sh says why). Until
// 13 Oct 2026 this installed whatever GitHub's main was, so a push to main
// from a stolen login reached every Mac at its next Upgrade click.
//
// The upgrade itself runs in a Terminal window, like Install: deploy.sh
// restarts this very process, so it cannot run as a child of it.
//
// The usage panel (claude-code-cost-sidebar) is checked beside it, against
// its own newest signed release, and Upgrade brings whichever of the two is
// behind up to date, or both. Until 14 Oct 2026 a panel release reached a
// Mac only with a Burst release, so with Burst current it never did.

// buildCommit is the commit this binary was built from, and whether the
// tree had uncommitted changes. A variable for tests.
var buildCommit = func() (rev string, modified bool) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "", false
	}
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			modified = s.Value == "true"
		}
	}
	return rev, modified
}

// runGit runs git in dir. A variable so tests never fetch from GitHub.
var runGit = func(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// verifyRelease returns the tag and commit of the newest release in the
// checkout at dir, verified by the checkout's own scripts/verify-release.sh:
// the copy already here, which the fetch does not touch. A variable for
// tests.
var verifyRelease = func(ctx context.Context, dir string) (tag, commit string, err error) {
	return verifyWith(ctx, filepath.Join(dir, "scripts", "verify-release.sh"), dir)
}

// verifyPanelRelease is the same for the usage panel's checkout at dir,
// verified by Burst's verifier, since the panel's repo carries none.
var verifyPanelRelease = verifyWith

func verifyWith(ctx context.Context, verifier, dir string) (tag, commit string, err error) {
	cmd := exec.CommandContext(ctx, "/bin/bash", verifier, dir)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		why := strings.TrimSpace(strings.ReplaceAll(stderr.String(), "verify-release: ", ""))
		if why == "" {
			why = err.Error()
		}
		return "", "", fmt.Errorf("%s", strings.ReplaceAll(why, "\n", " "))
	}
	tag, commit, ok := strings.Cut(strings.TrimSpace(string(out)), " ")
	if !ok || tag == "" || commit == "" {
		return "", "", fmt.Errorf("scripts/verify-release.sh printed %q, not a tag and a commit", strings.TrimSpace(string(out)))
	}
	return tag, commit, nil
}

const upgradeCheckEvery = 10 * time.Minute

// localGitWait bounds the local git command that runs before the fetch. A
// variable so tests need not wait it out.
var localGitWait = 10 * time.Second

type upgradeStatus struct {
	CheckedAt      time.Time `json:"checked_at"`
	RunningVersion string    `json:"running_version"`
	RunningCommit  string    `json:"running_commit"`
	RunningDirty   bool      `json:"running_dirty,omitempty"` // built from uncommitted changes
	LatestVersion  string    `json:"latest_version,omitempty"`
	LatestCommit   string    `json:"latest_commit,omitempty"`
	Behind         int       `json:"behind"`                // commits in the newest release the running build lacks
	Ahead          int       `json:"ahead"`                 // commits in the running build the newest release lacks (not released yet)
	NewCommits     []string  `json:"new_commits,omitempty"` // their subjects, newest first
	UpToDate       bool      `json:"up_to_date"`
	CanUpgrade     bool      `json:"can_upgrade"`
	Reason         string    `json:"reason,omitempty"` // why not, when it cannot
	Error          string    `json:"error,omitempty"`  // the check itself failed
	// The usage panel's side, checked even when Burst's check failed.
	Panel *panelStatus `json:"panel,omitempty"`
}

// panelStatus compares the usage panel installed here with the newest
// signed release of its repo.
type panelStatus struct {
	Installed bool   `json:"installed"`
	Version   string `json:"version,omitempty"` // installed here
	Latest    string `json:"latest,omitempty"`  // its newest signed release
	Behind    bool   `json:"behind"`
	Note      string `json:"note,omitempty"` // why they could not be compared
}

var (
	upgradeMu   sync.Mutex
	upgradeLast *upgradeStatus
)

func short(c string) string {
	if len(c) > 7 {
		return c[:7]
	}
	return c
}

func (s *Server) repoDir() (string, bool) {
	dir, ok := s.scriptsDir()
	if !ok {
		return "", false
	}
	return filepath.Dir(dir), true
}

// checkUpgrade fetches origin and compares. The fetch is the slow part, so
// the answer is cached; force skips the cache (the Upgrade click does).
func (s *Server) checkUpgrade(ctx context.Context, force bool) upgradeStatus {
	upgradeMu.Lock()
	defer upgradeMu.Unlock()
	if !force && upgradeLast != nil && time.Since(upgradeLast.CheckedAt) < upgradeCheckEvery {
		return *upgradeLast
	}
	st := s.computeUpgrade(ctx)
	st.Panel = s.computePanel(ctx)
	upgradeLast = &st
	alertUpgrade(st)
	return st
}

func (s *Server) computeUpgrade(ctx context.Context) upgradeStatus {
	rev, dirty := buildCommit()
	st := upgradeStatus{CheckedAt: time.Now(), RunningVersion: s.version, RunningCommit: short(rev), RunningDirty: dirty}
	fail := func(format string, a ...any) upgradeStatus {
		st.Error = fmt.Sprintf(format, a...)
		st.Reason = st.Error
		return st
	}
	dir, ok := s.repoDir()
	if !ok {
		return fail("no checkout of the claude-burst repo found next to this binary's scripts, so there is nothing to upgrade from")
	}
	// A local git command first: it needs no network, so when it hangs it
	// is macOS waiting on someone to answer whether git may read the
	// folder the checkout is in, not GitHub being slow.
	local, cancelLocal := context.WithTimeout(ctx, localGitWait)
	_, err := runGit(local, dir, "rev-parse", "--git-dir")
	waited := local.Err() != nil
	cancelLocal()
	if waited {
		if _, folder := s.checkoutFolder(); folder != "" {
			return fail("macOS is asking whether git may read %s, where the claude-burst checkout is. Click Allow on the Mac, or press Check folder access under Permissions while you are at it", folder)
		}
		return fail("git did not answer in %s in %s", localGitWait, dir)
	}
	if err != nil {
		return fail("%v", err)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err := runGit(ctx, dir, "fetch", "--quiet", "--tags", "origin", "main"); err != nil {
		return fail("could not reach GitHub: %v", err)
	}
	tag, latest, err := verifyRelease(ctx, dir)
	if err != nil {
		return fail("nothing on GitHub can be installed: %v", err)
	}
	st.LatestCommit = short(latest)
	st.LatestVersion = strings.TrimPrefix(tag, "v")

	// Compare what is RUNNING, not what is checked out: a checkout pulled
	// but never deployed is still an old gateway.
	base := rev
	if base == "" {
		if base, err = runGit(ctx, dir, "rev-parse", "HEAD"); err != nil {
			return fail("%v", err)
		}
	}
	if n, err := runGit(ctx, dir, "rev-list", "--count", base+".."+latest); err == nil {
		st.Behind, _ = strconv.Atoi(n)
	} else {
		return fail("the running build's commit %s is not in this checkout: %v", short(base), err)
	}
	if n, err := runGit(ctx, dir, "rev-list", "--count", latest+".."+base); err == nil {
		st.Ahead, _ = strconv.Atoi(n)
	}
	if st.Behind > 0 {
		if log, err := runGit(ctx, dir, "log", "--format=%s", "-n", "10", base+".."+latest); err == nil && log != "" {
			st.NewCommits = strings.Split(log, "\n")
		}
	}
	st.UpToDate = st.Behind == 0

	// Can the checkout fast-forward to it? deploy.sh builds the working
	// tree, so uncommitted changes would ship alongside the upgrade.
	switch {
	case st.UpToDate && st.Ahead > 0:
		st.Reason = "running a newer build than the newest release"
	case st.UpToDate:
		st.Reason = "already running the latest version"
	default:
		if b, err := runGit(ctx, dir, "rev-parse", "--abbrev-ref", "HEAD"); err != nil || b != "main" {
			st.Reason = "the checkout is not on main (it is on " + b + ")"
		} else if p, err := runGit(ctx, dir, "status", "--porcelain"); err != nil || p != "" {
			st.Reason = "the checkout has uncommitted changes, which the deploy would ship too; commit or stash them first"
		} else if a, err := runGit(ctx, dir, "rev-list", "--count", latest+"..HEAD"); err != nil || a != "0" {
			st.Reason = "the checkout has commits that are not in release " + tag + ", which the deploy would ship too"
		} else {
			st.CanUpgrade = true
		}
	}
	return st
}

// computePanel checks the usage panel. One that is not installed is left
// alone: it was declined or removed, and the dashboard installs it.
func (s *Server) computePanel(ctx context.Context) *panelStatus {
	p := &panelStatus{}
	if fi, err := os.Stat(filepath.Join(homeDir(), ".local", "bin", "ccusage-panel.sh")); err != nil || fi.Mode()&0o111 == 0 {
		return p
	}
	p.Installed = true
	p.Version = installedPanelVersion()
	repo, ok := s.repoDir()
	dir := s.panelRepoDir()
	if !ok || dir == "" {
		// scripts/update-panel.sh fetches a copy when there is none.
		p.Note = "no copy of the usage panel's repo is here to compare with; Upgrade fetches its newest release"
		p.Behind = true
		return p
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err := runGit(ctx, dir, "fetch", "--quiet", "--tags", "origin", "main"); err != nil {
		p.Note = "could not reach GitHub for the usage panel: " + err.Error()
		return p
	}
	tag, _, err := verifyPanelRelease(ctx, filepath.Join(repo, "scripts", "verify-release.sh"), dir)
	if err != nil {
		p.Note = "no usage panel release can be installed: " + err.Error()
		return p
	}
	p.Latest = strings.TrimPrefix(tag, "v")
	if p.Version == "" {
		p.Note = "the installed usage panel does not record its version"
		return p
	}
	p.Behind = versionNewer(p.Latest, p.Version)
	return p
}

// installedPanelVersion is the usage panel mod's version as Claude Code
// recorded it when the mod was installed, or "".
func installedPanelVersion() string {
	b, err := os.ReadFile(filepath.Join(homeDir(), ".claude", "plugins", "installed_plugins.json"))
	if err != nil {
		return ""
	}
	var f struct {
		Plugins map[string][]struct {
			Version string `json:"version"`
		} `json:"plugins"`
	}
	if json.Unmarshal(b, &f) != nil {
		return ""
	}
	for _, e := range f.Plugins["usage-panel@ccusage-panel"] {
		if e.Version != "" {
			return e.Version
		}
	}
	return ""
}

func (s *Server) handleUpgradeStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.checkUpgrade(r.Context(), r.URL.Query().Get("refresh") == "1"))
}

// handleUpgrade checks the version again, fresh, then starts the upgrade
// in a Terminal window only if there is one to install.
//
// With {"mode":"github"} it instead installs exactly the newest release,
// whatever is running: a reinstall, or a rollback from a local build (one
// with commits not released yet). That builds a temporary worktree of the
// release, so the checkout, its branch, its local commits and any
// uncommitted changes are left exactly as they are.
func (s *Server) handleUpgrade(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Mode string `json:"mode"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.Mode != "" && req.Mode != "upgrade" && req.Mode != "github" {
		http.Error(w, "mode must be upgrade or github", http.StatusBadRequest)
		return
	}
	st := s.checkUpgrade(r.Context(), true)
	// The usage panel is always brought up to date: by deploy.sh when Burst
	// upgrades, on its own otherwise, even when Burst's own check failed.
	_, haveRepo := s.repoDir()
	panel := st.Panel != nil && st.Panel.Installed && haveRepo
	if st.Error != "" && (req.Mode == "github" || !panel) {
		http.Error(w, "version check failed: "+st.Error, http.StatusBadGateway)
		return
	}
	if req.Mode == "github" {
		s.installGitHubVersion(w, st)
		return
	}
	burst := st.Error == "" && !st.UpToDate && st.CanUpgrade
	switch {
	case burst || panel:
	case st.UpToDate:
		http.Error(w, fmt.Sprintf("already up to date: running %s (%s), the newest release on GitHub", st.RunningVersion, st.RunningCommit), http.StatusConflict)
		return
	default:
		http.Error(w, "cannot upgrade: "+st.Reason, http.StatusConflict)
		return
	}
	dir, _ := s.repoDir()
	script, name := upgradeScript(dir, st), "upgrade"
	if !burst {
		script, name = panelUpdateScript(dir), "update-panel"
	}
	path, err := writeGeneratedScript(name, script)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var detail string
	switch {
	case burst:
		detail = fmt.Sprintf("Upgrading from %s (%s) to release %s (%s), %d new commit(s). A Terminal window is running it: "+
			"it checks the release's signature again, fast-forwards the checkout to it and runs scripts/deploy.sh, which tests, builds and restarts the gateway, "+
			"rolling back by itself if the new one is not healthy.",
			st.RunningVersion, st.RunningCommit, orDash(st.LatestVersion), st.LatestCommit, st.Behind)
		if panel {
			detail += " It brings the usage panel up to date as well."
		}
	case st.Error != "":
		detail = "Claude Burst's own version could not be checked (" + st.Error + "), so only the usage panel is updated, to its newest signed release. A Terminal window is running it. Run /reload-plugins --force in open Claude Code sessions afterwards."
	case st.UpToDate:
		detail = "Updating the usage panel to its newest signed release. A Terminal window is running it. Claude Burst is already current, so it is not restarted. Run /reload-plugins --force in open Claude Code sessions afterwards."
	default:
		detail = "Claude Burst cannot upgrade from here (" + st.Reason + "), so only the usage panel is updated, to its newest signed release. A Terminal window is running it. Run /reload-plugins --force in open Claude Code sessions afterwards."
	}
	if err := launchTerminal(path); err != nil {
		detail = fmt.Sprintf("could not open Terminal (%v). The script is ready, run it yourself:\n%s", err, path)
	}
	writeJSON(w, map[string]any{"detail": detail, "script": path, "status": st})
}

func (s *Server) installGitHubVersion(w http.ResponseWriter, st upgradeStatus) {
	dir, _ := s.repoDir()
	path, err := writeGeneratedScript("install-github", githubInstallScript(dir, st))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	what := "Reinstalling"
	switch {
	case st.Ahead > 0:
		what = fmt.Sprintf("Rolling back %d commit(s) not released yet:", st.Ahead)
	case st.Behind > 0:
		what = "Installing"
	}
	detail := fmt.Sprintf("%s release %s (%s) from GitHub in place of %s (%s). A Terminal window is running it from a temporary copy "+
		"of that release, so your checkout is not touched; scripts/deploy.sh tests, builds and restarts the gateway, "+
		"rolling back by itself if the new one is not healthy.",
		what, orDash(st.LatestVersion), st.LatestCommit, st.RunningVersion, st.RunningCommit)
	if err := launchTerminal(path); err != nil {
		detail = fmt.Sprintf("could not open Terminal (%v). The script is ready, run it yourself:\n%s", err, path)
	}
	writeJSON(w, map[string]any{"detail": detail, "script": path, "status": st})
}

// githubInstallScript deploys the newest release from a throwaway worktree.
// It verifies the release itself, with the checkout's own verifier, and
// builds the commit that printed, so what the dashboard saw is not trusted.
// deploy.sh builds the tree it lives in, so running the worktree's copy
// builds GitHub's code; it installs only the binary, so nothing refers to
// the worktree once it is removed.
func githubInstallScript(repo string, st upgradeStatus) string {
	return `#!/bin/zsh
# Generated by the Claude Burst dashboard's "Install GitHub version".
# Rewritten on every click.
set -uo pipefail
cd ` + shellQuote(repo) + ` || exit 1

echo "== Claude Burst: install the version on GitHub =="
echo ` + shellQuote("running: "+st.RunningVersion+" ("+st.RunningCommit+")") + `
echo

` + terminalFinish + `
git fetch --tags origin main || { echo "could not fetch from GitHub, nothing changed" >&2; finish 1; }
rel="$(/bin/bash scripts/verify-release.sh "$PWD")" || { echo "the newest release could not be verified (see above), nothing changed" >&2; finish 1; }
tag="${rel%% *}"; commit="${rel#* }"
echo "installing release $tag, signed by the release key: $(git log -1 --format='%h %s' "$commit")"
tmp="$(mktemp -d "${TMPDIR:-/tmp}/claude-burst-github.XXXXXX")" || finish 1
rmdir "$tmp"
git worktree add --quiet --detach "$tmp" "$commit" || { echo "could not check out release $tag, nothing changed" >&2; finish 1; }
echo "(built from a temporary copy at $tmp; your checkout is not touched)"
echo
# The usage panel is looked for beside the real checkout, not beside $tmp.
CLAUDE_BURST_REPO="$PWD" bash "$tmp/scripts/deploy.sh"
rc=$?
git worktree remove --force "$tmp" >/dev/null 2>&1 || rm -rf "$tmp"
git worktree prune >/dev/null 2>&1
echo
if (( rc == 0 )); then
  echo "Done: now running release $tag ($(git rev-parse --short "$commit")). Refresh the dashboard."
else
  echo "deploy.sh failed (exit $rc); see above. The gateway is on whatever deploy.sh left running." >&2
fi
finish $rc
`
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// upgradeScript repeats the checks rather than trusting the ones the
// dashboard made: time passes between the click and the window running.
func upgradeScript(repo string, st upgradeStatus) string {
	return `#!/bin/zsh
# Generated by the Claude Burst dashboard's Upgrade button. Rewritten on
# every click.
set -uo pipefail
cd ` + shellQuote(repo) + ` || exit 1

echo "== Claude Burst upgrade =="
echo ` + shellQuote("running: "+st.RunningVersion+" ("+st.RunningCommit+")") + `
echo

` + terminalFinish + `
[[ "$(git rev-parse --abbrev-ref HEAD)" == main ]] || { echo "not on main, nothing changed" >&2; finish 1; }
[[ -z "$(git status --porcelain)" ]] || { echo "uncommitted changes in $(pwd), nothing changed" >&2; finish 1; }
git fetch --tags origin main || { echo "could not fetch from GitHub, nothing changed" >&2; finish 1; }
# Verified here, by this checkout's own copy and before anything is merged,
# and the commit it prints is what is merged: a tag can be moved, a commit
# cannot.
rel="$(/bin/bash scripts/verify-release.sh "$PWD")" || { echo "the newest release could not be verified (see above), nothing changed" >&2; finish 1; }
tag="${rel%% *}"; commit="${rel#* }"

echo "new in release $tag, signed by the release key:"
git log --oneline HEAD.."$commit"
echo

git merge --ff-only "$commit" || { echo "cannot fast-forward to release $tag, nothing changed" >&2; finish 1; }
echo
bash scripts/deploy.sh
rc=$?
echo
if (( rc == 0 )); then
  echo "Upgrade complete: now running $(git rev-parse --short HEAD). Refresh the dashboard."
else
  echo "deploy.sh failed (exit $rc); see above. The checkout is on the new commit, the gateway on whatever deploy.sh left running." >&2
fi
finish $rc
`
}

// terminalFinish ends every generated script. On success the window closes
// itself after 10 seconds (a key keeps it open); on failure it waits, so
// the error can be read. Only in a real terminal: run any other way, it
// just exits.
const terminalFinish = `finish() {
  echo
  [[ -t 0 ]] || exit "$1"
  if (( $1 == 0 )); then
    if read -t 10 -k 1 "?Closing this window in 10 seconds; press any key to keep it open. "; then
      echo; read -k 1 "?Press any key to close this window..."
    fi
    local me; me="$(tty)"
    ( sleep 1; osascript -e 'on run {t}' -e 'tell application "Terminal" to repeat with w in windows' -e 'if tty of selected tab of w is t then close w' -e 'end repeat' -e 'end run' "$me" >/dev/null 2>&1 ) &!
    exit 0
  fi
  read -k 1 "?Press any key to close this window..."
  exit "$1"
}
`

// panelUpdateScript updates only the usage panel, when Burst has nothing
// to install. scripts/update-panel.sh verifies the panel's release itself
// and never fails, so a warning it prints keeps the window open instead.
func panelUpdateScript(repo string) string {
	return `#!/bin/zsh
# Generated by the Claude Burst dashboard's Upgrade button, when only the
# usage panel has anything to install. Rewritten on every click.
set -uo pipefail
cd ` + shellQuote(repo) + ` || exit 1

echo "== Usage panel update =="
echo

` + terminalFinish + `
log="$(mktemp "${TMPDIR:-/tmp}/claude-burst-panel.XXXXXX")" || finish 1
zsh scripts/update-panel.sh 2>&1 | tee "$log"
if grep -q '^WARNING' "$log"; then
  rm -f "$log"
  echo "The usage panel was not fully updated; see the warning above." >&2
  finish 1
fi
rm -f "$log"
echo "Done. Run /reload-plugins --force in open Claude Code sessions, or start new ones."
finish 0
`
}
