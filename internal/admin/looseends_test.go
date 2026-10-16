package admin

import (
	"strings"
	"testing"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
	"github.com/andrewbakercloudscale/claude-burst/internal/looseends"
)

func looseFixture(now time.Time) looseends.Report {
	old := float64(now.Add(-50 * time.Hour).Unix())
	fresh := float64(now.Add(-time.Hour).Unix())
	return looseends.Report{Repos: []looseends.Repo{
		{Root: "/r/alpha", Uncommitted: []looseends.File{
			{Path: "/r/alpha/old", MTime: old},
			{Path: "/r/alpha/new", MTime: fresh},
			// Being worked on right now: never a reminder, however old.
			{Path: "/r/alpha/live", MTime: old, Owner: &looseends.Owner{Since: old, Live: true}},
			// Left by a closed session: its age is when it was left.
			{Path: "/r/alpha/left", MTime: fresh, Owner: &looseends.Owner{Since: old}},
		}},
		{Root: "/r/beta", Ahead: 2, Unpushed: []looseends.Commit{{When: old}, {When: fresh}},
			Stashes: []looseends.Stash{{When: old}}},
		{Root: "/r/gamma", Uncommitted: []looseends.File{{Path: "/r/gamma/x", MTime: fresh}}},
	}}
}

func TestStaleCountsOnlyWhatIsOlderThanADayAndNotLive(t *testing.T) {
	now := time.Date(2026, 10, 16, 10, 0, 0, 0, time.Local)
	f, c, s, repos := staleCounts(looseFixture(now), now)
	if f != 2 || c != 1 || s != 1 || strings.Join(repos, ",") != "alpha,beta" {
		t.Fatalf("got %d files, %d commits, %d stashes in %v", f, c, s, repos)
	}
	if got := looseSummary(f, c, s); got != "2 uncommitted files, 1 unpushed commit, 1 stash" {
		t.Fatalf("summary %q", got)
	}
}

func TestTheLooseEndsReminderGoesUpOnceADayFromNine(t *testing.T) {
	alerts := captureAlerts(t)
	looseScan = func(config.Config, time.Time) looseends.Report { return looseFixture(time.Now()) }
	n := &notifier{}
	early := time.Date(2026, 10, 16, 8, 30, 0, 0, time.Local)
	n.looseNudge(config.Config{}, early)
	if n.looseDay != "" {
		t.Fatal("looked before 09:00")
	}
	nine := time.Date(2026, 10, 16, 9, 0, 5, 0, time.Local)
	looseRemind(looseScan, config.Config{}, nine)
	looseRemind(looseScan, config.Config{}, nine.Add(time.Hour)) // same day: shown once
	got := alerts()
	if len(got) != 1 || !strings.HasPrefix(got[0], "warn: Loose ends older than a day (16 Oct): 2 uncommitted files") {
		t.Fatalf("alerts = %q", got)
	}
	n.looseNudge(config.Config{}, nine)
	if n.looseDay != "16 Oct" {
		t.Fatalf("looseDay = %q", n.looseDay)
	}
}

func TestNothingStaleNoReminder(t *testing.T) {
	alerts := captureAlerts(t)
	now := time.Date(2026, 10, 16, 10, 0, 0, 0, time.Local)
	looseRemind(func(config.Config, time.Time) looseends.Report {
		return looseends.Report{Repos: []looseends.Repo{{Root: "/r/a", Uncommitted: []looseends.File{{Path: "/r/a/x", MTime: float64(now.Unix())}}}}}
	}, config.Config{}, now)
	if got := alerts(); len(got) != 0 {
		t.Fatalf("alerts = %q", got)
	}
}
