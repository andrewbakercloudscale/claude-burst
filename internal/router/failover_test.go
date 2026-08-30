package router

import (
	"net/http"
	"testing"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
)

func TestMeteredFailoverDetector_SingleFailureDoesNotTrigger(t *testing.T) {
	d := newMeteredFailureDetector(60, 3)
	dec := d.OnResponse(http.StatusTooManyRequests, http.Header{}, nil)
	if dec.Failover {
		t.Fatal("a single 429 must not trigger failover in metered mode")
	}
}

func TestMeteredFailoverDetector_SustainedFailuresTrigger(t *testing.T) {
	d := newMeteredFailureDetector(60, 3)
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
	d := newMeteredFailureDetector(60, 3)
	d.OnResponse(http.StatusTooManyRequests, http.Header{}, nil)
	d.OnResponse(http.StatusTooManyRequests, http.Header{}, nil)
	d.OnSuccess()
	dec := d.OnResponse(http.StatusTooManyRequests, http.Header{}, nil)
	if dec.Failover {
		t.Fatal("a success must reset the failure count")
	}
}

func TestMeteredFailoverDetector_WindowExpiryDoesNotAccumulate(t *testing.T) {
	d := newMeteredFailureDetector(60, 3)
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
	d := newMeteredFailureDetector(60, 1)
	for _, status := range []int{
		http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound,
		http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity,
	} {
		if dec := d.OnResponse(status, http.Header{}, nil); dec.Failover {
			t.Fatalf("status %d must never trigger metered failover (routing to a second paid provider won't fix it)", status)
		}
	}
}

func TestMeteredFailoverDetector_TransportErrorCounts(t *testing.T) {
	d := newMeteredFailureDetector(60, 2)
	dec := d.OnError(errTest{"connection refused"})
	if dec.Failover {
		t.Fatal("a single transport error must not trigger failover")
	}
	dec = d.OnError(errTest{"connection refused"})
	if !dec.Failover {
		t.Fatal("sustained transport errors must trigger failover")
	}
}

type errTest struct{ msg string }

func (e errTest) Error() string { return e.msg }

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
	d := newCombinedDetector(config.MeteredFailoverConfig{WindowSeconds: 60, MinFailures: 2})
	if dec := d.OnError(errTest{"context deadline exceeded"}); dec.Failover {
		t.Fatal("a single timeout must not fail over")
	}
	dec := d.OnError(errTest{"context deadline exceeded"})
	if !dec.Failover {
		t.Fatal("sustained timeouts must fail over under subscription-limit+metered-failures")
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
	d := newMeteredFailureDetector(60, 1000000) // high threshold: exercise the lock, not the trigger
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
