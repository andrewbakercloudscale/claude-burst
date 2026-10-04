package router

import (
	"github.com/andrewbakercloudscale/claude-burst/internal/notice"
	"strings"
	"testing"
)

// Claude Code's own history past 500k on a compacted session is announced
// once per session, never again as it grows; the on-screen alert once a day
// across sessions. A session Burst is not compacting is not announced.
func TestExposureAnnouncedOnce(t *testing.T) {
	notices := captureNotices(t)
	s, _ := newTestServer(t, "", "")
	a := &compactState{summary: "sum", lastContext: 135_000}
	b := &compactState{summary: "sum", lastContext: 90_000}
	for _, raw := range []int64{400_000, 520_000, 600_000, 720_000, 300_000, 910_000} {
		a.rawContext, b.rawContext = raw, raw
		s.noteExposure("A|claude-opus-5-5|x", a)
		s.noteExposure("B|claude-opus-5-5|y", b)
	}
	c := &compactState{rawContext: 900_000} // nothing compacted: nothing hidden
	s.noteExposure("C|claude-opus-5-5|z", c)
	exposureWG.Wait()

	var got []notice.Event
	for _, e := range notices() {
		if e.Kind == alertExposure {
			got = append(got, e)
		}
	}
	if len(got) != 1 || !strings.Contains(got[0].Detail, "Run /compact") {
		t.Fatalf("on-screen alerts %+v, want exactly one", got)
	}
	if len(a.notices) != 1 || len(b.notices) != 1 || len(c.notices) != 0 || !strings.Contains(a.notices[0], "Claude Code holds 520k") {
		t.Fatalf("session lines a=%q b=%q c=%q", a.notices, b.notices, c.notices)
	}
}
