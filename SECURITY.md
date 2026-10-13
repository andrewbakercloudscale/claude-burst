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
older releases are not patched. To update, press **Upgrade** on the dashboard, or check out the
newest release tag and rerun `./install.sh`.

## Releases are signed

Every release tag from v0.20.30 on is signed with an ed25519 key that is not held by GitHub.
Its public half is `scripts/release-signers`, with the fingerprint
`SHA256:o9VHa+3GhZRnNz34XijTOGenHGb9VYIc4gGvpoJpw9U`.

An installed Claude Burst only ever moves to the newest release that key signed. The
dashboard's Upgrade and Install GitHub version buttons, `burst-reinstall`, `burst-repair` and
the usage panel's installer (whose releases the same key signs) all go through
`scripts/verify-release.sh`, using the copy already on the Mac, never the one in the code just
fetched. Code pushed to `main` that is not a signed release is not installed by any of them,
and a newer tag that is not signed stops the update and says so. Before this, each of them ran
whatever GitHub's `main` held, two of them going on to ask for your password.

What this does not cover, plainly:

- **The first install.** Nothing on the Mac can check it yet, so a first `git clone`, and the
  one-line `curl ... | bash` commands in the README, trust GitHub and TLS. Check a clone
  yourself before you run its installer: `scripts/verify-release.sh .` prints the release tag
  and commit and exits 0 only for a signed release. Compare the fingerprint above with one
  from somewhere other than the clone, such as this page on github.com.
- **An install from before v0.20.30.** It has no verifier, so its next update is taken on
  trust as before, and is verified from then on.
- **A stolen release key.** Whoever holds it can sign a release. It is kept off GitHub for
  that reason.
- **The binary is built on your Mac** from the verified source, with the Go toolchain you
  have. There are no downloaded binaries to sign, and the build is not notarised by Apple.

## Threat model, in brief

Claude Burst is a local gateway that sits in the path of your Claude Code traffic. What it is
designed to protect, and what it is not:

**In scope**

- **Other websites must not be able to use the gateway or the dashboard.** Both listen on
  loopback only. The gateway refuses requests carrying a browser `Origin` header or a `Host`
  that is not its own loopback name or the intercepted host, and so does the Codex port
  (`Origin`, `Sec-Fetch-Site` and `Host`). The dashboard checks `Host`
  (against DNS rebinding) and requires a custom header on every change, which a cross-origin
  page cannot send without a CORS preflight the server never answers; no CORS headers are ever
  returned, so responses cannot be read cross-origin.
- **Another account on the same Mac must not be able to use the dashboard.** Loopback is
  shared between accounts, so the dashboard and support console API ask whose process is
  calling and answer only the account Burst runs as. One route is open to all, the alert
  the root pf guard posts, which can only raise a notice. A process of your own account is
  not kept out: it could read the same files directly. The gateway port does not make this
  check.
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
  never writes an Anthropic credential to disk or reads Claude Code's stored login. The auth
  header of the most recent request is held in memory, for the requests the gateway sends on
  the session's behalf (summaries, and the test message when you press it), and is never
  logged, served by the dashboard, or sent with certificate checks off. Secondary API keys live in the macOS login Keychain.
  The dashboard asks for Touch ID (or the login password) before it shows a stored key, before a
  save that would send a stored key to a host it does not go to now, and before a key typed in
  replaces a stored one.

**Out of scope**

- **Other processes running as you.** Any local process can send the gateway requests, just as
  it could read your Claude credentials itself. Malware running as your user is not something a
  loopback service can defend against.
- **Root on your Mac.** Root can read the CA key and change anything Burst installs.
- **The providers you choose.** Overflow requests go to the secondary you configured, under its
  terms and retention policy.

The full list of trade-offs is in [docs/limitations.md](docs/limitations.md), and every change
Burst makes, with its undo, is in [ROLLBACK.md](ROLLBACK.md).
