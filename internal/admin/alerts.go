package admin

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
	"github.com/andrewbakercloudscale/claude-burst/internal/handover"
	"github.com/andrewbakercloudscale/claude-burst/internal/keepawake"
	"github.com/andrewbakercloudscale/claude-burst/internal/metrics"
	"github.com/andrewbakercloudscale/claude-burst/internal/notice"
)

// On-screen alerts the notifier's round raises from state outside the
// request path: today's spend, keep-awake, other sessions' handovers and a
// newer release. Each is compared with the last round, so starting the
// gateway announces nothing that was already true. The macOS notification
// switches do not apply: the usage panel has its own.

// Seams for tests: each reads the real Mac or the real log otherwise.
var (
	readKeepAwake     = keepawake.Read
	summarizeSpend    = metrics.Summarize
	handoverLogPath   = handover.LogPath
	upgradeAlertEvery = 12 * time.Hour
)

type alertRounds struct {
	spendAt   time.Time
	spendDone map[string]bool // day|level shown

	awakeAt      time.Time
	awakeSeen    bool
	awakeOn      bool
	awakeProblem string

	handoverSeen bool
	handoverSize int64

	upgradeAt time.Time
}

// alertRound runs inside notifyRound with the config it loaded.
func (s *Server) alertRound(a *alertRounds, cfg config.Config, now time.Time) {
	s.alertSpend(a, cfg, now)
	s.alertKeepAwake(a, cfg, now)
	alertHandovers(a)
	if a.upgradeAt.IsZero() {
		a.upgradeAt = now
	} else if now.Sub(a.upgradeAt) >= upgradeAlertEvery {
		a.upgradeAt = now
		go s.checkUpgrade(context.Background(), false)
	}
}

// alertSpend: today's API-equivalent spend through the gateway passed the
// level set on the dashboard. Once per day per level, also across restarts
// (the title names both, and PublishOnce looks in notices.json).
func (s *Server) alertSpend(a *alertRounds, cfg config.Config, now time.Time) {
	level := cfg.AlertDailySpendUSD
	if level <= 0 || now.Sub(a.spendAt) < time.Minute {
		return
	}
	a.spendAt = now
	day := now.Format("2 Jan")
	key := day + "|" + usd(level)
	if a.spendDone[key] {
		return
	}
	y, m, d := now.Date()
	sum, err := summarizeSpend(s.metricsPath, time.Date(y, m, d, 0, 0, 0, 0, now.Location()))
	if err != nil || sum.APIEquivalentUSD < level {
		return
	}
	if a.spendDone == nil {
		a.spendDone = map[string]bool{}
	}
	a.spendDone[key] = true
	notice.PublishOnce("spend", notice.Warn, "Spend today passed $"+usd(level)+" ("+day+")",
		fmt.Sprintf("$%.2f API-equivalent through Burst so far today. The level is set under Session options on the dashboard.", sum.APIEquivalentUSD))
}

