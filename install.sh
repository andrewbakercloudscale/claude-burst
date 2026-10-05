#!/bin/zsh
# Claude Burst installer.
#
# Usage:
#   ./install.sh              install (or reinstall/update) claude-burst
#   ./install.sh uninstall    remove everything Burst installed: the transparent
#                             mode redirect and root daemons (asks for sudo only
#                             when they are there), the self-heal watchdog,
#                             every Claude Code hook, the LaunchAgent and the
#                             binary, then check and exit 1 if anything is left
#   ./install.sh uninstall --purge
#                             the same, and delete ~/.config/claude-burst too
#
# Uninstall without --purge keeps ~/.config/claude-burst (config, state,
# metrics, backups), and it always keeps the macOS Keychain secret: neither is
# something you want wiped by an accidental rerun. config.json is left as it
# was, so a reinstall comes back with the same features on.

# This is a zsh script (${0:A:h}, read "var?prompt"). `bash install.sh` dies at
# the first zsh-only expansion with "A: unbound variable", so hand it to zsh.
if [ -z "${ZSH_VERSION:-}" ]; then exec /bin/zsh "$0" "$@"; fi
set -euo pipefail

# Resolved here, at top level: inside a function zsh sets $0 to the FUNCTION's
# name, so ${0:A:h} there is the caller's working directory, not the repo.
ROOT="${0:A:h}"
LABEL="ninja.andrewbaker.claude-burst"
PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"
INSTALL_DIR="$HOME/.local/bin"
TARGET="$INSTALL_DIR/claude-burst"
OFF_DIR="$HOME/.local/share/claude-burst"

if [[ "$(uname -s)" != "Darwin" ]]; then
  echo "claude-burst is Mac-only in this MVP." >&2
  exit 1
fi

# offer_signing asks, once per Mac, to set up the local signing certificate
# (scripts/signing-setup.sh). Without it every build is signed ad hoc, macOS
# treats each update as a new program, and folder permissions such as Desktop
# access are asked for again after every update. CLAUDE_BURST_SIGNING=yes or
# no answers it without asking.
offer_signing() {
  source "$ROOT/scripts/codesign.sh"
  burst_sign_ready && return 0
  [[ "${CLAUDE_BURST_SIGNING:-ask}" == "no" ]] && return 0
  if [[ "${CLAUDE_BURST_SIGNING:-ask}" != "yes" ]]; then
    if [[ ! -t 0 ]]; then
      echo "Optional: run scripts/signing-setup.sh once so macOS remembers folder permissions across updates."
      return 0
    fi
    local answer
    echo
    echo "macOS asks again for folder access (Desktop, Documents) after every update unless"
    echo "builds are signed with one local certificate. Setting it up asks for your password"
    echo "or Touch ID, and possibly your login Keychain password."
    read -r "answer?Sign builds so macOS remembers your Allow? [Y/n] "
    [[ -z "$answer" || "$answer" == [Yy]* ]] || { echo "Skipped. Run scripts/signing-setup.sh any time."; return 0; }
  fi
  zsh "$ROOT/scripts/signing-setup.sh" --identity-only || echo "WARNING: signing setup did not finish; this build is signed ad hoc. Rerun scripts/signing-setup.sh any time."
}

# keep_awake_lid_closed (default false) keeps Claude Code in Ghostty, and
# Remote Control, running with the lid shut; keep_awake_lid_closed_power
# picks ac (default, plugged in only) or always. Re-applied from config.json
# on every install, so a reinstall or a new machine with the same config ends
# up in the same state. False touches nothing and asks for no password.

# Ghostty's App Nap as it was before Claude Burst first changed it, so an
# uninstall or the dashboard's restore button puts back exactly that. Same
# file and rules as internal/keepawake.SetGhosttyAppNap.
APPNAP_ORIGINAL="$HOME/.config/claude-burst/ghostty-appnap.original"
appnap_record_original() {
  [[ -f "$APPNAP_ORIGINAL" ]] && return 0
  local was
  was="$(defaults read com.mitchellh.ghostty NSAppSleepDisabled 2>/dev/null)" || was=unset
  [[ "$was" == 1 ]] && was=unset   # set by an older Claude Burst that kept no record
  mkdir -p "${APPNAP_ORIGINAL:h}" && echo "$was" > "$APPNAP_ORIGINAL"
}
appnap_restore_original() {
  local was=unset
  [[ -f "$APPNAP_ORIGINAL" ]] && was="$(<"$APPNAP_ORIGINAL")"
  case "$was" in
    0) defaults write com.mitchellh.ghostty NSAppSleepDisabled -bool NO ;;
    1) defaults write com.mitchellh.ghostty NSAppSleepDisabled -bool YES ;;
    *) defaults delete com.mitchellh.ghostty NSAppSleepDisabled >/dev/null 2>&1 || true ;;
  esac
  rm -f "$APPNAP_ORIGINAL"
}

