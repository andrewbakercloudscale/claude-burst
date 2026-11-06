package admin

import (
	"errors"
	"testing"
	"time"
)

// A slow pass is kept and not run again at once; a quick one and a failed
// one are run every time.
func TestCachedScanKeepsOnlySlowAnswers(t *testing.T) {
	n := 0
	slow := func() (int, error) { n++; time.Sleep(scanSlow + 10*time.Millisecond); return n, nil }
	if v, _ := cachedScan(t.Name()+"slow", slow); v != 1 {
		t.Fatalf("first %d", v)
	}
	if v, _ := cachedScan(t.Name()+"slow", slow); v != 1 || n != 1 {
		t.Fatalf("a slow pass must be kept: v=%d runs=%d", v, n)
	}
	q := 0
	quick := func() (int, error) { q++; return q, nil }
	cachedScan(t.Name()+"quick", quick)
	if v, _ := cachedScan(t.Name()+"quick", quick); v != 2 {
		t.Fatalf("a quick pass is always fresh, got %d", v)
	}
	f := 0
	failing := func() (int, error) { f++; time.Sleep(scanSlow + 10*time.Millisecond); return 0, errors.New("x") }
	cachedScan(t.Name()+"fail", failing)
	cachedScan(t.Name()+"fail", failing)
	if f != 2 {
		t.Fatalf("a failed pass is not kept: %d runs", f)
	}
}

// An answer many callers poll is kept for its minimum however quick it was,
// and runs again once that has passed; a failure is still never kept.
func TestCachedScanAtLeastKeepsQuickAnswers(t *testing.T) {
	q := 0
	quick := func() (int, error) { q++; return q, nil }
	cachedScanAtLeast(t.Name(), 50*time.Millisecond, quick)
	if v, _ := cachedScanAtLeast(t.Name(), 50*time.Millisecond, quick); v != 1 {
		t.Fatalf("kept for its minimum, got run %d", v)
	}
	time.Sleep(60 * time.Millisecond)
	if v, _ := cachedScanAtLeast(t.Name(), 50*time.Millisecond, quick); v != 2 {
		t.Fatalf("run again after the minimum, got run %d", v)
	}
	f := 0
	failing := func() (int, error) { f++; return 0, errors.New("x") }
	cachedScanAtLeast(t.Name()+"fail", time.Minute, failing)
	cachedScanAtLeast(t.Name()+"fail", time.Minute, failing)
	if f != 2 {
		t.Fatalf("a failed pass is not kept: %d runs", f)
	}
}
