package main

import (
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Claiming the gateway's ports from a leftover.
//
// On 2026-10-04 leftover Burst processes (pass-throughs a test run started)
// held the gateway's port. Every gateway launchd started died at once on
// "address already in use", the self-heal watchdog reloaded it every two
// minutes and logged "reloaded successfully", nothing ever stopped the
// holder, and after seven minutes the pf guard removed the redirect. So the
// gateway that launchd starts now clears its own port.
//
// An upgrade is not a leftover. launchd starts the new gateway only after
// the old one has exited, but a holder that is draining (a bootout followed
// at once by a bootstrap, or a hand-started serve) must be left to finish:
// its replies are someone's turn. So a Burst holder gets claimWait, longer
// than any drain (50s) and launchd's own 60s cap on one, before it is
// stopped. A pass-through is stopped at once: it exists to give the port
// back. Anything that is not Burst is never touched: the gateway says what
// holds the port and exits, as before.

// claimWait is how long a Burst holder may keep the port; a variable for tests.
var claimWait = 75 * time.Second

// claimTick is how often the port is looked at again; a variable for tests.
var claimTick = 500 * time.Millisecond

// launchedByAgent reports whether this process is the gateway LaunchAgent's
// own: only that one claims ports. A hand-started serve leaves a running
// gateway alone and fails as it always has.
func launchedByAgent() bool {
	return os.Getenv("XPC_SERVICE_NAME") == gatewayLabel
}

// portHolder is one process listening on a port.
type portHolder struct {
	pid     int
	command string
}

// burst says whether the holder is a Claude Burst binary, wherever it lives
// (an installed one, a deploy's temp build, a test's).
func (h portHolder) burst() bool {
	exe, _, _ := strings.Cut(h.command, " ")
	return filepath.Base(exe) == "claude-burst"
}

func (h portHolder) passthrough() bool {
	return h.burst() && strings.Contains(h.command, " passthrough")
}

// listHolders lists the processes listening on addr's port, this one
// excepted; a variable for tests.
var listHolders = func(addr string) []portHolder {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil
	}
	out, _ := exec.Command("lsof", "-nP", "-t", "-iTCP:"+port, "-sTCP:LISTEN").Output()
	var hs []portHolder
	for _, f := range strings.Fields(string(out)) {
		pid, err := strconv.Atoi(f)
		if err != nil || pid == os.Getpid() {
			continue
		}
		cmd, _ := exec.Command("ps", "-o", "command=", "-p", f).Output()
		hs = append(hs, portHolder{pid: pid, command: strings.TrimSpace(string(cmd))})
	}
	return hs
}

// stopHolder sends SIGTERM, then SIGKILL after five seconds; a variable for
// tests.
var stopHolder = func(pid int) {
	_ = syscall.Kill(pid, syscall.SIGTERM)
	for i := 0; i < 50; i++ {
		if syscall.Kill(pid, 0) != nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
}

// claimPort waits for addr's port to be free, stopping a Burst pass-through
// at once and any other Burst holder that outstays claimWait. It returns an
// error naming the holder when the port is held by something else, or still
// held after the wait.
func claimPort(addr string, logger *log.Logger) error {
	start := time.Now()
	waiting := map[int]bool{}
	for {
		hs := listHolders(addr)
		if len(hs) == 0 {
			return nil
		}
		for _, h := range hs {
			switch {
			case !h.burst():
				return fmt.Errorf("%s is held by pid %d (%s), which is not Claude Burst; stop it, or change listen in config.json", addr, h.pid, h.command)
			case h.passthrough():
				logger.Printf("port claim: %s is held by a pass-through (pid %d); stopping it", addr, h.pid)
				stopHolder(h.pid)
			case time.Since(start) >= claimWait:
				logger.Printf("port claim: %s still held by pid %d (%s) after %s, longer than any drain; stopping it", addr, h.pid, h.command, claimWait)
				stopHolder(h.pid)
			case !waiting[h.pid]:
				waiting[h.pid] = true
				logger.Printf("port claim: %s is held by pid %d (%s); giving it up to %s to finish (a draining gateway during an upgrade)", addr, h.pid, h.command, claimWait)
			}
		}
		if time.Since(start) >= claimWait+10*time.Second {
			return fmt.Errorf("%s is still held after %s", addr, time.Since(start).Round(time.Second))
		}
		time.Sleep(claimTick)
	}
}
