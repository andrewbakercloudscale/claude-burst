package admin

import (
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// Two probes open two connections: a connection kept from before the
// redirect changed cannot answer for the second one.
func TestProbeClientDialsFreshEachTime(t *testing.T) {
	t.Parallel()
	var conns atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			conns.Add(1)
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)

	c := probeClient(time.Second)
	for range 2 {
		resp, err := c.Get(srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	if got := conns.Load(); got != 2 {
		t.Errorf("connections = %d, want 2", got)
	}
}
