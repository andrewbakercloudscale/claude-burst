// Package alerting decides where each of the gateway's alerts goes: the
// on-screen toasts (notices.json, which the usage panel and the mod read),
// macOS Notification Center, both or neither, by the levels set under
// "alerts" in config.json.
//
// Notification Center is reached through a small helper app nested inside
// the Claude Burst app (internal/launcher), never osascript: until 3 Oct
// 2026 Burst posted through osascript, macOS filed those under Script
// Editor, and dropped them silently until Script Editor had been allowed to
// notify. The helper has its own bundle id and name, so macOS asks once for
// "Claude Burst" and lists it under that name.
package alerting

import (
	"strings"
	"sync"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
	"github.com/andrewbakercloudscale/claude-burst/internal/notice"
)

// criticalKinds are alerts that need a person now whatever their severity.
// Most are sent as errors anyway; the list is for the ones that are not,
// and for the ok that ends them, which must reach whoever saw the start.
//   - bypass: Claude Code no longer goes through Burst, so every long
//     session re-sends its whole uncompacted history.
//   - ext-pf-heal: the pf guard, which runs as root and speaks only through
//     alerts; its "redirect removed" is a warning that means bypassed.
//   - upstream, network: requests are failing, nothing works.
//   - intercept, guard-*: the redirect or its trust is broken.
var criticalKinds = map[string]bool{
	"bypass":         true,
	"ext-pf-heal":    true,
	"upstream":       true,
	"network":        true,
	"intercept":      true,
	"guard-pf":       true,
	"guard-watchdog": true,
}

// criticalTitles are single alerts of an otherwise routine kind, by kind
// and the start of the title: "Compaction failed" is a warning among the
// compaction kind's routine news, and it means the session goes on sending
// its full history until a retry works.
var criticalTitles = []struct{ kind, prefix string }{
	{"compaction", "Compaction failed"},
}

// alwaysKinds pass every level but off: the test alert is asked for, and a
// test that a level filtered out would say the channel is broken.
var alwaysKinds = map[string]bool{"test": true}

// Critical reports whether ev is one the "critical" level lets through: an
// error, or an alert listed above.
func Critical(ev notice.Event) bool {
	if ev.Severity == notice.Error || criticalKinds[ev.Kind] || alwaysKinds[ev.Kind] {
		return true
	}
	for _, c := range criticalTitles {
		if ev.Kind == c.kind && strings.HasPrefix(ev.Title, c.prefix) {
			return true
		}
	}
	return false
}

// gate is one channel's filter. Its state is the kinds it let a problem
// through for and has not yet seen resolved: an ok goes out wherever the
// problem it ends went, even at "critical" and even when the ok on its own
// would not, so "Config loads again" follows "config.json cannot be read"
// and a panel's red error is not left standing.
type gate struct {
	mu   sync.Mutex
	open map[string]bool
}

func (g *gate) pass(level string, ev notice.Event) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.open == nil {
		g.open = map[string]bool{}
	}
	if level == config.AlertLevelOff {
		// Nothing goes out, so nothing is left for an ok to end.
		g.open = map[string]bool{}
		return false
	}
	if ev.Severity == notice.OK {
		wasOpen := g.open[ev.Kind]
		delete(g.open, ev.Kind)
		return wasOpen || level == config.AlertLevelAll || Critical(ev)
	}
	ok := level == config.AlertLevelAll || Critical(ev)
	if ok && ev.Severity != notice.Info {
		g.open[ev.Kind] = true
	}
	return ok
}

// seed opens the kinds whose newest alert in evs is a problem: a gateway
// restarted between an error and its ok still sends the ok.
func (g *gate) seed(evs []notice.Event) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.open == nil {
		g.open = map[string]bool{}
	}
	for _, e := range evs {
		switch e.Severity {
		case notice.OK:
			delete(g.open, e.Kind)
		case notice.Warn, notice.Error:
			g.open[e.Kind] = true
		}
	}
}
