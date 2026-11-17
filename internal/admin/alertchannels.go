package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/alerting"
	"github.com/andrewbakercloudscale/claude-burst/internal/config"
	"github.com/andrewbakercloudscale/claude-burst/internal/notice"
)

// Where alerts go: the toasts and macOS Notification Center, each off, at
// critical only or at all (config "alerts", internal/alerting). Set from the
// dashboard's Notifications section, read live by every publisher.

// Variables so tests never run the real helper, which would put a banner
// on the Mac running them or ask it for permission.
var (
	ncStatus = alerting.Status
	ncPost   = func(ctx context.Context, n *alerting.Notifier, ev notice.Event) (alerting.Result, error) {
		return n.Post(ctx, ev)
	}
)

type alertChannelsView struct {
	Toasts             string `json:"toasts"`
	NotificationCenter string `json:"notification_center"`
	// Permission is macOS's answer for Claude Burst's notifications:
	// authorized, denied, not-determined, provisional, or "" when the
	// helper could not be asked (HelperError says why).
	Permission  string `json:"permission"`
	HelperError string `json:"helper_error,omitempty"`
	// Running: this gateway posts to Notification Center.
	Running bool `json:"running"`
}

func (s *Server) handleAlertChannels(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.readOnly(s.alertChannelsGet)(w, r)
	default:
		s.mutating(s.alertChannelsPost)(w, r)
	}
}

func (s *Server) alertChannelsView(ctx context.Context, a config.AlertsConfig) alertChannelsView {
	v := alertChannelsView{Toasts: a.ToastLevel(), NotificationCenter: a.NotificationCenterLevel()}
	_, n := alerting.Default()
	v.Running = n != nil
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	res, err := ncStatus(ctx)
	if err != nil {
		v.HelperError = err.Error()
	} else {
		v.Permission = res.Status
	}
	return v
}

func (s *Server) alertChannelsGet(w http.ResponseWriter, r *http.Request) {
	cfg, err := config.Load()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, s.alertChannelsView(r.Context(), cfg.Alerts))
}

// alertChannelsPost saves {"toasts": level, "notification_center": level};
// a channel left out keeps its setting.
func (s *Server) alertChannelsPost(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Toasts             *string `json:"toasts"`
		NotificationCenter *string `json:"notification_center"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	for _, v := range []*string{req.Toasts, req.NotificationCenter} {
		if v != nil && (*v == "" || !config.ValidAlertLevel(*v)) {
			http.Error(w, "a level is off, critical or all", http.StatusBadRequest)
			return
		}
	}
	var saved config.AlertsConfig
	if err := config.Update(func(c *config.Config) error {
		if req.Toasts != nil {
			c.Alerts.Toasts.Level = *req.Toasts
		}
		if req.NotificationCenter != nil {
			c.Alerts.NotificationCenter.Level = *req.NotificationCenter
		}
		saved = c.Alerts
		return nil
	}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if live, _ := alerting.Default(); live != nil {
		live.Set(saved)
	}
	s.gateway.Logf("alert: channels set: toasts %s, Notification Center %s", saved.ToastLevel(), saved.NotificationCenterLevel())
	writeJSON(w, s.alertChannelsView(r.Context(), saved))
}

// handleAlertTestNC posts one test notification straight through the
// helper, whatever the level, and waits for the answer: the first one is
// what makes macOS ask, and it waits for the person to answer.
func (s *Server) handleAlertTestNC(w http.ResponseWriter, r *http.Request) {
	_, n := alerting.Default()
	if n == nil {
		http.Error(w, "Notification Center is not running in this gateway", http.StatusServiceUnavailable)
		return
	}
	now := time.Now()
	ev := notice.Event{ID: fmt.Sprintf("test-%d", now.UnixNano()), Kind: "test", Severity: notice.Info,
		Title: "Test notification " + now.Format("15:04:05"), Detail: "If you can see this, Burst's alerts reach Notification Center.", At: now}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	res, err := ncPost(ctx, n, ev)
	if errors.Is(err, alerting.ErrNoHelper) {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	notice.Record("test", notice.Info, "Test notification to Notification Center", fmt.Sprintf("status %s, posted %v %s", res.Status, res.Posted, res.Error))
	writeJSON(w, res)
}
