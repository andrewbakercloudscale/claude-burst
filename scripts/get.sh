#!/bin/bash
# Installs Claude Burst on a Mac from one pasted line, for someone who has
# never used a terminal (a Claude Desktop user, say):
#
#   curl -fsSL https://raw.githubusercontent.com/andrewbakercloudscale/claude-burst/main/scripts/get.sh | bash
#
#   1. checks this is a Mac it supports, and stops saying so if not
#   2. checks for Apple's command line tools (git, python3) and Go, and
#      installs Go when it is missing or too old: Homebrew when there is one,
#      else Go's own installer from go.dev, checked against go.dev's SHA256
#   3. finds the checkout install.sh recorded, or clones ~/claude-burst,
#      and brings it up to the newest signed release
#   4. installs in transparent mode, which asks for the password once
#   5. restarts Claude Desktop if it is open, after asking, so it trusts Burst
#   6. points at the Notification Center permission
#   7. runs scripts/diagnose.sh --check and ends with one green or red line
#
# Safe to rerun: an installed Mac is updated and checked, and a checkout with
# local edits is installed as it is, never pulled over. Bash 3.2 on purpose
# (what macOS has), and run from a pipe, so prompts read the terminal, not
# stdin, which is this script.
#
#   --dry-run             say what would happen and change nothing
#   CLAUDE_BURST_DIR=...  where to clone (default ~/claude-burst)
#
# Undo: burst-off turns Burst off; ./install.sh uninstall removes it.

set -uo pipefail

GH_URL="${CLAUDE_BURST_GIT_URL:-https://github.com/andrewbakercloudscale/claude-burst.git}"
SHARE="$HOME/.local/share/claude-burst"
CFG="$HOME/.config/claude-burst"
BIN="$HOME/.local/bin/claude-burst"
# Older than this is not supported: the hotspot watcher is built on a
# networksetup change in 14.4 (README, Requirements).
MIN_MACOS="14.4"
# The prompts read the terminal: stdin is this script when piped from curl.
# Tests point it at a file of answers.
TTY="${CLAUDE_BURST_TTY:-/dev/tty}"
# Where Go and Homebrew live when the shell that ran curl does not have them
# on its PATH yet (a fresh install writes /etc/paths.d, which only a new
# Terminal window reads). Tests empty it, so the developer's own Go is not
# found. GO_PKG_BIN is where Go's installer puts it.
GO_PKG_BIN="${CLAUDE_BURST_GO_PKG_BIN:-/usr/local/go/bin}"
EXTRA_PATH="${CLAUDE_BURST_GET_EXTRA_PATH-/opt/homebrew/bin:/usr/local/bin:$GO_PKG_BIN}"
[ -n "$EXTRA_PATH" ] && PATH="$PATH:$EXTRA_PATH"
export PATH

DRY=""
for a in "$@"; do
  case "$a" in
    --dry-run|-n) DRY=1 ;;
    -h|--help) sed -n '2,27p' "$0" 2>/dev/null || echo "usage: get.sh [--dry-run]"; exit 0 ;;
    *) echo "get.sh: unknown option $a (only --dry-run)" >&2; exit 2 ;;
  esac
done

STEP="starting"
STOPPED=""   # set by stop(): a clear stop is not a failure to ask about
REPO=""
KEEPALIVE=""

say() { printf '\n== %s\n' "$1"; STEP="$1"; }
plan() { printf '   would: %s\n' "$1"; }
# stop ends the run with a message that is the whole story (wrong OS, rerun
# after the tools install): no help line, since there is nothing to report.
stop() { echo; echo "$1"; STOPPED=1; exit "${2:-1}"; }
# ask is one y/N question on the terminal. No terminal means no.
ask() {
  local a=""
  printf '%s [y/N] ' "$1"
  [ -r "$TTY" ] && read -r a <"$TTY"
  echo
  [ "$a" = y ] || [ "$a" = Y ] || [ "$a" = yes ]
}
# audit puts how this ended in Burst's audit trail (the Audit tab), once
# there is a checkout to record it with.
# shellcheck disable=SC2329 # called from finish, the EXIT trap
audit() { [ -n "$REPO" ] && [ -f "$REPO/scripts/audit-add.sh" ] && sh "$REPO/scripts/audit-add.sh" script "$1" "Script: get.sh (one-line install)" "$2" >/dev/null 2>&1; return 0; }

