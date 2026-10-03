// Package notice publishes what the gateway wants a person to see on
// screen right now: a failover, a dead network, a restart. It writes
// ~/.config/claude-burst/notices.json, which the usage panel watches and
// turns into the same floating overlay it shows for loading and for a
// pauseless compaction.
//
// Publishing never blocks the caller. Events go through a buffered channel
// to one writer goroutine, so a request path that publishes pays for a
// channel send and nothing else; a full channel drops the event and says
// so once in the log.
package notice

import (
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
}

type flushReq chan struct{}

// New starts a publisher writing path. logger may be nil.
func New(path string, logger *log.Logger) *Publisher {
	p := &Publisher{
		path: path, logger: logger, now: time.Now,
		ch:    make(chan any, 64),
		shown: map[string]time.Time{}, lastTitle: map[string]string{},
	}
	go p.run()
	return p
}

// Publish queues an event unless the same kind and title was shown within
// RepeatAfter and nothing else of that kind was shown since. It never
// blocks, and reports whether the event was queued.
func (p *Publisher) Publish(kind, severity, title, detail string) bool {
	if p == nil {
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
		Title: title, Detail: detail, At: now, TS: now.Unix()}
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
	p.logger.Printf("error stage=notice path=%s err=%v (on-screen alerts may be missing; logged once)", p.path, err)
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

// Flush flushes the default publisher.
func Flush(d time.Duration) { Default().Flush(d) }