# Which intercept mode a fresh install gets. Transparent is the default: it
# keeps Remote Control, which base-url turns off. Base-url is the second
# choice, taken only when asked for, when Claude Code already goes through
# another gateway (enable adopts it, and transparent would see nothing), or
# when there is no terminal to ask for the password on. A reinstall keeps the
# mode config.json already names. CLAUDE_BURST_MODE=transparent|base-url
# skips the question. Prompts go to stderr: the answer is the only stdout.
choose_intercept_mode() {
  local cfgjson="$1" set cur answer
  if [[ -n "${CLAUDE_BURST_MODE:-}" ]]; then echo "$CLAUDE_BURST_MODE"; return; fi
  set="$(python3 -c "import json;print((json.load(open('$cfgjson')).get('intercept') or {}).get('mode') or '')" 2>/dev/null || true)"
  if [[ -n "$set" ]]; then echo "$set"; return; fi
  cur="$(python3 -c "import json;print((json.load(open('$HOME/.claude/settings.json')).get('env') or {}).get('ANTHROPIC_BASE_URL') or '')" 2>/dev/null || true)"
  if [[ -n "$cur" && "$cur" != http://127.0.0.1:* ]]; then
    echo "Intercept mode: base-url, because Claude Code already goes through $cur (Burst sits in front of it)." >&2
    echo base-url; return
  fi
  if [[ ! -t 0 ]]; then
    echo "Intercept mode: base-url for now (no terminal to ask for the password transparent mode needs)." >&2
    echo "  Switch later from the dashboard: Install, transparent proxy." >&2
    echo base-url; return
  fi
  echo >&2
  echo "Intercept mode:" >&2
  echo "  transparent (recommended): Remote Control keeps working; asks for your password" >&2
  echo "      once, for /etc/hosts, a pf rule and trusting the local CA." >&2
  echo "  base-url: no password, but Claude Code turns Remote Control off while it is on." >&2
  read -r "answer?Use transparent mode? [Y/n] " || answer=n
  if [[ "$answer" == [nN]* ]]; then
    "$TARGET" configure --intercept-mode base-url >/dev/null
    echo base-url
  else
    echo transparent
  fi
}

apply_keep_awake() {
  local cfg="$HOME/.config/claude-burst/config.json" on mode
  on="$(python3 -c "import json;print(str(json.load(open('$cfg')).get('keep_awake_lid_closed',False)).lower())" 2>/dev/null || echo false)"
  [[ "$on" == true ]] || return 0
  mode="$(python3 -c "import json;print(json.load(open('$cfg')).get('keep_awake_lid_closed_power') or 'ac')" 2>/dev/null || echo ac)"
  echo "keep_awake_lid_closed is true (mode $mode): applying; sudo will ask for your password."
  appnap_record_original
  defaults write com.mitchellh.ghostty NSAppSleepDisabled -bool YES
  if ! sudo "$ROOT/scripts/lid-awake-root.sh" apply "$mode"; then
    echo "WARNING: keep-awake not applied; the lid will still sleep the Mac. Run:" >&2
    echo "  sudo $ROOT/scripts/lid-awake-root.sh apply $mode" >&2
  fi
}

# --- uninstall ----------------------------------------------------------------
#
# The machine-wide pieces first. In transparent mode /etc/hosts sends
# api.anthropic.com to 127.0.0.1, so deleting the gateway before the redirect
# leaves the whole Mac unable to reach Anthropic. Then everything that would
# bring the gateway back (the self-heal watchdog reloads its LaunchAgent, and a
# gateway that starts reinstalls its hooks), then the hooks while the binary
# that removes them still exists, then the binary. Then it checks, and says
# success only when the checks pass.
#
# Overridable only so tests can point the detection and the checks at temp
# files. The root scripts honour the same names, but sudo resets the
# environment, so a real uninstall always acts on the real files.
HOSTS_FILE="${CLAUDE_BURST_HOSTS_FILE:-/etc/hosts}"
PF_CONF="${CLAUDE_BURST_PF_CONF:-/etc/pf.conf}"
PF_ANCHOR_FILE="${CLAUDE_BURST_PF_ANCHOR:-/etc/pf.anchors/claude-burst}"
ROOT_STATE_DIR="${CLAUDE_BURST_ROOT_STATE_DIR:-/etc/claude-burst}"
DAEMONS_DIR="${CLAUDE_BURST_LAUNCHDAEMONS:-/Library/LaunchDaemons}"
PFHEAL_PLIST="${CLAUDE_BURST_PFHEAL_PLIST:-$DAEMONS_DIR/ninja.andrewbaker.claude-burst-pfheal.plist}"
PFHEAL_LIBEXEC="${CLAUDE_BURST_PFHEAL_LIBEXEC:-/usr/local/libexec/claude-burst}"
LIDAWAKE_PLIST="$DAEMONS_DIR/ninja.andrewbaker.claude-burst-lidawake.plist"
CA_CN="claude-burst local CA"
CONFIG_DIR="$HOME/.config/claude-burst"
SETTINGS="$HOME/.claude/settings.json"

# The markers transparent-root.sh writes; see BEGIN and TAG_* there.
hosts_block() { grep -q "^# BEGIN claude-burst $1\$" "$HOSTS_FILE" 2>/dev/null; }
pf_conf_ref() { grep -qE '^# BEGIN claude-burst (pf-rdr|pf-load)$' "$PF_CONF" 2>/dev/null; }
pfheal_installed() { [[ -f "$PFHEAL_PLIST" || -d "$PFHEAL_LIBEXEC" ]]; }
lidawake_applied() { [[ -f "$ROOT_STATE_DIR/lid-awake.state" || -f "$LIDAWAKE_PLIST" ]]; }
# Reading the System keychain needs no root.
ca_trusted() { security find-certificate -c "$CA_CN" /Library/Keychains/System.keychain >/dev/null 2>&1; }
config_mode() {
  python3 -c "import json;print((json.load(open('$CONFIG_DIR/config.json')).get('intercept') or {}).get('mode',''))" 2>/dev/null || true
}
transparent_detected() {
  hosts_block hosts || pf_conf_ref || [[ -f "$PF_ANCHOR_FILE" || -f "$ROOT_STATE_DIR/transparent.state" ]] ||
    [[ "$(config_mode)" == transparent ]]
}
# The live anchor needs root to read. sudo -n never prompts: it answers only
# when the root steps above left a cached credential, and otherwise this
# reports that it could not look rather than pretending the anchor is empty.
anchor_rules() {
  local out
  out="$(sudo -n pfctl -a claude-burst -s nat 2>/dev/null)" || return 2
  print -r -- "$out" | grep -q rdr
}

uninstall() {
  local purge=0 a
  for a in "$@"; do
    case "$a" in
      --purge) purge=1 ;;
      *) echo "Usage: $ROOT/install.sh uninstall [--purge]" >&2; exit 2 ;;
    esac
  done

  # 1. Root. Each step is the existing undo script, run only when its piece is
  #    actually there, so a base-url install asks for no password at all.
  #    Script and argument are kept apart so a checkout path with a space
  #    still runs.
  local -a root_script root_arg root_why
  root_step() { root_script+=("$ROOT/scripts/$1"); root_arg+=("$2"); root_why+=("$3"); }
  if pfheal_installed; then
    # Before the redirect goes: the daemon repairs a missing pf rule.
    root_step install-pf-heal.sh uninstall "the pf self-heal LaunchDaemon (ninja.andrewbaker.claude-burst-pfheal)"
  fi
  if transparent_detected; then
    root_step transparent-root.sh remove "transparent mode's api.anthropic.com redirect in /etc/hosts, and its pf rule and anchor"
  fi
  if hosts_block admin-host; then
    root_step transparent-root.sh admin-host-remove "the admin hostname entry in /etc/hosts"
  fi
  if ca_trusted; then
    root_step untrust-ca-systemwide.sh "" "\"$CA_CN\" from the System keychain"
  fi
  if lidawake_applied; then
    # An uninstall that kept a Mac that never sleeps would be a trap.
    root_step lid-awake-root.sh remove "the lid-closed keep-awake LaunchDaemon, restoring SleepDisabled"
  fi
  local i
  if (( ${#root_script} )); then
    echo "These are machine-wide and need root, so sudo will ask for your password:"
    for a in "${root_why[@]}"; do echo "  - $a"; done
    for i in {1..${#root_script}}; do
      echo "\$ sudo ${root_script[i]}${root_arg[i]:+ ${root_arg[i]}}"
      if ! sudo "${root_script[i]}" ${root_arg[i]:+"${root_arg[i]}"}; then
        echo "WARNING: failed: sudo ${root_script[i]}${root_arg[i]:+ ${root_arg[i]}}" >&2
      fi
    done
  fi
  # Stopping the gateway while the redirect still points at it is the one
  # step here that breaks the Mac, so a redirect that did not come out stops
  # the uninstall with the gateway still serving.
  if hosts_block hosts; then
    echo >&2
    echo "UNINSTALL STOPPED: $HOSTS_FILE still redirects api.anthropic.com to this Mac." >&2
    echo "Nothing else was removed, and the gateway is still running so Anthropic stays reachable." >&2
    echo "Fix it with:  sudo $ROOT/scripts/transparent-root.sh remove" >&2
    echo "then rerun:   $ROOT/install.sh uninstall" >&2
    exit 1
  fi

  # 2. Whatever would restart the gateway. bootout before the hooks come out:
  #    a gateway that starts (KeepAlive, or the watchdog) puts them back.
  "$ROOT/scripts/install-selfheal-watchdog.sh" uninstall || echo "WARNING: failed: $ROOT/scripts/install-selfheal-watchdog.sh uninstall" >&2
  "$ROOT/scripts/install-console.sh" uninstall >/dev/null || echo "WARNING: failed: $ROOT/scripts/install-console.sh uninstall" >&2
  launchctl bootout "gui/$UID/$LABEL" >/dev/null 2>&1 || true

  # 3. Claude Code's settings, while the binary that removes them exists.
  #    uninstall-hooks leaves config.json as it is, so a reinstall comes back
  #    with the same features on. disable's own transparent-mode note about
  #    the redirect is dropped: step 1 has dealt with it, and the check below
  #    says so if not.
  if [[ -x "$TARGET" ]]; then
    "$TARGET" uninstall-hooks || echo "WARNING: claude-burst uninstall-hooks failed" >&2
    "$TARGET" disable >/dev/null || echo "WARNING: claude-burst disable failed" >&2
  else
    echo "WARNING: $TARGET is already gone, so Claude Code's hooks could not be removed by it" >&2
  fi

  # 3b. Codex: without the gateway, Burst's provider in Codex's config is a
  #     dead port. Only the lines between Burst's markers go.
  "$ROOT/scripts/codex-unroute.sh" || echo "WARNING: Codex's config still routes to Burst: run scripts/codex-unroute.sh" >&2

  # 4. The LaunchAgent and the binary.
  rm -f "$PLIST" "$TARGET" "$INSTALL_DIR/burst-off" "$INSTALL_DIR/burst-reinstall"
  rm -rf "$OFF_DIR"
  zsh "$ROOT/scripts/update-mod.sh" uninstall
  appnap_restore_original
  if (( purge )); then
    rm -rf "$CONFIG_DIR"
  fi

  # 5. Check. Each failure names what is left and the command that fixes it.
  #    Plain ifs throughout: under set -e a failed `cond && x` as a
  #    function's last statement becomes the function's failure.
  local -a left
  if hosts_block hosts; then
    left+=("$HOSTS_FILE still redirects api.anthropic.com (Anthropic is unreachable from this Mac)
    fix: sudo $ROOT/scripts/transparent-root.sh remove")
  fi
  if hosts_block admin-host; then
    left+=("$HOSTS_FILE still has the admin hostname entry
    fix: sudo $ROOT/scripts/transparent-root.sh admin-host-remove")
  fi
  if pf_conf_ref || [[ -f "$PF_ANCHOR_FILE" ]]; then
    left+=("$PF_CONF or $PF_ANCHOR_FILE still references the claude-burst pf anchor
    fix: sudo $ROOT/scripts/transparent-root.sh remove")
  fi
  local rc=0
  anchor_rules || rc=$?
  if (( rc == 0 )); then
    left+=("the claude-burst pf anchor is still loaded with a redirect rule
    fix: sudo $ROOT/scripts/transparent-root.sh remove")
  fi
  if pfheal_installed; then
    left+=("the pf self-heal LaunchDaemon is still installed ($PFHEAL_PLIST)
    fix: sudo $ROOT/scripts/install-pf-heal.sh uninstall")
  fi
  if ca_trusted; then
    left+=("\"$CA_CN\" is still trusted in the System keychain
    fix: sudo $ROOT/scripts/untrust-ca-systemwide.sh")
  fi
  if grep -q claude-burst "$SETTINGS" 2>/dev/null; then
    left+=("$SETTINGS still mentions claude-burst:
$(grep -n claude-burst "$SETTINGS" | sed 's/^/      /')
    fix: delete those entries by hand; any Burst installed is also removed by reinstalling ($ROOT/install.sh) and rerunning $ROOT/install.sh uninstall")
  fi
  if [[ -e "$TARGET" || -e "$PLIST" ]]; then
    left+=("$TARGET or $PLIST is still there
    fix: launchctl bootout gui/$UID/$LABEL; rm -f $TARGET $PLIST")
  fi

  if (( ${#left} )); then
    echo >&2
    echo "UNINSTALL INCOMPLETE. Still in place:" >&2
    for a in "${left[@]}"; do echo "  - $a" >&2; done
    exit 1
  fi

  echo
  echo "Uninstalled Claude Burst. Checked: no /etc/hosts redirect, no pf anchor reference,"
  echo "no pf self-heal daemon, no CA in the System keychain, and nothing in $SETTINGS names claude-burst."
  if (( rc == 2 )); then
    echo "(The live pf anchor was not read: that needs root, and sudo had no cached password."
    echo " Check it with: sudo pfctl -a claude-burst -s nat   which should print nothing.)"
  fi
  echo "Removed: the redirect and root daemons (if any), the self-heal watchdog, the token-shunting,"
  echo "coordination, handover and prompt-notice hooks, /compact-async, the claude-burst mod, the LaunchAgent and the binary."
  if (( purge )); then
    echo "Purged $CONFIG_DIR."
  else
    echo "Kept $CONFIG_DIR (config, state, metrics, backups) for a reinstall."
    echo "To remove it too: $ROOT/install.sh uninstall --purge"
  fi
  echo "Kept the macOS Keychain secret. To delete whichever secondary key you stored:"
  echo "    security delete-generic-password -s claude-burst-together     # Together AI"
  echo "    security delete-generic-password -s claude-burst-openrouter   # OpenRouter"
  echo "    security delete-generic-password -s claude-burst-bedrock      # Amazon Bedrock"
}

install() {
  ARCH="$(uname -m)"
  case "$ARCH" in
    arm64) BIN="$ROOT/dist/claude-burst-darwin-arm64" ;;
    x86_64) BIN="$ROOT/dist/claude-burst-darwin-amd64" ;;
    *) echo "Unsupported Mac architecture: $ARCH" >&2; exit 1 ;;
  esac

  offer_signing
  mkdir -p "$INSTALL_DIR"
  # Staged in the SAME directory so the final mv is an atomic rename.
  staged="$INSTALL_DIR/.claude-burst.new.$$"
  trap 'rm -f "$staged"' EXIT

  # Always build the checkout when Go is here. dist/ is gitignored and nothing
  # refreshes it, so preferring it installed whatever was built there last:
  # on 2026-09-30 a rerun put a 28 Aug binary over a current one.
  if command -v go >/dev/null 2>&1; then
    echo "Building claude-burst from $ROOT..."
    # The working tree is built: say what in it is in no commit, and keep it.
    source "$ROOT/scripts/uncommitted.sh"
    record_uncommitted "$ROOT" install
    (cd "$ROOT" && go build -o "$staged" ./cmd/claude-burst)
  elif [[ -x "$BIN" ]]; then
    echo "WARNING: Go is not installed, so installing the prebuilt $BIN" >&2
    echo "  (built $(stat -f %Sm "$BIN")); it may be older than this checkout." >&2
    cp "$BIN" "$staged"
  else
    echo "No prebuilt binary found and Go is not installed." >&2
    echo "Install Go, then rerun ./install.sh" >&2
    exit 1
  fi
  chmod 755 "$staged"

  # mv, never cp over $TARGET: the running gateway has that file mapped, and
  # rewriting its bytes in place invalidates the code signature, so macOS
  # SIGKILLs every later launch of it (exit 137) while the old process keeps
  # serving. A rename gives the new binary a fresh inode and leaves the running
  # one alone until the LaunchAgent restart below. Same reason as scripts/deploy.sh.
  # shellcheck source=./scripts/codesign.sh
  source "$ROOT/scripts/codesign.sh"
  sha="$(burst_build_sha "$staged")"
  burst_sign "$staged"
  mv -f "$staged" "$TARGET"
  printf '%s\n' "$sha" > "$(dirname "$TARGET")/.claude-burst.build-sha"

  # burst-off: the one-command off switch, on the PATH (see the script).
  zsh "$ROOT/scripts/install-burst-off.sh"

  ZPROFILE="$HOME/.zprofile"
  PATH_LINE='export PATH="$HOME/.local/bin:$PATH" # claude-burst'
  if ! grep -Fq '# claude-burst' "$ZPROFILE" 2>/dev/null; then
    printf '\n%s\n' "$PATH_LINE" >> "$ZPROFILE"
  fi

  REGION="${AWS_REGION:-${AWS_DEFAULT_REGION:-us-east-1}}"
  "$TARGET" configure --region "$REGION"

  # Which secondary config.json names, if any. Read rather than assumed: a
  # reinstall keeps a Together or OpenRouter secondary chosen earlier, and
  # telling that user they have none would be wrong.
  local cfgjson="$HOME/.config/claude-burst/config.json" secondary seckc
  secondary="$(python3 -c "import json;s=json.load(open('$cfgjson')).get('secondary') or {};print(s.get('provider') or '')" 2>/dev/null || true)"
  seckc="$(python3 -c "import json;s=json.load(open('$cfgjson')).get('secondary') or {};print(s.get('keychain_service') or '')" 2>/dev/null || true)"

  if [[ -n "${AWS_BEARER_TOKEN_BEDROCK:-}" ]]; then
    "$TARGET" keychain-set
  elif [[ "$secondary" == openai-compatible ]] && security find-generic-password -s "${seckc:-claude-burst-together}" >/dev/null 2>&1; then
    echo "Secondary: kept the OpenAI-compatible secondary already in config.json, with its key in the Keychain."
  elif [[ "$secondary" == bedrock ]] && security find-generic-password -s claude-burst-bedrock >/dev/null 2>&1; then
    echo "Secondary: kept Amazon Bedrock, with the key already in the Keychain."
  else
    # A secondary named in config.json with no key in the Keychain is not a
    # secondary: every overflow to it would fail. So no key means none, set
    # explicitly, and the dashboard shows "not set up" instead of a broken one.
    if [[ -n "$secondary" && "$secondary" != none ]]; then
      echo "Secondary: $secondary was named but has no key in the Keychain, so it is switched off."
    fi
    "$TARGET" configure --secondary none >/dev/null
    echo "NOTE: no secondary chosen, so Claude Burst runs on your single plan (Claude Enterprise, Pro or Max)."
    echo "Everything but overflow works, and Anthropic's own limits reach Claude Code unchanged."
    echo "To overflow to Together AI, OpenRouter or Amazon Bedrock later, pick one on the dashboard"
    echo "(Routing, Secondary) or see docs/providers.md."
  fi

  local intercept_mode
  intercept_mode="$(choose_intercept_mode "$cfgjson")"
  # Transparent mode is enabled by install-proxy.sh below, once the gateway is
  # up: it backs up, starts the gateway in that mode, then redirects.
  if [[ "$intercept_mode" != transparent ]]; then
    "$TARGET" enable
  fi

  apply_keep_awake

  mkdir -p "$HOME/Library/LaunchAgents"
  mkdir -p "$HOME/.config/claude-burst"
  cat > "$PLIST" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>$LABEL</string>
  <key>ProgramArguments</key>
  <array>
    <string>$TARGET</string>
    <string>serve</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <!-- Seconds launchd waits after SIGTERM before SIGKILL (default 20, and
       launchd caps it at 60 for an agent). The gateway drains in-flight
       replies for up to 50s on SIGTERM (cmd/claude-burst/drain.go); this
       must stay above that. -->
  <key>ExitTimeOut</key><integer>60</integer>
  <key>ProcessType</key><string>Background</string>
  <key>StandardOutPath</key><string>$HOME/.config/claude-burst/launchd.out.log</string>
  <key>StandardErrorPath</key><string>$HOME/.config/claude-burst/launchd.err.log</string>
</dict>
</plist>
PLIST

  # bootout returns before the job is gone: the old gateway drains its
  # replies for up to 50s first, and a bootstrap meanwhile fails with
  # "Bootstrap failed: 5: Input/output error", which under set -e ended the
  # install half done (a reinstall over a running gateway, 2026-10-04). So
  # wait for the job to leave launchd, and retry the bootstrap.
  launchctl bootout "gui/$UID/$LABEL" >/dev/null 2>&1 || true
  local waited=0
  while launchctl print "gui/$UID/$LABEL" >/dev/null 2>&1 && (( waited < 75 )); do
    (( waited == 0 )) && echo "waiting for the running gateway to finish its replies and stop (up to a minute)..."
    sleep 1
    waited=$((waited + 1))
  done
  local tries=0
  until launchctl bootstrap "gui/$UID" "$PLIST" 2>/dev/null; do
    (( ++tries >= 10 )) && { echo "ERROR: launchctl bootstrap gui/$UID $PLIST keeps failing; run it by hand to see why" >&2; exit 1; }
    sleep 2
  done
  launchctl kickstart -k "gui/$UID/$LABEL"

  # The gateway watchdog restarts a gateway that died or hung; nothing else
  # does, and the dashboard showed it unarmed after every fresh install
  # (2026-10-04, a second laptop). A user LaunchAgent: no password. Armed on
  # every install so a change to it takes effect.
  "$ROOT/scripts/install-selfheal-watchdog.sh" >/dev/null ||
    echo "WARNING: could not arm the gateway watchdog; arm it under Guards on the dashboard" >&2

  # The support console: audit, log and repair buttons that stay up when
  # the gateway does not.
  "$ROOT/scripts/install-console.sh" >/dev/null ||
    echo "WARNING: could not start the support console; run scripts/install-console.sh" >&2

  if [[ "$intercept_mode" == transparent ]]; then
    "$TARGET" configure --intercept-mode transparent
    if ! "$ROOT/scripts/install-proxy.sh"; then
      # Base-url is only safe while nothing redirects api.anthropic.com here:
      # a redirect left by a half-finished run sends TLS to a plain HTTP port.
      if grep -q '^# BEGIN claude-burst hosts$' /etc/hosts 2>/dev/null; then
        echo "Transparent mode did not finish and its /etc/hosts redirect is in place. Undo it with:" >&2
        echo "  $ROOT/scripts/rollback.sh" >&2
        exit 1
      fi
      echo >&2
      echo "Transparent mode did not finish (see above); using base-url instead, so Burst works now." >&2
      echo "Retry transparent later: claude-burst configure --intercept-mode transparent && $ROOT/scripts/install-proxy.sh" >&2
      echo "  (or the dashboard: Install, transparent proxy)" >&2
      "$TARGET" configure --intercept-mode base-url
      "$TARGET" enable
      launchctl kickstart -k "gui/$UID/$LABEL"
    fi
  fi

  # Read the port back rather than printing a literal: the default moved off
  # 7777 (see internal/config's Default), and a summary naming a port nothing
  # is listening on is exactly the kind of confidently-wrong instruction this
  # project keeps getting bitten by.
  local gw dash mode sec_line health
  dash="$(python3 -c "import json;print(json.load(open('$cfgjson')).get('admin_listen') or '127.0.0.1:7788')" 2>/dev/null || echo '127.0.0.1:7788')"
  gw="$(python3 -c "import json;print(json.load(open('$cfgjson')).get('listen','127.0.0.1:7777'))" 2>/dev/null || echo '127.0.0.1:7777')"
  mode="$(python3 -c "import json;print((json.load(open('$cfgjson')).get('intercept') or {}).get('mode') or 'base-url')" 2>/dev/null || echo base-url)"
  # shellcheck source=./scripts/folder-access.sh
  source "$ROOT/scripts/folder-access.sh"
  burst_folder_access "$dash" "$ROOT"
  burst_lid_daemon_update "$ROOT"
  secondary="$(python3 -c "import json;s=json.load(open('$cfgjson')).get('secondary') or {};print(s.get('provider') or '')" 2>/dev/null || true)"
  case "$secondary" in
    openai-compatible) sec_line="Secondary: $(python3 -c "import json;s=json.load(open('$cfgjson')).get('secondary') or {};print(s.get('model') or '')" 2>/dev/null) at $(python3 -c "import json;s=json.load(open('$cfgjson')).get('secondary') or {};print(s.get('base_url') or '')" 2>/dev/null)" ;;
    bedrock)
      if security find-generic-password -s claude-burst-bedrock >/dev/null 2>&1; then
        sec_line="Secondary: Amazon Bedrock ($REGION)"
      else
        sec_line="Secondary: none (single plan)"
      fi ;;
    *) sec_line="Secondary: none (single plan)" ;;
  esac
  # In transparent mode the pf redirect makes the gateway's own port all but
  # unreachable to direct connections (issue #1), so a curl of it times out
  # on a healthy gateway. Probe the real path instead, as the guards do.
  if [[ "$mode" == transparent ]]; then
    health="  curl -sk https://api.anthropic.com/healthz   # a body containing \"overflow\" came from the gateway
  (do not curl http://$gw directly in transparent mode: it times out by design)"
  else
    health="  curl -s http://$gw/healthz"
  fi

  cat <<OUT

Installed claude-burst $($TARGET version)
Gateway: http://$gw (intercept mode: $mode)
Dashboard: http://$dash
Claude Code settings: enabled
LaunchAgent: $LABEL
$sec_line

Now restart Claude Code and run:
  claude-burst status

To test the gateway:
$health

Lid-closed keep-awake (off by default; see docs/lid-and-hotspot.md):
  claude-burst configure --keep-awake-lid-closed true [--keep-awake-power ac|always]

To remove everything later:
  ./install.sh uninstall
OUT

  offer_panel
  # Burst inside Claude Code sessions (mods/claude-burst). CLAUDE_BURST_MOD=no skips it.
  echo
  zsh "$ROOT/scripts/update-mod.sh"
}

# The usage panel (a separate repo) shows each turn's context and cost next
# to Claude Code, and reads Burst's metrics to mark auto compaction. It is
# optional in both directions, so a failure here never fails this install.
PANEL_REPO="https://github.com/andrewbakercloudscale/claude-code-cost-sidebar.git"
offer_panel() {
  if [[ -x "$HOME/.local/bin/ccusage-panel.sh" ]]; then
    echo "\nUsage panel: installed; bringing it up to date."
    zsh "$ROOT/scripts/update-panel.sh"
    return 0
  fi
  if [[ "${CLAUDE_BURST_PANEL:-ask}" == "no" ]]; then
    return 0
  fi
  if [[ "${CLAUDE_BURST_PANEL:-ask}" != "yes" ]]; then
    if [[ ! -t 0 ]]; then
      echo "\nOptional usage panel (live context and cost beside Claude Code): rerun with CLAUDE_BURST_PANEL=yes, or see $PANEL_REPO"
      return 0
    fi
    local answer
    echo
    echo "The usage panel shows each turn's context and cost in a sidebar beside Claude Code,"
    echo "including when Burst compacts a long session and what that saved."
    read -r "answer?Install the usage panel too? [Y/n] "
    [[ -z "$answer" || "$answer" == [Yy]* ]] || { echo "Skipped. Install it later from $PANEL_REPO"; return 0; }
  fi

  # A checkout beside this one wins; otherwise keep a clone of our own.
  # The repo was claudecode-cost-usage-panel until 2026-10-05; either name is found.
  local dir="${ROOT:h}/claude-code-cost-sidebar"
  [[ -f "$dir/claude-panel-setup.sh" ]] || dir="${ROOT:h}/claudecode-cost-usage-panel"
  if [[ ! -f "$dir/claude-panel-setup.sh" ]]; then
    dir="$HOME/.local/share/claude-burst/claudecode-cost-usage-panel"
    if [[ -d "$dir/.git" ]]; then
      git -C "$dir" pull --ff-only --quiet || echo "WARNING: could not update $dir; installing the copy already there"
    else
      mkdir -p "${dir:h}"
      git clone --quiet "$PANEL_REPO" "$dir" || { echo "WARNING: could not fetch the usage panel; install it later from $PANEL_REPO"; return 0; }
    fi
  fi
  echo "Installing the usage panel from $dir"
  bash "$dir/claude-panel-setup.sh" || echo "WARNING: the usage panel installer failed (Burst itself is installed); rerun: bash $dir/claude-panel-setup.sh"
}

case "${1:-install}" in
  install) install ;;
  uninstall) shift; uninstall "$@" ;;
  *) echo "Usage: $0 [install|uninstall [--purge]]" >&2; exit 2 ;;
esac
