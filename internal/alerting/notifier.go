package alerting

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/launcher"
	"github.com/andrewbakercloudscale/claude-burst/internal/notice"
)

// Variables so tests never run the real helper: a test that posted would
// put a banner on the Mac running it, and its first run would ask for
// permission there.
var (
	// helperPath is the installed helper's executable, "" when there is none.
	helperPath = func() string {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		p := launcher.NotifierExecutable(home)
		if st, err := os.Stat(p); err != nil || st.IsDir() {
			return ""
		}
		return p
	}
	runHelper = func(ctx context.Context, path string, args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, path, args...)
		cmd.WaitDelay = time.Second
		return cmd.Output()
	}
)

// postWait is how long one post may take: the first one waits for the
// answer to macOS's permission prompt (the helper gives it 60 seconds).
const postWait = 90 * time.Second

// ErrNoHelper is Post and Status without the helper installed.
var ErrNoHelper = errors.New("the Notification Center helper is not installed (claude-burst app install builds it)")

// Result is the helper's one line of output.
type Result struct {
	// Status is macOS's answer for Claude Burst: authorized, denied,
	// not-determined, provisional or unknown.
	Status string `json:"status"`
	Posted bool   `json:"posted"`
	Error  string `json:"error,omitempty"`
}

// Notifier posts alerts through the helper, one at a time, off the caller's
// path: Enqueue never blocks.
type Notifier struct {
	// URL is opened when a notification is clicked: the dashboard.
	URL  string
	Logf func(format string, args ...any)

	start  sync.Once
	ch     chan notice.Event
	mu     sync.Mutex
	logged map[string]bool // problems already logged, said once each
}

// NewNotifier returns a notifier; logf may be nil.
func NewNotifier(url string, logf func(string, ...any)) *Notifier {
	return &Notifier{URL: url, Logf: logf}
}

// queueLen bounds what waits behind a slow post (the first one waits for
// the permission prompt). Past it alerts are dropped and the log says so.
const queueLen = 32

// Enqueue hands ev to the helper and reports whether it was queued.
func (n *Notifier) Enqueue(ev notice.Event) bool {
	if n == nil {
		return false
	}
	n.start.Do(func() {
		n.ch = make(chan notice.Event, queueLen)
		go n.loop()
	})
	select {
	case n.ch <- ev:
		return true
	default:
		n.logOnce("queue full", "warn stage=notification-center queue full, alert dropped: %q", ev.Title)
		return false
	}
}

func (n *Notifier) loop() {
	for ev := range n.ch {
		ctx, cancel := context.WithTimeout(context.Background(), postWait)
		r, err := n.Post(ctx, ev)
		cancel()
		switch {
		case errors.Is(err, ErrNoHelper):
			n.logOnce("no helper", "warn stage=notification-center %v; alerts go to the toasts only", err)
		case err != nil:
			n.logf("warn stage=notification-center title=%q err=%q", ev.Title, err.Error())
		case !r.Posted:
			// Denied is said once: it is the person's choice, not a fault.
			n.logOnce("status "+r.Status, "notification-center not posted status=%s: %s (said once)", r.Status, r.Error)
		default:
			n.logf("notification-center posted kind=%q title=%q", ev.Kind, ev.Title)
		}
	}
}

// Post runs the helper for ev now and waits for it.
func (n *Notifier) Post(ctx context.Context, ev notice.Event) (Result, error) {
	args := []string{"post", "--title", ev.Title, "--body", clip(ev.Detail, 400), "--group", ev.Kind}
	if ev.ID != "" {
		args = append(args, "--id", ev.ID)
	}
	if n != nil && n.URL != "" {
		args = append(args, "--url", n.URL)
	}
	if ev.Severity == notice.Error {
		args = append(args, "--sound")
	}
	return run(ctx, args...)
}

// Status asks the helper what macOS says about Claude Burst's
// notifications, without asking the person anything.
func Status(ctx context.Context) (Result, error) { return run(ctx, "status") }

func run(ctx context.Context, args ...string) (Result, error) {
	p := helperPath()
	if p == "" {
		return Result{}, ErrNoHelper
	}
	out, err := runHelper(ctx, p, args...)
	var r Result
	// The helper prints its line on failure too (exit 2 is denied): the
	// line, when there is one, says more than the exit status.
	if jerr := json.Unmarshal(lastLine(out), &r); jerr == nil && r.Status != "" {
		return r, nil
	}
	if err != nil {
		return Result{}, fmt.Errorf("notifier helper: %w", err)
	}
	return Result{}, fmt.Errorf("notifier helper printed %q", strings.TrimSpace(string(out)))
}

func lastLine(b []byte) []byte {
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	return []byte(lines[len(lines)-1])
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}

func (n *Notifier) logf(format string, args ...any) {
	if n.Logf != nil {
		n.Logf(format, args...)
	}
}

func (n *Notifier) logOnce(key, format string, args ...any) {
	n.mu.Lock()
	if n.logged == nil {
		n.logged = map[string]bool{}
	}
	seen := n.logged[key]
	n.logged[key] = true
	n.mu.Unlock()
	if !seen {
		n.logf(format, args...)
	}
}

// startHelper starts the helper and does not wait for it; a variable for
// tests.
var startHelper = func(path string, args ...string) error {
	return exec.Command(path, args...).Start()
}

// AskIfUnasked starts macOS's permission prompt when it has never been
// answered, and returns the status it found. `claude-burst app install`
// runs it, so the question comes while someone is installing or deploying,
// not in the middle of the outage the first critical alert is about. The
// helper waits for the answer on its own, up to two minutes; nothing here
// waits for it.
func AskIfUnasked(ctx context.Context) (string, error) {
	r, err := Status(ctx)
	if err != nil {
		return "", err
	}
	if r.Status == "not-determined" {
		if err := startHelper(helperPath(), "request", "--wait", "120"); err != nil {
			return r.Status, err
		}
	}
	return r.Status, nil
}
