package router

import (
	"fmt"
	"log"
	"sync"
	"time"
)

// quietSlow is how long a request that is not a model request may take
// before its done line is written after all.
const quietSlow = 10 * time.Second

// quietEvery is how often the count of unlogged requests is written, and
// how often a line that repeats for every request of an outage is.
const quietEvery = time.Minute

// quietCounter counts the requests whose start and done lines are left out
// of the log, and says how many once a minute, when the next one arrives.
// The zero value is ready.
type quietCounter struct {
	mu    sync.Mutex
	n     int
	since time.Time
}

func (q *quietCounter) add(logger *log.Logger, now time.Time) {
	q.mu.Lock()
	if q.since.IsZero() {
		q.since = now
	}
	q.n++
	n, since := q.n, q.since
	due := now.Sub(since) >= quietEvery
	if due {
		q.n, q.since = 0, now
	}
	q.mu.Unlock()
	if due {
		logger.Printf("quiet: %d requests that are not model requests (health checks, heartbeats) were answered without error since %s; their start and done lines are not written",
			n, since.Format("15:04:05"))
	}
}

// repeatLimiter lets one line through per key each quietEvery and counts
// what it held back. The zero value is ready.
type repeatLimiter struct {
	mu      sync.Mutex
	last    map[string]time.Time
	skipped map[string]int
}

// allow reports whether the line for key may be written now, and how many
// were held back since the last one that was.
func (l *repeatLimiter) allow(key string, now time.Time) (ok bool, skipped int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.last == nil {
		l.last, l.skipped = map[string]time.Time{}, map[string]int{}
	}
	if t, seen := l.last[key]; seen && now.Sub(t) < quietEvery {
		l.skipped[key]++
		return false, 0
	}
	skipped = l.skipped[key]
	l.last[key], l.skipped[key] = now, 0
	return true, skipped
}

// andMore is the tail of a rate-limited line: how many like it were not
// written.
func andMore(skipped int) string {
	if skipped == 0 {
		return ""
	}
	return fmt.Sprintf(" [and %d more like this since the last such line]", skipped)
}
