package hotspot

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
)

func TestParsers(t *testing.T) {
	ports := "Hardware Port: Ethernet\nDevice: en5\n\nHardware Port: Wi-Fi\nDevice: en1\nEthernet Address: x\n"
	if d := parseWiFiDevice(ports); d != "en1" {
		t.Errorf("wifi device %q", d)
	}
	nets := parseKnownNetworks("Preferred networks on en0:\n\tHome\n\tMy Phone 15\n\n")
	if len(nets) != 2 || nets[1] != "My Phone 15" {
		t.Errorf("networks %q", nets)
	}
	if !parseLidClosed(`  |   "AppleClamshellState" = Yes`) || parseLidClosed(`  |   "AppleClamshellState" = No`) {
		t.Error("lid state misread")
	}
}

func TestDecide(t *testing.T) {
	lid := config.HotspotConfig{SSID: "Phone", When: config.HotspotLidClosed}
	always := config.HotspotConfig{SSID: "Phone", When: config.HotspotAlways}
	long := time.Hour
	cases := []struct {
		name  string
		cfg   config.HotspotConfig
		lid   bool
		off   int
		first time.Duration
		since time.Duration
		want  bool
	}{
		{"no network chosen", config.HotspotConfig{}, true, 5, 0, long, false},
		{"lid open, lid-closed mode", lid, false, 5, 0, long, false},
		{"lid shut, offline long enough", lid, true, 2, 0, long, true},
		{"one failed check is not offline", lid, true, 1, 0, long, false},
		{"tried a moment ago", lid, true, 5, 0, time.Second, false},
		{"a minute after the last attempt", lid, true, 5, time.Minute, time.Minute, true},
		{"59 seconds after the last attempt", lid, true, 5, time.Minute, 59 * time.Second, false},
		{"29 minutes in, the default keeps trying", lid, true, 5, 29 * time.Minute, long, true},
		{"30 minutes in, the default gives up", lid, true, 5, 30 * time.Minute, long, false},
		{"a shorter retry gap", config.HotspotConfig{SSID: "Phone", RetrySeconds: 20}, true, 5, time.Minute, 20 * time.Second, true},
		{"three checks asked for, two seen", config.HotspotConfig{SSID: "Phone", OfflineChecks: 3}, true, 2, 0, long, false},
		{"a longer give-up time keeps going", config.HotspotConfig{SSID: "Phone", GiveUpMinutes: 60}, true, 5, 45 * time.Minute, long, true},
		{"always mode ignores the lid", always, false, 2, 0, long, true},
		{"empty When means lid-closed", config.HotspotConfig{SSID: "Phone"}, false, 5, 0, long, false},
	}
	for _, c := range cases {
		if got := decide(c.cfg, c.lid, c.off, c.first, c.since); got != c.want {
			t.Errorf("%s: got %v", c.name, got)
		}
	}
}

func TestExplain(t *testing.T) {
	if !strings.Contains(Explain("Could not find network Phone."), "Personal Hotspot") {
		t.Error("not found: no advice")
	}
	if !strings.Contains(Explain("Failed to join network Phone. Error: -3900"), "password") {
		t.Error("-3900: no advice")
	}
	if Explain("something else") != "" {
		t.Error("unknown failure should not guess")
	}
}

// stubJoin replaces the real join, Online and restore for one test.
func stubJoin(t *testing.T, results ...error) (calls *int, restored *bool) {
	t.Helper()
	oldJoin, oldOnline, oldRestore, oldWait := join, Online, restore, betweenAttempts
	t.Cleanup(func() { join, Online, restore, betweenAttempts = oldJoin, oldOnline, oldRestore, oldWait })
	t.Setenv("HOME", t.TempDir()) // logEvent writes under HOME
	calls, restored = new(int), new(bool)
	betweenAttempts = 0
	join = func(context.Context, string) (string, error) {
		err := results[min(*calls, len(results)-1)]
		*calls++
		return "", err
	}
	Online = func() bool { return true }
	restore = func() string { *restored = true; return "back" }
	return calls, restored
}

func TestJoinTriesThreeTimes(t *testing.T) {
	fail := errors.New("Failed to join network Phone. Error: -3900")

	var seen []Step
	calls, restored := stubJoin(t, fail, fail, nil)
	steps := JoinSteps("Phone", func(st Step) { seen = append(seen, st) })
	if *calls != 3 || *restored || len(steps) != 4 || steps[0].OK || steps[1].OK || !steps[2].OK || steps[2].Name != "Attempt 3 of 3" || !steps[3].OK {
		t.Fatalf("two failures, success on the third, then internet: calls=%d restored=%v steps=%+v", *calls, *restored, steps)
	}
	if !strings.Contains(steps[0].Detail, "Trying again") || !strings.Contains(steps[0].Detail, "password") {
		t.Fatalf("a failed attempt says why and that another follows: %q", steps[0].Detail)
	}
	// Each attempt is reported as it starts and as it ends.
	if len(seen) != 8 || !seen[0].Pending || seen[1].Pending || seen[1].Name != "Attempt 1 of 3" {
		t.Fatalf("progress: %+v", seen)
	}

	calls, restored = stubJoin(t, fail)
	steps = JoinSteps("Phone", nil)
	if *calls != 3 || len(steps) != 4 || steps[2].OK || strings.Contains(steps[2].Detail, "Trying again") || !*restored || steps[3].Name != "Back on a network" {
		t.Fatalf("three failures, then restore: calls=%d restored=%v steps=%+v", *calls, *restored, steps)
	}

	calls, _ = stubJoin(t, nil)
	steps = JoinSteps("Phone", nil)
	if *calls != 1 || len(steps) != 2 || !steps[0].OK {
		t.Fatalf("a first-time join must not retry: calls=%d steps=%+v", *calls, steps)
	}
}

func TestBackgroundAttemptIsOneTryAndRestoresOnlyWhenAsked(t *testing.T) {
	fail := errors.New("Could not find network Phone.")
	calls, restored := stubJoin(t, fail)
	if _, err := Join("Phone", false); err == nil || *calls != 1 || *restored {
		t.Fatalf("one try, no Wi-Fi toggle: calls=%d restored=%v", *calls, *restored)
	}
	if _, err := Join("Phone", true); err == nil || !*restored {
		t.Fatal("the last attempt restores Wi-Fi")
	}
}

func TestLidOpenedWithNoNetworkTriesAtOnce(t *testing.T) {
	cfg := config.HotspotConfig{SSID: "Phone", When: config.HotspotLidClosed}
	if !lidJustOpened(cfg, true, false, 1) {
		t.Error("lid opened, offline: try now")
	}
	if lidJustOpened(cfg, true, false, 0) {
		t.Error("lid opened, online: nothing to do")
	}
	if lidJustOpened(cfg, false, false, 3) || lidJustOpened(cfg, true, true, 3) {
		t.Error("only the moment the lid opens")
	}
	if lidJustOpened(config.HotspotConfig{}, true, false, 3) {
		t.Error("no network chosen")
	}
}
