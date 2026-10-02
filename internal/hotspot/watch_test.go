package hotspot

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
)

// tick is the world as the watcher sees it at one check.
type tick struct {
	online, lidShut, onHotspot bool
}

// attempt is one join the watcher made.
type attempt struct {
	tick          int // 1-based check number
	at            time.Duration
	restoreOnFail bool
}

// watchRun drives Watch through a scripted sequence of checks on a fake
// clock: each wait returns at once and moves the clock on by
// the wait, and the run ends when the script does. A join takes joinTakes of
// fake time and always fails, as on a Mac that cannot see the phone.
type watchRun struct {
	cfg       config.HotspotConfig
	cfgErr    error
	lidAtBoot bool
	script    func(i int) tick
	checks    int
	joinTakes time.Duration
}

func (w watchRun) run(t *testing.T) (joins []attempt, events []string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home) // logEvent writes under HOME
	if err := os.MkdirAll(filepath.Join(home, ".config", "claude-burst"), 0o700); err != nil {
		t.Fatal(err)
	}

	old := struct {
		lid    func() bool
		load   func() (config.Config, error)
		now    func() time.Time
		after  func(time.Duration) <-chan time.Time
		join   func(string, bool) (string, error)
		online func() bool
		onHS   func() bool
		rawJ   func(context.Context, string) (string, error)
		rest   func() string
	}{lidClosed, loadConfig, now, after, watchJoin, Online, OnHotspot, join, restore}
	t.Cleanup(func() {
		lidClosed, loadConfig, now, after, watchJoin, Online, OnHotspot, join, restore =
			old.lid, old.load, old.now, old.after, old.join, old.online, old.onHS, old.rawJ, old.rest
	})
	// Nothing below may reach the real Wi-Fi: the low-level stubs fail the
	// test if the watcher ever goes past watchJoin.
	join = func(context.Context, string) (string, error) { t.Fatal("real join reached"); return "", nil }
	restore = func() string { t.Fatal("real restore reached"); return "" }

	start := time.Date(2026, 10, 2, 9, 0, 0, 0, time.Local)
	clock := start
	i := 0 // the check in progress, 1-based; 0 before the first
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cur := func() tick {
		if i < 1 {
			t.Fatal("world read before the first check")
		}
		return w.script(i)
	}

	now = func() time.Time { return clock }
	after = func(d time.Duration) <-chan time.Time {
		if i >= w.checks {
			cancel()
			return nil // never fires: ctx.Done wins
		}
		clock = clock.Add(d)
		i++
		ch := make(chan time.Time, 1)
		ch <- clock
		return ch
	}
	lidCalls := 0
	lidClosed = func() bool {
		lidCalls++
		if i == 0 {
			return w.lidAtBoot
		}
		return cur().lidShut
	}
	loadConfig = func() (config.Config, error) {
		c := config.Default()
		c.Hotspot = w.cfg
		return c, w.cfgErr
	}
	Online = func() bool { return cur().online }
	OnHotspot = func() bool { return cur().onHotspot }
	watchJoin = func(ssid string, restoreOnFail bool) (string, error) {
		if ssid != w.cfg.SSID {
			t.Errorf("joined %q, want %q", ssid, w.cfg.SSID)
		}
		joins = append(joins, attempt{tick: i, at: clock.Sub(start), restoreOnFail: restoreOnFail})
		clock = clock.Add(w.joinTakes)
		return "", errors.New("Could not find network")
	}

	// Synchronous: every wait is already satisfied, so Watch returns as
	// soon as the script ends, and a stub may call t.Fatal.
	Watch(ctx)
	if i != w.checks {
		t.Fatalf("ran %d checks, want %d", i, w.checks)
	}
	if lidCalls == 0 {
		t.Fatal("the lid stub never ran: the watcher read something else")
	}
	return joins, RecentEvents(1000)
}

func count(events []string, sub string) int {
	n := 0
	for _, e := range events {
		if strings.Contains(e, sub) {
			n++
		}
	}
	return n
}

var lidShutOffline = func(int) tick { return tick{lidShut: true} }

