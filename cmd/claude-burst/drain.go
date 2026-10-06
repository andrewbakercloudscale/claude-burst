package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
	"github.com/andrewbakercloudscale/claude-burst/internal/notice"
)

// Graceful restart.
//
// A deploy restarts the gateway with `launchctl kickstart -k`, which sends
// SIGTERM. Until 2026-09-28 the gateway did not handle it, so it died at once
// and every Claude Code session mid-reply lost its response ("API Error:
// Connection lost mid-response") -- two deploys that evening cut a session
// in another terminal.
//
// launchd runs one instance of a job at a time, so the new binary cannot
// take over while the old one finishes. What the old one can do is choose
// WHEN to go: on SIGTERM it keeps serving, including new requests, and exits
// at the first moment nothing is streaming. Sessions spend much of their
// time running tools between turns, so that moment usually comes within
// seconds. The only gap left is launchd starting the new process, and a
// client that hits it retries.
//
// drainDeadline bounds the wait for a machine that is never idle. It must
// stay below the LaunchAgent's ExitTimeOut (install.sh), or launchd
// SIGKILLs the process first and the log never says what was cut. launchd
// caps a user agent's exit timeout at 60s whatever the plist asks for
// (measured 2026-09-28: ExitTimeOut 150 in the plist, "exit timeout = 60"
// in `launchctl print`), so this is 50s, not the two minutes a long reply
// could want; one still streaming at 50s is cut and logged.
const drainDeadline = 50 * time.Second

// drainPoll is how often in-flight requests are counted while draining.
const drainPoll = 100 * time.Millisecond

// waitIdle returns once inflight has read zero on two consecutive polls, or
// at the deadline, and reports how many requests were still running then.
// Two reads, not one: a request accepted between a zero read and the exit
// would be cut, and a second read one poll later narrows that window to a
// connection arriving in the last 100ms with no request yet parsed.
func waitIdle(inflight func() int64, deadline, poll time.Duration) (remaining int64, waited time.Duration) {
	start := time.Now()
	zeros := 0
	for {
		n := inflight()
		if n == 0 {
			zeros++
			if zeros >= 2 {
				return 0, time.Since(start)
			}
		} else {
			zeros = 0
		}
		if time.Since(start) >= deadline {
			return n, time.Since(start)
		}
		time.Sleep(poll)
	}
}

// exitWhenIdleOnSignal runs in the background for the life of the gateway.
// A second SIGTERM or SIGINT skips the wait, so an impatient Ctrl-C in a
// foreground `claude-burst serve` still stops it at once.
func exitWhenIdleOnSignal(inflight func() int64, logger *log.Logger, beforeExit func()) {
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	sig := <-sigs
	logger.Printf("received %s: draining, %d inference request(s) in flight; exiting when none are (at most %s)", sig, inflight(), drainDeadline)
	announceDrain(inflight())

	done := make(chan struct{})
	go func() {
		remaining, waited := waitIdle(inflight, drainDeadline, drainPoll)
		if remaining > 0 {
			logger.Printf("drain deadline reached after %s: exiting with %d inference request(s) still streaming; those clients see a lost connection", waited.Round(time.Millisecond), remaining)
		} else {
			logger.Printf("drained in %s: no inference in flight, exiting for restart", waited.Round(time.Millisecond))
		}
		close(done)
	}()
	select {
	case <-done:
	case sig := <-sigs:
		logger.Printf("received %s again: exiting now with %d inference request(s) in flight", sig, inflight())
	}
	beforeExit()
	notice.Flush(2 * time.Second)
	os.Exit(0)
}

// announceDrain and announceReady put a restart on screen. The ok of a
// gateway that came back resolves the restart; a gateway that never comes
// back is the usage panel's to report, since nothing here is left to.
//
// A restart scripts/deploy.sh asked for is not news: it writes the marker
// first, and both lines then go to the log only. On 3 and 4 Oct 2026 every
// deploy put the pair on screen. A deploy whose gateway never comes back is
// still reported, by the panel's own health check.
func announceDrain(inflight int64) {
	detail := "No replies in flight."
	if inflight == 1 {
		detail = "1 reply in flight finishes first."
	} else if inflight > 1 {
		detail = fmt.Sprintf("%d replies in flight finish first.", inflight)
	}
	if plannedRestart(false) {
		return
	}
	notice.Publish("gateway", notice.Info, "Gateway restarting", detail)
}

func announceReady(version string) {
	if plannedRestart(true) {
		return
	}
	notice.Publish("gateway", notice.OK, "Gateway ready", "Claude Burst "+version+" is serving.")
}

// plannedRestartPath is the marker deploy.sh writes before it restarts the
// gateway. A var for the tests.
var plannedRestartPath = func() string {
	dir, err := config.ConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "planned-restart")
}

// plannedRestartFor is how long a marker counts: a deploy's drain can take
// 50 seconds, and a stale marker must not hide a crash hours later.
const plannedRestartFor = 3 * time.Minute

// plannedRestart reports a fresh marker, and removes it when the restart is
// over (consume), so it quiets one restart only.
func plannedRestart(consume bool) bool {
	p := plannedRestartPath()
	if p == "" {
		return false
	}
	fi, err := os.Stat(p)
	if err != nil {
		return false
	}
	fresh := time.Since(fi.ModTime()) < plannedRestartFor
	if consume || !fresh {
		os.Remove(p)
	}
	return fresh
}
