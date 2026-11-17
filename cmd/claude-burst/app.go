package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/alerting"
	"github.com/andrewbakercloudscale/claude-burst/internal/config"
	"github.com/andrewbakercloudscale/claude-burst/internal/launcher"
)

// openCmd shows the dashboard in the browser: `claude-burst open`. With the
// gateway down it shows the support console, which is up when the gateway is
// not. A config that does not load falls back to defaults, as the console does.
func openCmd(args []string) {
	rejectArgs("open", args)
	cfg, err := config.Load()
	if err != nil {
		cfg = config.Default()
	}
	var urls []string
	for _, listen := range []string{cfg.AdminListen, cfg.ConsoleListen()} {
		if listen == "" {
			continue
		}
		if u, err := launcher.URL(listen); err == nil {
			urls = append(urls, u)
		}
	}
	if len(urls) == 0 {
		fatal(fmt.Errorf("the dashboard and the support console are both off (admin_listen, console_listen)"))
	}
	u, err := launcher.Open(urls...)
	if err != nil {
		fatal(fmt.Errorf("%w: is the gateway running? See: claude-burst status", err))
	}
	fmt.Println(u)
}

// appCmd installs or removes the Claude Burst app in ~/Applications:
// `claude-burst app install|remove`. install.sh and deploy.sh run install,
// and install.sh uninstall runs remove.
func appCmd(args []string) {
	home, err := os.UserHomeDir()
	if err != nil {
		fatal(err)
	}
	switch {
	case len(args) == 1 && args[0] == "install":
		bin, err := os.Executable()
		if err != nil {
			fatal(err)
		}
		// The installed path, not what a symlink to it resolves to: an
		// update replaces that file and the app goes on running it.
		if !filepath.IsAbs(bin) {
			fatal(fmt.Errorf("cannot tell where claude-burst is installed (%q)", bin))
		}
		app, err := launcher.Install(home, bin, version)
		if err != nil {
			fatal(err)
		}
		fmt.Printf("%s: installed. Cmd-Space, type \"burst\", Return opens the dashboard; drag it to the Dock to keep it there.\n", app)
		installNotifier(home)
	case len(args) == 1 && args[0] == "remove":
		removed, err := launcher.Remove(home)
		if err != nil {
			fatal(err)
		}
		if removed {
			fmt.Printf("%s: removed\n", launcher.Path(home))
		} else {
			fmt.Printf("%s: not there, or not made by Claude Burst; nothing removed\n", launcher.Path(home))
		}
	case len(args) == 1 && args[0] == "notifications":
		// After "denied" macOS never asks again: System Settings is the
		// only way back, so this opens it at Claude Burst.
		if err := alerting.OpenSettings(); err != nil {
			fatal(fmt.Errorf("could not open System Settings: %w; open it at Notifications, Claude Burst", err))
		}
		fmt.Println("System Settings is open at Notifications, Claude Burst: turn on Allow notifications.")
	default:
		fmt.Fprintln(os.Stderr, "usage: claude-burst app install|remove|notifications")
		os.Exit(2)
	}
}

// installNotifier builds the Notification Center helper inside the app when
// it changed (deploy.sh and install.sh run app install every time), and has
// macOS ask for permission if it never has. A failure is a warning: the
// toasts still work, and the dashboard's Notifications section says the
// helper is missing.
func installNotifier(home string) {
	what, err := launcher.InstallNotifier(home)
	if err != nil {
		fmt.Fprintf(os.Stderr, "WARNING: the Notification Center helper was not built (%v); alerts still show as toasts. Retry: claude-burst app install\n", err)
		return
	}
	fmt.Printf("%s: %s\n", launcher.NotifierPath(home), what)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, err := alerting.AskIfUnasked(ctx)
	switch {
	case err != nil:
		fmt.Fprintf(os.Stderr, "WARNING: could not ask the Notification Center helper for its permission: %v\n", err)
	case st == "not-determined":
		fmt.Println("Notification Center: a prompt from Claude Burst asks whether it may send notifications; choose Allow.")
	case st == "denied":
		// Installs before 17 Nov 2026 asked from a background process,
		// which macOS answered "denied" without showing a prompt; it never
		// asks again, so say where to turn it on.
		fmt.Println("Notification Center: turned off for Claude Burst. Alerts reach you there only once it is allowed:")
		fmt.Println("    claude-burst app notifications    (opens System Settings, Notifications, Claude Burst)")
	default:
		fmt.Printf("Notification Center: %s\n", st)
	}
}
