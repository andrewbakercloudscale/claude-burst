// Package tlswatch counts the TLS handshakes the gateway's listener completes
// and the ones it loses, so the dashboard can see the client side of the
// handshake.
//
// Why it exists: on 2026-10-02 at 12:50 a deploy rotated the local intercept
// CA. Every Claude Code session already running kept the trust bundle it read
// at startup, so each one refused the gateway's new certificate
// (SELF_SIGNED_CERT_IN_CHAIN) and every request failed. The dashboard said
// "6/6 checks passed" throughout, because every check it had looked at the
// gateway's own side: the gateway probing itself trusts itself. The only
// trace was net/http's "http: TLS handshake error from 127.0.0.1:N: EOF"
// lines on stderr, which nothing counted.
//
// Two hooks on the listener's http.Server feed it:
//
//   - ErrorLog. net/http reports a failed handshake only as a log line, so
//     the Watcher is an io.Writer that parses those lines, counts and
//     classifies them, and passes every byte through to the real log
//     unchanged.
//   - ConnState. A connection reaches StateActive only once its handshake has
//     finished and the first request bytes arrived, so the first StateActive
//     per connection is one successful handshake. Later requests on the same
//     kept-alive connection are not counted again.
package tlswatch

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Class is what a failed handshake most likely means.
type Class string

const (
	// Distrust is a TLS alert from the client about our certificate
	// ("bad certificate", "unknown certificate authority", "unknown
	// certificate"): the client checked the chain and refused it.
	Distrust Class = "distrust"
	// Hangup is the client closing mid-handshake: EOF, connection reset,
	// broken pipe. It is how Node reports a certificate it does not trust:
	// it finishes its side, checks the chain afterwards, and destroys the
	// socket without an alert. The 2026-10-02 outage logged only these.
	Hangup Class = "hangup"
	// PlainHTTP is a client speaking plain HTTP to the TLS port. Not a
	// trust problem, so it never counts towards a rejection.
	PlainHTTP Class = "plain_http"
	// Other is everything else (bad record MAC, timeouts).
	Other Class = "other"
)

// Window is how far back a Snapshot counts.
const Window = 5 * time.Minute

// RejectThreshold is the number of failed handshakes in Window at or above
// which a rejection is reported, provided failures also outnumber successes.
//
// Chosen from launchd.err.log on the machine this was written on (5,349
// handshake errors, 2026-08-31 to 2026-10-02):
//
//   - quiet weeks: under 15 a DAY, a background that must never trip it;
//   - a misconfigured non-Claude client retrying all day (2026-08-31): a
//     steady 20 per 5 minutes, every one "unknown certificate";
//   - the outage (2026-10-02 12:50): 75 in one minute, 85 in five, every
//     one EOF or connection reset.
//
// 30 sits above the steadiest background and well below the outage. The
// second condition, failures outnumbering successes, is what keeps a busy
// machine with one noisy client from reading as an outage: while Claude
// Code is working it completes handshakes of its own.
const RejectThreshold = 30

// HangupThreshold is the lower bar for hang-ups alone, with no condition on
// successes. A hang-up mid-handshake is how Node, and so Claude Code,
// refuses a certificate it does not trust. At 13:23 on 2026-10-02, after a
// redeploy, every open session refused the gateway: 27 hang-ups and one
// reset in two minutes, under RejectThreshold, and the page said 7/7. The
// noisy client RejectThreshold was set against sent certificate alerts, not
// hang-ups, and quiet days see under 15 failures of any kind a DAY.
const HangupThreshold = 10

// maxEvents bounds memory if something hammers the port. The worst storm on
// record was about 70 a minute; this is two orders of magnitude above it.
const maxEvents = 50000

type event struct {
	at    time.Time
	ok    bool
	class Class
}

// Watcher counts handshakes. The zero value is not usable; call New.
type Watcher struct {
	out io.Writer
	now func() time.Time

	mu        sync.Mutex
	events    []event
	active    map[net.Conn]struct{}
	started   time.Time
	lastFail  time.Time
	lastError string
	lastClass Class
}

// New returns a Watcher that passes every log line through to out.
func New(out io.Writer) *Watcher {
	return newWithClock(out, time.Now)
}

func newWithClock(out io.Writer, now func() time.Time) *Watcher {
	return &Watcher{out: out, now: now, active: map[net.Conn]struct{}{}, started: now()}
}

const marker = "http: TLS handshake error from "

