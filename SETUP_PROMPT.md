Set up Claude Burst on this Mac from https://github.com/andrewbakercloudscale/claude-burst

1. Read the README first: "What it changes on your Mac", Quickstart, Requirements and Uninstall.
   Tell me in three lines what the install will change, and wait for my go-ahead.
2. Check the requirements: macOS, Go 1.23 or later (go version), the Xcode Command Line Tools
   (xcode-select -p), zsh, and Claude Code logged in. If one is missing, tell me the command
   to install it and ask before running it.
3. Clone the newest release tag (the Releases page, or
   git ls-remote --tags --sort=-v:refname <repo> | head -1) to ~/claude-burst.
4. Install in transparent mode, the default: Remote Control keeps working. It needs my
   password, which you cannot type, so do not run the installer yourself. Give me this one
   line to paste into a terminal of my own, and wait until I say it has finished:
     cd ~/claude-burst && CLAUDE_BURST_MODE=transparent CLAUDE_BURST_PANEL=yes ./install.sh
   CLAUDE_BURST_PANEL=yes also installs the cost sidebar; ask me first, and use =no if I
   decline. Only if I say I cannot give a password, run it yourself with
   CLAUDE_BURST_MODE=base-url CLAUDE_BURST_SIGNING=no instead, and tell me Remote Control
   is off in that mode.
5. Verify: claude-burst status, and that the dashboard answers at http://127.0.0.1:7788.
6. Tell me what was installed and where, that I must restart Claude Code for it to take
   effect, how to open the dashboard (claude-burst open), and that ./install.sh uninstall
   removes everything.

Never run sudo yourself, and do not set up a secondary provider or an API key: list those as
optional next steps with the README section for each, for me to do myself.
If a step fails, stop and show me its output; do not work around it.
