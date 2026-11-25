package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"runtime/metrics"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/admin"
)

// Memory logging.
//
// On 27 Oct 2026 the gateway stopped answering for half a minute at a time,
// three times in half an hour, and then for good until it was restarted.
// The Mac was paging hard (11.2 of 12GB of swap used), but nothing said how
// much of that was the gateway, or how long it had stood still: the process
// was gone by the time anyone asked. So the gateway now writes what it holds
// every few minutes, and a line as soon as it notices it did not run.
//
// On 25 Nov 2026 every session toasted "Burst dashboard not answering"
// again and again while a Python job grew to 6GB, and the log could not say
// so: a freeze under 15s wrote nothing, and none said what held the memory.
// So the check now runs every 2s, a freeze of 5s or more is a line, and
// that line names macOS's memory pressure and the five largest processes.

const (
	// memEvery is how often the memory line is written.
	memEvery = 5 * time.Minute
	// stallTick is how often the gateway checks that it is being run, and
	// stallLate how late a check may be before that is worth a line.
	stallTick = 2 * time.Second
	stallLate = 3 * time.Second
	// topEvery is how often a freeze line lists the largest processes: a
	// Mac stalling every few seconds would otherwise run ps as often.
	topEvery = 30 * time.Second
	// topN is how many processes it lists.
	topN = 5
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
	pressure         string // macOS's memory pressure: normal, warn or critical; "" unknown
	top              []proc // the largest processes, when asked for
}

// proc is one process in a freeze line.
type proc struct {
	pid  int
	rss  uint64 // bytes
	name string
}

// watchMemory writes a memory line every memEvery, and a warning when a
// check every stallTick runs stallLate or more behind. Time is measured on
// Go's monotonic clock, which on macOS does not run while the Mac sleeps,
// so a night with the lid shut is not a stall.
func watchMemory(ctx context.Context, logger *log.Logger) {
	logger.Print(memLine(readMemory(ctx, false)))
	tick := time.NewTicker(stallTick)
	defer tick.Stop()
	last, lastLine := time.Now(), time.Now()
	var lastTop time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		now := time.Now()
		if line := stallLine(now.Sub(last), stallTick); line != "" {
			withTop := now.Sub(lastTop) >= topEvery
			s := readMemory(ctx, withTop)
			if withTop {
				lastTop = now
			}
			logger.Print(line + "; " + memLine(s))
			admin.NoteStall(admin.Stall{EndedMS: now.UnixMilli(), Seconds: now.Sub(last).Seconds(),
				SwapUsedMB: s.swapUsed >> 20, SwapOfMB: s.swapOf >> 20, Pressure: s.pressure})
			lastLine = now
		} else if now.Sub(lastLine) >= memEvery {
			logger.Print(memLine(readMemory(ctx, false)))
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
	if s.pressure != "" {
		fmt.Fprintf(&b, "; memory pressure %s", s.pressure)
	}
	if s.goTotal > s.rss*2 && s.rss > 0 {
		b.WriteString(" (most of the gateway is swapped out)")
	}
	if len(s.top) > 0 {
		b.WriteString("; largest:")
		for i, p := range s.top {
			if i > 0 {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, " %s %s (pid %d)", mb(p.rss), p.name, p.pid)
		}
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
// withTop also lists the largest processes.
func readMemory(ctx context.Context, withTop bool) memSample {
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
	if out, err := exec.CommandContext(ctx, "/usr/sbin/sysctl", "-n", "kern.memorystatus_vm_pressure_level").Output(); err == nil {
		s.pressure = pressureName(strings.TrimSpace(string(out)))
	}
	if withTop {
		s.top = largest(ctx)
	}
	return s
}

// pressureName is kern.memorystatus_vm_pressure_level in words.
func pressureName(level string) string {
	switch level {
	case "1":
		return "normal"
	case "2":
		return "warn"
	case "4":
		return "critical"
	}
	return ""
}

// largest is the topN processes by memory in RAM, each named by its program
// and what it was started with: "Python -c" alone does not say whose worker
// it is, so a worker started with -c is named by its parent as well.
func largest(ctx context.Context) []proc {
	out, err := exec.CommandContext(ctx, "/bin/ps", "-axo", "rss=,pid=,ppid=,comm=").Output()
	if err != nil {
		return nil
	}
	all := parsePS(string(out))
	parent := map[int]int{}
	comm := map[int]string{}
	for _, p := range all {
		parent[p.pid], comm[p.pid] = p.ppid, p.comm
	}
	sort.Slice(all, func(i, j int) bool { return all[i].rss > all[j].rss })
	if len(all) > topN {
		all = all[:topN]
	}
	pids := []string{}
	for _, p := range all {
		pids = append(pids, strconv.Itoa(p.pid), strconv.Itoa(p.ppid))
	}
	args := map[int]string{}
	if out, err := exec.CommandContext(ctx, "/bin/ps", "-o", "pid=,args=", "-p", strings.Join(pids, ",")).Output(); err == nil {
		for _, l := range strings.Split(string(out), "\n") {
			f := strings.Fields(l)
			if len(f) < 2 {
				continue
			}
			if pid, err := strconv.Atoi(f[0]); err == nil {
				args[pid] = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l), f[0]))
			}
		}
	}
	top := make([]proc, 0, len(all))
	for _, p := range all {
		name := procName(comm[p.pid], args[p.pid])
		if strings.HasSuffix(name, " -c") && parent[p.pid] > 1 {
			name += ", child of " + procName(comm[parent[p.pid]], args[parent[p.pid]])
		}
		top = append(top, proc{pid: p.pid, rss: p.rss, name: name})
	}
	return top
}

// psRow is one line of `ps -axo rss=,pid=,ppid=,comm=`.
type psRow struct {
	pid, ppid int
	rss       uint64
	comm      string
}

// parsePS reads `ps -axo rss=,pid=,ppid=,comm=`; comm, last, may hold spaces.
func parsePS(out string) []psRow {
	var rows []psRow
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) < 4 {
			continue
		}
		kb, e1 := strconv.ParseUint(f[0], 10, 64)
		pid, e2 := strconv.Atoi(f[1])
		ppid, e3 := strconv.Atoi(f[2])
		if e1 != nil || e2 != nil || e3 != nil {
			continue
		}
		rest := l
		for range 3 {
			rest = strings.TrimLeft(rest, " \t")
			rest = rest[strings.IndexAny(rest+" ", " \t"):]
		}
		rows = append(rows, psRow{pid: pid, ppid: ppid, rss: kb << 10, comm: strings.TrimSpace(rest)})
	}
	return rows
}

// procName is the program's file name and the first two words it was
// started with, paths cut to their last part: "Python sg_console.py serve",
// or up to -c, whose code says nothing about whose worker it is.
func procName(comm, args string) string {
	name := filepath.Base(comm)
	if comm == "" {
		return "?"
	}
	rest := strings.TrimSpace(strings.TrimPrefix(args, comm))
	if rest == args {
		// argv[0] is not the path ps named: drop its first word.
		if f := strings.Fields(args); len(f) > 0 {
			rest = strings.TrimSpace(strings.TrimPrefix(args, f[0]))
		}
	}
	for i, w := range strings.Fields(rest) {
		if i == 2 {
			break
		}
		name += " " + filepath.Base(w)
		if w == "-c" {
			break // what follows is code, not a name
		}
	}
	if len(name) > 60 {
		name = name[:60]
	}
	return name
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
