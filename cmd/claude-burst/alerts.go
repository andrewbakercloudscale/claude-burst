package main

import (
	"context"
	"log"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/alerting"
	"github.com/andrewbakercloudscale/claude-burst/internal/config"
	"github.com/andrewbakercloudscale/claude-burst/internal/launcher"
	"github.com/andrewbakercloudscale/claude-burst/internal/notice"
)

// routeAlerts gives a publisher the "alerts" levels of config.json: what
// goes on screen as a toast, and what goes to Notification Center. Only the
// gateway posts to Notification Center itself (direct). Every other
// process leaves its alerts to the support console's watcher, which posts
// them from the person's login session.
func routeAlerts(p *notice.Publisher, direct bool, logger *log.Logger) (*alerting.Live, *alerting.Notifier) {
	live := alerting.NewLive()
	r := &alerting.Router{Config: live.Get}
	var n *alerting.Notifier
	if direct {
		n = alerting.NewNotifier(dashboardURL(), logf(logger))
		r.Direct = n
	}
	if evs, err := notice.Read(p.Path()); err == nil {
		r.Seed(evs)
	}
	p.SetChannels(r)
	return live, n
}

// watchAlerts runs the support console's Notification Center watcher.
func watchAlerts(live *alerting.Live, auditPath string, logger *log.Logger) {
	w := &alerting.Watcher{
		AuditPath: auditPath,
		Notifier:  alerting.NewNotifier(dashboardURL(), logf(logger)),
		Config:    live.Get,
		Now:       time.Now,
	}
	go w.Run(context.Background())
}

// dashboardURL is what a click on a notification opens.
func dashboardURL() string {
	cfg, err := config.Load()
	if err != nil {
		cfg = config.Default()
	}
	if cfg.AdminListen == "" {
		return ""
	}
	u, err := launcher.URL(cfg.AdminListen)
	if err != nil {
		return ""
	}
	return u
}

func logf(logger *log.Logger) func(string, ...any) {
	if logger == nil {
		return nil
	}
	return logger.Printf
}
