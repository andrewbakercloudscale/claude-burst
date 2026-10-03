# Sourced by deploy.sh and install.sh: sign claude-burst with a stable local
# identity, so macOS privacy grants survive a rebuild.
#
# Go signs every build ad hoc, and an ad hoc signature's identity is the hash
# of the binary itself. macOS privacy permissions (TCC: "claude-burst would
# like to access files in your Desktop folder") are recorded against that
# identity, so every deploy produced a new program as far as macOS could
# tell and the prompt came back. Reading a repository under ~/Desktop is
# how the gateway names a session's repository (spend by repository,
# per-repository Compact at), so the prompt came back after every deploy.
#
# Signed with one certificate and a fixed identifier, the identity is that
# certificate plus the identifier, the same for every build: one Allow
# lasts. scripts/signing-setup.sh creates the certificate once. Without it
# the build stays ad hoc and everything works as before, prompts included.

BURST_SIGN_NAME="Claude Burst Local Signing"
BURST_SIGN_ID="ninja.andrewbaker.claude-burst"

# burst_sign_ready succeeds when the identity exists and macOS trusts it for
# code signing.
burst_sign_ready() {
  security find-identity -v -p codesigning 2>/dev/null | grep -Fq "\"$BURST_SIGN_NAME\""
}

# burst_sign FILE signs FILE in place when the identity is ready, and says
# which it did. Never fails the caller: an unsigned build still runs.
burst_sign() {
  if ! burst_sign_ready; then
    echo "  signing: ad hoc (run scripts/signing-setup.sh once to stop macOS asking for Desktop access after every deploy)"
    return 0
  fi
  if codesign -f -s "$BURST_SIGN_NAME" -i "$BURST_SIGN_ID" "$1" 2>/dev/null; then
    echo "  signing: $BURST_SIGN_NAME ($BURST_SIGN_ID)"
  else
    echo "  signing: FAILED with $BURST_SIGN_NAME, left ad hoc"
  fi
  return 0
}

# burst_build_sha FILE is the hash deploy.sh compares to tell an unchanged
# build: a signed binary differs every time it is signed, so the comparison
# is made on the unsigned build and recorded beside the installed one.
burst_build_sha() {
  shasum -a 256 "$1" | cut -d' ' -f1
}