# shellcheck disable=SC2329 # the EXIT trap
finish() {
  local rc=$?
  [ -n "$KEEPALIVE" ] && kill "$KEEPALIVE" 2>/dev/null
  [ -n "$DRY" ] && exit "$rc"
  if [ "$rc" -eq 0 ] || [ -n "$STOPPED" ]; then
    [ "$rc" -eq 0 ] && audit ok "done"
    exit "$rc"
  fi
  audit error "failed (exit $rc) while $STEP"
  local where="no checkout yet" off=""
  [ -n "$REPO" ] && where="$REPO at $(git -C "$REPO" log -1 --format=%h 2>/dev/null || echo unknown)"
  [ -x "$HOME/.local/bin/burst-off" ] && off=1
  echo
  printf '\033[31m%s\033[0m\n' "SETUP DID NOT FINISH: it stopped while $STEP (exit $rc)."
  [ -n "$off" ] && echo "If Claude Code stops answering meanwhile, run: burst-off   (burst-on puts Burst back)"
  echo "When asking for help, paste this line:"
  echo "  claude-burst get.sh failed while $STEP, exit $rc, macOS $(sw_vers -productVersion 2>/dev/null || echo '?') $(uname -m 2>/dev/null), $where"
  [ -n "$REPO" ] && echo "and the report from: bash $REPO/scripts/diagnose.sh   (it copies itself to the clipboard)"
  echo "Help: https://github.com/andrewbakercloudscale/claude-burst/issues"
  exit "$rc"
}
trap finish EXIT

# ver_ge A B: is version A (1.24.3) at least B (1.24)? Missing parts are 0.
ver_ge() {
  local IFS=.
  # shellcheck disable=SC2206 # splitting on dots is the point
  local a=($1) b=($2) i x y
  for i in 0 1 2; do
    x="${a[i]:-0}"; y="${b[i]:-0}"
    x="${x%%[!0-9]*}"; y="${y%%[!0-9]*}"
    [ "${x:-0}" -gt "${y:-0}" ] && return 0
    [ "${x:-0}" -lt "${y:-0}" ] && return 1
  done
  return 0
}

# go_version prints the installed Go's version (1.24.3), or nothing. From /
# with GOTOOLCHAIN=local: inside a module that names a newer toolchain,
# `go version` would download that one and report it instead.
go_version() {
  command -v go >/dev/null 2>&1 || return 0
  (cd / && GOTOOLCHAIN=local go version 2>/dev/null) | sed -n 's/.* go\([0-9][0-9.]*\).*/\1/p' | head -1
}

# ---------------------------------------------------------------- 1. the Mac
say "checking this Mac"
[ "$(id -u)" -eq 0 ] && stop "Run this without sudo: it asks for your password itself, only where it is needed."
OS="$(uname -s 2>/dev/null)"
[ "$OS" = Darwin ] || stop "Claude Burst runs on macOS only, and this is $OS. Nothing was changed."
MACOS="$(sw_vers -productVersion 2>/dev/null)"
ver_ge "${MACOS:-0}" "$MIN_MACOS" || stop "Claude Burst needs macOS $MIN_MACOS or later, and this Mac has ${MACOS:-an unknown version}. Update macOS (System Settings, General, Software Update) and run this again. Nothing was changed."
ARCH="$(uname -m)"
case "$ARCH" in
  arm64) GOARCH=arm64 ;;
  x86_64) GOARCH=amd64 ;;
  *) stop "Claude Burst does not support this Mac's processor ($ARCH). Nothing was changed." ;;
