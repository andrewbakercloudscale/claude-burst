package config

import (
	"testing"
	"time"
)

// The watcher's timing getters: 0 or negative (absent from config.json, or
// hand-edited) means the default; anything else is used as given. The
// dashboard enforces the Min/Max range on save (internal/admin/settings.go).
func TestHotspotTimingGetters(t *testing.T) {
	cases := []struct {
		name         string
		h            HotspotConfig
		check, retry time.Duration
		offline      int
		giveUp       time.Duration
	}{
		{"absent: defaults", HotspotConfig{},
			DefaultHotspotCheckSeconds * time.Second, DefaultHotspotRetrySeconds * time.Second,
			DefaultHotspotOfflineChecks, DefaultHotspotGiveUpMinutes * time.Minute},
		{"negative: defaults", HotspotConfig{CheckSeconds: -1, RetrySeconds: -5, OfflineChecks: -2, GiveUpMinutes: -30},
			DefaultHotspotCheckSeconds * time.Second, DefaultHotspotRetrySeconds * time.Second,
			DefaultHotspotOfflineChecks, DefaultHotspotGiveUpMinutes * time.Minute},
		{"set: as given", HotspotConfig{CheckSeconds: 10, RetrySeconds: 90, OfflineChecks: 3, GiveUpMinutes: 45},
			10 * time.Second, 90 * time.Second, 3, 45 * time.Minute},
		{"at the minimums", HotspotConfig{CheckSeconds: MinHotspotCheckSeconds, RetrySeconds: MinHotspotRetrySeconds, OfflineChecks: 1, GiveUpMinutes: 1},
			MinHotspotCheckSeconds * time.Second, MinHotspotRetrySeconds * time.Second, 1, time.Minute},
		{"at the maximums", HotspotConfig{CheckSeconds: MaxHotspotCheckSeconds, RetrySeconds: MaxHotspotRetrySeconds, OfflineChecks: MaxHotspotOfflineChecks, GiveUpMinutes: MaxHotspotGiveUpMinutes},
			MaxHotspotCheckSeconds * time.Second, MaxHotspotRetrySeconds * time.Second, MaxHotspotOfflineChecks, MaxHotspotGiveUpMinutes * time.Minute},
		{"one set, the rest default", HotspotConfig{RetrySeconds: 20},
			DefaultHotspotCheckSeconds * time.Second, 20 * time.Second,
			DefaultHotspotOfflineChecks, DefaultHotspotGiveUpMinutes * time.Minute},
	}
	for _, c := range cases {
		if got := c.h.CheckEvery(); got != c.check {
			t.Errorf("%s: CheckEvery %v, want %v", c.name, got, c.check)
		}
		if got := c.h.RetryEvery(); got != c.retry {
			t.Errorf("%s: RetryEvery %v, want %v", c.name, got, c.retry)
		}
		if got := c.h.OfflineAfter(); got != c.offline {
			t.Errorf("%s: OfflineAfter %d, want %d", c.name, got, c.offline)
		}
		if got := c.h.GiveUp(); got != c.giveUp {
			t.Errorf("%s: GiveUp %v, want %v", c.name, got, c.giveUp)
		}
	}
}

// The defaults must sit inside the range the dashboard accepts, or a fresh
// config would be one the dashboard refuses to save back.
func TestHotspotDefaultsAreInRange(t *testing.T) {
	if DefaultHotspotCheckSeconds < MinHotspotCheckSeconds || DefaultHotspotCheckSeconds > MaxHotspotCheckSeconds {
		t.Error("check default out of range")
	}
	if DefaultHotspotRetrySeconds < MinHotspotRetrySeconds || DefaultHotspotRetrySeconds > MaxHotspotRetrySeconds {
		t.Error("retry default out of range")
	}
	if DefaultHotspotOfflineChecks < 1 || DefaultHotspotOfflineChecks > MaxHotspotOfflineChecks {
		t.Error("offline-checks default out of range")
	}
	if DefaultHotspotGiveUpMinutes < 1 || DefaultHotspotGiveUpMinutes > MaxHotspotGiveUpMinutes {
		t.Error("give-up default out of range")
	}
}
