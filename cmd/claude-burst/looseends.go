package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/admin"
	"github.com/andrewbakercloudscale/claude-burst/internal/config"
)

// looseEndsCmd prints what is lying uncommitted, unpushed or stashed in
// the repositories Claude Code sessions have worked in: the dashboard's
// Loose ends section, for a terminal. It only reads.
func looseEndsCmd(args []string) {
	fs := flag.NewFlagSet("loose-ends", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "print the report as JSON")
	_ = fs.Parse(args)
	cfg, _ := config.Load()
	now := time.Now()
	rp := admin.LooseEnds(cfg, now)
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(rp)
		return
	}
	home, _ := os.UserHomeDir()
	tilde := func(p string) string {
		if home != "" && strings.HasPrefix(p, home+"/") {
			return "~" + p[len(home):]
		}
		return p
	}
	ago := func(t float64) string {
		if t <= 0 {
			return "?"
		}
		d := now.Sub(time.Unix(int64(t), 0))
		switch {
		case d < 90*time.Minute:
			return fmt.Sprintf("%dm", int(d.Minutes()))
		case d < 48*time.Hour:
			return fmt.Sprintf("%dh", int(d.Hours()))
		}
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
	if len(rp.Repos) == 0 {
		fmt.Printf("Nothing loose in %d repositories (local time %s).\n", rp.Scanned, now.Format("15:04"))
		return
	}
	f, c, s := rp.Counts()
	fmt.Printf("Loose ends in %d of %d repositories, at %s: %d uncommitted, %d unpushed, %d stashed\n", len(rp.Repos), rp.Scanned, now.Format("15:04"), f, c, s)
	for _, r := range rp.Repos {
		fmt.Printf("\n%s  (%s)\n", tilde(r.Root), r.Branch)
		if r.Error != "" {
			fmt.Printf("  error: %s\n", r.Error)
		}
		for _, u := range r.Uncommitted {
			rel, err := filepath.Rel(r.Root, u.Path)
			if err != nil {
				rel = u.Path
			}
			who := "no session recorded, changed " + ago(u.MTime) + " ago"
			if o := u.Owner; o != nil && o.Live {
				who = "being worked on by " + o.Label
			} else if o != nil {
				who = "left by " + o.Label + " when it closed, " + ago(o.Since) + " ago"
			}
			fmt.Printf("  %s %s  %s\n", u.Status, rel, who)
		}
		if r.Ahead > 0 {
			to := r.Upstream
			if to == "" {
				to = "any remote"
			}
			fmt.Printf("  %d unpushed to %s\n", r.Ahead, to)
			for _, cm := range r.Unpushed {
				fmt.Printf("    %s %s (%s ago)\n", cm.Hash, cm.Subject, ago(cm.When))
			}
		}
		for _, st := range r.Stashes {
			fmt.Printf("  %s %s (%s ago)\n", st.Ref, st.Subject, ago(st.When))
		}
	}
}