func usd(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// alertKeepAwake: keep-awake is on in config, and the Mac stopped keeping
// itself awake (the idle limit passed, it went onto battery in plugged-in
// mode, or something broke), or started again, or a new problem appeared.
// Read every 30 seconds: it runs pmset.
func (s *Server) alertKeepAwake(a *alertRounds, cfg config.Config, now time.Time) {
	if now.Sub(a.awakeAt) < 30*time.Second {
		return
	}
	a.awakeAt = now
	if !cfg.KeepAwakeLidClosed {
		a.awakeSeen = false
		return
	}
	st := readKeepAwake()
	if !st.SleepDisabledKnown {
		return
	}
	on := st.SleepDisabled
	problem := st.Problem(true, cfg.KeepAwakeLidClosedPower, cfg.KeepAwakeIdleMinutes)
	if a.awakeSeen {
		switch {
		case a.awakeOn && !on:
			notice.Publish("keep-awake", notice.Warn, "Keep-awake turned off", keepAwakeOffReason(st, cfg, now, problem))
		case !a.awakeOn && on:
			notice.Publish("keep-awake", notice.OK, "Keep-awake back on", "The Mac stays awake with the lid shut again.")
		case problem != "" && problem != a.awakeProblem:
			notice.Publish("keep-awake", notice.Warn, "Keep-awake problem", problem+". Details under Lid and power on the dashboard.")
		}
	}
	a.awakeSeen, a.awakeOn, a.awakeProblem = true, on, problem
}

func keepAwakeOffReason(st keepawake.Status, cfg config.Config, now time.Time, problem string) string {
	idle := time.Duration(cfg.KeepAwakeIdleMinutes) * time.Minute
	switch {
	case idle > 0 && !st.LastActivity.IsZero() && now.Sub(st.LastActivity) >= idle:
		return fmt.Sprintf("No Claude Code use for %d minutes, the time limit, so the Mac sleeps if the lid is shut. Using Claude Code turns it back on.", cfg.KeepAwakeIdleMinutes)
	case cfg.KeepAwakeLidClosedPower == config.KeepAwakeOnAC && !st.OnAC:
		return "On battery, and keep-awake applies only while plugged in, so the Mac sleeps if the lid is shut."
	case problem != "":
		return problem + ". The Mac sleeps if the lid is shut."
	}
	return "The Mac sleeps if the lid is shut."
}

// handoverLine is one line of the handover log: date, time, what, the
// rest, and the session in brackets.
var handoverLine = regexp.MustCompile(`^\S+ \S+ (\w+)\s+(.*) \[([0-9A-Za-z-]+)\]$`)

// alertHandovers: another session finished or had its handover written,
// read from what the handover hooks append to their log. The event names
// the session, so its own panel leaves it to the others.
func alertHandovers(a *alertRounds) {
	p, err := handoverLogPath()
	if err != nil {
		return
	}
	fi, err := os.Stat(p)
	if err != nil {
		return
	}
	size := fi.Size()
	if !a.handoverSeen || size < a.handoverSize {
		a.handoverSeen, a.handoverSize = true, size
		return
	}
	if size == a.handoverSize {
		return
	}
	f, err := os.Open(p)
	if err != nil {
		return
	}
	defer f.Close()
	b, err := io.ReadAll(io.NewSectionReader(f, a.handoverSize, size-a.handoverSize))
	a.handoverSize = size
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(b), "\n") {
		m := handoverLine.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		what, rest, sid := m[1], m[2], m[3]
		switch {
		case what == "wrote":
			repo, _, _ := strings.Cut(rest, " (")
			notice.PublishFor(sid, "handover", notice.Info, "Handover written: "+filepath.Base(repo),
				"Another session's HANDOFF.md is up to date.")
		case what == "queue" && strings.Contains(rest, "session ended"):
			repo, _, _ := strings.Cut(rest, ":")
			notice.PublishFor(sid, "handover", notice.Info, "Session finished: "+filepath.Base(repo),
				"Another Claude Code session in "+filepath.Base(repo)+" ended.")
		}
	}
}

// alertUpgrade: GitHub has a newer release than the one running. Once per
// version, also across restarts.
func alertUpgrade(st upgradeStatus) {
	if st.Error != "" || !versionNewer(st.LatestVersion, st.RunningVersion) {
		return
	}
	notice.PublishOnce("update", notice.Info, "Claude Burst "+st.LatestVersion+" available",
		"Running "+st.RunningVersion+". Upgrade from the dashboard.")
}

// versionNewer reports whether a is a later x.y.z than b. Anything that
// does not parse is not newer.
func versionNewer(a, b string) bool {
	pa, okA := parseVersion(a)
	pb, okB := parseVersion(b)
	if !okA || !okB {
		return false
	}
	for i := range pa {
		if pa[i] != pb[i] {
			return pa[i] > pb[i]
		}
	}
	return false
}

func parseVersion(v string) ([3]int, bool) {
	var out [3]int
	parts := strings.Split(strings.TrimPrefix(strings.TrimSpace(v), "v"), ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return out, false
		}
		out[i] = n
	}
	return out, true
}
