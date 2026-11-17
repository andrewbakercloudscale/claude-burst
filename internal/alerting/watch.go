package alerting

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
	"github.com/andrewbakercloudscale/claude-burst/internal/notice"
)

// Watcher takes Notification Center for the alerts the gateway did not
// post itself, run by the support console (always up, and in the person's
// login session). Those are the alerts of `claude-burst notice`, which the
// pf guard runs when the gateway is down, exactly when its alerts matter,
// and of the console's own publisher.
//
// It follows audit.jsonl, not notices.json: every publisher appends every
// alert there first, including the ones the toasts' level kept out of
// notices.json, which Notification Center may still want.
type Watcher struct {
	AuditPath string
	Notifier  *Notifier
	Config    func() config.AlertsConfig
	Now       func() time.Time

	offset  int64
	started bool
	nc      gate
}

// watchMaxAge: an alert older than this when first seen is old news (the
// console was down when it was written) and is not posted.
const watchMaxAge = 10 * time.Minute

// Run follows the audit until ctx ends. What is in it at start is not
// posted.
func (w *Watcher) Run(ctx context.Context) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		w.Tick()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Tick reads what was appended since the last tick and posts what passes.
func (w *Watcher) Tick() {
	st, err := os.Stat(w.AuditPath)
	if err != nil {
		return
	}
	if !w.started {
		w.started, w.offset = true, st.Size()
		return
	}
	if st.Size() < w.offset {
		// Moved to audit.jsonl.1 at its size limit: a new file.
		w.offset = 0
	}
	if st.Size() == w.offset {
		return
	}
	f, err := os.Open(w.AuditPath)
	if err != nil {
		return
	}
	defer f.Close()
	if _, err := f.Seek(w.offset, io.SeekStart); err != nil {
		return
	}
	b, err := io.ReadAll(io.LimitReader(f, 1<<20))
	if err != nil {
		return
	}
	// Only whole lines: one still being written is read next tick.
	end := bytes.LastIndexByte(b, '\n')
	if end < 0 {
		return
	}
	w.offset += int64(end + 1)
	level := w.Config().NotificationCenterLevel()
	now := w.Now()
	for _, line := range bytes.Split(b[:end], []byte("\n")) {
		var ev notice.Event
		if json.Unmarshal(line, &ev) != nil || ev.AuditOnly || ev.NotifyDone || ev.Title == "" {
			continue
		}
		if now.Sub(ev.At) > watchMaxAge {
			continue
		}
		if w.nc.pass(level, ev) {
			w.Notifier.Enqueue(ev)
		}
	}
}
