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
	s := readMemory(context.Background())
	if s.goTotal == 0 || s.heap == 0 || s.goroutines == 0 || s.peakRSS == 0 {
		t.Fatalf("%+v", s)
	}
}
