package router

import (
	"bufio"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A sleep, or a move to another network, is why the held streams are ended;
// going offline, coming back where the Mac was, and an ordinary look are not.
func TestPathWatchSeesAWakeAndAChangeOfNetwork(t *testing.T) {
	t0 := time.Date(2026, 10, 6, 18, 0, 0, 0, time.Local)
	var p pathWatch
	steps := []struct {
		after time.Duration
		addr  string
		want  string
	}{
		{0, "10.0.0.5", ""},               // the first look has nothing to compare
		{5 * time.Second, "10.0.0.5", ""}, // an ordinary look
		{5 * time.Second, "", ""},         // offline: nothing to open a stream on
		{5 * time.Second, "", ""},         //
		{5 * time.Second, "10.0.0.5", ""}, // back where it was
		{5 * time.Second, "", ""},         // offline again, lid shut
		{5 * time.Second, "172.20.10.3", "the network changed (10.0.0.5 to 172.20.10.3)"}, // the hotspot
		{5 * time.Second, "172.20.10.3", ""},
		{24 * time.Minute, "172.20.10.3", "the Mac woke after 24m0s"},
		{5 * time.Second, "172.20.10.3", ""},
	}
	now := t0
	for i, st := range steps {
		now = now.Add(st.after)
		if got := p.look(now, st.addr); got != st.want {
			t.Errorf("step %d (+%s, %q): got %q, want %q", i, st.after, st.addr, got, st.want)
		}
	}
}

// Remote Control's event stream is ended by the gateway when the Mac wakes
// or changes network, so Claude Code opens it again: the client sees the
// stream finish, where it would have waited on a dead connection. A request
// that is not a held stream is left alone.
func TestHeldStreamsAreEndedSoClaudeCodeOpensThemAgain(t *testing.T) {
	opened := make(chan string, 4)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/events/stream") {
			fmt.Fprint(w, `{"ok":true}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: hello\ndata: {}\n\n")
		w.(http.Flusher).Flush()
		opened <- r.URL.RawQuery
		<-r.Context().Done() // silent from here on, as a dead connection is
	}))
	defer upstream.Close()
	s, logBuf := newTestServer(t, upstream.URL, "")
	gw := httptest.NewServer(s)
	defer gw.Close()

	if n := s.DropHeldStreams("nothing is open"); n != 0 {
		t.Fatalf("ended %d streams with none open", n)
	}
	resp, err := http.Get(gw.URL + "/v1/code/sessions/cse_1/worker/events/stream?from_sequence_num=7")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	select {
	case q := <-opened:
		if q != "from_sequence_num=7" {
			t.Fatalf("upstream saw query %q", q)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the stream never reached the upstream")
	}
	ended := make(chan string, 1)
	go func() {
		var got strings.Builder
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			got.WriteString(sc.Text() + "\n")
		}
		ended <- got.String()
	}()
	select {
	case got := <-ended:
		t.Fatalf("the stream ended by itself: %q", got)
	case <-time.After(200 * time.Millisecond):
	}
	// An ordinary control-plane request while the stream is held is not one.
	if r2, err := http.Post(gw.URL+"/v1/code/sessions/cse_1/worker/heartbeat", "application/json", strings.NewReader("{}")); err != nil || r2.StatusCode != 200 {
		t.Fatalf("heartbeat: %v %v", r2, err)
	}
	if n := s.DropHeldStreams("the Mac woke after 24m0s"); n != 1 {
		t.Fatalf("ended %d streams, want the one held", n)
	}
	select {
	case got := <-ended:
		if !strings.Contains(got, "event: hello") {
			t.Errorf("what arrived before the end was lost: %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the client is still waiting on the stream after it was ended")
	}
	if !strings.Contains(logBuf.String(), "remote control: the Mac woke after 24m0s, ended 1 held stream(s)") {
		t.Errorf("log does not say why: %s", logBuf.String())
	}
	// Ended streams are forgotten, and the gateway serves the next one.
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.held.mu.Lock()
		n := len(s.held.cancels)
		s.held.mu.Unlock()
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d streams still held after they ended", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
	again, err := http.Get(gw.URL + "/v1/code/sessions/cse_1/worker/events/stream?from_sequence_num=8")
	if err != nil {
		t.Fatal(err)
	}
	defer again.Body.Close()
	select {
	case q := <-opened:
		if q != "from_sequence_num=8" {
			t.Errorf("the reopened stream asked for %q", q)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the stream could not be opened again")
	}
}