func TestWatchJoinsAfterOfflineChecksThenWaitsTheRetryGap(t *testing.T) {
	cfg := config.HotspotConfig{SSID: "Phone", When: config.HotspotLidClosed, CheckSeconds: 5, OfflineChecks: 2, RetrySeconds: 60}
	joins, events := watchRun{cfg: cfg, lidAtBoot: true, script: lidShutOffline, checks: 30}.run(t)
	// Checks at 5s, 10s, ... 150s. Offline from the first, so the second
	// check (10s) is OfflineAfter and joins; the retry gap puts the next
	// at 70s and 130s.
	want := []time.Duration{10 * time.Second, 70 * time.Second, 130 * time.Second}
	if len(joins) != len(want) {
		t.Fatalf("joins %+v, want at %v", joins, want)
	}
	for k, j := range joins {
		if j.at != want[k] {
			t.Errorf("join %d at %v, want %v", k, j.at, want[k])
		}
		if j.restoreOnFail {
			t.Errorf("join %d at %v restores Wi-Fi, but the give-up time is far off", k, j.at)
		}
	}
	if joins[0].tick != cfg.OfflineAfter() {
		t.Errorf("first join on check %d, want check %d", joins[0].tick, cfg.OfflineAfter())
	}
	if n := count(events, "offline (lid shut)"); n != 1 {
		t.Errorf("%d offline events, want 1: %q", n, events)
	}
}

func TestWatchOneOfflineCheckIsNotEnough(t *testing.T) {
	cfg := config.HotspotConfig{SSID: "Phone", CheckSeconds: 5, OfflineChecks: 2}
	// Offline every other check: never two in a row.
	script := func(i int) tick { return tick{online: i%2 == 0, lidShut: true} }
	if joins, _ := (watchRun{cfg: cfg, lidAtBoot: true, script: script, checks: 40}.run(t)); len(joins) != 0 {
		t.Fatalf("joins %+v, want none", joins)
	}
}

func TestWatchLidOpenedOfflineJoinsAtOnceAndKeepsTrying(t *testing.T) {
	cfg := config.HotspotConfig{SSID: "Phone", When: config.HotspotLidClosed, CheckSeconds: 5, OfflineChecks: 2, RetrySeconds: 60}
	// Online with the lid shut for two checks, then the lid opens on a Mac
	// with no network, and stays open.
	script := func(i int) tick {
		if i <= 2 {
			return tick{online: true, lidShut: true}
		}
		return tick{}
	}
	joins, events := watchRun{cfg: cfg, lidAtBoot: true, script: script, checks: 40}.run(t)
	if len(joins) < 2 {
		t.Fatalf("joins %+v: the lid-open attempt must not be the only one (the 2026-10-02 bug)", joins)
	}
	if joins[0].tick != 3 || joins[0].restoreOnFail {
		t.Errorf("first join %+v, want check 3 (the moment the lid opened) without a Wi-Fi toggle", joins[0])
	}
	// Then every RetryEvery, though the lid is open and When is lid-closed.
	for k := 1; k < len(joins); k++ {
		if gap := joins[k].at - joins[k-1].at; gap != cfg.RetryEvery() {
			t.Errorf("gap %d is %v, want %v", k, gap, cfg.RetryEvery())
		}
	}
	if n := count(events, "lid opened with no network"); n != 1 {
		t.Errorf("%d lid-opened events, want 1", n)
	}
}

func TestWatchLidOpenFromTheStartIsLeftToTheUser(t *testing.T) {
	cfg := config.HotspotConfig{SSID: "Phone", When: config.HotspotLidClosed, CheckSeconds: 5}
	open := func(int) tick { return tick{} }
	if joins, _ := (watchRun{cfg: cfg, lidAtBoot: false, script: open, checks: 40}.run(t)); len(joins) != 0 {
		t.Fatalf("lid never shut, lid-closed mode: joins %+v, want none", joins)
	}
	cfg.When = config.HotspotAlways
	if joins, _ := (watchRun{cfg: cfg, lidAtBoot: false, script: open, checks: 4}.run(t)); len(joins) != 1 {
		t.Fatalf("always mode: joins %+v, want one", joins)
	}
}

