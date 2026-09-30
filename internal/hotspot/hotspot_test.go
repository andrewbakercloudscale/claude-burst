package hotspot

import (
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
		since time.Duration
		want  bool
	}{
		{"no network chosen", config.HotspotConfig{}, true, 5, long, false},
		{"lid open, lid-closed mode", lid, false, 5, long, false},
		{"lid shut, offline long enough", lid, true, offlineAfter, long, true},
		{"one failed check is not offline", lid, true, offlineAfter - 1, long, false},
		{"tried a moment ago", lid, true, 5, time.Second, false},
		{"always mode ignores the lid", always, false, offlineAfter, long, true},
		{"empty When means lid-closed", config.HotspotConfig{SSID: "Phone"}, false, 5, long, false},
	}
	for _, c := range cases {
		if got := decide(c.cfg, c.lid, c.off, c.since); got != c.want {
			t.Errorf("%s: got %v", c.name, got)
		}
	}
}
