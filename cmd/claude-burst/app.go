package main

import (
	"fmt"
	"os"
	"path/filepath"

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
	default:
		fmt.Fprintln(os.Stderr, "usage: claude-burst app install|remove")
		os.Exit(2)
	}
}
