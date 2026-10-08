# Roadmap

What is planned next, in the order it will be done. Each task says what is wrong today, what will be built, and how to tell it is done. Nothing here is a promise of a date.

Last updated 8 October 2026, at v0.20.21.

| # | Task | Area | State |
|---|---|---|---|
| 1 | [Keep state across a gateway restart](#1-keep-state-across-a-gateway-restart) | Resilience | Started |
| 2 | [Count what a restart costs](#2-count-what-a-restart-costs) | Resilience | Open |
| 3 | [Find the cause of the repeated "still over" notice](#3-find-the-cause-of-the-repeated-still-over-notice) | Resilience | Open |
| 4 | [Split the dashboard page](#4-split-the-dashboard-page) | Code quality | Open |
| 5 | [Lint in CI](#5-lint-in-ci) | Code quality | Open |
| 6 | [Break up the largest Go files](#6-break-up-the-largest-go-files) | Code quality | Open |
| 7 | [A fast deploy](#7-a-fast-deploy) | Development | Open |
| 8 | [One release command](#8-one-release-command) | Development | Open |
| 9 | [A live test script](#9-a-live-test-script) | Development | Open |

## Resilience

### 1. Keep state across a gateway restart

**Today.** A restart is routine: every deploy and every upgrade is one. Compaction state (summaries, limits, notices) already survives it. Four things did not:

- **Which conversation a reply belongs to.** A session on a message thread sends only what is new and names the reply it continues. After a restart that reply was unknown, so the session went on under a conversation of its own, apart from its summary and its limit, until it next sent its history whole.
- **The context inspector.** It keeps each session's latest whole request in memory only, by design: prompts are never written to disk.
- **Automask's masks.** The table of value to mask is in memory only, by design: the values are the secrets.
- **Automask's counts and its Last 50 masks list.** These hold no values.

**Plan.**

- Done: the latest 512 replies are saved in `compaction-threads.json` (ids and sizes, no content) and read at start.
- Save Automask's counts and Last 50 masks list the same way.
- Leave the inspector's requests and Automask's table in memory. Say so on the dashboard after a restart instead of showing an empty list.
- Check one risk: a session on a thread after a restart gets a new mask table, so a value could be given a mask number that the history the API holds already uses for another value. If it can happen, ask that session for its history once so the table is rebuilt in the same order.

**Done when.** After a deploy, a session on a thread keeps its conversation, its summary and its limit with no request for its history, and the Automask section reads as it did before.

### 2. Count what a restart costs

**Today.** When the gateway asks a session for its whole history, the request that comes back is a cache write of the whole context at full price. Nothing counts it, so the savings figure is too kind by that much.

**Plan.** Mark the request that answers a request for the history in `metrics.jsonl` with why it was asked for. Show the total as its own line beside compaction savings: how many, and what they cost.

**Done when.** The dashboard shows the number and cost of whole history re-sends for the period, by reason, and net savings takes them off.

### 3. Find the cause of the repeated "still over" notice

**Today.** "Still over after the summary" fired three times in 11 minutes in one session, and a summary was dropped soon after with no confirmed cause.

**Plan.** Read the log for that session around both times, reproduce each in a test from the logged sizes, and fix what the test shows. The notice should fire once for a summary, not once for each request.

**Done when.** A test fails on the old code for each of the two, and the notice appears once.

## Code quality

### 4. Split the dashboard page

**Today.** `internal/admin/admin.html` is one file of 7,374 lines holding the markup, the styles and all the script. Its script is checked by parsing it and by one smoke test in a browser.

**Plan.** Move the script into files by section (usage, compaction, inspector, Automask, settings), embedded in the binary at build time and served as now. Add unit tests in Node for the functions that work out what is shown.

**Done when.** No file of the page is over 1,500 lines, the page looks and behaves the same in the smoke test, and the functions that do sums have tests.

### 5. Lint in CI

**Today.** CI is one workflow that builds and tests. There is no lint configuration.

**Plan.** Add `gofmt` and `go vet` as a check that fails, then `golangci-lint` with a short list of linters (unused, ineffassign, errcheck on writes to disk), fixing what it finds before it is made to fail.

**Done when.** A badly formatted file or an unchecked write to disk fails CI.

### 6. Break up the largest Go files

**Today.** `internal/coord/coord.go` is 1,494 lines, `internal/config/config.go` 1,038 and `internal/admin/trace.go` 1,004.

**Plan.** Split each by what it does, with no change in behaviour and no change to the tests other than their file names. One commit a file.

**Done when.** No Go file outside the tests is over 800 lines, and the tests pass unchanged.

## Development

### 7. A fast deploy

**Today.** `scripts/deploy.sh` runs every test before it installs, which takes 2 to 4 minutes, even for a change to one line of the page.

**Plan.** `scripts/deploy.sh --quick` runs the tests of the packages that changed since the last deploy and the page checks, and says which it left out. The full run stays the default and is what a release uses.

**Done when.** A change to the page alone deploys in under a minute, and a release cannot be cut from a quick deploy.

### 8. One release command

**Today.** A release is done by hand: two lines of the README, a commit, a push, waiting for CI, a tag, and notes. One of the README lines was once left at the old version.

**Plan.** `scripts/release.sh <version>` checks the tree is clean and the version constant matches, edits the README, commits, pushes, waits for CI, tags and publishes with notes from a file. It stops at the first step that fails and says how to go on.

**Done when.** A release is one command and a notes file.

### 9. A live test script

**Today.** These are covered by tests only and have not been tried on a real session: `/burst-dump` and `/burst-prune` typed in a session, the dashboard's Prune buttons, a compaction made while a session is idle, the inspector across a restart, and the newer Automask rules.

**Plan.** One script that starts a throwaway Claude Code session through the gateway, drives each of those, and reports what it saw against what was expected.

**Done when.** The script passes on this Mac and its report is kept with each release.

## Smaller things, not yet ordered

- Automask: password hashes, customer rows in the output of a query, internal host names and private addresses.
- Pruning by itself, only when it costs no cache rewrite: at a compaction swap, or when the cache has gone cold after an idle spell.
- The 1 hour cache write extra for a long Claude Haiku 5.5 prompt is counted at the short prompt rate.
- The strategies table says "over 31 days" while its chart starts at the first day with cost data.
- A log line when a Remote Control stream opens.
- Moving the summary into Anthropic's own compaction block.
