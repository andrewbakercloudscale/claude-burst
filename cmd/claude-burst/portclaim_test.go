package main

import (
	"bytes"
	"log"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakePort is a port's holders, changed by the stub stopHolder; nothing here
// runs lsof or signals a real process.
type fakePort struct {
	mu      sync.Mutex
	holders []portHolder
	stopped []int
}

func (f *fakePort) install(t *testing.T) {
	oldList, oldStop, oldWait, oldTick := listHolders, stopHolder, claimWait, claimTick
	t.Cleanup(func() { listHolders, stopHolder, claimWait, claimTick = oldList, oldStop, oldWait, oldTick })
	listHolders = func(string) []portHolder {
		f.mu.Lock()
		defer f.mu.Unlock()
		return append([]portHolder(nil), f.holders...)
	}
	stopHolder = func(pid int) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.stopped = append(f.stopped, pid)
		var keep []portHolder
		for _, h := range f.holders {
			if h.pid != pid {
				keep = append(keep, h)
			}
		}
		f.holders = keep
	}
	claimTick = 10 * time.Millisecond
}

// A pass-through (the leftovers of 2026-10-04) is stopped at once.
func TestClaimPortStopsAPassthroughAtOnce(t *testing.T) {
	f := &fakePort{holders: []portHolder{{pid: 7, command: "/tmp/x/claude-burst passthrough --listen 127.0.0.1:17777"}}}
	f.install(t)
	claimWait = time.Hour
	start := time.Now()
	if err := claimPort("127.0.0.1:17777", log.New(&bytes.Buffer{}, "", 0)); err != nil {
		t.Fatal(err)
	}
	if len(f.stopped) != 1 || f.stopped[0] != 7 || time.Since(start) > time.Second {
		t.Fatalf("stopped %v after %s", f.stopped, time.Since(start))
	}
}

// An upgrade: the old gateway drains and exits on its own within the wait,
// and is never signalled.
func TestClaimPortLeavesADrainingGatewayAlone(t *testing.T) {
	f := &fakePort{holders: []portHolder{{pid: 8, command: "/Users/u/.local/bin/claude-burst serve"}}}
	f.install(t)
	claimWait = time.Second
	go func() { time.Sleep(200 * time.Millisecond); f.mu.Lock(); f.holders = nil; f.mu.Unlock() }() // drain done
	var logs bytes.Buffer
	if err := claimPort("127.0.0.1:17777", log.New(&logs, "", 0)); err != nil {
		t.Fatal(err)
	}
	if len(f.stopped) != 0 {
		t.Fatalf("a draining gateway was stopped: %v", f.stopped)
	}
	if !strings.Contains(logs.String(), "giving it up to") {
		t.Fatalf("log: %s", logs.String())
	}
}

// A Burst holder that outstays any drain is stopped.
func TestClaimPortStopsAnOrphanAfterTheWait(t *testing.T) {
	f := &fakePort{holders: []portHolder{{pid: 9, command: "/tmp/old/claude-burst serve"}}}
	f.install(t)
	claimWait = 100 * time.Millisecond
	if err := claimPort("127.0.0.1:17777", log.New(&bytes.Buffer{}, "", 0)); err != nil {
		t.Fatal(err)
	}
	if len(f.stopped) != 1 || f.stopped[0] != 9 {
		t.Fatalf("stopped %v", f.stopped)
	}
}

// Something that is not Burst is never touched.
func TestClaimPortNeverStopsSomethingElse(t *testing.T) {
	f := &fakePort{holders: []portHolder{{pid: 10, command: "/usr/bin/python3 -m http.server 17777"}}}
	f.install(t)
	err := claimPort("127.0.0.1:17777", log.New(&bytes.Buffer{}, "", 0))
	if err == nil || !strings.Contains(err.Error(), "not Claude Burst") || len(f.stopped) != 0 {
		t.Fatalf("err=%v stopped=%v", err, f.stopped)
	}
}