esac
echo "macOS $MACOS on $ARCH: supported"

# ------------------------------------------- 2a. Apple's command line tools
# Before git: without the tools, /usr/bin/git is a stub that opens the same
# install window and then fails, which reads as a broken script.
say "checking Apple's command line tools (git, python3)"
if xcode-select -p >/dev/null 2>&1 && command -v git >/dev/null 2>&1; then
  echo "installed"
elif [ -n "$DRY" ]; then
  plan "open Apple's installer for the command line tools (xcode-select --install), then stop until it finishes"
  NO_TOOLS=1
else
  xcode-select --install >/dev/null 2>&1 || true
  stop "Apple's command line tools are needed (they include git). A window has opened to install them:
click Install, wait for it to finish (a few minutes), then paste the same line again." 3
fi

# ------------------------------------------------------------ 3. the checkout
# Before Go: go.mod in the checkout says which Go is needed.
say "finding Claude Burst's checkout"
is_checkout() { [ -f "$1/install.sh" ] && [ -d "$1/.git" ]; }
TARGET_DIR="${CLAUDE_BURST_DIR:-}"
if [ -n "$TARGET_DIR" ]; then
  # Asked for by name: that folder, nowhere else.
  if is_checkout "$TARGET_DIR"; then REPO="$TARGET_DIR"
  elif [ -e "$TARGET_DIR" ] && [ -n "$(ls -A "$TARGET_DIR" 2>/dev/null)" ]; then
    stop "CLAUDE_BURST_DIR=$TARGET_DIR is a folder with other things in it, not a Claude Burst checkout. Pick an empty or new folder. Nothing was changed."
  fi
else
  # The path install.sh recorded first: the checkout can be anywhere. Then
  # the places the README and burst-reinstall put one.
  recorded="$(cat "$SHARE/repo" 2>/dev/null || true)"
  for d in "$recorded" "$HOME/claude-burst" "$HOME/claude-burst-repo" "$SHARE/src"; do
    [ -n "$d" ] || continue
    if is_checkout "$d"; then REPO="$d"; break; fi
  done
  TARGET_DIR="$HOME/claude-burst"
  if [ -z "$REPO" ] && [ -e "$TARGET_DIR" ] && [ -n "$(ls -A "$TARGET_DIR" 2>/dev/null)" ]; then
    stop "$TARGET_DIR already exists and is not a Claude Burst checkout, so it was left alone. Move it, or choose another folder:
  curl -fsSL https://raw.githubusercontent.com/andrewbakercloudscale/claude-burst/main/scripts/get.sh | CLAUDE_BURST_DIR=~/somewhere-else bash"
  fi
fi

# newest_release prints "<tag> <commit>" for the newest signed release in
# checkout $1 (scripts/verify-release.sh), as repair.sh does: the copy the
# installer keeps outside the checkout, or the checkout's own as it is
# BEFORE anything is merged into it. Exit 4: no verifier anywhere, a
# checkout from before releases were signed.
newest_release() {
  local v
  for v in "$SHARE/verify-release.sh" "$1/scripts/verify-release.sh"; do
    [ -x "$v" ] && [ -s "$(dirname "$v")/release-signers" ] || continue
    "$v" "$1"
    return
  done
  return 4
}

FRESH=""
if [ -z "$REPO" ]; then
  if [ -n "$DRY" ]; then
    plan "clone $GH_URL to $TARGET_DIR and put it on the newest signed release"
  else
    echo "none found: cloning to $TARGET_DIR"
    STEP="cloning Claude Burst from GitHub"
    mkdir -p "$(dirname "$TARGET_DIR")" || exit 1
    # A full clone, as repair.sh makes: its own verifier is what checks the
    # release. One that does not verify is deleted below, so it is never
    # taken for one that did.
    git clone -q "$GH_URL" "$TARGET_DIR" || exit 1
    REPO="$TARGET_DIR"
    FRESH=1
  fi
