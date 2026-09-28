package main

import (
	"os"
	"regexp"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

func TestWaitIdleReturnsOnceNothingIsStreaming(t *testing.T) {
	var n atomic.Int64
	n.Store(2)
	go func() {
		time.Sleep(30 * time.Millisecond)
		n.Store(1)
		time.Sleep(30 * time.Millisecond)
		n.Store(0)
	}()
	remaining, waited := waitIdle(n.Load, time.Second, 5*time.Millisecond)
	if remaining != 0 {
		t.Fatalf("remaining = %d, want 0", remaining)
	}
	if waited < 60*time.Millisecond || waited > 500*time.Millisecond {
		t.Fatalf("waited %s: must wait for the streams, and return promptly once they end", waited)
	}
}

// A single zero between two requests is not idle: exiting on it would cut
// the request that arrives next.
func TestWaitIdleNeedsTwoZeroReadsInARow(t *testing.T) {
	reads := []int64{1, 0, 1, 0, 0}
	i := 0
	remaining, _ := waitIdle(func() int64 {
		v := reads[i]
		if i < len(reads)-1 {
			i++
		}
		return v
	}, time.Second, time.Millisecond)
	if remaining != 0 || i != len(reads)-1 {
		t.Fatalf("returned after %d reads, want %d (the lone zero must not count)", i, len(reads)-1)
	}
}

func TestWaitIdleGivesUpAtTheDeadlineAndSaysWhatItCut(t *testing.T) {
	remaining, waited := waitIdle(func() int64 { return 3 }, 50*time.Millisecond, 5*time.Millisecond)
	if remaining != 3 {
		t.Fatalf("remaining = %d, want 3", remaining)
	}
	if waited < 50*time.Millisecond || waited > 300*time.Millisecond {
		t.Fatalf("waited %s, want about the 50ms deadline", waited)
	}
}

// launchd SIGKILLs a job that outlives its ExitTimeOut. The drain must end
// first, or the log never records what was cut. The value lives in the
// plist install.sh writes, so it is read from there.
func TestDrainDeadlineFitsInsideLaunchdExitTimeout(t *testing.T) {
	b, err := os.ReadFile("../../install.sh")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`<key>ExitTimeOut</key><integer>(\d+)</integer>`).FindSubmatch(b)
	if m == nil {
		t.Fatal("install.sh's plist has no ExitTimeOut: launchd would SIGKILL the drain after its 20s default")
	}
	secs, _ := strconv.Atoi(string(m[1]))
	if drainDeadline >= time.Duration(secs)*time.Second {
		t.Fatalf("drainDeadline %s must be below the plist's ExitTimeOut %ds", drainDeadline, secs)
	}
}
