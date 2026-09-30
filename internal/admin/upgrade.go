package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Upgrade: the header's button that installs the latest version from
// GitHub. It checks the version first: the running binary's own commit
// (Go stamps it into every build made in the repo) against origin/main
// after a fetch, and the version number origin/main declares. Only when
// the running build is behind, and the checkout can fast-forward cleanly,
// does it offer the upgrade.
//
// The upgrade itself runs in a Terminal window, like Install: deploy.sh
// restarts this very process, so it cannot run as a child of it.

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

const upgradeCheckEvery = 10 * time.Minute

type upgradeStatus struct {
	CheckedAt      time.Time `json:"checked_at"`
	RunningVersion string    `json:"running_version"`
	RunningCommit  string    `json:"running_commit"`
	RunningDirty   bool      `json:"running_dirty,omitempty"` // built from uncommitted changes
	LatestVersion  string    `json:"latest_version,omitempty"`
	LatestCommit   string    `json:"latest_commit,omitempty"`
	Behind         int       `json:"behind"`                // commits on GitHub the running build lacks
	Ahead          int       `json:"ahead"`                 // commits in the running build GitHub lacks (not pushed yet)
	NewCommits     []string  `json:"new_commits,omitempty"` // their subjects, newest first
	UpToDate       bool      `json:"up_to_date"`
	CanUpgrade     bool      `json:"can_upgrade"`
	Reason         string    `json:"reason,omitempty"` // why not, when it cannot
	Error          string    `json:"error,omitempty"`  // the check itself failed
}

var (
	upgradeMu   sync.Mutex
	upgradeLast *upgradeStatus
)

var versionConst = regexp.MustCompile(`(?m)^const version = "([^"]+)"`)

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
	upgradeLast = &st
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
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err := runGit(ctx, dir, "fetch", "--quiet", "origin", "main"); err != nil {
		return fail("could not reach GitHub: %v", err)
	}
	latest, err := runGit(ctx, dir, "rev-parse", "origin/main")
	if err != nil {
		return fail("%v", err)
	}
	st.LatestCommit = short(latest)
	if src, err := runGit(ctx, dir, "show", "origin/main:cmd/claude-burst/main.go"); err == nil {
		if m := versionConst.FindStringSubmatch(src); m != nil {
			st.LatestVersion = m[1]
		}
	}

	// Compare what is RUNNING, not what is checked out: a checkout pulled
	// but never deployed is still an old gateway.
	base := rev
	if base == "" {
		if base, err = runGit(ctx, dir, "rev-parse", "HEAD"); err != nil {
			return fail("%v", err)
		}
	}
	if n, err := runGit(ctx, dir, "rev-list", "--count", base+"..origin/main"); err == nil {
		st.Behind, _ = strconv.Atoi(n)
	} else {
		return fail("the running build's commit %s is not in this checkout: %v", short(base), err)
	}
	if n, err := runGit(ctx, dir, "rev-list", "--count", "origin/main.."+base); err == nil {
		st.Ahead, _ = strconv.Atoi(n)
	}
	if st.Behind > 0 {
		if log, err := runGit(ctx, dir, "log", "--format=%s", "-n", "10", base+"..origin/main"); err == nil && log != "" {
			st.NewCommits = strings.Split(log, "\n")
		}
	}
	st.UpToDate = st.Behind == 0

	// Can the checkout fast-forward to it? deploy.sh builds the working
	// tree, so uncommitted changes would ship alongside the upgrade.
	switch {
	case st.UpToDate && st.Ahead > 0:
		st.Reason = "running a newer build than GitHub has"
	case st.UpToDate:
		st.Reason = "already running the latest version"
	default:
		if b, err := runGit(ctx, dir, "rev-parse", "--abbrev-ref", "HEAD"); err != nil || b != "main" {
			st.Reason = "the checkout is not on main (it is on " + b + ")"
		} else if p, err := runGit(ctx, dir, "status", "--porcelain"); err != nil || p != "" {
			st.Reason = "the checkout has uncommitted changes, which the deploy would ship too; commit or stash them first"
		} else if a, err := runGit(ctx, dir, "rev-list", "--count", "origin/main..HEAD"); err != nil || a != "0" {
			st.Reason = "the checkout has local commits GitHub does not, so it cannot fast-forward"
		} else {
			st.CanUpgrade = true
		}
	}
	return st
}

func (s *Server) handleUpgradeStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.checkUpgrade(r.Context(), r.URL.Query().Get("refresh") == "1"))
}

