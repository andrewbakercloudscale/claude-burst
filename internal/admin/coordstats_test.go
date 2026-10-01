package admin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCoordStatsCountsAndIssues(t *testing.T) {
	log := `2026-09-20 10:00:00 share /r/old.go: aaaaaaaa edits, master bbbbbbbb
2026-09-30 09:00:00 share /r/a.go: aaaaaaaa edits, master bbbbbbbb
2026-09-30 09:01:00 refuse Write by aaaaaaaa on /r/a.go (master bbbbbbbb)
2026-10-01 08:00:00 refuse sweeping git by aaaaaaaa in /r
2026-10-01 08:01:00 master of /r/b.go taken over by aaaaaaaa from bbbbbbbb (idle 20 min)
2026-10-01 08:02:00 hold cccccccc to commit /r/c.go, /r/d.go
2026-10-01 08:03:00 cccccccc stopped with uncommitted /r/c.go, /r/d.go
2026-10-01 08:04:00 free /r/c.go (committed)
2026-10-01 08:05:00 dddddddd stopped with uncommitted /r/e.go
2026-10-01 08:06:00 master of /r/e.go passes from dddddddd to aaaaaaaa (ended)
2026-10-01 08:07:00 ERROR in pre-tool: disk full
not a log line
`
	p := filepath.Join(t.TempDir(), "coord.log")
	os.WriteFile(p, []byte(log), 0o644)
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.Local)
	// git says d.go still has uncommitted changes; no other file does.
	old := uncommitted
	uncommitted = func(p string) bool { return p == "/r/d.go" }
	defer func() { uncommitted = old }()

	today := coordStats(p, 1, now)
	if today.Totals["refused"] != 1 || today.Totals["shared"] != 0 || today.Totals["taken"] != 1 || today.Totals["errors"] != 1 || today.Totals["stopped"] != 2 {
		t.Fatalf("today: %+v", today.Totals)
	}
	if today.AllTime["shared"] != 2 || today.AllTime["refused"] != 2 || today.Since != "2026-09-20 10:00:00" {
		t.Fatalf("all time: %+v since %s", today.AllTime, today.Since)
	}
	week := coordStats(p, 7, now)
	if week.Totals["shared"] != 1 || len(week.PerDay) != 7 || week.PerDay[5].Counts["shared"] != 1 || week.PerDay[6].Day != "2026-10-01" {
		t.Fatalf("7 days: %+v %+v", week.Totals, week.PerDay)
	}
	if coordStats(p, 14, now).Totals["shared"] != 2 {
		t.Fatal("14 days should reach 09-20")
	}

	// Newest first: the error, then e.go (handed on: resolved), then
	// c.go/d.go (d.go still pending).
	is := today.Issues
	if len(is) != 3 || is[0].Kind != "error" || !is[1].Resolved || is[2].Resolved || len(is[2].Pending) != 1 || is[2].Pending[0] != "/r/d.go" {
		t.Fatalf("issues: %+v", is)
	}
	if today.Unresolved != 2 {
		t.Fatalf("unresolved: %d", today.Unresolved)
	}
	// A day later the error no longer counts as open, and says resolved.
	next := coordStats(p, 7, now.Add(24*time.Hour))
	if next.Unresolved != 1 || next.Issues[0].Kind != "error" || !next.Issues[0].Resolved {
		t.Fatalf("next day: open %d, %+v", next.Unresolved, next.Issues)
	}
}

func TestCoordStatsMissingLog(t *testing.T) {
	m := coordStats(filepath.Join(t.TempDir(), "none"), 14, time.Now())
	if len(m.PerDay) != 14 || m.Totals["shared"] != 0 || m.Issues != nil {
		t.Fatalf("%+v", m)
	}
}

func TestCoordWindow(t *testing.T) {
	for q, want := range map[string]int{"": 1, "1": 1, "7": 7, "14": 14, "99": 1} {
		if got := coordWindow(q); got != want {
			t.Errorf("%q: %d", q, got)
		}
	}
}

