Set up Claude Burst on this Mac from https://github.com/andrewbakercloudscale/claude-burst

1. Read the README first: "What it changes on your Mac", Quickstart, Requirements and Uninstall.
   Tell me in three lines what the install will change, and wait for my go-ahead.
2. Check the requirements: macOS, Go 1.23 or later (go version), the Xcode Command Line Tools
   (xcode-select -p), zsh, and Claude Code logged in. If one is missing, tell me the command
   to install it and ask before running it.
3. Clone the newest release tag (the Releases page, or
   git ls-remote --tags --sort=-v:refname <repo> | head -1) to ~/claude-burst, and from that
   folder run:
     CLAUDE_BURST_MODE=base-url CLAUDE_BURST_SIGNING=no CLAUDE_BURST_PANEL=yes ./install.sh
   base-url mode needs no password. CLAUDE_BURST_PANEL=yes also installs the cost sidebar;
   ask me first, and use =no if I decline.
4. Verify: claude-burst status, curl -s http://127.0.0.1:7777/healthz, and that the
   dashboard answers at http://127.0.0.1:7788.
5. Tell me what was installed and where, that I must restart Claude Code for it to take
   effect, how to open the dashboard (claude-burst open), and that ./install.sh uninstall
   removes everything.

Never run sudo, and do not set up transparent mode, signing, a secondary provider or an API key:
list those as optional next steps with the README section for each, for me to do myself.
If a step fails, stop and show me its output; do not work around it.
