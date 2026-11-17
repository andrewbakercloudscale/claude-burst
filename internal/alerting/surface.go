package alerting

import (
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Whether anything on this Mac shows Burst's toasts. The toasts are drawn
// by the usage panel (a terminal pane beside Claude Code) and by the mod
// inside a terminal session; Claude Desktop draws neither. Someone who uses
// Claude Code only through Claude Desktop would see no alert at all, so
// where nothing shows the toasts Notification Center takes every alert by
// default (config.AlertsConfig.NotificationCenterLevelFor, 17 Nov 2026).

// claimRecent is how recently a toast must have been claimed for the
// surface that claimed it to count as in use: a panel installed once and
// abandoned is not where the person looks.
const claimRecent = 14 * 24 * time.Hour

// ToastsShown reports whether something under home shows Burst's toasts:
// the usage panel installed (~/.local/bin/ccusage-panel.sh), the mod
// installed by update-mod.sh (~/.config/claude-panel/mod-installed), or a
// toast claimed in the last two weeks (~/.config/claude-panel/
// alerts-claimed/<id>, which the panels and the mod each make as they show
// one).
func ToastsShown(home string, now time.Time) bool {
	if fi, err := os.Stat(filepath.Join(home, ".local", "bin", "ccusage-panel.sh")); err == nil && fi.Mode()&0o111 != 0 {
		return true
	}
	panel := filepath.Join(home, ".config", "claude-panel")
	if _, err := os.Stat(filepath.Join(panel, "mod-installed")); err == nil {
		return true
	}
	ents, err := os.ReadDir(filepath.Join(panel, "alerts-claimed"))
	if err != nil {
		return false
	}
	for _, e := range ents {
		if fi, err := e.Info(); err == nil && now.Sub(fi.ModTime()) < claimRecent {
			return true
		}
	}
	return false
}

// shownEvery is how long ToastsShownCached trusts its answer: alerts are
// routed from request paths, which must not list a directory each time.
const shownEvery = 5 * time.Minute

var shown struct {
	mu   sync.Mutex
	at   time.Time
	have bool
	v    bool
}

// ToastsShownCached is ToastsShown for this user, read again at most every
// five minutes.
func ToastsShownCached() bool {
	shown.mu.Lock()
	defer shown.mu.Unlock()
	if !shown.have || time.Since(shown.at) >= shownEvery {
		home, err := os.UserHomeDir()
		shown.v = err == nil && ToastsShown(home, time.Now())
		shown.at, shown.have = time.Now(), true
	}
	return shown.v
}
