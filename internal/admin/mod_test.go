package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/notice"
)

func modOf(t *testing.T, s *Server, query string) modResponse {
	t.Helper()
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, localRequest(http.MethodGet, "http://127.0.0.1/api/mod"+query, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("/api/mod status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got modResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	return got
}

// The mod sees alerts for its own session and for nobody, newer than since;
// another session's events stay with that session.
func TestModEndpointFiltersAlertsBySessionAndTime(t *testing.T) {
	s := newTestServer(t)
	alerts := captureAlerts(t)
	notice.Publish("failover", notice.Warn, "for everyone", "")
	notice.PublishFor("S1", "context", notice.Info, "for S1", "")
	notice.PublishFor("S2", "context", notice.Info, "for S2", "")
	alerts() // flush

	got := modOf(t, s, "?session=S1")
	if got.Route != "PRIMARY" || got.Overflow || got.Session != nil {
		t.Fatalf("idle gateway: %+v", got)
	}
	var titles []string
	for _, e := range got.Alerts {
		titles = append(titles, e.Title)
	}
	if len(titles) != 2 || titles[0] != "for everyone" || titles[1] != "for S1" {
		t.Fatalf("alerts for S1: %q", titles)
	}
	if later := modOf(t, s, "?session=S1&since="+strconv.FormatInt(time.Now().Add(time.Minute).Unix(), 10)); len(later.Alerts) != 0 {
		t.Fatalf("since in the future still returned %d alerts", len(later.Alerts))
	}
	// No alerts is an empty list, not null, so the mod can loop over it.
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, localRequest(http.MethodGet, "http://127.0.0.1/api/mod?since=9999999999", nil))
	if !json.Valid(rr.Body.Bytes()) || !strings.Contains(rr.Body.String(), `"alerts":[]`) {
		t.Fatalf("body=%s", rr.Body.String())
	}
}
