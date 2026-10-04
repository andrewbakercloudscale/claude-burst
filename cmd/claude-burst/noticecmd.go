package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
	"github.com/andrewbakercloudscale/claude-burst/internal/notice"
)

// noticeCmd puts an alert on screen without the gateway: it writes
// notices.json itself, which the usage panel watches. For the guards, whose
// alerts matter most exactly when the gateway is down, so the dashboard's
// /api/alert is not there to take them (2026-10-04: every pf guard alert of
// a 7-minute outage went to its log as "not delivered"). Same "ext-" kinds
// as /api/alert, so an ok from either clears a warning from either.
func noticeCmd(args []string) {
	fs := flag.NewFlagSet("notice", flag.ExitOnError)
	kind := fs.String("kind", "", "alert kind, e.g. pf-heal")
	severity := fs.String("severity", notice.Info, "info, ok, warn or error")
	title := fs.String("title", "", "the alert's title")
	detail := fs.String("detail", "", "the alert's detail line")
	_ = fs.Parse(args)
	switch *severity {
	case notice.Info, notice.OK, notice.Warn, notice.Error:
	default:
		fmt.Fprintln(os.Stderr, "severity must be info, ok, warn or error")
		os.Exit(2)
	}
	if *kind == "" || *title == "" {
		fmt.Fprintln(os.Stderr, "usage: claude-burst notice --kind K --severity S --title T [--detail D]")
		os.Exit(2)
	}
	dir, err := config.ConfigDir()
	if err != nil {
		fatal(err)
	}
	p := notice.New(notice.Path(dir), nil)
	p.Publish("ext-"+*kind, *severity, *title, *detail)
	p.Flush(3 * time.Second)
}