fi

if [ -n "$REPO" ]; then
  echo "$REPO"
  say "bringing it up to date"
  branch="$(git -C "$REPO" symbolic-ref --quiet --short HEAD 2>/dev/null || true)"
  if [ -n "$FRESH" ]; then
    rel="$(newest_release "$REPO")" || {
      rm -rf "$REPO"
      echo "The newest release on GitHub could not be verified (see above), so nothing was installed."
      REPO=""
      exit 1
    }
    git -C "$REPO" checkout -q -B main "${rel#* }" || exit 1
    echo "on release ${rel%% *}, signed by the release key"
  elif [ -n "$(git -C "$REPO" status --porcelain --untracked-files=no 2>/dev/null)" ]; then
    echo "it has local edits: left exactly as it is and installed from as it is (not updated)"
  elif [ "$branch" != main ]; then
    echo "it is on ${branch:-a detached commit}, not main: installed from as it is (not updated)"
  elif [ -n "$DRY" ]; then
    plan "fetch from GitHub and fast-forward $REPO to the newest signed release"
  elif ! git -C "$REPO" fetch -q --tags origin; then
    echo "WARNING: could not reach GitHub; installing the copy already here."
  else
    rel="$(newest_release "$REPO")"; rc=$?
    if [ $rc -eq 0 ] && git -C "$REPO" merge -q --ff-only "${rel#* }"; then
      echo "on release ${rel%% *}, signed by the release key"
    elif [ $rc -eq 0 ]; then
      echo "WARNING: this checkout cannot fast-forward to release ${rel%% *}; installing it as it is."
    elif [ $rc -eq 4 ] && git -C "$REPO" merge -q --ff-only origin/main; then
      # From before releases were signed: nothing to check with, brought up
      # to date this once, as repair.sh does. It has the verifier from now on.
      echo "up to date with GitHub (this copy predates signed releases, so this one update was not verified)"
    else
      echo "WARNING: the newest release could not be verified or merged (see above); installing the copy already here."
    fi
  fi
  git -C "$REPO" log --oneline -1 2>/dev/null
fi

# ---------------------------------------------------------------- 2b. Go
say "checking Go (Claude Burst is built from source on this Mac)"
MIN_GO="$(sed -n 's/^go \([0-9][0-9.]*\).*/\1/p' "${REPO:-/nonexistent}/go.mod" 2>/dev/null | head -1)"
[ -n "$MIN_GO" ] || MIN_GO="1.24"
HAVE_GO="$(go_version)"
GO_HOW=""
if [ -n "$HAVE_GO" ] && ver_ge "$HAVE_GO" "$MIN_GO"; then
  echo "Go $HAVE_GO: fine (needs $MIN_GO or later)"
else
  if [ -n "$HAVE_GO" ]; then echo "Go $HAVE_GO is older than the $MIN_GO this needs"; else echo "Go is not installed"; fi
  if command -v brew >/dev/null 2>&1; then GO_HOW=brew; else GO_HOW=pkg; fi
  case "$GO_HOW" in
    brew) echo "it will be installed with Homebrew" ;;
    pkg) echo "it will be installed with Go's own installer from go.dev (needs your password)" ;;
  esac
fi

# ------------------------------------------------------------- the plan
# Which mode install.sh gets. Transparent unless this Mac already chose:
# config.json's mode is kept on a rerun, and Claude Code already sent
# through another gateway gets base-url, both decided by install.sh
# without a question (choose_intercept_mode). Forcing transparent there
# would put Burst where it sees nothing.
MODE_SET="$(/usr/bin/python3 -c 'import json,sys;print((json.load(open(sys.argv[1])).get("intercept") or {}).get("mode") or "")' "$CFG/config.json" 2>/dev/null || true)"
FOREIGN="$(/usr/bin/python3 -c 'import json,sys;print((json.load(open(sys.argv[1])).get("env") or {}).get("ANTHROPIC_BASE_URL") or "")' "$HOME/.claude/settings.json" 2>/dev/null || true)"
case "$FOREIGN" in http://127.0.0.1:*|"") FOREIGN="" ;; esac
MODE=transparent
if [ -n "${CLAUDE_BURST_MODE:-}" ]; then MODE="$CLAUDE_BURST_MODE"
elif [ -n "$MODE_SET" ]; then MODE="$MODE_SET"
elif [ -n "$FOREIGN" ]; then MODE=base-url
fi

