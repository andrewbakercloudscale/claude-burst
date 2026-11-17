package alerting

import (
	"sync"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
	"github.com/andrewbakercloudscale/claude-burst/internal/notice"
)

// Router is a publisher's notice.Channels: the toasts' level decides what
// goes in notices.json, Notification Center's what goes to the helper.
//
// The gateway's router posts to Notification Center itself (Direct set).
// Any other process that publishes (`claude-burst notice`, run by the pf
// guard as the console user when the gateway is down, and the support
// console) leaves it to the support console's Watcher: those alerts are in
// the audit without NotifyDone, and the console is a LaunchAgent in the
// person's login session, where the helper can reach Notification Center.
// A process started by a root daemon through sudo is not.
type Router struct {
	// Direct posts to Notification Center; nil leaves it to the Watcher.
	Direct *Notifier
	// Config returns the current "alerts" settings.
	Config func() config.AlertsConfig

	toast, nc gate
}

// Route implements notice.Channels.
func (r *Router) Route(ev notice.Event) (toast, notifyDone bool) {
	cfg := r.Config()
	toast = r.toast.pass(cfg.ToastLevel(), ev)
	if r.Direct == nil {
		return toast, false
	}
	if r.nc.pass(cfg.NotificationCenterLevel(), ev) {
		r.Direct.Enqueue(ev)
	}
	return toast, true
}

// Seed tells the toasts' filter which problems notices.json shows standing,
// so the ok that ends one after a restart still goes on screen.
func (r *Router) Seed(evs []notice.Event) { r.toast.seed(evs) }

// Live is config.json's "alerts" block, read again at most every interval.
// Alerts are published from request paths, which must not read a file each
// time; Set makes a change saved from the dashboard apply at once.
type Live struct {
	Load     func() (config.Config, error)
	Interval time.Duration

	mu   sync.Mutex
	at   time.Time
	have bool
	cur  config.AlertsConfig
}

// NewLive reads config.json through config.Load every 10 seconds at most.
func NewLive() *Live { return &Live{Load: config.Load, Interval: 10 * time.Second} }

// Get returns the settings, reloading when they are older than Interval.
// A config.json that does not load keeps the last settings that did, or
// the defaults: an alert is never lost to a typo elsewhere in the file.
func (l *Live) Get() config.AlertsConfig {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.have || time.Since(l.at) >= l.Interval {
		l.at, l.have = time.Now(), true
		if c, err := l.Load(); err == nil {
			l.cur = c.Alerts
		}
	}
	return l.cur
}

// Set replaces the settings now.
func (l *Live) Set(c config.AlertsConfig) {
	l.mu.Lock()
	l.cur, l.at, l.have = c, time.Now(), true
	l.mu.Unlock()
}

var (
	defMu   sync.Mutex
	defLive *Live
	defNote *Notifier
)

// SetDefault records the live settings and the notifier of this process,
// for the dashboard's handlers: saving a level, sending a test. serve sets
// them; in tests that never do, the handlers say Notification Center is
// not running here.
func SetDefault(l *Live, n *Notifier) {
	defMu.Lock()
	defLive, defNote = l, n
	defMu.Unlock()
}

// Default returns what SetDefault recorded.
func Default() (*Live, *Notifier) {
	defMu.Lock()
	defer defMu.Unlock()
	return defLive, defNote
}
