package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"regexp"
	"runtime/debug"
	"runtime/metrics"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Memory logging.
//
// On 27 Oct 2026 the gateway stopped answering for half a minute at a time,
// three times in half an hour, and then for good until it was restarted.
// The Mac was paging hard (11.2 of 12GB of swap used), but nothing said how
// much of that was the gateway, or how long it had stood still: the process
// was gone by the time anyone asked. So the gateway now writes what it holds
// every few minutes, and a line as soon as it notices it did not run.

const (
	// memEvery is how often the memory line is written.
	memEvery = 5 * time.Minute
	// stallTick is how often the gateway checks that it is being run, and
	// stallLate how late a check may be before that is worth a line.
	stallTick = 10 * time.Second
	stallLate = 5 * time.Second
	// memWarnBytes is how much the gateway may hold before the line is a
	// warning: it held under 100MB on the morning of 27 Oct 2026.
	memWarnBytes = 1 << 30
	// memSoftLimit is the Go runtime's soft memory limit. Without one Go
	// keeps what a burst of large requests took, and returns it to macOS
	// slowly: on 30 Oct 2026 it held 964MB mapped with 250MB in RAM and
	// the rest pushed to swap on a Mac short of memory. Near the limit the
	// collector runs sooner and gives pages back; it is soft, so a burst
	// past it still succeeds, with the collector capped at half a core.
	// GOMEMLIMIT in the environment overrides it.
	memSoftLimit = 512 << 20
)

// limitMemory sets memSoftLimit, unless GOMEMLIMIT already set one.
func limitMemory() {
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(memSoftLimit)
	}
}

// memSample is what one memory line says. RSS and swap come from ps and
// sysctl, and are zero when those did not answer.
type memSample struct {
	rss, peakRSS     uint64 // bytes in RAM now, and the most ever
	goTotal, heap    uint64 // bytes the Go runtime has mapped, and live objects
	goroutines, gcs  uint64
	swapUsed, swapOf uint64 // the Mac's swap, bytes
}

// watchMemory writes a memory line every memEvery, and a warning when a
// check every stallTick runs stallLate or more behind. Time is measured on
// Go's monotonic clock, which on macOS does not run while the Mac sleeps,
// so a night with the lid shut is not a stall.
func watchMemory(ctx context.Context, logger *log.Logger) {
	logger.Print(memLine(readMemory(ctx)))
	tick := time.NewTicker(stallTick)
	defer tick.Stop()
	last, lastLine := time.Now(), time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		now := time.Now()
		if line := stallLine(now.Sub(last), stallTick); line != "" {
			logger.Print(line + "; " + memLine(readMemory(ctx)))
			lastLine = now
		} else if now.Sub(lastLine) >= memEvery {
			logger.Print(memLine(readMemory(ctx)))
			lastLine = now
		}
		last = now
	}
}

// stallLine is the warning for a check that came gap after the one before,
// when it should have come every, or "" when it was on time.
func stallLine(gap, every time.Duration) string {
	late := gap - every
	if late < stallLate {
		return ""
	}
	return fmt.Sprintf("WARNING memory: the gateway did not run for %s (a check due every %s came %s late), so nothing was answered meanwhile; a Mac short of memory does this",
		gap.Round(time.Second), every, late.Round(time.Second))
}

// memLine is the line for s.
func memLine(s memSample) string {
	var b strings.Builder
	if s.goTotal >= memWarnBytes {
		b.WriteString("WARNING ")
	}
	fmt.Fprintf(&b, "memory: rss=%s peak_rss=%s go=%s heap=%s goroutines=%d gc=%d",
		mb(s.rss), mb(s.peakRSS), mb(s.goTotal), mb(s.heap), s.goroutines, s.gcs)
	if s.swapOf > 0 {
		fmt.Fprintf(&b, "; Mac swap %s of %s used", mb(s.swapUsed), mb(s.swapOf))
	}
	if s.goTotal > s.rss*2 && s.rss > 0 {
		b.WriteString(" (most of the gateway is swapped out)")
	}
	return b.String()
}

// mb is n in whole megabytes, "?" for an unknown 0.
func mb(n uint64) string {
	if n == 0 {
		return "?"
	}
	return strconv.FormatUint((n+(1<<19))>>20, 10) + "MB"
}

// readMemory takes a sample. ps and sysctl are bounded: on a Mac paging
// hard they can be slow, and a missing figure is better than a late line.
func readMemory(ctx context.Context) memSample {
	var s memSample
	ms := []metrics.Sample{
		{Name: "/memory/classes/total:bytes"},
		{Name: "/memory/classes/heap/objects:bytes"},
		{Name: "/sched/goroutines:goroutines"},
		{Name: "/gc/cycles/total:gc-cycles"},
	}
	metrics.Read(ms)
	vals := []*uint64{&s.goTotal, &s.heap, &s.goroutines, &s.gcs}
	for i, m := range ms {
		if m.Value.Kind() == metrics.KindUint64 {
			*vals[i] = m.Value.Uint64()
		}
	}
	var ru syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &ru) == nil && ru.Maxrss > 0 {
		s.peakRSS = uint64(ru.Maxrss) // bytes on macOS (kilobytes on Linux)
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "/bin/ps", "-o", "rss=", "-p", strconv.Itoa(os.Getpid())).Output(); err == nil {
		if kb, err := strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64); err == nil {
			s.rss = kb << 10
		}
	}
	if out, err := exec.CommandContext(ctx, "/usr/sbin/sysctl", "-n", "vm.swapusage").Output(); err == nil {
		s.swapOf, s.swapUsed = parseSwap(string(out))
	}
	return s
}

var swapField = regexp.MustCompile(`(total|used) = ([0-9]+)[.,]?([0-9]*)M`)

// parseSwap reads `sysctl -n vm.swapusage`, "total = 12288.00M  used =
// 11191.12M  free = ...", whose decimal mark follows the locale.
func parseSwap(s string) (total, used uint64) {
	for _, m := range swapField.FindAllStringSubmatch(s, -1) {
		n, _ := strconv.ParseFloat(m[2]+"."+m[3]+"0", 64)
		v := uint64(n * (1 << 20))
		if m[1] == "total" {
			total = v
		} else {
			used = v
		}
	}
	return total, used
}
