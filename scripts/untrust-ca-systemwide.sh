#!/bin/zsh
# Removes the gateway's local CA from the macOS System keychain trust
# store, undoing trust-ca-systemwide.sh. Safe to run even if it was never
# added -- "not found" is treated as success, matching the idempotent
# pattern every other rollback script in this repo already follows.
#
# Needs root. Usage: sudo scripts/untrust-ca-systemwide.sh
set -uo pipefail

CN="claude-burst local CA"

if [[ $EUID -ne 0 ]]; then
  echo "needs root -- run: sudo $0" >&2
  exit 1
fi

if security delete-certificate -c "$CN" /Library/Keychains/System.keychain 2>&1; then
  echo "removed \"$CN\" from the System keychain"
else
  echo "\"$CN\" not found in the System keychain (already removed, or never added)"
fi