// Write implements io.Writer for http.Server.ErrorLog. log.Logger calls it
// once per line. The line always reaches out, whatever is parsed from it.
func (w *Watcher) Write(p []byte) (int, error) {
	if i := bytes.Index(p, []byte(marker)); i >= 0 {
		rest := string(bytes.TrimSpace(p[i+len(marker):]))
		// "127.0.0.1:64352: EOF". The address never contains ": ", the
		// error text usually does, so the first ": " separates them.
		if _, msg, ok := strings.Cut(rest, ": "); ok {
			w.recordFailure(msg)
		} else {
			w.recordFailure(rest)
		}
	}
	if w.out == nil {
		return len(p), nil
	}
	return w.out.Write(p)
}

// Classify sorts a handshake error's text into a Class.
func Classify(msg string) Class {
	m := strings.ToLower(msg)
	switch {
	case strings.Contains(m, "remote error: tls:") && (strings.Contains(m, "certificate") || strings.Contains(m, "unknown ca")):
		return Distrust
	case strings.Contains(m, "client sent an http request to an https server"):
		return PlainHTTP
	case m == "eof" || strings.HasSuffix(m, ": eof") || strings.Contains(m, "connection reset by peer") ||
		strings.Contains(m, "broken pipe") || strings.Contains(m, "unexpected eof"):
		return Hangup
	default:
		return Other
	}
}

func (w *Watcher) recordFailure(msg string) {
	c := Classify(msg)
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.now()
	w.add(event{at: now, class: c})
	if c != PlainHTTP {
		w.lastFail, w.lastError, w.lastClass = now, msg, c
	}
}

// ConnState is installed as http.Server.ConnState.
func (w *Watcher) ConnState(c net.Conn, st http.ConnState) {
	w.mu.Lock()
	defer w.mu.Unlock()
	switch st {
	case http.StateActive:
		if _, seen := w.active[c]; seen {
			return
		}
		w.active[c] = struct{}{}
		w.add(event{at: w.now(), ok: true})
	case http.StateClosed, http.StateHijacked:
		delete(w.active, c)
	}
}

// add appends under w.mu and drops what has aged out of Window.
func (w *Watcher) add(e event) {
	w.events = append(w.events, e)
	w.prune(e.at)
}

func (w *Watcher) prune(now time.Time) {
	cut := 0
	for cut < len(w.events) && now.Sub(w.events[cut].at) > Window {
		cut++
	}
	if over := len(w.events) - cut - maxEvents; over > 0 {
		cut += over
	}
	if cut > 0 {
		w.events = append(w.events[:0], w.events[cut:]...)
	}
}

// Snapshot is what /api/state carries for the dashboard.
type Snapshot struct {
	WindowSeconds int `json:"window_seconds"`
	// Successes are handshakes that completed and carried a request.
	Successes int `json:"successes"`
	// Failures are failed handshakes that can mean distrust: Distrust,
	// Hangup and Other. PlainHTTP is counted in ByClass only.
	Failures int            `json:"failures"`
	ByClass  map[string]int `json:"by_class"`
	// Threshold is RejectThreshold, sent so the page can say what
	// "too many" was without restating the number.
	Threshold int `json:"threshold"`
	// Rejecting is the verdict: Failures >= Threshold and Failures >
	// Successes.
	Rejecting     bool   `json:"rejecting"`
	LastFailure   string `json:"last_failure,omitempty"`
	LastError     string `json:"last_error,omitempty"`
	LastClass     string `json:"last_class,omitempty"`
	WatchingSince string `json:"watching_since"`
}

// Snapshot counts the last Window.
func (w *Watcher) Snapshot() Snapshot {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.now()
	w.prune(now)
	s := Snapshot{WindowSeconds: int(Window / time.Second), ByClass: map[string]int{},
		Threshold: RejectThreshold, WatchingSince: w.started.Format(time.RFC3339)}
	for _, e := range w.events {
		if e.ok {
			s.Successes++
			continue
		}
		s.ByClass[string(e.class)]++
		if e.class != PlainHTTP {
			s.Failures++
		}
	}
	s.Rejecting = (s.Failures >= RejectThreshold && s.Failures > s.Successes) ||
		s.ByClass[string(Hangup)] >= HangupThreshold
	if !w.lastFail.IsZero() {
		s.LastFailure = w.lastFail.Format(time.RFC3339)
		s.LastError, s.LastClass = w.lastError, string(w.lastClass)
	}
	return s
}
