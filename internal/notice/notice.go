// Package notice publishes what the gateway wants a person to see on
// screen right now: a failover, a dead network, a restart. It writes
// ~/.config/claude-burst/notices.json, which the usage panel watches and
// turns into the same floating overlay it shows for loading and for a
// pauseless compaction.
//
// Every event shown is also appended to audit.jsonl beside it, the support
// trail the dashboard and the console show: notices.json keeps only the
// last few for the overlay, the audit keeps weeks. Actions taken from the
// dashboard or the console are recorded there too (Record), without being
// shown on screen.
//
// Publishing never blocks the caller. Events go through a buffered channel
// to one writer goroutine, so a request path that publishes pays for a
// channel send and nothing else; a full channel drops the event and says
// so once in the log.
package notice

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/atomicfile"
)

// Severities. An ok event resolves the earlier events of its own kind: the
// panel keeps an error on screen until one arrives.
const (
	Info  = "info"
	OK    = "ok"
	Warn  = "warn"
	Error = "error"
)

// Keep is how many events notices.json holds, newest last.
const Keep = 20

// RepeatAfter is how soon the same kind and title may be shown again. A
// change of title for a kind (failed over, then back) always shows.
const RepeatAfter = 5 * time.Minute

// Event is one entry in notices.json.
type Event struct {
	ID       string    `json:"id"`
	Kind     string    `json:"kind"`
	Severity string    `json:"severity"`
	Title    string    `json:"title"`
	Detail   string    `json:"detail,omitempty"`
	At       time.Time `json:"at"`
	TS       int64     `json:"ts"` // At in Unix seconds, for the panel's shell
	Resolves string    `json:"resolves,omitempty"`
	// Session is the Claude Code session the event is about, when it is
	// about one: that session's own panel leaves it to the others.
	Session string `json:"session,omitempty"`
	// AuditOnly marks an entry recorded for the audit and never shown.
	AuditOnly bool `json:"audit_only,omitempty"`
	// Hidden marks an alert the toasts' level held back (config "alerts"):
	// it is in the audit and not in notices.json.
	Hidden bool `json:"hidden,omitempty"`
	// NotifyDone says the process that published the alert decided about
	// Notification Center itself (the gateway does). Without it the support
	// console, watching the audit, decides: a script's alert written with
	// the gateway down still reaches Notification Center that way.
	NotifyDone bool `json:"notify_done,omitempty"`
}

// Channels routes each alert that passed the repeat limit. Route says
// whether it goes in notices.json, the toasts' source, and whether
// Notification Center has been dealt with here (Event.NotifyDone). It is
// called on the writer goroutine, once per alert, never for Record.
type Channels interface {
	Route(ev Event) (toast, notifyDone bool)
}

type file struct {
	Events []Event `json:"events"`
}

// Publisher writes events to one notices.json.
type Publisher struct {
	path   string
	logger *log.Logger
	now    func() time.Time
	ch     chan any

	mu        sync.Mutex
	shown     map[string]time.Time // kind|title -> last shown
	lastTitle map[string]string    // kind -> last title shown
	seq       int64
	errLogged bool
	channels  Channels
	// once is the kind|title pairs PublishOnce has found or published, so
	// a pair the toasts' level kept out of notices.json is not published
	// again every round.
	once map[string]bool
}

type flushReq chan struct{}

// New starts a publisher writing path. logger may be nil.
func New(path string, logger *log.Logger) *Publisher {
	p := &Publisher{
		path: path, logger: logger, now: time.Now,
		ch:    make(chan any, 64),
		shown: map[string]time.Time{}, lastTitle: map[string]string{}, once: map[string]bool{},
	}
	go p.run()
	return p
}

// Publish queues an event unless the same kind and title was shown within
// RepeatAfter and nothing else of that kind was shown since. It never
// blocks, and reports whether the event was queued.
func (p *Publisher) Publish(kind, severity, title, detail string) bool {
	return p.publish("", kind, severity, title, detail, false)
}

// PublishFor is Publish for an event about one Claude Code session.
func (p *Publisher) PublishFor(session, kind, severity, title, detail string) bool {
	return p.publish(session, kind, severity, title, detail, false)
}

