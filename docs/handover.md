# Session handover: HANDOFF.md read at start, written at close

[Back to the README](../README.md)

Two Claude Code hooks, installed and adapted from the dashboard's **Session handover** section. They only act in a repo whose git root has a `HANDOFF.md`: create one (even empty) to opt a repo in.

- **SessionStart** (new session or `/clear`): Claude is given a briefing, the newest handover section, the commits since `HANDOFF.md` last changed, `git status` and the repo's layout, and told to skim the code it names before answering.
- **SessionEnd** (`/exit`, Ctrl-D, `/clear`, or closing the Ghostty window, which Claude Code reports as reason `other`): a detached job (`setsid`, so the window's SIGHUP cannot kill it) resumes a fork of the finished session with the writer model, has it prepend a dated section to `HANDOFF.md`, and commits that one file locally. It never pushes. Sessions with fewer typed prompts than the minimum are skipped, and so is a session with nothing worth handing over. A macOS notification says when it is done.

The dashboard edits the briefing text, the writer's instructions, the writer model, the minimum prompts and whether to commit, and shows the log. Settings: `~/.config/claude-burst/handover.json` (defaults stored as empty, so they follow new defaults). Scripts, the settings they read, the log and the writer's last reply: `~/.config/claude-burst/handover/`. The scripts are embedded in the binary and rewritten when they differ, so edit `internal/handover/scripts/`, not the installed copies.

**Cost**: the writer re-reads the whole session, so a long session costs about one more turn of it. Pick a cheaper writer model to spend less.
