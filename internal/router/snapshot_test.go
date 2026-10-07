package router

import (
	"bytes"
	"errors"
	"log"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// An unchanged network is logged in full once, then in short form; a
// change is logged in full again.
func TestNetworkSnapshotRepeatsShort(t *testing.T) {
	var buf bytes.Buffer
	s := &Server{logger: log.New(&buf, "", 0)}
	up := netProbe{ifaces: []string{"192.168.0.9"}, dnsOK: true}
	s.logSnapshot("r1", "anthropic", errors.New("timeout 1"), up)
	s.logSnapshot("r2", "anthropic", errors.New("timeout 2"), up)
	// More of the same inside the minute are counted, not written.
	s.logSnapshot("r2b", "anthropic", errors.New("timeout 2b"), up)
	s.logSnapshot("r2c", "anthropic", errors.New("timeout 2c"), up)
	s.logSnapshot("r3", "anthropic", errors.New("timeout 3"), netProbe{ifaces: []string{"172.20.10.2"}, dnsOK: true})
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("want 3 lines:\n%s", buf.String())
	}
	if !strings.Contains(lines[0], "local_ifaces=192.168.0.9") {
		t.Errorf("first is full: %s", lines[0])
	}
	if strings.Contains(lines[1], "local_ifaces") || !strings.Contains(lines[1], "timeout 2") || !strings.Contains(lines[1], "network unchanged since") {
		t.Errorf("repeat is short and keeps its error: %s", lines[1])
	}
	if !strings.Contains(lines[2], "local_ifaces=172.20.10.2") {
		t.Errorf("a changed network is full again: %s", lines[2])
	}
}

// Requests that are not model requests leave no start or done line while
// they succeed, and one line a minute says how many there were. One that
// fails is logged with its id, and a model request is logged as before.
func TestQuietRequestsAreCountedNotLogged(t *testing.T) {
	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)
	var q quietCounter
	t0 := time.Date(2026, 10, 7, 12, 0, 0, 0, time.Local)
	for i := 0; i < 50; i++ {
		q.add(logger, t0.Add(time.Duration(i)*time.Second))
	}
	if buf.Len() != 0 {
		t.Fatalf("inside the minute nothing is written:\n%s", buf.String())
	}
	q.add(logger, t0.Add(61*time.Second))
	if got := buf.String(); !strings.Contains(got, "quiet: 51 requests") || !strings.Contains(got, "since 12:00:00") {
		t.Fatalf("the minute's count: %q", got)
	}

	var l repeatLimiter
	if ok, _ := l.allow("network down", t0); !ok {
		t.Fatal("the first line is written")
	}
	for i := 1; i < 40; i++ {
		if ok, _ := l.allow("network down", t0.Add(time.Duration(i)*time.Second)); ok {
			t.Fatal("inside the minute the line is held back")
		}
	}
	if ok, _ := l.allow("another line", t0.Add(time.Second)); !ok {
		t.Fatal("another line has its own minute")
	}
	ok, skipped := l.allow("network down", t0.Add(61*time.Second))
	if !ok || skipped != 39 || andMore(skipped) != " [and 39 more like this since the last such line]" {
		t.Fatalf("after the minute: ok=%v skipped=%d", ok, skipped)
	}
}

func TestServeHTTPLogsModelRequestsAndFailuresOnly(t *testing.T) {
	up := newRecordingUpstream(t)
	s, logged := newTestServer(t, up.srv.URL, "")
	get := func(method, path string) {
		s.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(method, path, strings.NewReader("{}")))
	}
	get("GET", "/healthz")
	if got := logged.String(); strings.Contains(got, "/healthz") {
		t.Fatalf("a health check that answered is not logged:\n%s", got)
	}
	get("POST", "/v1/messages")
	if got := logged.String(); !strings.Contains(got, `start method="POST" path="/v1/messages"`) || !strings.Contains(got, `done method="POST" path="/v1/messages"`) {
		t.Fatalf("a model request keeps its start and done lines:\n%s", got)
	}
}
