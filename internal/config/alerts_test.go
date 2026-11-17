package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A config.json from before the "alerts" block keeps today's behaviour,
// every alert as a toast, and gets Notification Center at critical only.
func TestAlertsDefaultsForAnOlderConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".config", "claude-burst")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	old := `{"listen": "127.0.0.1:7777", "admin_listen": "127.0.0.1:7788", "notify": {"failover": true}, "alert_daily_spend_usd": 5}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Alerts.ToastLevel(); got != AlertLevelAll {
		t.Errorf("toasts = %q, want all", got)
	}
	if got := cfg.Alerts.NotificationCenterLevel(); got != AlertLevelCritical {
		t.Errorf("Notification Center = %q, want critical", got)
	}
	if !cfg.Notify.Failover || cfg.AlertDailySpendUSD != 5 {
		t.Errorf("the older settings did not load: %+v %v", cfg.Notify, cfg.AlertDailySpendUSD)
	}
	if keys, _ := UnknownKeys(); len(keys) != 0 {
		t.Errorf("unknown keys %v", keys)
	}
	if d := Default().Alerts; d.ToastLevel() != AlertLevelAll || d.NotificationCenterLevel() != AlertLevelCritical {
		t.Errorf("Default() alerts = %+v", d)
	}
}

func TestAlertsSaveAndLoad(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := Update(func(c *Config) error {
		c.Alerts.Toasts.Level = AlertLevelOff
		c.Alerts.NotificationCenter.Level = AlertLevelAll
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Alerts.ToastLevel() != AlertLevelOff || cfg.Alerts.NotificationCenterLevel() != AlertLevelAll {
		t.Fatalf("alerts = %+v", cfg.Alerts)
	}
	p, _ := ConfigPath()
	b, _ := os.ReadFile(p)
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"notification_center"`) {
		t.Errorf("config.json lacks the setting:\n%s", b)
	}
	if keys, _ := UnknownKeys(); len(keys) != 0 {
		t.Errorf("unknown keys %v", keys)
	}
}

// A level a hand edit got wrong falls back to the channel's default, never
// to off: a typo must not be what silences the alerts.
func TestAlertsBadLevelIsTheDefault(t *testing.T) {
	a := AlertsConfig{Toasts: AlertChannel{Level: "loud"}, NotificationCenter: AlertChannel{Level: "Critical"}}
	if a.ToastLevel() != AlertLevelAll || a.NotificationCenterLevel() != AlertLevelCritical {
		t.Fatalf("levels = %q, %q", a.ToastLevel(), a.NotificationCenterLevel())
	}
	for v, want := range map[string]bool{"": true, "off": true, "critical": true, "all": true, "loud": false} {
		if ValidAlertLevel(v) != want {
			t.Errorf("ValidAlertLevel(%q) = %v", v, !want)
		}
	}
}
