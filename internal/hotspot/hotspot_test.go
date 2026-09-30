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
		tries int
		since time.Duration
		want  bool
	}{
		{"no network chosen", config.HotspotConfig{}, true, 5, 0, long, false},
		{"lid open, lid-closed mode", lid, false, 5, 0, long, false},
		{"lid shut, offline long enough", lid, true, offlineAfter, 0, long, true},
		{"one failed check is not offline", lid, true, offlineAfter - 1, 0, long, false},
		{"tried a moment ago", lid, true, 5, 0, time.Second, false},
		{"30 seconds after the last attempt", lid, true, 5, 1, 30 * time.Second, true},
		{"29 seconds after the last attempt", lid, true, 5, 1, 29 * time.Second, false},
		{"attempt 30 is allowed", lid, true, 5, maxTries - 1, long, true},
		{"no attempt 31", lid, true, 5, maxTries, long, false},
		{"always mode ignores the lid", always, false, offlineAfter, 0, long, true},
		{"empty When means lid-closed", config.HotspotConfig{SSID: "Phone"}, false, 5, 0, long, false},
	}
	for _, c := range cases {
		if got := decide(c.cfg, c.lid, c.off, c.tries, c.since); got != c.want {
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

	calls, restored := stubJoin(t, fail, fail, nil)
	steps := JoinSteps("Phone")
	if *calls != 3 || !steps[0].OK || !strings.Contains(steps[0].Detail, "attempt 3 of 3") || *restored {
		t.Fatalf("third attempt should succeed: calls=%d restored=%v steps=%+v", *calls, *restored, steps)
	}

	calls, restored = stubJoin(t, fail)
	steps = JoinSteps("Phone")
	if *calls != 3 || steps[0].OK || !strings.Contains(steps[0].Detail, "all 3 attempts failed") || !*restored {
		t.Fatalf("three failures, then restore: calls=%d restored=%v steps=%+v", *calls, *restored, steps)
	}

	calls, _ = stubJoin(t, nil)
	steps = JoinSteps("Phone")
	if *calls != 1 || !strings.Contains(steps[0].Detail, "first attempt") {
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
