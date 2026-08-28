package router

import (
	"net/http"
	"testing"
	"time"
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
