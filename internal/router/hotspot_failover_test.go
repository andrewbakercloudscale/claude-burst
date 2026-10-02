package router

import (
	"bytes"
	"errors"
	"log"
	"net"
	"net/http"
	"strings"
	"syscall"
	"testing"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
)

// brokenPipe is the 2026-10-02 12:04 failure: a write on a phone hotspot
// address whose mobile uplink had dropped.
func brokenPipe(src string) error {
	return &net.OpError{
		Op: "write", Net: "tcp",
		Source: &net.TCPAddr{IP: net.ParseIP(src), Port: 53938},
		Addr:   &net.TCPAddr{IP: net.ParseIP("160.79.104.10"), Port: 443},
		Err:    &net.OpError{Op: "write", Err: syscall.EPIPE},
	}
}

func stubHotspot(t *testing.T, on bool) {
	t.Helper()
	old := failedOnHotspot
	failedOnHotspot = func(error) bool { return on }
	t.Cleanup(func() { failedOnHotspot = old })
}

// The detector the config builds: transport threshold 1, the user's setting
// on 2026-10-02, and the default multiplier.
func hotspotDetector(t *testing.T, logf func(string, ...any)) *meteredFailureDetector {
	t.Helper()
	fd, err := buildDetector("subscription-limit+metered-failures",
		config.MeteredFailoverConfig{WindowSeconds: 60, MinFailures: 3, TransportErrorMinFailures: 1}, logf)
	if err != nil {
		t.Fatal(err)
	}
	return fd.(*combinedDetector).metered
}

func TestHotspotTransportFailuresNeedMore(t *testing.T) {
	stubHotspot(t, true)
	var logBuf bytes.Buffer
	d := hotspotDetector(t, log.New(&logBuf, "", 0).Printf)
	if dec := d.OnError(brokenPipe("172.20.10.2")); dec.Failover {
		t.Fatal("one transport failure on a phone hotspot armed a window; the default multiplier needs 2")
	}
	if !strings.Contains(logBuf.String(), "on a phone hotspot: needs 2 failures") {
		t.Fatalf("the held-back failure was not explained in the log:\n%s", logBuf.String())
	}
	dec := d.OnError(brokenPipe("172.20.10.2"))
	if !dec.Failover {
		t.Fatal("the second failure on a hotspot must still fail over: the threshold is raised, not removed")
	}
	if !strings.Contains(dec.Reason, "on a phone hotspot: needs 2 failures") {
		t.Fatalf("reason does not say why it took two: %q", dec.Reason)
	}
}

func TestOffHotspotTransportThresholdUnchanged(t *testing.T) {
	stubHotspot(t, false)
	var logBuf bytes.Buffer
	d := hotspotDetector(t, log.New(&logBuf, "", 0).Printf)
	dec := d.OnError(brokenPipe("192.168.1.20"))
	if !dec.Failover {
		t.Fatal("off a hotspot one transport failure must still fail over, as configured")
	}
	if strings.Contains(dec.Reason, "hotspot") || logBuf.Len() != 0 {
		t.Fatalf("hotspot wording off a hotspot: reason %q log %q", dec.Reason, logBuf.String())
	}
}

// A 429 or 5xx is Anthropic answering; a phone cannot fake that, so the
// HTTP threshold is never scaled.
func TestHotspotDoesNotChangeHTTPFailures(t *testing.T) {
	stubHotspot(t, true)
	d := hotspotDetector(t, nil)
	for i := 1; i <= 3; i++ {
		dec := d.OnResponse(http.StatusTooManyRequests, http.Header{}, []byte(`{"type":"error"}`))
		if want := i == 3; dec.Failover != want {
			t.Fatalf("429 #%d: failover=%v, want %v (min_failures 3, unscaled)", i, dec.Failover, want)
		}
		if strings.Contains(dec.Reason, "hotspot") {
			t.Fatalf("an HTTP failure mentions the hotspot: %q", dec.Reason)
		}
	}
}

func TestHotspotMultiplierOneTurnsItOff(t *testing.T) {
	stubHotspot(t, true)
	fd, err := buildDetector("metered-failures",
		config.MeteredFailoverConfig{TransportErrorMinFailures: 1, HotspotTransportMultiplier: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !fd.OnError(brokenPipe("172.20.10.2")).Failover {
		t.Fatal("multiplier 1 must leave the threshold as configured")
	}
}

// A detector made directly (as most tests do) never asks where traffic goes.
func TestDirectDetectorIgnoresHotspot(t *testing.T) {
	asked := false
	old := failedOnHotspot
	failedOnHotspot = func(error) bool { asked = true; return true }
	t.Cleanup(func() { failedOnHotspot = old })
	d := newMeteredFailureDetector(60, 3, 1)
	if !d.OnError(brokenPipe("172.20.10.2")).Failover || asked {
		t.Fatalf("direct detector: asked=%v", asked)
	}
}

func TestFailedOnHotspotReadsTheConnectionsSourceAddress(t *testing.T) {
	// The real function, not the TestMain stub: an error that carries its
	// source address must be answered from it, without asking the routing
	// table.
	fromSource := failedOnHotspotFromSource
	if on, ok := fromSource(brokenPipe("172.20.10.2")); !ok || !on {
		t.Fatalf("172.20.10.2: on=%v ok=%v", on, ok)
	}
	if on, ok := fromSource(brokenPipe("192.168.1.20")); !ok || on {
		t.Fatalf("192.168.1.20: on=%v ok=%v", on, ok)
	}
	if _, ok := fromSource(errors.New("dial timeout")); ok {
		t.Fatal("an error with no source address must fall back to the route check")
	}
}