# Claude Desktop builds its CA bundle when it starts, from the System
# keychain (docs/claude-desktop.md): one open during the install refuses
# the gateway until it restarts. Only asked when its bundle does not hold
# Burst's CA already, so a rerun on a working Mac asks nothing.
DESKTOP_APP="${CLAUDE_BURST_APPLICATIONS_DIR:-/Applications}/Claude.app"
[ -d "$DESKTOP_APP" ] || DESKTOP_APP="$HOME/Applications/Claude.app"
DESKTOP_BUNDLE="$HOME/Library/Application Support/Claude/ca-bundle.pem"
desktop_trusts_burst() {
  local body
  body="$(grep -v -- '-----' "$CFG/ca/ca-cert.pem" 2>/dev/null | tr -d '\r\n')"
  [ -n "$body" ] && [ -f "$DESKTOP_BUNDLE" ] && tr -d '\r\n' <"$DESKTOP_BUNDLE" | grep -qF -- "$body"
}
desktop_running() { pgrep -xq Claude 2>/dev/null; }
DESKTOP_RESTART=""   # y: restart it, n: the user said no, "": not needed
DESKTOP_WAS_OPEN=""
if [ -d "$DESKTOP_APP" ] && [ "$MODE" = transparent ] && desktop_running && ! desktop_trusts_burst; then
  DESKTOP_WAS_OPEN=1
fi

# Root is needed for Go's installer and for transparent mode's /etc/hosts,
# pf redirect and System keychain trust. Asked for once, here, with the
# reason, and kept fresh until the end: install-proxy.sh then finds it
# cached instead of stopping with nothing to prompt on.
NEED_ROOT=""
[ "$MODE" = transparent ] && NEED_ROOT=1
[ "$GO_HOW" = pkg ] && NEED_ROOT=1

if [ -n "$DRY" ]; then
  say "the plan (dry run: nothing was changed)"
  [ -n "${NO_TOOLS:-}" ] && plan "stop after opening the command line tools installer; everything below happens on the rerun"
  case "$GO_HOW" in
    brew) plan "brew install go (Go $MIN_GO or later)" ;;
    pkg) plan "download Go's newest macOS $GOARCH installer from go.dev, check its SHA256 against go.dev's list, and install it with sudo installer" ;;
  esac
  [ -n "$NEED_ROOT" ] && plan "ask for your Mac password once (sudo)"
  plan "run ${REPO:-$TARGET_DIR}/install.sh in $MODE mode (builds Claude Burst, starts it, adds the Claude Burst app)"
  [ "$MODE" = transparent ] && plan "make sure Burst's certificate is trusted system-wide, so Claude Desktop accepts it"
  if [ -n "$DESKTOP_WAS_OPEN" ]; then plan "ask whether to quit and reopen Claude Desktop (it is open), so it trusts Burst"
  elif [ -d "$DESKTOP_APP" ]; then plan "leave Claude Desktop alone (not open, or it trusts Burst already)"
  fi
  plan "check Notification Center permission for Claude Burst"
  plan "run scripts/diagnose.sh --check and print a summary"
  exit 0
fi

[ -n "$DESKTOP_WAS_OPEN$NEED_ROOT" ] && say "questions first, so the rest can run by itself"
if [ -n "$DESKTOP_WAS_OPEN" ]; then
  echo "Claude Desktop is open. It has to quit and reopen once at the end to trust Burst"
  echo "(it reads trusted certificates only as it starts). A Code or Cowork task running"
  echo "in it now would be interrupted; chats are kept."
  if ask "Quit and reopen Claude Desktop automatically at the end?"; then DESKTOP_RESTART=y; else DESKTOP_RESTART=n; fi