func TestWatchStillOnTheHotspotNeverJoins(t *testing.T) {
	cfg := config.HotspotConfig{SSID: "Phone", When: config.HotspotAlways, CheckSeconds: 5, OfflineChecks: 2}
	script := func(int) tick { return tick{lidShut: true, onHotspot: true} }
	joins, events := watchRun{cfg: cfg, lidAtBoot: true, script: script, checks: 50}.run(t)
	if len(joins) != 0 {
		t.Fatalf("joins %+v: joining again drops the Mac off the phone", joins)
	}
	if n := count(events, "still joined to the phone's hotspot"); n != 1 {
		t.Errorf("%d waiting events, want 1: %q", n, events)
	}
}

func TestWatchGivesUpOnceAndTheLastAttemptRestores(t *testing.T) {
	// Each attempt takes the full attemptTimeout, as a join that never
	// finds the network does. Joins at 10s (to 40s), 55s (to 85s) and
	// 100s (to 130s); the third is the last to fit in two minutes, so it
	// toggles Wi-Fi. The check at 135s is past the give-up time.
	cfg := config.HotspotConfig{SSID: "Phone", CheckSeconds: 5, OfflineChecks: 2, RetrySeconds: 15, GiveUpMinutes: 2}
	joins, events := watchRun{cfg: cfg, lidAtBoot: true, script: lidShutOffline, checks: 200, joinTakes: attemptTimeout}.run(t)
	want := []attempt{
		{tick: 2, at: 10 * time.Second},
		{tick: 5, at: 55 * time.Second},
		{tick: 8, at: 100 * time.Second, restoreOnFail: true},
	}
	if len(joins) != len(want) {
		t.Fatalf("joins %+v, want %+v", joins, want)
	}
	for k := range want {
		if joins[k] != want[k] {
			t.Errorf("join %d: %+v, want %+v", k, joins[k], want[k])
		}
	}
	if n := count(events, "gave up"); n != 1 {
		t.Errorf("%d gave-up events over 160 offline checks, want exactly 1: %q", n, events)
	}
}

func TestWatchBackOnlineStartsTheNextOutageFresh(t *testing.T) {
	cfg := config.HotspotConfig{SSID: "Phone", CheckSeconds: 5, OfflineChecks: 2, RetrySeconds: 15, GiveUpMinutes: 2}
	// Offline long enough to give up (checks 1-60), online at check 61,
	// then a second outage.
	script := func(i int) tick { return tick{online: i == 61, lidShut: true} }
	joins, events := watchRun{cfg: cfg, lidAtBoot: true, script: script, checks: 120, joinTakes: attemptTimeout}.run(t)
	var second []attempt
	for _, j := range joins {
		if j.tick > 61 {
			second = append(second, j)
		}
	}
	if len(second) != 3 {
		t.Fatalf("second outage joins %+v (all %+v): a give-up must not carry over", second, joins)
	}
	if second[0].tick != 61+cfg.OfflineAfter() || second[0].restoreOnFail || !second[2].restoreOnFail {
		t.Errorf("second outage %+v: should restart from its own first attempt", second)
	}
	backOnline := 0
	for _, e := range events {
		// The whole message after the timestamp: the give-up line also
		// ends in "back online".
		if _, msg, _ := strings.Cut(e[len("2006-01-02 "):], " "); msg == "back online" {
			backOnline++
		}
	}
	if n := backOnline; n != 1 {
		t.Errorf("%d back-online events, want 1: %q", n, events)
	}
	if n := count(events, "gave up"); n != 2 {
		t.Errorf("%d gave-up events, want one per outage: %q", n, events)
	}
}

func TestWatchDoesNothingWithoutANetworkOrConfig(t *testing.T) {
	off := config.HotspotConfig{When: config.HotspotAlways, CheckSeconds: 5}
	if joins, _ := (watchRun{cfg: off, lidAtBoot: true, script: lidShutOffline, checks: 40}.run(t)); len(joins) != 0 {
		t.Errorf("no SSID: joins %+v", joins)
	}
	// An unreadable config file: the open-lid path must not fire either.
	bad := config.HotspotConfig{SSID: "Phone", When: config.HotspotAlways, CheckSeconds: 5}
	script := func(i int) tick { return tick{lidShut: i < 3} }
	if joins, _ := (watchRun{cfg: bad, cfgErr: errors.New("bad json"), lidAtBoot: true, script: script, checks: 40}.run(t)); len(joins) != 0 {
		t.Errorf("config error: joins %+v", joins)
	}
}
