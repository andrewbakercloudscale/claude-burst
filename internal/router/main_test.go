package router

import (
	"os"
	"testing"
	"time"
)

// TestMain keeps every test off the real Mac's network state: without this a
// test run on a phone hotspot would see different failover thresholds from
// one run at a desk. Tests that need the hotspot answer set it themselves.
func TestMain(m *testing.M) {
	failedOnHotspot = func(error) bool { return false }
	// Burst's own wait-and-resend on a 5xx or a dead network: instant in
	// tests. outage_retry_test.go covers it.
	outageRetryDelays = []time.Duration{time.Millisecond, time.Millisecond}
	os.Exit(m.Run())
}