fi
if [ -n "$NEED_ROOT" ]; then
  echo "Your Mac password is needed once (it is typed into macOS's own sudo, never stored):"
  [ "$GO_HOW" = pkg ] && echo "  - to install Go, which builds Claude Burst"
  [ "$MODE" = transparent ] && echo "  - to send api.anthropic.com to Burst on this Mac (/etc/hosts and a firewall rule)" &&
    echo "    and to trust Burst's own certificate, which only works for api.anthropic.com"
  echo "Nothing is typed back on screen while you type the password; press Return after it."
  if [ ! -r "$TTY" ]; then
    stop "There is no terminal to ask for the password on. Paste the line into the Terminal app and run it there."
  fi
  STEP="asking for your password"
  # shellcheck disable=SC2024 # the terminal is sudo's input, not a file it reads
  sudo -v <"$TTY" || exit 1
  # Kept fresh while the build runs, which can take longer than sudo's
  # five minutes on a slow Mac. Its output goes nowhere: a sleep holding
  # the terminal's pipe would keep a caller reading it waiting 50 seconds.
  ( while sleep 50; do kill -0 $$ 2>/dev/null && sudo -n true 2>/dev/null || exit; done ) >/dev/null 2>&1 &
  KEEPALIVE=$!
  # Not a job of this shell: killing it at the end prints no "Terminated".
  disown "$KEEPALIVE" 2>/dev/null
fi

# ------------------------------------------------------------ 2c. install Go
if [ "$GO_HOW" = brew ]; then
  say "installing Go with Homebrew"
  if brew list --versions go >/dev/null 2>&1; then brew upgrade go || exit 1
  else brew install go || exit 1; fi
  # Ahead of an older Go elsewhere on the PATH, which would fail the check below.
  PATH="$(brew --prefix 2>/dev/null || echo /opt/homebrew)/bin:$PATH"
elif [ "$GO_HOW" = pkg ]; then
  say "installing Go from go.dev"
  tmp="$(mktemp -d)" || exit 1
  curl -fsSL --max-time 60 "https://go.dev/dl/?mode=json" -o "$tmp/go.json" || exit 1
  # The newest stable macOS installer for this processor: its file name and
  # SHA256, from go.dev's own list. Both are checked for shape before use.
  read -r PKG SUM <<EOF
