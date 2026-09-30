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
	checkEvery   = 10 * time.Second // so a 30s gap is 30 to 40s, not up to 60
	offlineAfter = 2                // consecutive failed checks before acting
	retryEvery   = 30 * time.Second // gap after one background attempt ends
	maxTries     = 30               // background attempts per offline spell
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

// Explain turns networksetup's failures into what to do about them.
func Explain(msg string) string {
	switch {
	case strings.Contains(msg, "Could not find network"):
		return "the Mac cannot see it. An iPhone only broadcasts its hotspot while Settings > Personal Hotspot is open on the phone, or while something is connected to it: open that screen, turn on Maximise Compatibility, and try again."
	case strings.Contains(msg, "-3900"), strings.Contains(msg, "Failed to join"):
		return "the Mac saw it but could not join. Usually the password: macOS keeps it where a background process cannot read it, so type it into the Password field and Save, then try again."
	}
	return ""
}

// restore gives Wi-Fi back if a failed join left the Mac offline: leaving
// the old network is the first thing a join does. Turning Wi-Fi off and on
// makes macOS rejoin its best known network, as it would at wake.
var restore = func() string {
	for i := 0; i < 5; i++ {
		if Online() {
			return "macOS rejoined a known network by itself"
		}
		time.Sleep(2 * time.Second)
	}
	dev := WiFiDevice()
	_ = exec.Command("networksetup", "-setairportpower", dev, "off").Run()
	time.Sleep(time.Second)
	_ = exec.Command("networksetup", "-setairportpower", dev, "on").Run()
	logEvent("still offline after the failed join: turned Wi-Fi off and on so macOS rejoins a known network")
	for i := 0; i < 10; i++ {
		if Online() {
			return "turned Wi-Fi off and on; macOS rejoined a known network"
		}
		time.Sleep(2 * time.Second)
	}
	return "turned Wi-Fi off and on, but still offline: pick a network from the Wi-Fi menu"
}

// Step is one stage of a join, for the dashboard's test checklist.
type Step struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// joinAttempts is how many times a join is tried before giving up: an
// iPhone hotspot often refuses the first try while it wakes its radio.
const joinAttempts = 3

// attemptTimeout bounds one networksetup join.
const attemptTimeout = 30 * time.Second

// betweenAttempts is the pause before trying again; a variable for tests.
var betweenAttempts = 5 * time.Second

// JoinSteps joins ssid now, trying up to joinAttempts times, and reports
// each stage: the join itself, whether the internet is reachable through it,
// and, after every attempt failed, getting the Mac back onto a network.
func JoinSteps(ssid string) []Step { return joinSteps(ssid, joinAttempts, true) }

// joinSteps is JoinSteps with the number of attempts, and whether a final
// failure turns Wi-Fi off and on to get back onto a known network.
func joinSteps(ssid string, attempts int, restoreOnFail bool) []Step {
	var steps []Step
	start := time.Now()
	var msg string
	var err error
	attempt := 1
	for ; ; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), attemptTimeout)
		msg, err = join(ctx, ssid)
		cancel()
		if err == nil {
			break
		}
		if attempts > 1 {
			logEvent("join %q failed (attempt %d of %d): %v", ssid, attempt, attempts, err)
		}
		if attempt >= attempts {
			break
		}
		time.Sleep(betweenAttempts)
	}
	if err != nil {
		detail := Explain(err.Error() + " " + msg)
		if detail == "" {
			detail = err.Error()
		}
		if attempts > 1 {
			detail = fmt.Sprintf("all %d attempts failed. %s", attempts, detail)
		}
		steps = append(steps, Step{Name: "Join " + ssid, Detail: detail})
		if !restoreOnFail {
			return steps
		}
		r := restore()
		steps = append(steps, Step{Name: "Back on a network", OK: Online(), Detail: r})
		return steps
	}
	tries := "first attempt"
	if attempt > 1 {
		tries = fmt.Sprintf("attempt %d of %d", attempt, attempts)
	}
	steps = append(steps, Step{Name: "Join " + ssid, OK: true, Detail: fmt.Sprintf("joined on the %s, in %s", tries, time.Since(start).Round(time.Second))})
	for i := 0; i < 10 && !Online(); i++ {
		time.Sleep(2 * time.Second)
	}
	if !Online() {
		logEvent("joined %q but still offline", ssid)
		return append(steps, Step{Name: "Internet through it", Detail: "joined, but nothing is reachable: check the phone has mobile data and Personal Hotspot is allowed on your plan"})
	}
	logEvent("joined %q: online", ssid)
	return append(steps, Step{Name: "Internet through it", OK: true, Detail: "reached 1.1.1.1 / 8.8.8.8"})
}

// Join is one attempt as one error, for the background watcher, which does
// its own retrying. restoreOnFail is for its last attempt.
func Join(ssid string, restoreOnFail bool) (string, error) {
	steps := joinSteps(ssid, 1, restoreOnFail)
	for _, st := range steps {
		if !st.OK {
			return "", fmt.Errorf("%s: %s", st.Name, st.Detail)
		}
	}
	return "", nil
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
func decide(cfg config.HotspotConfig, lidClosed bool, offlineChecks, tries int, sinceLastTry time.Duration) bool {
	if tries >= maxTries {
		return false
	}
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
	offline, tries := 0, 0
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
			offline, tries = 0, 0
			continue
		}
		if Online() {
			if offline >= offlineAfter {
				logEvent("back online")
			}
			offline, tries = 0, 0
			continue
		}
		offline++
		lid := LidClosed()
		if offline == offlineAfter {
			logEvent("offline (lid %s)", map[bool]string{true: "shut", false: "open"}[lid])
		}
		if decide(cfg.Hotspot, lid, offline, tries, time.Since(lastTry)) {
			tries++
			last := tries == maxTries
			logEvent("joining %q (attempt %d of %d)", cfg.Hotspot.SSID, tries, maxTries)
			if _, err := Join(cfg.Hotspot.SSID, last); err != nil && last {
				logEvent("gave up after %d attempts; trying again after the Mac has been back online", maxTries)
			}
			// The gap runs from the end of the attempt, not its start.
			lastTry = time.Now()
		}
	}
}
