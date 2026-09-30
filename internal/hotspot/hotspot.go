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

// probeAddrs are dialled to tell online from offline.
var (
	probeAddrs = []string{"1.1.1.1:443", "8.8.8.8:443"}
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
		return "the Mac saw it but could not join. Usually a wrong password: press Show beside the Password field to check it, correct it and Save, then try again."
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
// Pending marks a stage that has started and not finished; the stage's
// result follows under the same name.
type Step struct {
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	Pending bool   `json:"pending,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

// joinAttempts is how many times the dashboard's test tries a join before
// giving up: an iPhone hotspot often refuses the first try while it wakes
// its radio.
const joinAttempts = 3

// attemptTimeout bounds one networksetup join.
const attemptTimeout = 30 * time.Second

// betweenAttempts is the pause before trying again; a variable for tests.
var betweenAttempts = 5 * time.Second

// JoinSteps is the dashboard's test: up to joinAttempts tries, each one
// reported to progress as it starts and as it ends, then whether the
// internet is reachable through it, or, when every attempt failed, getting
// the Mac back onto a network. progress may be nil.
func JoinSteps(ssid string, progress func(Step)) []Step {
	return joinSteps(ssid, joinAttempts, true, progress)
}

// joinSteps is JoinSteps with the number of attempts, and whether a final
// failure turns Wi-Fi off and on to get back onto a known network.
func joinSteps(ssid string, attempts int, restoreOnFail bool, progress func(Step)) []Step {
	var steps []Step
	emit := func(st Step) {
		if !st.Pending {
			steps = append(steps, st)
		}
		if progress != nil {
			progress(st)
		}
	}
	name := func(i int) string {
		if attempts == 1 {
			return "Join " + ssid
		}
		return fmt.Sprintf("Attempt %d of %d", i, attempts)
	}
	var err error
	for i := 1; i <= attempts; i++ {
		emit(Step{Name: name(i), Pending: true, Detail: "joining " + ssid})
		start := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), attemptTimeout)
		var msg string
		msg, err = join(ctx, ssid)
		cancel()
		if err == nil {
			emit(Step{Name: name(i), OK: true, Detail: fmt.Sprintf("joined %s in %s", ssid, time.Since(start).Round(time.Second))})
			break
		}
		logEvent("join %q failed (attempt %d of %d): %v", ssid, i, attempts, err)
		detail := Explain(err.Error() + " " + msg)
		if detail == "" {
			detail = strings.Join(strings.Fields(err.Error()), " ")
		}
		if i < attempts {
			detail = fmt.Sprintf("%s Trying again in %s.", detail, betweenAttempts)
		}
		emit(Step{Name: name(i), Detail: detail})
		if i < attempts {
			time.Sleep(betweenAttempts)
		}
	}
	if err != nil {
		if restoreOnFail {
			emit(Step{Name: "Back on a network", Pending: true, Detail: "waiting for macOS to rejoin a known network"})
			r := restore()
			emit(Step{Name: "Back on a network", OK: Online(), Detail: r})
		}
		return steps
	}
	emit(Step{Name: "Internet through it", Pending: true, Detail: "checking 1.1.1.1 / 8.8.8.8"})
	for i := 0; i < 10 && !Online(); i++ {
		time.Sleep(2 * time.Second)
	}
	if !Online() {
		logEvent("joined %q but still offline", ssid)
		emit(Step{Name: "Internet through it", Detail: "joined, but nothing is reachable: check the phone has mobile data and Personal Hotspot is allowed on your plan"})
		return steps
	}
	logEvent("joined %q: online", ssid)
	emit(Step{Name: "Internet through it", OK: true, Detail: "reached 1.1.1.1 / 8.8.8.8"})
	return steps
}

// Join is one attempt as one error, for the background watcher, which does
// its own retrying. restoreOnFail is for its last attempt.
func Join(ssid string, restoreOnFail bool) (string, error) {
	steps := joinSteps(ssid, 1, restoreOnFail, nil)
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
// not again until the retry gap has passed, and not once the spell's first
// attempt is further back than the give-up time (sinceFirstTry is 0 before
// the first attempt).
func decide(cfg config.HotspotConfig, lidClosed bool, offlineChecks int, sinceFirstTry, sinceLastTry time.Duration) bool {
	if sinceFirstTry >= cfg.GiveUp() {
		return false
	}
	if cfg.SSID == "" {
		return false
	}
	if cfg.When != config.HotspotAlways && !lidClosed {
		return false
	}
	return offlineChecks >= cfg.OfflineAfter() && sinceLastTry >= cfg.RetryEvery()
}

// Watch runs until ctx ends. Config is re-read every check, so the dashboard's
// changes apply without a restart.
func Watch(ctx context.Context) {
	offline := 0
	var firstTry, lastTry time.Time
	gaveUp := false
	wait := config.HotspotConfig{}.CheckEvery()
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		cfg, err := config.Load()
		if err == nil {
			wait = cfg.Hotspot.CheckEvery() // a change applies from the next check
		}
		if err != nil || cfg.Hotspot.SSID == "" {
			offline, firstTry, gaveUp = 0, time.Time{}, false
			continue
		}
		if Online() {
			if offline >= cfg.Hotspot.OfflineAfter() {
				logEvent("back online")
			}
			offline, firstTry, gaveUp = 0, time.Time{}, false
			continue
		}
		offline++
		lid := LidClosed()
		if offline == cfg.Hotspot.OfflineAfter() {
			logEvent("offline (lid %s)", map[bool]string{true: "shut", false: "open"}[lid])
		}
		var sinceFirst time.Duration
		if !firstTry.IsZero() {
			sinceFirst = time.Since(firstTry)
		}
		giveUp := cfg.Hotspot.GiveUp()
		if !decide(cfg.Hotspot, lid, offline, sinceFirst, time.Since(lastTry)) {
			if !firstTry.IsZero() && sinceFirst >= giveUp && !gaveUp {
				gaveUp = true
				logEvent("gave up after %s; trying again after the Mac has been back online", giveUp)
			}
			continue
		}
		if firstTry.IsZero() {
			firstTry = time.Now()
		}
		// The last attempt that fits in the give-up time also turns Wi-Fi
		// off and on, in case the Mac is stuck with no network at all.
		last := time.Since(firstTry)+cfg.Hotspot.RetryEvery()+attemptTimeout >= giveUp
		logEvent("joining %q (%s of %s tried)", cfg.Hotspot.SSID, time.Since(firstTry).Round(time.Minute), giveUp)
		_, _ = Join(cfg.Hotspot.SSID, last)
		// The gap runs from the end of the attempt, not its start.
		lastTry = time.Now()
	}
}