$(/usr/bin/python3 -c '
import json, sys
for r in json.load(open(sys.argv[1])):
    if not r.get("stable"):
        continue
    for f in r.get("files", []):
        if f.get("os") == "darwin" and f.get("arch") == sys.argv[2] and f.get("kind") == "installer":
            print(f["filename"], f["sha256"]); sys.exit(0)
' "$tmp/go.json" "$GOARCH" 2>/dev/null)
EOF
  if ! printf '%s' "${PKG:-}" | grep -Eq '^go[0-9][0-9.]*\.darwin-(arm64|amd64)\.pkg$' || ! printf '%s' "${SUM:-}" | grep -Eq '^[0-9a-f]{64}$'; then
    echo "go.dev's download list had no macOS $GOARCH installer this script could read."
    exit 1
  fi
  echo "downloading $PKG"
  curl -fsSL --max-time 600 "https://go.dev/dl/$PKG" -o "$tmp/$PKG" || exit 1
  got="$(shasum -a 256 "$tmp/$PKG" | awk '{print $1}')"
  if [ "$got" != "$SUM" ]; then
    echo "The download's SHA256 does not match go.dev's ($got, expected $SUM), so it was not installed."
    rm -rf "$tmp"
    exit 1
  fi
  echo "SHA256 matches go.dev's: $SUM"
  sudo -n installer -pkg "$tmp/$PKG" -target / || exit 1
  rm -rf "$tmp"
  PATH="$GO_PKG_BIN:$PATH"
fi
if [ -n "$GO_HOW" ]; then
  HAVE_GO="$(go_version)"
  if [ -z "$HAVE_GO" ] || ! ver_ge "$HAVE_GO" "$MIN_GO"; then
    echo "Go is still ${HAVE_GO:-missing} after installing it; Claude Burst needs $MIN_GO or later."
    exit 1
  fi
  echo "Go $HAVE_GO installed"
fi

# ------------------------------------------------------------ 4. install
say "installing Claude Burst ($MODE mode); building takes a minute or two"
# CLAUDE_BURST_FORCE: Claude Code sessions open now are told to restart
# rather than holding transparent mode back (repair.sh does the same).
# SIGNING=no unless asked for: setting up the signing certificate asks for
# the login Keychain password too, one more prompt than this needs.
# install.sh reads the terminal, not this script, as its stdin.
env_mode=""
[ -n "${CLAUDE_BURST_MODE:-}" ] || [ -n "$MODE_SET" ] || [ -n "$FOREIGN" ] || env_mode=transparent
if [ -r "$TTY" ]; then
  CLAUDE_BURST_MODE="${CLAUDE_BURST_MODE:-$env_mode}" CLAUDE_BURST_FORCE=1 CLAUDE_BURST_SIGNING="${CLAUDE_BURST_SIGNING:-no}" \
    /bin/zsh "$REPO/install.sh" <"$TTY" || exit 1
else
  CLAUDE_BURST_MODE="${CLAUDE_BURST_MODE:-$env_mode}" CLAUDE_BURST_FORCE=1 CLAUDE_BURST_SIGNING="${CLAUDE_BURST_SIGNING:-no}" \
    /bin/zsh "$REPO/install.sh" </dev/null || exit 1
fi
MODE="$(/usr/bin/python3 -c 'import json,sys;print((json.load(open(sys.argv[1])).get("intercept") or {}).get("mode") or "base-url")' "$CFG/config.json" 2>/dev/null || echo "$MODE")"

TRUST_OK=1
if [ "$MODE" = transparent ]; then
  say "checking that Burst's certificate is trusted system-wide"
  # install-proxy.sh tries this with sudo -n and only warns when macOS
  # refuses it, which it can: trusting a root certificate may need a macOS
  # window (password or Touch ID) as well as sudo. Without it Claude
  # Desktop's sessions refuse the gateway, so here it is asked for again,
  # with the person at the keyboard.
  ca_trusted() { security find-certificate -c "claude-burst local CA" /Library/Keychains/System.keychain >/dev/null 2>&1; }
  if ca_trusted; then
    echo "trusted"
  else
    echo "Not yet. macOS may show a window asking for your password or Touch ID to allow it."
    # shellcheck disable=SC2024 # as above
    sudo "$REPO/scripts/trust-ca-systemwide.sh" <"$TTY" || true
    if ca_trusted; then echo "trusted"
    else
      TRUST_OK=""
      echo "WARNING: it is still not trusted, so Claude Desktop's Code and Cowork sessions will refuse Burst."
      echo "  Run this in Terminal and allow it when macOS asks: sudo $REPO/scripts/trust-ca-systemwide.sh"
    fi
  fi
fi
[ -n "$KEEPALIVE" ] && kill "$KEEPALIVE" 2>/dev/null; KEEPALIVE=""

# ------------------------------------------------------- 5. Claude Desktop
DESKTOP_LINE=""
if [ -d "$DESKTOP_APP" ] && [ "$MODE" = transparent ]; then
  say "Claude Desktop"
  if ! desktop_running; then
    echo "not open: it trusts Burst from the next time it opens"
  elif desktop_trusts_burst; then
    echo "open, and it trusts Burst already: nothing to do"
  elif [ "$DESKTOP_RESTART" = y ]; then
    echo "quitting Claude Desktop so it picks up Burst's certificate..."
    osascript -e 'tell application "Claude" to quit' >/dev/null 2>&1 || true
    for _ in $(seq 1 30); do desktop_running || break; sleep 1; done
    if desktop_running; then
      DESKTOP_LINE="Claude Desktop did not quit (it may be asking you something). Quit it yourself (Claude, Quit) and open it again."
      echo "$DESKTOP_LINE"
    else
      open -a Claude >/dev/null 2>&1 || open "$DESKTOP_APP" >/dev/null 2>&1 || true
      echo "reopened"
    fi
  else
    DESKTOP_LINE="Quit Claude Desktop (Claude, Quit; not just the window) and open it again, so it trusts Burst."
    echo "$DESKTOP_LINE"
  fi
fi

# ------------------------------------------------- 6. Notification Center
# Where nothing shows Burst's toasts (Claude Desktop has none), alerts go to
# Notification Center. install.sh's `claude-burst app install` has macOS ask
# when it never has; a "denied" is never asked again, so open the setting.
say "Notification Center"
NOTIFIER="$HOME/Applications/Claude Burst.app/Contents/Helpers/Claude Burst Notifier.app/Contents/MacOS/claude-burst-notifier"
NOTIFY_LINE=""
if [ -x "$NOTIFIER" ]; then
  st="$("$NOTIFIER" status 2>/dev/null | tail -1 | sed -n 's/.*"status" *: *"\([^"]*\)".*/\1/p')"
  case "$st" in
    denied)
      "$BIN" app notifications || true
      NOTIFY_LINE="Notifications are off for Claude Burst: System Settings is open there; turn on Allow notifications." ;;
    not-determined)
      NOTIFY_LINE="A Claude Burst notification asks whether it may send notifications: choose Allow." ;;
    "") NOTIFY_LINE="Could not read the notification setting; check it any time with: claude-burst app notifications" ;;
    *) echo "allowed ($st)" ;;
  esac
