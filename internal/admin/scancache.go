package admin

import (
	"sync"
	"time"
)

// The page asks for /api/state every few seconds and each answer ran four
// passes over the metrics history: 1.8 seconds an answer on 7 Oct 2026,
// with 61MB of it on disk. A pass that was slow is now kept for twenty
// times what it took, a minute at most, so the page costs a few percent of
// one core however much history there is. A pass that was quick (a new
// install, a test) is never kept: its answer is always fresh.
const (
	scanSlow    = 50 * time.Millisecond
	scanKeepMax = time.Minute
)

type scanEntry struct {
	at   time.Time
	keep time.Duration
	v    any
}

var scans = struct {
	sync.Mutex
	m map[string]scanEntry
}{m: map[string]scanEntry{}}

// cachedScan returns run's answer for key, from the last call when that was
// slow and is recent enough. A failed run is not kept.
func cachedScan[T any](key string, run func() (T, error)) (T, error) {
	scans.Lock()
	if e, ok := scans.m[key]; ok && time.Since(e.at) < e.keep {
		scans.Unlock()
		return e.v.(T), nil
	}
	scans.Unlock()
	start := time.Now()
	v, err := run()
	took := time.Since(start)
	if err != nil || took < scanSlow {
		return v, err
	}
	keep := 20 * took
	if keep > scanKeepMax {
		keep = scanKeepMax
	}
	scans.Lock()
	scans.m[key] = scanEntry{at: time.Now(), keep: keep, v: v}
	scans.Unlock()
	return v, nil
}
