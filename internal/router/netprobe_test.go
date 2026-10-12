package router

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The retry decisions are made on errors net/http does not export, so these
// two tests provoke the real ones on loopback instead of building look-alikes
// (timeoutErr and friends in network_resilience_test.go). A Go release that
// changes what either error is, or how it reads, fails here instead of
// quietly changing which requests are sent twice.

// realHeaderTimeout is what http.Transport returns when a request was sent
// whole and no response headers came in time.
func realHeaderTimeout(t *testing.T) error {
	t.Helper()
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(up.Close)
	t.Cleanup(func() { close(release) })
	tr := &http.Transport{ResponseHeaderTimeout: 50 * time.Millisecond}
	t.Cleanup(tr.CloseIdleConnections)
	resp, err := (&http.Client{Transport: tr}).Post(up.URL, "application/json", http.NoBody)
	if err == nil {
		resp.Body.Close()
		t.Fatal("setup: the upstream never answers, so the request must time out")
	}
	return err
}

func TestRealHeaderTimeoutIsNeverResent(t *testing.T) {
	err := realHeaderTimeout(t)
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("setup: want net/http's header timeout, got %T %v", err, err)
	}
	if deadConnection(err) {
		t.Fatalf("%q was taken for a dead connection: it would be resent, and the request it answers may be running", err)
	}
	if safeToResend(err) {
		t.Fatalf("%q was judged safe to resend: the whole request was sent", err)
	}
	if isStaleWriteFailure(err) {
		t.Fatalf("%q was taken for a failed write", err)
	}
}

// A TLS handshake that times out happened before any request was written, so
// it is safe to resend. net/http reports it with an unexported type, which
// safeToResend can only recognise by its text.
func TestRealTLSHandshakeTimeoutIsSafeToResend(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	// Accepts and says nothing: the handshake never gets its ServerHello.
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { c.Close() })
		}
	}()
	tr := &http.Transport{TLSHandshakeTimeout: 50 * time.Millisecond}
	t.Cleanup(tr.CloseIdleConnections)
	resp, err := (&http.Client{Transport: tr}).Get("https://" + ln.Addr().String() + "/")
	if err == nil {
		resp.Body.Close()
		t.Fatal("setup: nothing answers the handshake, so it must time out")
	}
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("setup: want net/http's TLS handshake timeout, got %T %v", err, err)
	}
	if !safeToResend(err) {
		t.Fatalf("%q is no longer recognised as a TLS handshake timeout: nothing was sent, and it must stay safe to resend", err)
	}
	if deadConnection(err) {
		t.Fatalf("%q was taken for a dead connection", err)
	}
}
