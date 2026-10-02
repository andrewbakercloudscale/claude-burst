package keepawake

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
)

func TestParseSleepDisabled(t *testing.T) {
	cases := []struct {
		name      string
		out       string
		on, known bool
	}{
		{"on", "System-wide power settings:\n SleepDisabled\t\t1\nCurrently in use:\n sleep 0\n", true, true},
		{"off", "System-wide power settings:\n SleepDisabled\t\t0\n", false, true},
		{"absent", "Currently in use:\n sleep 0\n", false, false},
		{"empty", "", false, false},
		{"unexpected value", " SleepDisabled\t\t2\n", false, false},
		{"extra field is not the setting", " SleepDisabled 1 (forced)\n", false, false},
		{"a longer key", " SleepDisabledFoo 1\n", false, false},
	}
	for _, c := range cases {
		if on, known := ParseSleepDisabled(c.out); on != c.on || known != c.known {
			t.Errorf("%s: got %v,%v want %v,%v", c.name, on, known, c.on, c.known)
		}
	}
}

func TestOnACPowerAndWantSleepDisabled(t *testing.T) {
	cases := []struct {
		name string
		batt string
		want bool
	}{
		{"AC", "Now drawing from 'AC Power'\n -InternalBattery-0 (id=1)\t59%; charging; present: true\n", true},
		{"battery", "Now drawing from 'Battery Power'\n -InternalBattery-0 (id=1)\t59%; discharging; present: true\n", false},
		// Only the first line names the source: a battery line further down
		// must not read as on battery.
		{"battery word later", "Now drawing from 'AC Power'\n'Battery Power' mentioned\n", true},
		// pmset failed: no output. Treated as AC, the side that keeps a
		// session alive rather than the one that sleeps it.
		{"no output", "", true},
	}
	for _, c := range cases {
		if got := OnACPower(c.batt); got != c.want {
			t.Errorf("OnACPower %s: got %v", c.name, got)
		}
	}
	for _, c := range []struct {
		mode string
		onAC bool
		want bool
	}{
		{config.KeepAwakeOnAC, true, true},
		{config.KeepAwakeOnAC, false, false}, // a laptop on battery must be allowed to sleep
		{config.KeepAwakeAlways, true, true},
		{config.KeepAwakeAlways, false, true},
		{"", false, false},
	} {
		if got := WantSleepDisabled(c.mode, c.onAC); got != c.want {
			t.Errorf("WantSleepDisabled(%q, onAC=%v) = %v", c.mode, c.onAC, got)
		}
	}
}

func TestParseBlockers(t *testing.T) {
	out := "Assertion status system-wide:\n" +
		"   PreventSystemSleep             1\n" +
		"Listed by owning process:\n" +
		"   pid 412(caffeinate): [0x0000a1b2000190c4] 00:12:03 PreventUserIdleSystemSleep named: \"caffeinate command-line tool\"\n" +
		"\tDetails: caffeinate asserting on behalf of '/usr/local/bin/claude' (pid 400)\n" +
		"   pid 977(caffeinate): [0x0000a1b2000190c5] 00:01:10 PreventSystemSleep named: \"caffeinate command-line tool\"\n" +
		"\tDetails: caffeinate asserting on behalf of 'make' (pid 970)\n" +
		"\tLocalized=THE CAFFEINATE TOOL IS PREVENTING SLEEP.\n" +
		"   pid 88(Some App): [0x0000a1b2000190c6] 00:00:05 PreventSystemSleep named: \"sync\"\n" +
		"   pid 89(other): [0x0000a1b2000190c7] 00:00:05 PreventSystemSleepFoo named: \"not this\"\n"
	want := []Blocker{
		{PID: "977", Process: "caffeinate", For: "caffeinate asserting on behalf of 'make' (pid 970)"},
		{PID: "88", Process: "Some App"},
	}
	if got := ParseBlockers(out); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
	if got := ParseBlockers(""); got != nil {
		t.Fatalf("no output: %+v", got)
	}
	// The idle assertions every `caffeinate -i` takes do not stop lid-close
	// sleep, so they are not blockers (2026-09-29).
	if got := ParseBlockers("   pid 1(caffeinate): [0x1] 00:00:01 PreventUserIdleSystemSleep named: \"x\"\n"); got != nil {
		t.Fatalf("idle assertion counted: %+v", got)
	}
}

func TestProblem(t *testing.T) {
	good := Status{SleepDisabled: true, SleepDisabledKnown: true, OnAC: true, AppliedMode: "ac", DaemonInstalled: true, GhosttyNapOff: true}
	with := func(f func(*Status)) Status { s := good; f(&s); return s }
	cases := []struct {
		name  string
		st    Status
		on    bool
		mode  string
		idle  int
		empty bool // want no problem
	}{
		{"all applied", good, true, "ac", 0, true},
		{"unknown pmset", with(func(s *Status) { s.SleepDisabledKnown = false }), true, "ac", 0, false},
		{"off and clean", with(func(s *Status) { s.AppliedMode = "" }), false, "ac", 0, true},
		{"off but root half applied", good, false, "ac", 0, false},
		{"wrong mode", good, true, "always", 0, false},
		{"idle window not applied", good, true, "ac", 30, false},
		{"ac mode needs the daemon", with(func(s *Status) { s.DaemonInstalled = false }), true, "ac", 0, false},
		{"always mode without idle needs no daemon", with(func(s *Status) { s.AppliedMode = "always"; s.DaemonInstalled = false }), true, "always", 0, true},
		{"on battery, SleepDisabled still 1", with(func(s *Status) { s.OnAC = false }), true, "ac", 0, false},
		{"on battery, SleepDisabled 0", with(func(s *Status) { s.OnAC = false; s.SleepDisabled = false }), true, "ac", 0, true},
		// With an idle window the daemon follows the clock and SleepDisabled
		// may read 0 legitimately.
		{"idle window past", with(func(s *Status) { s.AppliedIdle = 30; s.SleepDisabled = false }), true, "ac", 30, true},
		{"Ghostty may nap", with(func(s *Status) { s.GhosttyNapOff = false }), true, "ac", 0, false},
	}
	for _, c := range cases {
		got := c.st.Problem(c.on, c.mode, c.idle)
		if (got == "") != c.empty {
			t.Errorf("%s: Problem = %q", c.name, got)
		}
	}
}

func TestActivityMarks(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".config", "claude-burst"), 0o700); err != nil {
		t.Fatal(err)
	}
	if p := ActivityPath(); p != filepath.Join(home, ".config", "claude-burst", "last-activity") {
		t.Fatalf("ActivityPath %s", p)
	}
	if !LastActivity().IsZero() {
		t.Fatal("no file yet: zero")
	}
	// Touch is off under go test, so router tests never write the real file.
	Touch()
	if !LastActivity().IsZero() {
		t.Fatal("Touch wrote under go test")
	}
	MarkNow()
	first := LastActivity()
	if time.Since(first) > time.Minute {
		t.Fatalf("MarkNow did not create the file: %v", first)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(ActivityPath(), old, old); err != nil {
		t.Fatal(err)
	}
	MarkNow()
	if !LastActivity().After(old.Add(time.Minute)) {
		t.Fatal("MarkNow did not move the mtime forward")
	}
}
