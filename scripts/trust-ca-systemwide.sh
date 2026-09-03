#!/bin/zsh
# Imports the gateway's local CA into the macOS System keychain as a
# trusted root, so ANY process's TLS stack accepts the gateway's
# certificate -- not just Claude Code CLI, which is the only thing that
# respects NODE_EXTRA_CA_CERTS (the CA bundle `claude-burst enable`
# already manages). Everything else on this Mac -- Claude Desktop, curl,
# any other app -- has never heard of this CA and rejects it whenever
# transparent mode's machine-wide redirect catches its traffic.
#
# Root-caused 2026-09-03 (see INVESTIGATION-TLS-STORM.md): Claude
# Desktop's own auto-updater checks api.anthropic.com roughly hourly and
# got caught by the redirect, producing the TLS handshake-error storm --
# and interfered with Claude Desktop's Cowork/MCP-filesystem startup at
# least once. This is the actual fix, not a workaround: it makes the
# machine-wide redirect transparent to machine-wide traffic, which was
# the design intent all along -- transparent mode already accepts
# machine-wide blast radius as its cost (see ROLLBACK.md); this closes
# the one place that cost was landing as breakage instead of silence.
#
# Machine-wide, security-relevant change: any application that trusts the
# System Roots now also trusts a certificate this gateway generated
# locally. Reversible any time with untrust-ca-systemwide.sh, or by hand
# via Keychain Access (System keychain -> "claude-burst local CA" ->
# delete).
#
# Needs root. Run this yourself, interactively.
#
# Usage: sudo scripts/trust-ca-systemwide.sh
set -uo pipefail

DIR="$(cd "$(dirname "$0")" && pwd)"
CA_CERT="$HOME/.config/claude-burst/ca/ca-cert.pem"

if [[ $EUID -ne 0 ]]; then
  echo "needs root -- run: sudo $0" >&2
  exit 1
fi

if [[ ! -f "$CA_CERT" ]]; then
  echo "no CA cert at $CA_CERT -- run 'claude-burst enable' first" >&2
  exit 1
fi

echo "importing $CA_CERT into the System keychain as a trusted root..."
security add-trusted-cert -d -r trustRoot -k /Library/Keychains/System.keychain "$CA_CERT"

echo
echo "verifying..."
if security find-certificate -c "claude-burst local CA" /Library/Keychains/System.keychain >/dev/null 2>&1; then
  echo "confirmed: \"claude-burst local CA\" is in the System keychain"
else
  echo "WARNING: import command succeeded but the cert was not found on lookup -- check Keychain Access manually" >&2
  exit 1
fi

echo
echo "done. undo any time with: sudo $DIR/untrust-ca-systemwide.sh"
