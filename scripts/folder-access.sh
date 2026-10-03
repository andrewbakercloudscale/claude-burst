# Sourced by install.sh and deploy.sh: get what needs a person at the Mac
# (macOS's folder prompts, the keep-awake daemon's password) done while they
# are still at the Terminal.
#
# The gateway reads repositories under ~/Desktop (to name a session's
# repository) and runs git in its own checkout (the update check). macOS
# asks about each folder only when the gateway first tries, which is
# usually hours later with nobody watching, and while a prompt sits
# unanswered the gateway's git waits behind it: on 3 Oct 2026 every update
# check timed out until the Documents and Downloads prompts were answered.
#
# So straight after an install or deploy, with the person still at the
# Terminal, ask the gateway to look now (the dashboard's Check folder
# access) and say what to click. Never fails the caller.

# burst_folder_access DASH_ADDR CHECKOUT_DIR
burst_folder_access() {
  local dash="$1" checkout="$2" folder=""
  case "$checkout" in
    "$HOME"/Desktop/*) folder=Desktop ;;
    "$HOME"/Documents/*) folder=Documents ;;
    "$HOME"/Downloads/*) folder=Downloads ;;
  esac
  if [[ ! -t 0 || ! -t 1 ]]; then
    [[ -n "$folder" ]] && echo "  folder access: not checked (no one at this Terminal). At the Mac, press Check folder access under Permissions on http://$dash and click Allow on each macOS prompt."
    return 0
  fi
  local i
  for i in {1..30}; do
    curl -s -m 2 -o /dev/null "http://$dash/api/permissions" && break
    sleep 0.5
  done
  echo
  echo "Folder access: macOS may now ask whether claude-burst, and git, may access files in"
  echo "your Desktop, Documents or Downloads folder. Click Allow on each prompt: the gateway"
  echo "needs it to name a session's repository and to check for updates."
  local out
  out="$(curl -s -m 240 -X POST -H 'X-Claude-Burst-Admin: 1' -H 'Content-Type: application/json' -d '{}' "http://$dash/api/permissions-check")"
  if [[ -z "$out" ]]; then
    echo "  folder access: no answer from the gateway; press Check folder access under Permissions on http://$dash later."
    return 0
  fi
  python3 - "$out" <<'PY' || echo "  folder access: could not read the gateway's answer"
import json, sys
for r in json.loads(sys.argv[1]).get("results", []):
    line = f"  {r['folder']}: {r['state']}"
    if r.get("detail"):
        line += f" ({r['detail']})"
    print(line)
PY
  return 0
}

# burst_lid_daemon_update CHECKOUT_DIR: the keep-awake daemon runs a
# root-owned copy of lid-awake-root.sh and never rereads the checkout, so a
# fix to it (the screen going off behind a shut lid) is not live until
# someone types a password. With someone at this Terminal, ask now; without,
# say where to do it. Only when keep-awake is on. Never fails the caller.
burst_lid_daemon_update() {
  local root="$1" cfg="$HOME/.config/claude-burst/config.json" installed=/usr/local/libexec/claude-burst/lid-awake-root.sh
  local on mode idle
  on="$(python3 -c "import json;print(str(json.load(open('$cfg')).get('keep_awake_lid_closed',False)).lower())" 2>/dev/null || echo false)"
  [[ "$on" == true ]] || return 0
  if [[ -f "$installed" ]] && cmp -s "$root/scripts/lid-awake-root.sh" "$installed" && pgrep -f 'lid-awake-root.sh watch' >/dev/null 2>&1; then
    return 0
  fi
  if [[ ! -t 0 || ! -t 1 ]]; then
    echo "  keep-awake: the lid daemon needs your password to update (no one at this Terminal). Press Grant permission under Lid & hotspot on the dashboard."
    return 0
  fi
  mode="$(python3 -c "import json;print(json.load(open('$cfg')).get('keep_awake_lid_closed_power') or 'ac')" 2>/dev/null || echo ac)"
  idle="$(python3 -c "import json;print(int(json.load(open('$cfg')).get('keep_awake_idle_minutes') or 0))" 2>/dev/null || echo 0)"
  echo
  echo "Keep-awake: the lid daemon (it keeps the Mac awake and turns the screen off behind"
  echo "the shut lid) is out of date or not running. sudo will ask for your password."
  sudo "$root/scripts/lid-awake-root.sh" apply "$mode" "$idle" "$HOME/.config/claude-burst/last-activity" \
    || echo "  keep-awake: not updated. Press Grant permission under Lid & hotspot on the dashboard."
  return 0
}
