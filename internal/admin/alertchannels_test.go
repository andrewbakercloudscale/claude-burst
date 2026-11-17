package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/andrewbakercloudscale/claude-burst/internal/alerting"
	"github.com/andrewbakercloudscale/claude-burst/internal/config"
	"github.com/andrewbakercloudscale/claude-burst/internal/notice"
)

// stubNC keeps the real helper out: no banner, no permission prompt on the
// Mac running the tests. It returns the alerts "posted".
func stubNC(t *testing.T, status string) *[]notice.Event {
	t.Helper()
	posted := &[]notice.Event{}
	oldS, oldP := ncStatus, ncPost
	ncStatus = func(context.Context) (alerting.Result, error) { return alerting.Result{Status: status}, nil }
	ncPost = func(_ context.Context, _ *alerting.Notifier, ev notice.Event) (alerting.Result, error) {
		*posted = append(*posted, ev)
		return alerting.Result{Status: status, Posted: status == "authorized"}, nil
	}
	t.Cleanup(func() { ncStatus, ncPost = oldS, oldP })
	return posted
}

func alertChannelsDo(t *testing.T, s *Server, method, path, body string) (*httptest.ResponseRecorder, alertChannelsView) {
	t.Helper()
	r := localRequest(method, "http://127.0.0.1:7788"+path, strings.NewReader(body))
	r.Host = "127.0.0.1:7788"
	if method == http.MethodPost {
		r.Header.Set("X-Claude-Burst-Admin", "1")
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	var v alertChannelsView
	_ = json.Unmarshal(w.Body.Bytes(), &v)
	return w, v
}

func TestAlertChannelsShowsDefaultsAndSaves(t *testing.T) {
	stubNC(t, "not-determined")
	s := newTestServer(t)
	live := &alerting.Live{Load: config.Load}
	alerting.SetDefault(live, alerting.NewNotifier("", nil))
	t.Cleanup(func() { alerting.SetDefault(nil, nil) })

	w, v := alertChannelsDo(t, s, http.MethodGet, "/api/alert-channels", "")
	if w.Code != http.StatusOK || v.Toasts != "all" || v.NotificationCenter != "critical" || v.Permission != "not-determined" || !v.Running {
		t.Fatalf("GET = %d %+v", w.Code, v)
	}
	w, v = alertChannelsDo(t, s, http.MethodPost, "/api/alert-channels", `{"notification_center":"all"}`)
	if w.Code != http.StatusOK || v.Toasts != "all" || v.NotificationCenter != "all" {
		t.Fatalf("POST = %d %s", w.Code, w.Body)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Alerts.NotificationCenter.Level != "all" || cfg.Alerts.Toasts.Level != "" {
		t.Fatalf("saved %+v", cfg.Alerts)
	}
	// The running publishers see it at once, not after their reload.
	if live.Get().NotificationCenterLevel() != "all" {
		t.Fatal("the live settings were not updated")
	}
	for _, bad := range []string{`{"toasts":"loud"}`, `{"toasts":""}`, `not json`} {
		if w, _ := alertChannelsDo(t, s, http.MethodPost, "/api/alert-channels", bad); w.Code != http.StatusBadRequest {
			t.Errorf("POST %s = %d, want 400", bad, w.Code)
		}
	}
}

func TestAlertTestNotificationGoesThroughTheHelper(t *testing.T) {
	posted := stubNC(t, "authorized")
	s := newTestServer(t)
	if w, _ := alertChannelsDo(t, s, http.MethodPost, "/api/alert-test-nc", ""); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("without a notifier = %d, want 503", w.Code)
	}
	alerting.SetDefault(&alerting.Live{Load: config.Load}, alerting.NewNotifier("", nil))
	t.Cleanup(func() { alerting.SetDefault(nil, nil) })
	w, _ := alertChannelsDo(t, s, http.MethodPost, "/api/alert-test-nc", "")
	var r alerting.Result
	_ = json.Unmarshal(w.Body.Bytes(), &r)
	if w.Code != http.StatusOK || !r.Posted || len(*posted) != 1 || (*posted)[0].Kind != "test" {
		t.Fatalf("test = %d %s, posted %+v", w.Code, w.Body, *posted)
	}
}
