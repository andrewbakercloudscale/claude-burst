package router

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
)

func TestMeteredFailoverDetector_SingleFailureDoesNotTrigger(t *testing.T) {
	d := newMeteredFailureDetector(60, 3, 3)
	dec := d.OnResponse(http.StatusTooManyRequests, http.Header{}, nil)
	if dec.Failover {
		t.Fatal("a single 429 must not trigger failover in metered mode")
	}
}

func TestMeteredFailoverDetector_SustainedFailuresTrigger(t *testing.T) {
	d := newMeteredFailureDetector(60, 3, 3)
	var dec FailoverDecision
	for i := 0; i < 3; i++ {
		dec = d.OnResponse(http.StatusInternalServerError, http.Header{}, nil)
	}
	if !dec.Failover {
		t.Fatal("3 failures within the window must trigger failover")
	}
	if dec.Claim == "" || dec.Reason == "" {
		t.Fatalf("failover decision missing claim/reason: %+v", dec)
	}
}

func TestMeteredFailoverDetector_SuccessResetsCounter(t *testing.T) {
	d := newMeteredFailureDetector(60, 3, 3)
	d.OnResponse(http.StatusTooManyRequests, http.Header{}, nil)
	d.OnResponse(http.StatusTooManyRequests, http.Header{}, nil)
	d.OnSuccess()
	dec := d.OnResponse(http.StatusTooManyRequests, http.Header{}, nil)
	if dec.Failover {
		t.Fatal("a success must reset the failure count")
	}
}

func TestMeteredFailoverDetector_WindowExpiryDoesNotAccumulate(t *testing.T) {
	d := newMeteredFailureDetector(60, 3, 3)
	base := time.Now()
	tick := 0
	d.now = func() time.Time {
		tick++
		return base.Add(time.Duration(tick) * 40 * time.Second) // spaced beyond the 60s window
	}
	var dec FailoverDecision
	for i := 0; i < 3; i++ {
		dec = d.OnResponse(http.StatusInternalServerError, http.Header{}, nil)
	}
	if dec.Failover {
		t.Fatal("failures spaced beyond the window must not accumulate")
	}
}

func TestMeteredFailoverDetector_NonRetryableStatusesNeverTrigger(t *testing.T) {
	d := newMeteredFailureDetector(60, 1, 1)
	for _, status := range []int{
		http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound,
		http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity,
	} {
		if dec := d.OnResponse(status, http.Header{}, nil); dec.Failover {
			t.Fatalf("status %d must never trigger metered failover (routing to a second paid provider won't fix it)", status)
		}
	}
}

// TestMeteredFailoverDetector_TransportErrorCounts exercises the transport
// leg's own threshold (transportMinFailures), which is tracked and counted
// separately from the HTTP-response leg (minFailures) -- see
// TestMeteredFailoverDetector_TransportErrorFailsOverImmediatelyByDefault for
// the actual production default of 1.
func TestMeteredFailoverDetector_TransportErrorCounts(t *testing.T) {
	d := newMeteredFailureDetector(60, 100, 2) // minFailures set high and unused; only the transport leg is exercised
	dec := d.OnError(errTest{"connection refused"})
	if dec.Failover {
		t.Fatal("a single transport error must not trigger failover when transportMinFailures is explicitly raised to 2")
	}
	dec = d.OnError(errTest{"connection refused"})
	if !dec.Failover {
		t.Fatal("sustained transport errors must trigger failover once the configured transport threshold is reached")
	}
}

// TestMeteredFailoverDetector_TransportErrorFailsOverImmediatelyByDefault
// covers the actual request from 2026-09-03: opening Claude Code while
// Anthropic is unreachable should fail over on the very first genuine
// transport error, not sit through a run of failed requests first. A
// transport error means the connection couldn't even be established, which
// is a stronger "Anthropic is down" signal than an HTTP error response (see
// the metered vs. transport threshold split in meteredFailureDetector's doc
// comment), so it defaults to a threshold of 1 rather than sharing
// minFailures's default of 3.
func TestMeteredFailoverDetector_TransportErrorFailsOverImmediatelyByDefault(t *testing.T) {
	d := newMeteredFailureDetector(60, 3, 0) // 0 -> transportMinFailures defaults to 1
	dec := d.OnError(errTest{"dial tcp 160.79.104.10:443: i/o timeout"})
	if !dec.Failover {
		t.Fatal("a genuine transport error must fail over on the first occurrence by default")
	}
}

type errTest struct{ msg string }

func (e errTest) Error() string { return e.msg }

