package router

import (
	"context"
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// On 2026-09-30 a network reset every TLS connection to cloudflare-dns.com
// while https://1.1.1.1/dns-query answered. One endpoint failing must not
// fail the lookup, and the endpoint that answered is asked first next time.
func TestLookupFallsBackToNextEndpointAndPrefersIt(t *testing.T) {
	var deadCalls int32
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&deadCalls, 1)
		hj, _ := w.(http.Hijacker)
		c, _, _ := hj.Hijack()
		c.Close() // the reset, as near as a test server gets
	}))
	defer dead.Close()
	good := dohServer(t, dohResponse{Answer: []dohAnswer{{Name: "api.anthropic.com", Type: 1, TTL: 1, Data: "160.79.104.10"}}}, nil)
	defer good.Close()

	r := newInterceptResolver("api.anthropic.com", dead.URL, "")
	r.fallbacks = []string{good.URL}
	clock := time.Now()
	r.now = func() time.Time { return clock }

	addrs, err := r.lookup(context.Background(), "api.anthropic.com")
	if err != nil || len(addrs) != 1 || addrs[0] != "160.79.104.10" {
		t.Fatalf("lookup = %v, %v; want the fallback's answer", addrs, err)
	}
	clock = clock.Add(time.Hour) // past the TTL, so it asks again
	before := atomic.LoadInt32(&deadCalls)
	if _, err := r.lookup(context.Background(), "api.anthropic.com"); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&deadCalls) != before {
		t.Fatal("the endpoint that answered last must be asked first")
	}
}

// The last good answer survives a restart: with every resolver failing, a
// fresh resolver still has an address to dial. Without this, the gateway
// restarted by a deploy on a blocking network had none (2026-09-30).
func TestLastGoodAnswerSurvivesRestart(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "resolver-cache.json")
	good := dohServer(t, dohResponse{Answer: []dohAnswer{{Name: "api.anthropic.com", Type: 1, TTL: 60, Data: "160.79.104.10"}}}, nil)
	r := newInterceptResolver("api.anthropic.com", good.URL, "")
	r.cachePath = cache
	if _, err := r.lookup(context.Background(), "api.anthropic.com"); err != nil {
		t.Fatal(err)
	}
	good.Close()

	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", http.StatusInternalServerError)
	}))
	defer failing.Close()
	r2 := newInterceptResolver("api.anthropic.com", failing.URL, "")
	r2.cachePath = cache
	r2.loadCache()
	addrs, err := r2.lookup(context.Background(), "api.anthropic.com")
	if err != nil || len(addrs) != 1 || addrs[0] != "160.79.104.10" {
		t.Fatalf("lookup after restart = %v, %v; want the saved address", addrs, err)
	}
}

func dnsReply(id uint16, ip [4]byte) []byte {
	b := make([]byte, 12)
	binary.BigEndian.PutUint16(b[0:], id)
	binary.BigEndian.PutUint16(b[2:], 0x8180)
	binary.BigEndian.PutUint16(b[4:], 1)
	binary.BigEndian.PutUint16(b[6:], 1)
	b = append(b, 3, 'a', 'p', 'i', 9, 'a', 'n', 't', 'h', 'r', 'o', 'p', 'i', 'c', 3, 'c', 'o', 'm', 0, 0, 1, 0, 1)
	b = append(b, 0xc0, 12, 0, 1, 0, 1, 0, 0, 0, 90, 0, 4)
	return append(b, ip[:]...)
}

func TestParseAResponse(t *testing.T) {
	addrs, ttl, err := parseAResponse(dnsReply(7, [4]byte{160, 79, 104, 10}), 7, "api.anthropic.com")
	if err != nil || len(addrs) != 1 || addrs[0] != "160.79.104.10" || ttl != 90*time.Second {
		t.Fatalf("got %v %v %v", addrs, ttl, err)
	}
	if _, _, err := parseAResponse(dnsReply(7, [4]byte{160, 79, 104, 10}), 8, "api.anthropic.com"); err == nil {
		t.Fatal("a reply to another query id must be refused")
	}
	if _, _, err := parseAResponse(dnsReply(7, [4]byte{127, 0, 0, 1}), 7, "api.anthropic.com"); err == nil {
		t.Fatal("loopback would loop into the gateway and must be refused")
	}
}
