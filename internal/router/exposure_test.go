package router

import (
	"testing"
)

// What Claude Code holds beyond what Burst sends is the tool working: it is
// logged once per compacted session and never alerted, in the session or on
// screen.
func TestExposureIsNotAnAlert(t *testing.T) {
	notices := captureNotices(t)
	s, _ := newTestServer(t, "", "")
	a := &compactState{summary: "sum", lastContext: 135_000}
	for _, raw := range []int64{400_000, 520_000, 910_000} {
		a.rawContext = raw
		s.noteExposure("A|claude-opus-5-5|x", a)
	}
	c := &compactState{rawContext: 900_000} // nothing compacted: nothing hidden
	s.noteExposure("C|claude-opus-5-5|z", c)
	if got := notices(); len(got) != 0 {
		t.Fatalf("on-screen alerts %+v, want none", got)
	}
	if len(a.notices) != 0 || len(c.notices) != 0 {
		t.Fatalf("session lines a=%q c=%q, want none", a.notices, c.notices)
	}
	if a.exposureWarned != 520_000 || c.exposureWarned != 0 {
		t.Fatalf("logged at a=%d c=%d, want 520000 and 0", a.exposureWarned, c.exposureWarned)
	}
}