// TestMeteredFailoverDetector_LocalConnectivityFailuresDoNotCount is a
// regression test for a real incident (2026-08-31): walking out of WiFi
// range produced a burst of transport errors on the primary, which the
// metered detector could not distinguish from a genuine Anthropic outage,
// so it failed over to a secondary that was equally unreachable over the
// same dead connection -- and then kept preferring it for
// unknown_reset_seconds (5 minutes by default) after connectivity returned,
// since a transport error carries no reset header to shorten that window.
// DNS resolution failure and "no route to host"/"network unreachable" are
// the unambiguous cases: they mean this machine has no path to the
// internet at all, not that Anthropic specifically is having trouble.
func TestMeteredFailoverDetector_LocalConnectivityFailuresDoNotCount(t *testing.T) {
	dnsErr := &net.DNSError{Err: "no such host", Name: "api.anthropic.com", IsNotFound: true}
	netUnreachable := &net.OpError{Op: "dial", Net: "tcp", Err: &os.SyscallError{Syscall: "connect", Err: syscall.ENETUNREACH}}
	hostUnreachable := &net.OpError{Op: "dial", Net: "tcp", Err: &os.SyscallError{Syscall: "connect", Err: syscall.EHOSTUNREACH}}

	for name, err := range map[string]error{
		"DNS resolution failure": dnsErr,
		"network unreachable":    netUnreachable,
		"host unreachable":       hostUnreachable,
	} {
		t.Run(name, func(t *testing.T) {
			// transportMinFailures=1: even one call must not trigger.
			d := newMeteredFailureDetector(60, 1, 1)
			if dec := d.OnError(err); dec.Failover {
				t.Fatalf("%v must not count toward metered failover -- it means no network path exists, not that Anthropic is down", err)
			}
		})
	}
}

// TestMeteredFailoverDetector_GenuineTransportErrorsStillCount guards the
// other direction of the fix above: connection refused, timeouts, and
// other transport errors that could plausibly be Anthropic's side (as
// opposed to "this machine has no network at all") must keep counting
// toward whatever transport threshold is configured. Narrowing the
// exclusion too far would silently break the resilience-to-a-real-outage
// behavior this detector exists for.
func TestMeteredFailoverDetector_GenuineTransportErrorsStillCount(t *testing.T) {
	connRefused := &net.OpError{Op: "dial", Net: "tcp", Err: &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED}}
	for name, err := range map[string]error{
		"connection refused": connRefused,
		"generic timeout":    errTest{"context deadline exceeded"},
	} {
		t.Run(name, func(t *testing.T) {
			d := newMeteredFailureDetector(60, 3, 2)
			d.OnError(err)
			if dec := d.OnError(err); !dec.Failover {
				t.Fatalf("%v should still count toward the transport threshold -- it's not one of the unambiguous local-connectivity cases", err)
			}
		})
	}
}

func TestCombinedDetector_SubscriptionSignalFiresImmediately(t *testing.T) {
	d := newCombinedDetector(config.MeteredFailoverConfig{WindowSeconds: 60, MinFailures: 3})
	h := http.Header{}
	h.Set("anthropic-ratelimit-unified-status", "rejected")
	dec := d.OnResponse(http.StatusTooManyRequests, h, nil)
	if !dec.Failover {
		t.Fatal("a genuine subscription-exhaustion signal must fail over on the first occurrence, same as plain subscription-limit")
	}
}

func TestCombinedDetector_BareServerErrorNeedsSustainedFailures(t *testing.T) {
	d := newCombinedDetector(config.MeteredFailoverConfig{WindowSeconds: 60, MinFailures: 3})
	for i := 0; i < 2; i++ {
		if dec := d.OnResponse(http.StatusInternalServerError, http.Header{}, nil); dec.Failover {
			t.Fatalf("failure %d: a bare 500 must not fail over before min_failures is reached", i+1)
		}
	}
	dec := d.OnResponse(http.StatusInternalServerError, http.Header{}, nil)
	if !dec.Failover {
		t.Fatal("3 bare 500s within the window must fail over under subscription-limit+metered-failures")
	}
}

func TestCombinedDetector_TimeoutCountsTowardMeteredWindow(t *testing.T) {
	d := newCombinedDetector(config.MeteredFailoverConfig{WindowSeconds: 60, MinFailures: 2, TransportErrorMinFailures: 2})
	if dec := d.OnError(errTest{"context deadline exceeded"}); dec.Failover {
		t.Fatal("a single timeout must not fail over when transport_error_min_failures is explicitly raised above 1")
	}
	dec := d.OnError(errTest{"context deadline exceeded"})
	if !dec.Failover {
		t.Fatal("sustained timeouts must fail over once the configured transport threshold is reached")
	}
}

