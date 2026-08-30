#!/bin/zsh
# Reports whether TLS traffic to Anthropic is being intercepted by a corporate
# inspecting proxy (Zscaler, Netskope, Palo Alto, ...) on this machine, right now.
#
# WHY THIS EXISTS
#
# claude-burst's optional "transparent" intercept mode terminates TLS locally so
# that ANTHROPIC_BASE_URL can stay unset and Claude Code's Remote Control keeps
# working (Remote Control is disabled whenever that variable points anywhere
# other than api.anthropic.com). That design rests on one assumption:
#
#     TLS interception does not, by itself, break Remote Control.
#
# The evidence for it is that Remote Control works on corporate networks that
# inspect TLS. But "my corporate proxy is running" is NOT the same as "my
# corporate proxy is inspecting Anthropic" -- many deployments bypass specific
# domains, and a bare issuer check cannot tell "not intercepted" apart from
# "not on the corporate network today". This script distinguishes all three.
#
# HOW IT DECIDES
#
# An inspecting proxy re-signs every site with its own CA. Genuine public sites
# have varied issuers (Google Trust Services, Sectigo, Let's Encrypt, ...), so:
#
#   control hosts all share ONE issuer  ->  interception is active
#     ...and Anthropic has that issuer  ->  INTERCEPTED   (assumption confirmed)
#     ...and Anthropic has another      ->  BYPASSED      (assumption unsupported)
#   control hosts have varied issuers   ->  NOT ACTIVE    (inconclusive; re-run
#                                                          on the corporate network)
#
# Exit: 0 intercepted, 1 inconclusive, 2 bypassed.
set -uo pipefail

TARGET="api.anthropic.com"
# Deliberately spread across different public CAs, so that "all controls share
# one issuer" is a real signal rather than a coincidence.
CONTROLS=(www.google.com github.com en.wikipedia.org)

# Well-known public CAs. If every control host shares one of these, that is a
# coincidence of CA choice, not an inspecting proxy.
PUBLIC_CAS=(
  "Google Trust Services" "DigiCert" "Let's Encrypt" "Sectigo Limited"
  "GlobalSign nv-sa" "Amazon" "Microsoft Corporation" "Internet Security Research Group"
)

issuer_of() {
  local host="$1"
  echo | openssl s_client -connect "${host}:443" -servername "$host" 2>/dev/null \
    | openssl x509 -noout -issuer 2>/dev/null \
    | sed 's/^issuer=//; s/^ *//'
}

# Organisation (O=) is the stable part of an issuer DN; the CN varies per
# intermediate even within one CA, so comparing full DNs would miss matches.
org_of() {
  print -r -- "$1" | sed -n 's/.*O *= *\([^,\/]*\).*/\1/p' | sed 's/ *$//'
}

is_public_ca() {
  local candidate="$1"
  for ca in "${PUBLIC_CAS[@]}"; do
    [[ "$candidate" == "$ca" ]] && return 0
  done
  return 1
}

# classify <target_org> <control_org>...
# Prints the verdict and returns the script's exit code. Kept as a pure
# function of its arguments so --self-test can exercise every branch: this
# script is meant to be run once, months from now, on a network we cannot
# reach today, so a branch that has never executed is a branch that will be
# wrong exactly when it matters.
classify() {
  local target_org="$1"; shift
  local control_orgs=("$@")

  if (( ${#control_orgs[@]} < 2 )); then
    echo "  INCONCLUSIVE -- fewer than two control hosts reachable; cannot compare issuers."
    return 1
  fi

  # (@f) splits on newlines only -- a plain $(...) would word-split an org name
  # like "Google Trust Services" into three separate entries.
  local uniq_orgs=("${(@f)$(print -l -- "${control_orgs[@]}" | sort -u)}")

  if (( ${#uniq_orgs[@]} == 1 )) && ! is_public_ca "${uniq_orgs[1]}"; then
    local mitm_org="${uniq_orgs[1]}"
    echo "  interception IS active on this network (all control hosts signed by: $mitm_org)"
    if [[ "$target_org" == "$mitm_org" ]]; then
      echo
      echo "  INTERCEPTED -- $TARGET is being inspected too."
      echo "  If Remote Control works right now, the transparent-mode assumption is confirmed."
      return 0
    else
      echo
      echo "  BYPASSED -- $TARGET is signed by '$target_org', not the inspecting CA."
      echo "  Anthropic is exempted from inspection, so Remote Control working here says"
      echo "  NOTHING about whether it survives TLS interception. The assumption is"
      echo "  unsupported; validate with a local mitmproxy instead."
      return 2
    fi
  else
    if (( ${#uniq_orgs[@]} == 1 )); then
      echo "  interception is NOT active (control hosts share '${uniq_orgs[1]}',"
      echo "  which is a well-known public CA -- a coincidence, not an inspecting proxy)"
    else
      echo "  interception is NOT active (control hosts have varied issuers:"
      print -l -- "${uniq_orgs[@]}" | sed 's/^/    - /'
      echo "  )"
    fi
    echo
    echo "  INCONCLUSIVE -- you are not on the inspecting network right now."
    echo "  Re-run this while the corporate proxy is switched on."
    return 1
  fi
}

self_test() {
  local failures=0
  check() {
    local name="$1" want="$2"; shift 2
    local out; out="$(classify "$@")"; local got=$?
    if [[ "$got" == "$want" ]]; then
      echo "PASS: $name (exit $got)"
    else
      echo "FAIL: $name -- wanted exit $want, got $got"
      print -r -- "$out" | sed 's/^/        /'
      failures=$((failures + 1))
    fi
  }

  echo "== self-test =="
  check "intercepted: target and controls share a corporate CA" 0 \
    "Zscaler Inc." "Zscaler Inc." "Zscaler Inc." "Zscaler Inc."
  check "bypassed: controls inspected, target is not" 2 \
    "Google Trust Services" "Zscaler Inc." "Zscaler Inc." "Zscaler Inc."
  check "not enrolled: varied public issuers" 1 \
    "Google Trust Services" "Google Trust Services" "Sectigo Limited" "Let's Encrypt"
  check "coincidence: controls share a PUBLIC CA, not a proxy" 1 \
    "Google Trust Services" "DigiCert" "DigiCert" "DigiCert"
  check "too few controls reachable" 1 \
    "Google Trust Services" "Sectigo Limited"

  echo
  if (( failures == 0 )); then
    echo "self-test: all branches OK"
    return 0
  fi
  echo "self-test: $failures branch(es) broken"
  return 1
}

if [[ "${1:-}" == "--self-test" ]]; then
  self_test
  exit $?
fi

echo "== probing =="
target_issuer="$(issuer_of "$TARGET")"
if [[ -z "$target_issuer" ]]; then
  echo "FAIL: could not complete a TLS handshake with $TARGET -- no network?"
  exit 1
fi
printf '  %-22s %s\n' "$TARGET" "$target_issuer"

control_orgs=()
for h in "${CONTROLS[@]}"; do
  iss="$(issuer_of "$h")"
  printf '  %-22s %s\n' "$h" "${iss:-<connect failed>}"
  [[ -n "$iss" ]] && control_orgs+=("$(org_of "$iss")")
done

echo
echo "== verdict =="
classify "$(org_of "$target_issuer")" "${control_orgs[@]}"
exit $?
