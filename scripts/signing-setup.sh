#!/bin/zsh
# One-time setup: a local code-signing certificate for claude-burst, so the
# macOS "claude-burst would like to access files in your Desktop folder"
# prompt is answered once instead of after every deploy. Why that happens:
# see scripts/codesign.sh.
#
# What it does, all in your login Keychain:
#   1. creates a self-signed certificate "Claude Burst Local Signing", valid
#      for code signing only, ten years;
#   2. trusts it for code signing (macOS asks for your password);
#   3. lets codesign use its key without asking each time (asks for your
#      login Keychain password in this terminal);
#   4. signs the installed claude-burst with it and restarts the gateway
#      (replies in flight finish first).
# Then allow the Desktop prompt once more: it is the last one.
#
# Undo: Keychain Access, login, My Certificates, delete "Claude Burst Local
# Signing". Builds go back to ad hoc signing, prompts and all.
set -euo pipefail
ROOT="${0:A:h}/.."
source "$ROOT/scripts/codesign.sh"
KC="$HOME/Library/Keychains/login.keychain-db"
TARGET="$HOME/.local/bin/claude-burst"

# Step 3 alone, for a run stopped at its password prompt: the identity
# exists then, but codesign may not be allowed to use its key unasked.
allow_codesign() {
  echo "Letting codesign use it without asking (enter your login Keychain password, the one you log in to this Mac with)..."
  security set-key-partition-list -S apple-tool:,apple:,codesign: -s -D "$BURST_SIGN_NAME" -t private "$KC" >/dev/null
}

if burst_sign_ready; then
  echo "\"$BURST_SIGN_NAME\" is already set up."
  probe=$(mktemp)
  cp /usr/bin/true "$probe"
  if ! codesign -f -s "$BURST_SIGN_NAME" "$probe" </dev/null >/dev/null 2>&1; then
    allow_codesign
  fi
  rm -f "$probe"
else
  tmp=$(mktemp -d)
  trap 'rm -rf "$tmp"' EXIT
  cat > "$tmp/cert.cnf" <<CNF
[req]
distinguished_name=dn
prompt=no
[dn]
CN=$BURST_SIGN_NAME
[v3]
basicConstraints=critical,CA:false
keyUsage=critical,digitalSignature
extendedKeyUsage=critical,codeSigning
CNF
  # /usr/bin/openssl (LibreSSL) on purpose: its PKCS#12 is one macOS's
  # security can import; Homebrew's OpenSSL 3 needs -legacy for that.
  /usr/bin/openssl req -x509 -newkey rsa:2048 -nodes -days 3650 \
    -keyout "$tmp/key.pem" -out "$tmp/cert.pem" -config "$tmp/cert.cnf" -extensions v3 2>/dev/null
  pass=$(/usr/bin/openssl rand -hex 16)
  /usr/bin/openssl pkcs12 -export -inkey "$tmp/key.pem" -in "$tmp/cert.pem" \
    -out "$tmp/id.p12" -passout "pass:$pass"
  echo "1/3 Adding the certificate to your login Keychain..."
  security import "$tmp/id.p12" -k "$KC" -P "$pass" -T /usr/bin/codesign >/dev/null
  echo "2/3 Trusting it for code signing (macOS asks for your password)..."
  security add-trusted-cert -p codeSign -k "$KC" "$tmp/cert.pem"
  echo -n "3/3 "
  allow_codesign
  if ! burst_sign_ready; then
    echo "The certificate is in the Keychain but macOS does not list it as a valid signing identity." >&2
    echo "Check Keychain Access, login, My Certificates, \"$BURST_SIGN_NAME\"." >&2
    exit 1
  fi
  echo "Signing identity ready."
fi

if [[ -x "$TARGET" ]]; then
  # Captured, not piped into grep -q: under pipefail, grep exiting at the
  # first match can fail codesign with SIGPIPE and so the whole test.
  sig="$(codesign -dvv "$TARGET" 2>&1 || :)"
  if [[ "$sig" == *"Authority=$BURST_SIGN_NAME"* ]]; then
    echo "The installed claude-burst is already signed with it."
    exit 0
  fi
  # A signed copy renamed over the original, never signed in place: the
  # running gateway has the file mapped (see deploy.sh's swap).
  staged="$(dirname "$TARGET")/.claude-burst.signing.$$"
  cp "$TARGET" "$staged"
  burst_sign "$staged"
  "$staged" --help >/dev/null 2>&1 || { rm -f "$staged"; echo "signed copy failed its smoke test; left as it was" >&2; exit 1; }
  mv -f "$staged" "$TARGET"
  echo "Restarting the gateway (replies in flight finish first)..."
  launchctl kickstart -k "gui/$(id -u)/ninja.andrewbaker.claude-burst" || true
  echo "Done. Allow the Desktop prompt once more if it appears; later deploys keep the same identity."
fi