// PublishOnce publishes only if no event of the same kind and title is in
// notices.json already, so a title naming a version or a day is shown once
// even across gateway restarts. It reads the file, so it is for the
// notifier's rounds, not a request path.
func (p *Publisher) PublishOnce(kind, severity, title, detail string) bool {
	return p.publish("", kind, severity, title, detail, true)
}

// SetChannels routes this publisher's alerts from now on. nil, the
// default, puts every alert in notices.json, as before channels existed.
func (p *Publisher) SetChannels(c Channels) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.channels = c
	p.mu.Unlock()
}

// Path is the notices.json this publisher writes.
func (p *Publisher) Path() string {
	if p == nil {
		return ""
	}
	return p.path
}

func (p *Publisher) publish(session, kind, severity, title, detail string, once bool) bool {
	if p == nil {
		return false
	}
	if once && p.seenOnce(kind, title) {
		return false
	}
	now := p.now()
	key := kind + "|" + title
	p.mu.Lock()
	if last, ok := p.shown[key]; ok && p.lastTitle[kind] == title && now.Sub(last) < RepeatAfter {
		p.mu.Unlock()
		return false
	}
	p.shown[key] = now
	p.lastTitle[kind] = title
	p.seq++
	ev := Event{ID: fmt.Sprintf("%d-%d", now.UnixNano(), p.seq), Kind: kind, Severity: severity,
		Title: title, Detail: detail, At: now, TS: now.Unix(), Session: session}
	if severity == OK {
		ev.Resolves = kind
	}
	p.mu.Unlock()
	select {
	case p.ch <- ev:
		return true
	default:
		p.logOnce(errors.New("queue full, event dropped: " + title))
		return false
	}
}

// seenOnce is PublishOnce's check: this process has published the pair,
// or notices.json holds it, or the audit holds it held back by the toasts'
// level (Event.Hidden), which notices.json never will. Only a miss reads
// the files, so the rounds that call it read them once per pair.
func (p *Publisher) seenOnce(kind, title string) bool {
	key := kind + "|" + title
	p.mu.Lock()
	seen := p.once[key]
	p.once[key] = true
	p.mu.Unlock()
	// Flushed either way: the caller expects what it published before to
	// be on disk once PublishOnce returns, as it always was.
	p.Flush(2 * time.Second)
	if seen {
		return true
	}
	evs, _ := Read(p.path)
	for _, e := range evs {
		if e.Kind == kind && e.Title == title {
			return true
		}
	}
	for _, e := range ReadAudit(AuditPath(p.path), onceAuditLook) {
		if e.Hidden && e.Kind == kind && e.Title == title {
			return true
		}
	}
	return false
}

// onceAuditLook is how far back seenOnce looks in the audit: days of it.
const onceAuditLook = 2000

// Record adds an entry to the audit trail only: something done (a restart
// from the console, a setting saved) that support needs to see later but
// nobody needs on screen. Never deduplicated, never blocks.
func (p *Publisher) Record(kind, severity, title, detail string) {
	if p == nil {
		return
	}
	now := p.now()
	p.mu.Lock()
	p.seq++
	ev := Event{ID: fmt.Sprintf("%d-%d", now.UnixNano(), p.seq), Kind: kind, Severity: severity,
		Title: title, Detail: detail, At: now, TS: now.Unix(), AuditOnly: true}
	p.mu.Unlock()
	select {
	case p.ch <- ev:
	default:
		p.logOnce(errors.New("queue full, audit entry dropped: " + title))
	}
}

// Flush waits until every queued event is on disk, or for at most d.
func (p *Publisher) Flush(d time.Duration) {
	if p == nil {
		return
	}
	done := make(flushReq)
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case p.ch <- done:
	case <-t.C:
		return
	}
	select {
	case <-done:
	case <-t.C:
	}
}

func (p *Publisher) run() {
	for m := range p.ch {
		switch v := m.(type) {
		case Event:
			if err := p.write(v); err != nil {
				p.logOnce(err)
			}
		case flushReq:
			close(v)
		}
	}
}

func (p *Publisher) write(ev Event) error {
	if !ev.AuditOnly {
		p.mu.Lock()
		ch := p.channels
		p.mu.Unlock()
		if ch != nil {
			toast, done := ch.Route(ev)
			ev.Hidden, ev.NotifyDone = !toast, done
		}
	}
	// The audit first: a notices.json that will not write must not cost the
	// support trail its record of the event.
	auditErr := appendAudit(AuditPath(p.path), ev)
	if p.logger != nil {
		p.logger.Printf("notice kind=%q severity=%q audit_only=%v hidden=%v title=%q detail=%q", ev.Kind, ev.Severity, ev.AuditOnly, ev.Hidden, ev.Title, ev.Detail)
	}
	if ev.AuditOnly || ev.Hidden {
		return auditErr
	}
	if err := p.writeNotices(ev); err != nil {
		return err
	}
	return auditErr
}

