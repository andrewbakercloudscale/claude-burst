# Contributing to Claude Burst

Thank you for helping. Bug reports, fixes and documentation corrections are all welcome. For
anything larger than a fix, open an issue first so we can agree the shape before you write it.

## Build and run

```bash
go build ./...                                   # Go 1.23 or later
go build -o /tmp/claude-burst ./cmd/claude-burst # a binary you can run without installing
./install.sh                                     # build, install and start it for real
```

`./install.sh` builds the working tree, including anything you have not committed, and says
so. To ship exactly what is committed, use `scripts/deploy.sh --only-committed`.

## Test

```bash
go test ./... -race
go vet ./...
python3 scripts/ci/check-doc-links.py            # every relative link and #anchor in the Markdown resolves
zsh -n install.sh                                # syntax of a zsh script you changed
```

CI runs all of these on every push and pull request (see `.github/workflows/test.yml`),
plus `bash -n` or `zsh -n` on every script and a parse check of the dashboard's JavaScript.

The tests include a simulated Anthropic subscription rejection that verifies the same request is replayed to the secondary, the model is remapped, the OAuth beta is removed from a Bedrock call, and the overflow reset state is persisted (the suite covers both Bedrock and the OpenAI-compatible translator), plus an equivalent suite for the metered-failures strategy (sustained-failure threshold, window expiry, success reset, and the no-subscription primary forwarding its own auth header unchanged).

**Session coordination** has two layers. `go test ./internal/coord` drives the hooks with simulated sessions, including two sessions writing a sentence one word each in turn. `scripts/coord-live-test.sh` does the same with two real Claude Code sessions: it builds the binary, makes a throwaway repository and passes the hooks with `--settings`, so your own settings are not touched and coordination does not need to be on. It checks every word lands, nothing waits, the first session is master with the second recorded as coordinating with it, the second may not commit the file, and the master's commit settles it.

```bash
bash scripts/coord-live-test.sh    # KEEP=1 keeps the temp directory to look at
```

**`coord-live-test.sh` costs real requests**: about 11 short Haiku requests on whatever
account Claude Code is logged into. Run it by hand when you change coordination, never in CI.

## Rules for tests and CI

- **Never run a root script in CI or in a test.** `transparent-root.sh`, `lid-awake-root.sh`,
  `install-pf-heal.sh`, `trust-ca-systemwide.sh` and `rollback.sh` change `/etc/hosts`, pf,
  the System keychain and power settings on the machine they run on. Tests use the self-test
  modes (for example `scripts/pf-heal.sh --self-test`) and stubs.
- **A temporary `HOME` does not isolate a test from the Mac.** `defaults`, `sudo`, `pmset`,
  `security` and `launchctl` act on the real machine whatever `HOME` says. Stub them, and
  assert that the stub ran, so a test cannot pass by quietly touching the real thing.
- **A checker must fail when it cannot check.** A gate that reports OK because it found
  nothing to look at is worse than no gate. Have it say how many things it checked.

## Style

- **No em dashes or en dashes, anywhere**: code, comments, docs, commit messages, UI text.
  Use a colon, a comma or a full stop; use a hyphen for ranges (`2-120`).
- British spelling in prose (behaviour, organisation, licence as a noun).
- Plain and direct. Say what something does and **why**, especially when the why is an
  incident: the date and what broke is what stops the next person undoing the fix.
- Times shown to people are local time, never UTC or raw ISO timestamps.
- Comments explain decisions and constraints, not what the next line obviously does.
- `gofmt` everything. Keep scripts zsh unless there is a reason (recovery scripts also
  work under bash, because someone in an emergency will type `bash`).
- Documentation lives in `README.md` (the front door) and `docs/`. Proposals go in
  `docs/design/` marked as proposals, decisions in `docs/decisions/`, finished
  investigations in `docs/history/`. Fix every link you move: the link checker will tell you.

## Commits and pull requests

- One logical change per commit, with a message that says what changed and why.
- Keep pull requests small enough to review in one sitting, and fill in the template.
- If your change alters what Burst installs or touches on a Mac, update
  [ROLLBACK.md](ROLLBACK.md) and the "What it changes on your Mac" box in the README.

## Security issues

Do not open a public issue for a vulnerability. See [SECURITY.md](SECURITY.md).