// handleUpgrade checks the version again, fresh, then starts the upgrade
// in a Terminal window only if there is one to install.
//
// With {"mode":"github"} it instead installs exactly what GitHub's main is,
// whatever is running: a reinstall, or a rollback from a local build (one
// with commits not pushed yet). That builds a temporary worktree of
// origin/main, so the checkout, its branch, its local commits and any
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
	if st.Error != "" {
		http.Error(w, "version check failed: "+st.Error, http.StatusBadGateway)
		return
	}
	if req.Mode == "github" {
		s.installGitHubVersion(w, st)
		return
	}
	if st.UpToDate {
		http.Error(w, fmt.Sprintf("already up to date: running %s (%s), the latest on GitHub", st.RunningVersion, st.RunningCommit), http.StatusConflict)
		return
	}
	if !st.CanUpgrade {
		http.Error(w, "cannot upgrade: "+st.Reason, http.StatusConflict)
		return
	}
	dir, _ := s.repoDir()
	path, err := writeGeneratedScript("upgrade", upgradeScript(dir, st))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	detail := fmt.Sprintf("Upgrading from %s (%s) to %s (%s), %d new commit(s). A Terminal window is running it: "+
		"it fast-forwards the checkout and runs scripts/deploy.sh, which tests, builds and restarts the gateway, "+
		"rolling back by itself if the new one is not healthy.",
		st.RunningVersion, st.RunningCommit, orDash(st.LatestVersion), st.LatestCommit, st.Behind)
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
		what = fmt.Sprintf("Rolling back %d commit(s) not pushed yet:", st.Ahead)
	case st.Behind > 0:
		what = "Installing"
	}
	detail := fmt.Sprintf("%s GitHub's version %s (%s) in place of %s (%s). A Terminal window is running it from a temporary copy "+
		"of GitHub's main, so your checkout is not touched; scripts/deploy.sh tests, builds and restarts the gateway, "+
		"rolling back by itself if the new one is not healthy.",
		what, orDash(st.LatestVersion), st.LatestCommit, st.RunningVersion, st.RunningCommit)
	if err := launchTerminal(path); err != nil {
		detail = fmt.Sprintf("could not open Terminal (%v). The script is ready, run it yourself:\n%s", err, path)
	}
	writeJSON(w, map[string]any{"detail": detail, "script": path, "status": st})
}

// githubInstallScript deploys origin/main from a throwaway worktree.
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
echo "running: ` + st.RunningVersion + ` (` + st.RunningCommit + `)"
echo

# On success the window closes itself after 10 seconds (a key keeps it
# open); on failure it waits, so the error can be read. Only in a real
# terminal: run any other way, it just exits.
finish() {
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

git fetch origin main || { echo "could not fetch from GitHub, nothing changed" >&2; finish 1; }
echo "installing: $(git log -1 --format='%h %s' origin/main)"
tmp="$(mktemp -d "${TMPDIR:-/tmp}/claude-burst-github.XXXXXX")" || finish 1
rmdir "$tmp"
git worktree add --quiet --detach "$tmp" origin/main || { echo "could not check out GitHub's main, nothing changed" >&2; finish 1; }
echo "(built from a temporary copy at $tmp; your checkout is not touched)"
echo
bash "$tmp/scripts/deploy.sh"
rc=$?
git worktree remove --force "$tmp" >/dev/null 2>&1 || rm -rf "$tmp"
git worktree prune >/dev/null 2>&1
echo
if (( rc == 0 )); then
  echo "Done: now running GitHub's $(git rev-parse --short origin/main). Refresh the dashboard."
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
echo "running: ` + st.RunningVersion + ` (` + st.RunningCommit + `)"
echo

# On success the window closes itself after 10 seconds (a key keeps it
# open); on failure it waits, so the error can be read. Only in a real
# terminal: run any other way, it just exits.
finish() {
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

[[ "$(git rev-parse --abbrev-ref HEAD)" == main ]] || { echo "not on main, nothing changed" >&2; finish 1; }
[[ -z "$(git status --porcelain)" ]] || { echo "uncommitted changes in $(pwd), nothing changed" >&2; finish 1; }
git fetch origin main || { echo "could not fetch from GitHub, nothing changed" >&2; finish 1; }

echo "new on GitHub:"
git log --oneline HEAD..origin/main
echo

git merge --ff-only origin/main || { echo "cannot fast-forward, nothing changed" >&2; finish 1; }
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
