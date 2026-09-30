// Package hotspot joins a chosen Wi-Fi network (normally a phone's Personal
// Hotspot) when this Mac is offline, so a Claude Code session left running
// with the lid shut keeps reaching Anthropic after the Mac leaves home Wi-Fi.
//
// macOS does not do this by itself: it joins an iPhone hotspot through
// Instant Hotspot, which is driven from the Wi-Fi menu and Bluetooth handoff
// while someone is using the Mac. With the lid shut nobody is, and the Mac
// stays offline even with the hotspot broadcasting beside it.
//
// Being offline is judged by reachability, not by which network is joined:
// since macOS 14.4 `networksetup -getairportnetwork` reports "not associated"
// to processes without Location access even while connected, so an SSID
// check would read offline all the time. A TCP connect to two public
// resolvers, by address, answers the question that matters and is untouched
// by the /etc/hosts redirect transparent mode installs.
package hotspot

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
	"github.com/andrewbakercloudscale/claude-burst/internal/keychain"
)

// KeychainService holds the hotspot's password when one is stored. Optional:
// without it networksetup uses the password macOS already has for the network.
const KeychainService = "claude-burst-hotspot"

// Tunables, as variables for tests.
var (
	checkEvery   = 30 * time.Second
	offlineAfter = 2 // consecutive failed checks before acting
	retryEvery   = 2 * time.Minute
	probeAddrs   = []string{"1.1.1.1:443", "8.8.8.8:443"}
)

// WiFiDevice finds the Wi-Fi interface (en0 on every current Mac, but not
// guaranteed) from `networksetup -listallhardwareports`.
func WiFiDevice() string {
	out, err := exec.Command("networksetup", "-listallhardwareports").Output()
	if err != nil {
		return "en0"
	}
	return parseWiFiDevice(string(out))
}

func parseWiFiDevice(out string) string {
	sc := bufio.NewScanner(strings.NewReader(out))
	wifi := false
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(l, "Hardware Port:") {
			wifi = strings.Contains(l, "Wi-Fi") || strings.Contains(l, "AirPort")
		}
		if wifi && strings.HasPrefix(l, "Device:") {
			return strings.TrimSpace(strings.TrimPrefix(l, "Device:"))
		}
	}
	return "en0"
}

// KnownNetworks lists the networks this Mac has joined before, in its own
// preference order: the dropdown's choices. A hotspot has to be in this list
// (joined once by hand) for macOS to have its password.
func KnownNetworks() []string {
	out, err := exec.Command("networksetup", "-listpreferredwirelessnetworks", WiFiDevice()).Output()
	if err != nil {
		return nil
	}
	return parseKnownNetworks(string(out))
}

func parseKnownNetworks(out string) []string {
	var nets []string
	for i, l := range strings.Split(out, "\n") {
		if i == 0 || strings.TrimSpace(l) == "" {
			continue // "Preferred networks on en0:"
		}
		nets = append(nets, strings.TrimSpace(l))
	}
	return nets
}

// LidClosed reads AppleClamshellState from the IO registry.
func LidClosed() bool {
	out, err := exec.Command("ioreg", "-r", "-k", "AppleClamshellState", "-d", "4").Output()
	return err == nil && parseLidClosed(string(out))
}

func parseLidClosed(out string) bool {
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, `"AppleClamshellState" = Yes`) {
			return true
		}
	}
	return false
}

// Online reports whether either probe address accepts a TCP connection.
var Online = func() bool {
	for _, a := range probeAddrs {
		c, err := net.DialTimeout("tcp", a, 4*time.Second)
		if err == nil {
			c.Close()
			return true
		}
	}
	return false
}

// join is a variable so tests never touch the real Wi-Fi.
var join = func(ctx context.Context, ssid string) (string, error) {
	args := []string{"-setairportnetwork", WiFiDevice(), ssid}
	if pw, err := keychain.Load(KeychainService, "CLAUDE_BURST_HOTSPOT_PASSWORD"); err == nil {
		args = append(args, pw)
	}
	out, err := exec.CommandContext(ctx, "networksetup", args...).CombinedOutput()
	msg := strings.TrimSpace(string(out))
	// networksetup exits 0 on some failures and says so on stdout.
	if err == nil && (strings.Contains(msg, "Could not find network") || strings.Contains(msg, "Failed to join") || strings.Contains(msg, "Error")) {
		err = fmt.Errorf("%s", msg)
	}
	return msg, err
}

// Join joins ssid now and reports whether the Mac is online afterwards.
func Join(ssid string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	msg, err := join(ctx, ssid)
	if err != nil {
		logEvent("join %q failed: %v", ssid, err)
		return msg, err
	}
	for i := 0; i < 10 && !Online(); i++ {
		time.Sleep(2 * time.Second)
	}
	if !Online() {
		logEvent("joined %q but still offline", ssid)
		return msg, fmt.Errorf("joined %q but the internet is still unreachable", ssid)
	}
	logEvent("joined %q: online", ssid)
	return msg, nil
}

// --- event log -----------------------------------------------------------

var logMu sync.Mutex

func LogPath() string {
	dir, err := config.ConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "hotspot.log")
}

func logEvent(format string, a ...any) {
	p := LogPath()
	if p == "" {
		return
	}
	logMu.Lock()
	defer logMu.Unlock()
	line := time.Now().Format("2006-01-02 15:04:05") + " " + fmt.Sprintf(format, a...) + "\n"
	b, _ := os.ReadFile(p)
	if lines := strings.SplitAfter(string(b), "\n"); len(lines) > 400 {
		b = []byte(strings.Join(lines[len(lines)-300:], ""))
	}
	_ = os.WriteFile(p, append(b, line...), 0o600)
}

// RecentEvents returns up to n newest log lines, newest last.
func RecentEvents(n int) []string {
	b, err := os.ReadFile(LogPath())
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}

// --- watcher -------------------------------------------------------------

// decide is the watcher's whole policy, separate so it is testable: act only
// with a network chosen, the lid condition met, and offline for long enough,
// and not again until retryEvery has passed.
func decide(cfg config.HotspotConfig, lidClosed bool, offlineChecks int, sinceLastTry time.Duration) bool {
	if cfg.SSID == "" {
		return false
	}
	if cfg.When != config.HotspotAlways && !lidClosed {
		return false
	}
	return offlineChecks >= offlineAfter && sinceLastTry >= retryEvery
}

// Watch runs until ctx ends. Config is re-read every check, so the dashboard's
// changes apply without a restart.
func Watch(ctx context.Context) {
	offline := 0
	lastTry := time.Time{}
	t := time.NewTicker(checkEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		cfg, err := config.Load()
		if err != nil || cfg.Hotspot.SSID == "" {
			offline = 0
			continue
		}
		if Online() {
			if offline >= offlineAfter {
				logEvent("back online")
			}
			offline = 0
			continue
		}
		offline++
		lid := LidClosed()
		if offline == offlineAfter {
			logEvent("offline (lid %s)", map[bool]string{true: "shut", false: "open"}[lid])
		}
		if decide(cfg.Hotspot, lid, offline, time.Since(lastTry)) {
			lastTry = time.Now()
			logEvent("joining %q", cfg.Hotspot.SSID)
			_, _ = Join(cfg.Hotspot.SSID)
		}
	}
}