// TestCombinedDetector_TransportErrorFailsOverImmediatelyByDefault mirrors
// the actual configured strategy for oauth-passthrough in production
// (subscription-limit+metered-failures, transport_error_min_failures unset
// in config.json): when Anthropic can't be reached at all, failover must not
// wait for a run of failures -- most noticeable as a broken session right
// when Claude Code starts up.
func TestCombinedDetector_TransportErrorFailsOverImmediatelyByDefault(t *testing.T) {
	d := newCombinedDetector(config.MeteredFailoverConfig{WindowSeconds: 60, MinFailures: 3})
	dec := d.OnError(errTest{"dial tcp 160.79.104.10:443: i/o timeout"})
	if !dec.Failover {
		t.Fatal("a genuine transport error (Anthropic unreachable) must fail over on the first occurrence by default")
	}
}

func TestCombinedDetector_SuccessResetsMeteredCount(t *testing.T) {
	d := newCombinedDetector(config.MeteredFailoverConfig{WindowSeconds: 60, MinFailures: 2})
	d.OnResponse(http.StatusInternalServerError, http.Header{}, nil)
	d.OnSuccess()
	dec := d.OnResponse(http.StatusInternalServerError, http.Header{}, nil)
	if dec.Failover {
		t.Fatal("a success must reset the metered failure count even under the combined strategy")
	}
}

func TestMeteredFailoverDetector_ConcurrentSafe(t *testing.T) {
	d := newMeteredFailureDetector(60, 1000000, 1000000) // high thresholds: exercise the lock, not the trigger
	done := make(chan struct{})
	for i := 0; i < 20; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 50; j++ {
				d.OnResponse(http.StatusInternalServerError, http.Header{}, nil)
				d.OnSuccess()
			}
		}()
	}
	for i := 0; i < 20; i++ {
		<-done
	}
}

// The exact error the running gateway recorded on 2026-09-08 at
// transportMinFailures=1: a *url.Error wrapping context.Canceled, produced
// when Claude Code interrupted a turn and the outbound request inherited the
// cancelled client context. It armed a 300s overflow window and sent two
// turns to the paid secondary.
func canceledTransportError() error {
	return &url.Error{
		Op:  "Post",
		URL: "https://api.anthropic.com/v1/messages?beta=true",
		Err: context.Canceled,
	}
}

func TestMeteredFailoverDetector_ClientCancellationNeverFailsOver(t *testing.T) {
	// transportMinFailures=1 is the shipped default and the setting that made
	// this expensive: one cancellation was one overflow window.
	d := newMeteredFailureDetector(60, 3, 1)
	for i := 0; i < 5; i++ {
		if dec := d.OnError(canceledTransportError()); dec.Failover {
			t.Fatalf("client cancellation must never trigger failover (attempt %d): %+v", i+1, dec)
		}
	}
}

// Cancellation must not merely be excused -- it must not be COUNTED either,
// or two cancellations plus one real transport error would fail over at a
// threshold of three, which is the same bug wearing a bigger number.
func TestMeteredFailoverDetector_CancellationDoesNotCountTowardWindow(t *testing.T) {
	d := newMeteredFailureDetector(60, 3, 3)
	d.OnError(canceledTransportError())
	d.OnError(canceledTransportError())
	if dec := d.OnError(&url.Error{Op: "Post", URL: "https://api.anthropic.com", Err: syscall.ECONNREFUSED}); dec.Failover {
		t.Fatalf("two cancellations must not count toward a 3-failure window: %+v", dec)
	}
}

// The carve-out is for cancellation specifically. A deadline that expires is
// a real stalled upstream -- the case the metered window exists for -- and
// must still count, or the detector goes blind to a slow primary.
func TestMeteredFailoverDetector_DeadlineExceededStillCounts(t *testing.T) {
	d := newMeteredFailureDetector(60, 3, 1)
	dec := d.OnError(&url.Error{
		Op:  "Post",
		URL: "https://api.anthropic.com/v1/messages",
		Err: context.DeadlineExceeded,
	})
	if !dec.Failover {
		t.Fatal("a deadline-exceeded transport error must still count toward metered failover")
	}
}

// A genuine transport failure still fires on the first one, unchanged.
func TestMeteredFailoverDetector_RealTransportErrorStillFailsOverFirstTime(t *testing.T) {
	d := newMeteredFailureDetector(60, 3, 1)
	dec := d.OnError(&url.Error{Op: "Post", URL: "https://api.anthropic.com", Err: syscall.ECONNREFUSED})
	if !dec.Failover {
		t.Fatal("connection refused must still trigger failover on the first occurrence")
	}
}

// The combined detector is what oauth-passthrough actually runs with
// "subscription-limit+metered-failures", so the carve-out has to survive the
// delegation rather than only existing on the metered leg in isolation.
func TestCombinedDetector_ClientCancellationNeverFailsOver(t *testing.T) {
	d := newCombinedDetector(config.MeteredFailoverConfig{WindowSeconds: 60, MinFailures: 3, TransportErrorMinFailures: 1})
	if dec := d.OnError(canceledTransportError()); dec.Failover {
		t.Fatalf("combined detector must not fail over on client cancellation: %+v", dec)
	}
}