// The page renders the metrics: Coordinated sums the meeting events, hook
// errors turn red, the per-day table appears only for 7 or 14 days, and
// issues show open or resolved.
func TestCoordMetricsOnThePage(t *testing.T) {
	src := string(indexHTML)
	for _, want := range []string{`id="coDays"`, `<option value="7">7 days</option>`, `<option value="14">14 days</option>`, `id="coTiles"`, `id="coIssues"`, `"/api/coordination?days=" + coDays`} {
		if !strings.Contains(src, want) {
			t.Errorf("page lacks %s", want)
		}
	}
	i := strings.Index(src, "const coTileDefs = [")
	j := strings.Index(src[i:], "\n];\n")
	defs := src[i : i+j+4]
	var got struct{ Tiles, PerDay, Today, Issues, None string }
	runPageJS(t, []string{"coSum", "coTilesHTML", "coPerDayHTML", "coIssuesHTML"}, defs+`
let coNames = {cccccccc: "Fix the build"}, coTasks = {cccccccc: "add the comment flood limits"};
const m = {days: 7, totals: {shared: 2, refused: 1, taken: 1, errors: 1}, all_time: {shared: 9},
  per_day: [{day: "2026-09-30", counts: {}}, {day: "2026-10-01", counts: {errors: 1}}],
  issues: [{at: "2026-10-01 08:07:00", kind: "error", text: "ERROR in pre-tool: x", resolved: false},
           {at: "2026-10-01 08:03:00", kind: "stopped", session: "cccccccc", files: ["/r/c.go", "/r/d.go"], pending: ["/r/d.go"], resolved: false}]};
out({Tiles: coTilesHTML(m), PerDay: coPerDayHTML(m), Today: coPerDayHTML({...m, days: 1}), Issues: coIssuesHTML(m), None: coIssuesHTML({days: 1})});
`, &got)
	if !strings.Contains(got.Tiles, `Coordinated</div><div class="v">4</div>`) || !strings.Contains(got.Tiles, `color:var(--bad)">1</div>`) || !strings.Contains(got.Tiles, "9 all time") {
		t.Errorf("tiles: %s", got.Tiles)
	}
	if !strings.Contains(got.PerDay, "2026-10-01") || strings.Index(got.PerDay, "2026-10-01") > strings.Index(got.PerDay, "2026-09-30") || got.Today != "" {
		t.Errorf("per day (newest first, none for today): %q / %q", got.PerDay, got.Today)
	}
	if !strings.Contains(got.Issues, "Fix the build") || !strings.Contains(got.Issues, "add the comment flood limits") || !strings.Contains(got.Issues, "still uncommitted: 1") || strings.Count(got.Issues, `pill bad">open`) != 2 {
		t.Errorf("issues: %s", got.Issues)
	}
	if !strings.Contains(got.None, "No issues today") {
		t.Errorf("no issues: %s", got.None)
	}
}

// The Clear button takes the errors so far off the Issues list; they stay
// counted, and a later error shows again.
func TestCoordStatsClearedErrors(t *testing.T) {
	p := filepath.Join(t.TempDir(), "coord.log")
	os.WriteFile(p, []byte(`2026-10-01 18:18:20 ERROR in prompt: state lock: resource temporarily unavailable
2026-10-01 18:19:36 ERROR in post-tool: state lock: resource temporarily unavailable
2026-10-01 18:40:00 `+coordClearedLine+`
2026-10-01 19:00:00 ERROR in stop: something new
`), 0o644)
	m := coordStats(p, 1, time.Date(2026, 10, 1, 20, 0, 0, 0, time.Local))
	if m.Totals["errors"] != 3 || len(m.Issues) != 1 || m.Issues[0].Text != "ERROR in stop: something new" || m.Unresolved != 1 {
		t.Fatalf("totals %v, issues %+v, open %d", m.Totals, m.Issues, m.Unresolved)
	}
}

func TestClearErrorsAction(t *testing.T) {
	s := newTestServer(t)
	writeConfig(t, os.Getenv("HOME"))
	dir := t.TempDir()
	t.Setenv("CLAUDE_BURST_COORD_DIR", dir)
	rr := mutate(t, s, "/api/coordination-act", `{"clear_errors":true}`)
	if rr.Code != 200 {
		t.Fatalf("%d %s", rr.Code, rr.Body)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "coord.log"))
	if !strings.Contains(string(b), coordClearedLine) {
		t.Fatalf("log: %s", b)
	}
}

// A file that left tracking with no log line (committed in a worktree that
// was then merged) still resolves its issue: git is asked.
func TestCoordStatsAsksGitAboutPendingFiles(t *testing.T) {
	p := filepath.Join(t.TempDir(), "coord.log")
	os.WriteFile(p, []byte("2026-10-01 18:08:13 ef7f6a74 stopped with uncommitted /w/a.php, /w/b.php\n"), 0o644)
	old := uncommitted
	defer func() { uncommitted = old }()
	now := time.Date(2026, 10, 1, 20, 0, 0, 0, time.Local)
	uncommitted = func(string) bool { return true }
	if m := coordStats(p, 1, now); m.Unresolved != 1 {
		t.Fatalf("still uncommitted: %+v", m.Issues)
	}
	uncommitted = func(p string) bool { return p == "/w/b.php" }
	if m := coordStats(p, 1, now); m.Unresolved != 1 || len(m.Issues[0].Pending) != 1 {
		t.Fatalf("a.php committed: %+v", m.Issues)
	}
	uncommitted = func(string) bool { return false }
	if m := coordStats(p, 1, now); m.Unresolved != 0 || !m.Issues[0].Resolved {
		t.Fatalf("both committed: %+v", m.Issues)
	}
}
