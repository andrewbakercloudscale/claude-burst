package notice

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTest(t *testing.T) (*Publisher, string, *time.Time) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "notices.json")
	p := New(path, nil)
	clock := time.Date(2026, 10, 3, 9, 0, 0, 0, time.Local)
	p.now = func() time.Time { return clock }
	return p, path, &clock
}

func read(t *testing.T, p *Publisher, path string) []Event {
	t.Helper()
	p.Flush(2 * time.Second)
	evs, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

func TestRepeatIsHeldBackForFiveMinutes(t *testing.T) {
	p, path, clock := newTest(t)
	if !p.Publish("network", Error, "Network offline", "") {
		t.Fatal("first event not queued")
	}
	if p.Publish("network", Error, "Network offline", "") {
		t.Fatal("repeat within 5 minutes was queued")
	}
	*clock = clock.Add(RepeatAfter)
	if !p.Publish("network", Error, "Network offline", "") {
		t.Fatal("repeat after 5 minutes was held back")
	}
	if n := len(read(t, p, path)); n != 2 {
		t.Fatalf("got %d events, want 2", n)
	}
}

func TestStateChangeAlwaysShows(t *testing.T) {
	p, path, _ := newTest(t)
	p.Publish("failover", Warn, "Failed over to together", "")
	p.Publish("failover", OK, "Back on Claude", "")
	if !p.Publish("failover", Warn, "Failed over to together", "") {
		t.Fatal("a second failover straight after recovery was held back")
	}
	evs := read(t, p, path)
	if len(evs) != 3 {
		t.Fatalf("got %d events, want 3", len(evs))
	}
	if evs[1].Resolves != "failover" || evs[0].Resolves != "" {
		t.Errorf("resolves = %q, %q; want only the ok event to resolve its kind", evs[0].Resolves, evs[1].Resolves)
	}
}

func TestKeepsTheNewestTwenty(t *testing.T) {
	p, path, _ := newTest(t)
	for i := 0; i < Keep+5; i++ {
		p.Publish("test", Info, fmt.Sprintf("event %d", i), "")
	}
	evs := read(t, p, path)
	if len(evs) != Keep {
		t.Fatalf("got %d events, want %d", len(evs), Keep)
	}
	if evs[0].Title != "event 5" || evs[Keep-1].Title != fmt.Sprintf("event %d", Keep+4) {
		t.Errorf("kept %q to %q, want the newest, oldest first", evs[0].Title, evs[Keep-1].Title)
	}
}

func TestWriteIsAtomicAndReplacesADamagedFile(t *testing.T) {
	p, path, _ := newTest(t)
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	p.Publish("test", Info, "after damage", "detail")
	evs := read(t, p, path)
	if len(evs) != 1 || evs[0].Detail != "detail" || evs[0].TS == 0 {
		t.Fatalf("events = %+v", evs)
	}
	// No temporary files left beside it: notices.json and the audit only.
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 2 {
		t.Errorf("directory holds %d entries, want notices.json and audit.jsonl", len(entries))
	}
	var raw map[string]any
	b, _ := os.ReadFile(path)
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Errorf("file is not valid JSON: %v", err)
	}
}

func TestPublishOnceSurvivesARestart(t *testing.T) {
	p, path, clock := newTest(t)
	if !p.PublishOnce("update", Info, "Claude Burst 0.6.0 available", "") {
		t.Fatal("first not published")
	}
	*clock = clock.Add(time.Hour)
	if p.PublishOnce("update", Info, "Claude Burst 0.6.0 available", "") {
		t.Fatal("published twice")
	}
	// A new publisher on the same file is a restarted gateway.
	q := New(path, nil)
	if q.PublishOnce("update", Info, "Claude Burst 0.6.0 available", "") {
		t.Fatal("published again after a restart")
	}
	if !q.PublishOnce("update", Info, "Claude Burst 0.7.0 available", "") {
		t.Fatal("a new version was held back")
	}
	q.PublishFor("abc", "handover", Info, "Handover written", "")
	evs := read(t, q, path)
	if len(evs) != 3 || evs[2].Session != "abc" {
		t.Fatalf("events = %+v", evs)
	}
}

func TestNoDefaultIsANoOp(t *testing.T) {
	SetDefault(nil)
	if Publish("x", Info, "y", "") {
		t.Fatal("published with no default publisher")
	}
	Flush(time.Millisecond)
}

// Every event shown lands in the audit, and a Record lands only there.
func TestAuditKeepsShownEventsAndRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notices.json")
	p := New(path, nil)
	p.Publish("failover", Warn, "Failed over", "because")
	p.Record("action", Info, "Gateway restarted from the console", "")
	p.Flush(2 * time.Second)
	got := ReadAudit(AuditPath(path), 10)
	if len(got) != 2 || got[0].Title != "Gateway restarted from the console" || !got[0].AuditOnly || got[1].Title != "Failed over" {
		t.Fatalf("audit = %+v", got)
	}
	evs, _ := Read(path)
	if len(evs) != 1 || evs[0].Title != "Failed over" {
		t.Fatalf("notices = %+v", evs)
	}
}

// The audit rotates instead of growing without bound, and reads across both.
func TestAuditRotates(t *testing.T) {
	dir := t.TempDir()
	ap := filepath.Join(dir, "audit.jsonl")
	big := strings.Repeat("x", 1024)
	for i := 0; i < AuditMax/1024+10; i++ {
		if err := appendAudit(ap, Event{Title: "t", Detail: big}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(ap + ".1"); err != nil {
		t.Fatal("no rotation")
	}
	if n := len(ReadAudit(ap, 1<<20)); n != AuditMax/1024+10 {
		t.Errorf("read %d entries across the two files", n)
	}
}
