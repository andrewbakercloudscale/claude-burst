# Security policy

## Reporting a vulnerability

Please report vulnerabilities privately, through GitHub's private vulnerability reporting:
the repository's **Security** tab, then **Report a vulnerability**
(<https://github.com/andrewbakercloudscale/claude-burst/security/advisories/new>).
Do not open a public issue.

Include what you found, how to reproduce it, the version (`claude-burst version`), the intercept
mode (`base-url` or `transparent`) and your macOS version. I aim to reply within
a week. This is a one-person project, so please allow reasonable time for a fix before you
disclose publicly, and I will agree a date with you.

## Supported versions

Only the latest release is supported. Fixes are made on `main` and shipped in the next release;
older releases are not patched. To update, check out the newest release tag and rerun `./install.sh`.

## Threat model, in brief

Claude Burst is a local gateway that sits in the path of your Claude Code traffic. What it is
designed to protect, and what it is not:

**In scope**

- **Other websites must not be able to use the gateway or the dashboard.** Both listen on
  loopback only. The gateway refuses requests carrying a browser `Origin` header or a `Host`
  that is not its own loopback name or the intercepted host. The dashboard checks `Host`
  (against DNS rebinding) and requires a custom header on every change, which a cross-origin
  page cannot send without a CORS preflight the server never answers; no CORS headers are ever
  returned, so responses cannot be read cross-origin.
- **The local CA must not be usable against any other site.** In transparent mode the CA is
  generated on your Mac, its key is mode 0600 in `~/.config/claude-burst/ca/`, and it is
  name-constrained to the intercepted host (`api.anthropic.com`), so even trusted in the System
  keychain it cannot vouch for any other name.
- **Nothing privileged happens without you watching.** Steps needing root are run by scripts you
  invoke with `sudo`, or opened in Terminal by the dashboard for you to read and approve. The
  dashboard never escalates by itself.
- **Logs must not hold conversation content.** Logs and metrics are metadata only. The one place
  conversation content is stored is pauseless compaction's summaries, in
  `~/.config/claude-burst/compaction-state.json` at mode 0600, removed once a session has gone
  48 hours without a request.
- **Credentials stay where they were.** The gateway forwards Claude Code's own auth header and
  never stores an Anthropic credential. Secondary API keys live in the macOS login Keychain.

**Out of scope**

- **Other processes running as you.** Any local process can send the gateway requests, just as
  it could read your Claude credentials itself. Malware running as your user is not something a
  loopback service can defend against.
- **Root on your Mac.** Root can read the CA key and change anything Burst installs.
- **The providers you choose.** Overflow requests go to the secondary you configured, under its
  terms and retention policy.

The full list of trade-offs is in [docs/limitations.md](docs/limitations.md), and every change
Burst makes, with its undo, is in [ROLLBACK.md](ROLLBACK.md).
