package router

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
)

func watchServer(t *testing.T) (*Server, *bytes.Buffer) {
	t.Helper()
	cfg := config.Default()
	cfg.PrimaryCompaction = config.CompactionConfig{Enabled: true, CompactAtTokens: 175_000, WindowMinutes: 1}
	dir := t.TempDir()
	logs := &bytes.Buffer{}
	s, err := New(cfg, filepath.Join(dir, "state.json"), filepath.Join(dir, "metrics.jsonl"), log.New(logs, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	return s, logs
}

func turnFrom(sid string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/v1/messages", nil)
	req.Header.Set("x-claude-code-session-id", sid)
	return req
}

// A session that keeps sending over its limit past the compaction window
// is reported once, whatever compaction measured: on 11 and 12 Nov 2026 a
// session sent 366k a turn under a 175k limit for a day with nothing said.
// Small turns of the same session (subagents) do not end the run.
func TestSessionSendingOverItsLimitIsReported(t *testing.T) {
	defer func(g time.Duration) { sentOverGrace = g }(sentOverGrace)
	sentOverGrace = -time.Minute // window (1 min) plus this: report at once
	s, logs := watchServer(t)
	for i := 0; i < 8; i++ {
		s.watchSent(turnFrom("sess-over"), tokenUsage{input: 10, cacheRead: 360_000})
		s.watchSent(turnFrom("sess-over"), tokenUsage{input: 10, cacheRead: 20_000})
	}
	if n := strings.Count(logs.String(), "compaction not holding session=sess-over"); n != 1 {
		t.Fatalf("reported %d times, want once:\n%s", n, logs)
	}
	if !strings.Contains(logs.String(), "175k limit, up to 360k") {
		t.Errorf("limit and peak not named:\n%s", logs)
	}
}

// Under the limit, or over it for fewer turns than a run, says nothing.
func TestSessionUnderItsLimitIsNotReported(t *testing.T) {
	defer func(g time.Duration) { sentOverGrace = g }(sentOverGrace)
	sentOverGrace = -time.Minute
	s, logs := watchServer(t)
	for i := 0; i < 20; i++ {
		s.watchSent(turnFrom("sess-under"), tokenUsage{input: 10, cacheRead: 170_000})
	}
	for i := 0; i < sentOverTurns-1; i++ {
		s.watchSent(turnFrom("sess-short"), tokenUsage{input: 10, cacheRead: 200_000})
	}
	if strings.Contains(logs.String(), "compaction not holding") {
		t.Fatalf("reported:\n%s", logs)
	}
}

// Over the limit within the window plus grace is compaction's ordinary
// wait, not reported.
func TestSessionOverItsLimitWithinTheWindowIsNotReported(t *testing.T) {
	s, logs := watchServer(t)
	for i := 0; i < 20; i++ {
		s.watchSent(turnFrom("sess-wait"), tokenUsage{input: 10, cacheRead: 300_000})
	}
	if strings.Contains(logs.String(), "compaction not holding") {
		t.Fatalf("reported:\n%s", logs)
	}
}

// A session whose requests Burst cannot read shows its last reading as old,
// not as what it sends.
func TestUnreadSessionShowsNotMeasuredSince(t *testing.T) {
	s, _ := watchServer(t)
	seen := time.Now().Add(-26 * time.Hour)
	s.compaction.mu.Lock()
	st := s.compaction.state("sess-old|claude-opus-4-8|abc")
	st.lastContext, st.seen = 187_050, seen
	s.compaction.mu.Unlock()
	row := MainConversation(s.CompactionSessions(), "sess-old")
	if row == nil || !row.NotMeasuredSince.IsZero() || strings.HasPrefix(row.State, "not measured") {
		t.Fatalf("stale before any unread request: %+v", row)
	}
	s.noteUnread("sess-old", time.Now())
	row = MainConversation(s.CompactionSessions(), "sess-old")
	if row == nil || !row.NotMeasuredSince.Equal(seen) || !strings.HasPrefix(row.State, "not measured since "+seen.Local().Format("2 Jan 15:04")) {
		t.Fatalf("not shown as old: %+v", row)
	}
}
