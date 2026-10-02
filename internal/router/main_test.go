package router

import (
	"os"
	"testing"
)

// TestMain keeps every test off the real Mac's network state: without this a
// test run on a phone hotspot would see different failover thresholds from
// one run at a desk. Tests that need the hotspot answer set it themselves.
func TestMain(m *testing.M) {
	failedOnHotspot = func(error) bool { return false }
	os.Exit(m.Run())
}