else
  NOTIFY_LINE="The notification helper is not installed; alerts still show on the dashboard. Retry: claude-burst app install"
fi
[ -n "$NOTIFY_LINE" ] && echo "$NOTIFY_LINE"

# ---------------------------------------------------------------- 7. check
say "checking that it works"
CHECK_OUT="$(bash "$REPO/scripts/diagnose.sh" --check 2>&1)"; CHECK_RC=$?
printf '%s\n' "$CHECK_OUT"

DASH="$(/usr/bin/python3 -c 'import json,sys;print(json.load(open(sys.argv[1])).get("admin_listen") or "127.0.0.1:7788")' "$CFG/config.json" 2>/dev/null || echo 127.0.0.1:7788)"
echo
if [ "$CHECK_RC" -eq 0 ] && [ -n "$TRUST_OK" ]; then
  printf '\033[32m%s\033[0m\n' "DONE: Claude Burst is installed and working."
else
  printf '\033[31m%s\033[0m\n' "INSTALLED, BUT A CHECK FAILED: see the FAIL lines above, each says what to run."
fi
echo "  Mode:       $MODE   Version: $("$BIN" version 2>/dev/null | head -1)"
echo "  Checkout:   $REPO"
echo "  Dashboard:  http://$DASH   (or Cmd-Space, type burst, Return)"
echo "  Help:       http://127.0.0.1:7789   (the support console, up even when Burst is not)"
echo "  Turn off:   burst-off   (burst-on turns it back on)"
echo "  Remove:     cd $REPO && ./install.sh uninstall"
[ -n "$DESKTOP_LINE" ] && echo "  To do:      $DESKTOP_LINE"
[ -n "$NOTIFY_LINE" ] && echo "  To do:      $NOTIFY_LINE"
echo "  Claude Code open in a terminal: quit it and run claude --continue (keeps the conversation)."
if [ "$CHECK_RC" -ne 0 ] || [ -z "$TRUST_OK" ]; then
  STEP="checking that it works (diagnose.sh --check)"
  exit 1
fi
exit 0
