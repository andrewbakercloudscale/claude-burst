package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestParseSwapEitherDecimalMark(t *testing.T) {
	for _, in := range []string{
		"total = 12288.00M  used = 11191.12M  free = 1096.88M  (encrypted)",
		"total = 12288,00M  used = 11191,12M  free = 1096,88M  (encrypted)",
	} {
		total, used := parseSwap(in)
		if total != 12288<<20 || mb(used) != "11191MB" {
			t.Fatalf("%q: total %d used %d", in, total, used)
		}
	}
	if total, used := parseSwap("nonsense"); total != 0 || used != 0 {
		t.Fatal("nonsense read as swap")
	}
}

func TestStallLine(t *testing.T) {
	if l := stallLine(12*time.Second, 10*time.Second); l != "" {
		t.Fatalf("2s of jitter is a stall: %q", l)
	}
	l := stallLine(41*time.Second, 10*time.Second)
	if !strings.HasPrefix(l, "WARNING memory: the gateway did not run for 41s") || !strings.Contains(l, "31s late") {
		t.Fatalf("%q", l)
	}
}

func TestMemLine(t *testing.T) {
	l := memLine(memSample{rss: 87 << 20, peakRSS: 120 << 20, goTotal: 60 << 20, heap: 30 << 20, goroutines: 210, gcs: 5, swapUsed: 11191 << 20, swapOf: 12288 << 20})
	if l != "memory: rss=87MB peak_rss=120MB go=60MB heap=30MB goroutines=210 gc=5; Mac swap 11191MB of 12288MB used" {
		t.Fatalf("%q", l)
	}
	l = memLine(memSample{rss: 100 << 20, goTotal: 2 << 30})
	if !strings.HasPrefix(l, "WARNING ") || !strings.Contains(l, "swapped out") || !strings.Contains(l, "swap") {
		t.Fatalf("%q", l)
	}
}

// The real sample, on this Mac: the Go figures are always there.
func TestReadMemory(t *testing.T) {
	s := readMemory(context.Background(), true)
	if s.goTotal == 0 || s.heap == 0 || s.goroutines == 0 || s.peakRSS == 0 || len(s.top) == 0 {
		t.Fatalf("%+v", s)
	}
	t.Log(memLine(s))
}

// A freeze of 5s is a line now that the check runs every 2s: on 25 Nov 2026
// freezes of 10 to 15s raised toasts in every session and wrote nothing.
func TestShortFreezeIsALine(t *testing.T) {
	if l := stallLine(4*time.Second, stallTick); l != "" {
		t.Fatalf("2s of jitter is a stall: %q", l)
	}
	if l := stallLine(5*time.Second, stallTick); !strings.HasPrefix(l, "WARNING memory: the gateway did not run for 5s") {
		t.Fatalf("%q", l)
	}
}

// A freeze line says how hard macOS is pressed and who holds the memory.
func TestMemLineNamesPressureAndLargest(t *testing.T) {
	l := memLine(memSample{rss: 87 << 20, goTotal: 60 << 20, swapUsed: 8469 << 20, swapOf: 9216 << 20, pressure: "warn",
		top: []proc{{pid: 26956, rss: 6225 << 20, name: "Python -c, child of Python sg_console.py serve"}, {pid: 2, rss: 754 << 20, name: "Google Chrome Helper (Renderer)"}}})
	for _, want := range []string{"; memory pressure warn", "; largest: 6225MB Python -c, child of Python sg_console.py serve (pid 26956), 754MB Google Chrome Helper (Renderer) (pid 2)"} {
		if !strings.Contains(l, want) {
			t.Fatalf("%q lacks %q", l, want)
		}
	}
}

func TestParsePSKeepsSpacesInThePath(t *testing.T) {
	rows := parsePS("6225000 26956 87110 /Applications/Xcode.app/Python.app/Contents/MacOS/Python\n  754000   812     1 /Applications/Google Chrome.app/Helpers/Google Chrome Helper (Renderer)\nbad line\n")
	if len(rows) != 2 || rows[0].pid != 26956 || rows[0].ppid != 87110 || rows[0].rss != 6225000<<10 ||
		rows[1].comm != "/Applications/Google Chrome.app/Helpers/Google Chrome Helper (Renderer)" {
		t.Fatalf("%+v", rows)
	}
}

func TestProcName(t *testing.T) {
	py := "/Applications/Xcode.app/Python.app/Contents/MacOS/Python"
	for _, c := range []struct{ comm, args, want string }{
		{py, py + " /Users/me/.sg-tightener/app/sg_console.py serve --service --port 8765", "Python sg_console.py serve"},
		{py, py + " -c from multiprocessing.spawn import spawn_main", "Python -c"},
		{"/usr/local/bin/claude", "claude --resume x", "claude --resume x"},
		{"", "", "?"},
	} {
		if got := procName(c.comm, c.args); got != c.want {
			t.Errorf("procName(%q, %q) = %q, want %q", c.comm, c.args, got, c.want)
		}
	}
}

func TestSoftLimitFor(t *testing.T) {
	for _, c := range []struct {
		ram  uint64
		want int64
	}{
		{8 << 30, memSoftLimitSmall},
		{16 << 30, memSoftLimit},
		{32 << 30, memSoftLimit},
		{0, memSoftLimit},
	} {
		if got := softLimitFor(c.ram); got != c.want {
			t.Errorf("softLimitFor(%dGB) = %dMB, want %dMB", c.ram>>30, got>>20, c.want>>20)
		}
	}
}

func TestGiveBackOnlyWhenWellOverHeap(t *testing.T) {
	if giveBack(150<<20, 100<<20) {
		t.Error("50MB over the heap was given back")
	}
	if !giveBack(515<<20, 66<<20) {
		t.Error("515MB mapped for a 66MB heap was kept")
	}
}
