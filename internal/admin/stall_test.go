package admin

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// /api/mod carries the gateway's last freeze for ten minutes, so the mod can
// say Burst was paused rather than gone.
func TestModCarriesARecentFreeze(t *testing.T) {
	s := newTestServer(t)
	t.Cleanup(func() { lastStall.Store(nil) })
	lastStall.Store(nil)
	if m := modOf(t, s, "?session=S1"); m.Stall != nil {
		t.Fatalf("no freeze yet, got %+v", m.Stall)
	}
	NoteStall(Stall{EndedMS: time.Now().UnixMilli(), Seconds: 103, SwapUsedMB: 8696, SwapOfMB: 10240, Pressure: "warn"})
	if m := modOf(t, s, "?session=S1"); m.Stall == nil || m.Stall.Seconds != 103 || m.Stall.Pressure != "warn" {
		t.Fatalf("freeze not reported: %+v", m.Stall)
	}
	NoteStall(Stall{EndedMS: time.Now().Add(-11 * time.Minute).UnixMilli(), Seconds: 30})
	if m := modOf(t, s, "?session=S1"); m.Stall != nil {
		t.Fatalf("an 11 minute old freeze is still reported: %+v", m.Stall)
	}
}

// A session's "not answering" toast is logged when the dashboard is back,
// beside the gateway's own freeze, so the two can be matched.
func TestModDownIsLogged(t *testing.T) {
	s := newTestServer(t)
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(old); lastStall.Store(nil) })
	fired := time.Date(2026, 11, 25, 17, 49, 5, 0, time.Local)
	NoteStall(Stall{EndedMS: time.Now().UnixMilli(), Seconds: 16, SwapUsedMB: 7645, SwapOfMB: 9216, Pressure: "warn"})
	body := `{"session":"S1","shown":"Burst paused (Mac short of memory)","fired_ms":` + unixMS(fired) + `,"first_ms":` + unixMS(fired.Add(-31*time.Second)) +
		`,"misses":6,"error":"timed out","back_ms":` + unixMS(fired.Add(9*time.Second)) + `,"freezing":true}`
	rr := httptest.NewRecorder()
	r := localRequest(http.MethodPost, "http://127.0.0.1/api/mod/down", strings.NewReader(body))
	r.Header.Set(mutationHeader, "1")
	s.Handler().ServeHTTP(rr, r)
	if rr.Code != 200 {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	got := buf.String()
	for _, want := range []string{
		`WARNING mod: session S1 showed "Burst paused (Mac short of memory)" at 17:49:05 after 6 checks in a row went unanswered from 17:48:34`,
		"timed out: the gateway was there but did not answer",
		"answered again at 17:49:14, 40s after the first miss",
		"the gateway's last freeze: 16s ending",
		"swap 7645MB of 9216MB, memory pressure warn",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("log %q lacks %q", got, want)
		}
	}
}

func TestModDownNeedsWhenItFired(t *testing.T) {
	s := newTestServer(t)
	rr := httptest.NewRecorder()
	r := localRequest(http.MethodPost, "http://127.0.0.1/api/mod/down", strings.NewReader(`{"session":"S1"}`))
	r.Header.Set(mutationHeader, "1")
	s.Handler().ServeHTTP(rr, r)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status %d", rr.Code)
	}
}

func unixMS(t time.Time) string { return strconv.FormatInt(t.UnixMilli(), 10) }