func (p *Publisher) writeNotices(ev Event) error {
	var f file
	b, err := os.ReadFile(p.path)
	switch {
	case err == nil:
		// A damaged file is replaced rather than left failing every write.
		_ = json.Unmarshal(b, &f)
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}
	f.Events = append(f.Events, ev)
	if len(f.Events) > Keep {
		f.Events = f.Events[len(f.Events)-Keep:]
	}
	out, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p.path), 0o700); err != nil {
		return err
	}
	return atomicfile.Write(p.path, append(out, '\n'), 0o600)
}

func (p *Publisher) logOnce(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.errLogged || p.logger == nil {
		return
	}
	p.errLogged = true
	p.logger.Printf("error stage=notice path=%q err=%q (on-screen alerts may be missing; logged once)", p.path, err.Error())
}

// Read returns the events in path, oldest first. A missing file is no events.
func Read(path string) ([]Event, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var f file
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, err
	}
	return f.Events, nil
}

// Path is where serve writes notices.json, beside config.json.
func Path(configDir string) string { return filepath.Join(configDir, "notices.json") }

// AuditPath is the audit trail beside notices.json.
func AuditPath(noticesPath string) string {
	return filepath.Join(filepath.Dir(noticesPath), "audit.jsonl")
}

// AuditMax is the size at which audit.jsonl moves to audit.jsonl.1; the two
// together hold weeks of events.
const AuditMax = 2 << 20

func appendAudit(path string, ev Event) error {
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	if st, err := os.Stat(path); err == nil && st.Size() > AuditMax {
		_ = os.Rename(path, path+".1")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// AppendAudit writes one audit-only entry to path at once, for a process
// that is not the gateway and has no publisher: the command line. The
// scripts do the same with scripts/audit-add.sh.
func AppendAudit(path, kind, severity, title, detail string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	now := time.Now()
	return appendAudit(path, Event{ID: fmt.Sprintf("%d-%d", now.UnixNano(), os.Getpid()), Kind: kind,
		Severity: severity, Title: title, Detail: detail, At: now, TS: now.Unix(), AuditOnly: true})
}

// ReadAudit returns up to limit audit entries, newest first, across the
// current file and the one before it.
func ReadAudit(path string, limit int) []Event {
	var out []Event
	for _, p := range []string{path, path + ".1"} {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		lines := bytes.Split(bytes.TrimSpace(b), []byte("\n"))
		for i := len(lines) - 1; i >= 0 && len(out) < limit; i-- {
			var e Event
			if json.Unmarshal(lines[i], &e) == nil && e.Title != "" {
				out = append(out, e)
			}
		}
	}
	return out
}

var (
	defMu sync.RWMutex
	def   *Publisher
)

// SetDefault makes p the publisher the package functions use. serve sets
// it at startup; until then, and in tests that never set it, publishing is
// a no-op.
func SetDefault(p *Publisher) {
	defMu.Lock()
	def = p
	defMu.Unlock()
}

// Default returns the publisher SetDefault installed, or nil.
func Default() *Publisher {
	defMu.RLock()
	defer defMu.RUnlock()
	return def
}

// Publish publishes through the default publisher.
func Publish(kind, severity, title, detail string) bool {
	return Default().Publish(kind, severity, title, detail)
}

// PublishFor publishes an event about one session through the default
// publisher.
func PublishFor(session, kind, severity, title, detail string) bool {
	return Default().PublishFor(session, kind, severity, title, detail)
}

// PublishOnce publishes through the default publisher unless the same kind
// and title is already in notices.json.
func PublishOnce(kind, severity, title, detail string) bool {
	return Default().PublishOnce(kind, severity, title, detail)
}

// Record adds an audit-only entry through the default publisher.
func Record(kind, severity, title, detail string) { Default().Record(kind, severity, title, detail) }

// Flush flushes the default publisher.
func Flush(d time.Duration) { Default().Flush(d) }
