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
