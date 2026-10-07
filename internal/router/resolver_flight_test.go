package router

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A resolver that answers after `delay`, or never answers well when `fail`.
func slowDoH(t *testing.T, delay time.Duration, fail bool, calls *int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(calls, 1)
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
		if fail {
			http.Error(w, "blocked", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte(`{"Status":0,"Answer":[{"type":1,"TTL":60,"data":"160.79.104.10"}]}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// Twenty requests arriving with nothing cached used to make twenty lookups.
func TestRequestsArrivingTogetherShareOneLookup(t *testing.T) {
	var calls int32
	srv := slowDoH(t, 100*time.Millisecond, false, &calls)
	r := newInterceptResolver("api.anthropic.com", srv.URL, "")

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			addrs, err := r.lookup(context.Background(), "api.anthropic.com")
			if err != nil || len(addrs) != 1 || addrs[0] != "160.79.104.10" {
				t.Errorf("lookup = %v, %v", addrs, err)
			}
		}()
	}
	wg.Wait()
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("20 requests made %d lookups, want 1", got)
	}
}

// With no address at all, each request used to wait out every resolver in
// turn. Now the first pays for the failure and the rest are told at once,
// until negativeDNSTTL has passed and it is worth asking again.
func TestAFailedLookupIsNotRepeatedStraightAway(t *testing.T) {
	var calls int32
	srv := slowDoH(t, 150*time.Millisecond, true, &calls)
	now := time.Now()
	var mu sync.Mutex
	r := newInterceptResolver("api.anthropic.com", srv.URL, "")
	r.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }

	_, err := r.lookup(context.Background(), "api.anthropic.com")
	var le *LookupError
	if !errors.As(err, &le) {
		t.Fatalf("first lookup: %v, want a LookupError", err)
	}
	start := time.Now()
	for i := 0; i < 10; i++ {
		if _, err := r.lookup(context.Background(), "api.anthropic.com"); !errors.As(err, &le) {
			t.Fatalf("lookup %d: %v, want the remembered LookupError", i, err)
		}
	}
	if took := time.Since(start); took > 100*time.Millisecond {
		t.Errorf("10 lookups after a failure took %v: they asked again", took)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("%d lookups reached the resolver, want 1", got)
	}

	mu.Lock()
	now = now.Add(negativeDNSTTL + time.Second)
	mu.Unlock()
	r.lookup(context.Background(), "api.anthropic.com")
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("after the failure aged out %d lookups had reached the resolver, want 2", got)
	}
}

// An expired address with the resolvers hanging: the request goes ahead with
// the old address after staleDNSWait, and the ones behind it do not wait at
// all once the lookup has failed.
func TestAnExpiredAddressIsUsedRatherThanWaitedOn(t *testing.T) {
	old := staleDNSWait
	staleDNSWait = 50 * time.Millisecond
	t.Cleanup(func() { staleDNSWait = old })

	var calls int32
	srv := slowDoH(t, 400*time.Millisecond, true, &calls)
	r := newInterceptResolver("api.anthropic.com", srv.URL, "")
	r.mu.Lock()
	r.cache["api.anthropic.com"] = dnsEntry{addrs: []string{"160.79.104.10"}, expires: time.Now().Add(-time.Minute)}
	r.mu.Unlock()

	start := time.Now()
	addrs, err := r.lookup(context.Background(), "api.anthropic.com")
	if err != nil || len(addrs) != 1 || addrs[0] != "160.79.104.10" {
		t.Fatalf("lookup = %v, %v, want the old address", addrs, err)
	}
	if took := time.Since(start); took > 300*time.Millisecond {
		t.Errorf("waited %v for a lookup that was never going to answer in time", took)
	}

	// Let the lookup finish failing; the old address is still the answer
	// and nothing asks again inside negativeDNSTTL.
	r.mu.Lock()
	fl := r.flights["api.anthropic.com"]
	r.mu.Unlock()
	if fl != nil {
		<-fl.done
	}
	start = time.Now()
	for i := 0; i < 5; i++ {
		if addrs, err := r.lookup(context.Background(), "api.anthropic.com"); err != nil || len(addrs) != 1 {
			t.Fatalf("lookup %d = %v, %v", i, addrs, err)
		}
	}
	if took := time.Since(start); took > 40*time.Millisecond {
		t.Errorf("5 lookups after the failure took %v", took)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("%d lookups reached the resolver, want 1", got)
	}
}

// The request that started the lookup going away must not fail the others
// waiting on the same answer.
func TestACancelledRequestDoesNotFailTheOthersWaiting(t *testing.T) {
	var calls int32
	srv := slowDoH(t, 200*time.Millisecond, false, &calls)
	r := newInterceptResolver("api.anthropic.com", srv.URL, "")

	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() {
		_, err := r.lookup(ctx, "api.anthropic.com")
		first <- err
	}()
	time.Sleep(30 * time.Millisecond)
	second := make(chan error, 1)
	go func() {
		_, err := r.lookup(context.Background(), "api.anthropic.com")
		second <- err
	}()
	time.Sleep(30 * time.Millisecond)
	cancel()

	var le *LookupError
	if err := <-first; !errors.As(err, &le) || !errors.Is(err, context.Canceled) {
		t.Errorf("the cancelled request got %v, want a LookupError for its cancellation", err)
	}
	if err := <-second; err != nil {
		t.Errorf("the request still waiting got %v, want the address", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("%d lookups reached the resolver, want 1", got)
	}
}

// A good answer clears the memory of a failure, so the next expiry asks.
func TestASuccessfulLookupForgetsTheFailure(t *testing.T) {
	var calls, bad int32
	atomic.StoreInt32(&bad, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		if atomic.LoadInt32(&bad) == 1 {
			http.Error(w, "blocked", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte(`{"Status":0,"Answer":[{"type":1,"TTL":60,"data":"160.79.104.10"}]}`))
	}))
	defer srv.Close()
	now := time.Now()
	r := newInterceptResolver("api.anthropic.com", srv.URL, "")
	r.now = func() time.Time { return now }

	if _, err := r.lookup(context.Background(), "api.anthropic.com"); err == nil {
		t.Fatal("the first lookup must fail")
	}
	atomic.StoreInt32(&bad, 0)
	now = now.Add(negativeDNSTTL + time.Second)
	if _, err := r.lookup(context.Background(), "api.anthropic.com"); err != nil {
		t.Fatalf("after the failure aged out: %v", err)
	}
	r.mu.Lock()
	_, remembered := r.failed["api.anthropic.com"]
	r.mu.Unlock()
	if remembered {
		t.Error("a failure is still remembered after a good answer")
	}
}
