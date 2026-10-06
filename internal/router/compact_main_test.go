package router

import (
	"testing"
	"time"
)

// 6 Oct 2026: a compacted session sending 95k also had requests on record
// under other first messages, sent whole at 343k. The largest row spoke for
// the session, so the panel said "over threshold" for half an hour.
func TestTheConversationWithTheSummarySpeaksForTheSession(t *testing.T) {
	now := time.Now()
	rows := []CompactionSession{ // largest first, as CompactionSessions gives them
		{Session: "s", Context: 343_000, State: "over threshold, compacts at the next request", Seen: now.Add(-5 * time.Minute)},
		{Session: "other", Context: 200_000, State: "compacted", Seen: now},
		{Session: "s", Context: 95_000, State: "compacted", Seen: now},
		{Session: "s", Context: 5_000, State: "ok", Seen: now},
	}
	if got := MainConversation(rows, "s"); got == nil || got.Context != 95_000 {
		t.Fatalf("want the compacted conversation, got %+v", got)
	}
	// Not compacted: the largest, which is not a subagent.
	rows[2].State = "ok"
	if got := MainConversation(rows, "s"); got == nil || got.Context != 343_000 {
		t.Fatalf("want the largest, got %+v", got)
	}
	// A summary nobody has used for a long time is not the session now.
	rows[2].State, rows[2].Seen = "compacted", now.Add(-2*time.Hour)
	if got := MainConversation(rows, "s"); got == nil || got.Context != 343_000 {
		t.Fatalf("want the largest over a summary long unused, got %+v", got)
	}
	if MainConversation(rows, "nobody") != nil {
		t.Fatal("a session with no rows has none")
	}
}
