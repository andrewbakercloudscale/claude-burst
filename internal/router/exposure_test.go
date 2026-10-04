package router

import (
	"strings"
	"testing"
)

// Claude Code's own history past 500k on a compacted session is announced
// once, then again only after another 200k, with its uncached price; a
// session Burst is not compacting, or a history back under 500k, is not.
func TestExposureAnnouncedOncePerStep(t *testing.T) {
	notices := captureNotices(t)
	s, _ := newTestServer(t, "", "")
	key := "S|claude-opus-5-5|abc"
	st := &compactState{summary: "sum", lastContext: 135_000}

	for _, raw := range []int64{400_000, 520_000, 600_000, 720_000, 300_000, 510_000} {
		st.rawContext = raw
		s.noteExposure(key, st)
	}
	st.summary, st.rawContext = "", 900_000 // nothing compacted: nothing hidden
	s.noteExposure(key, st)

	var got []string
	for _, e := range notices() {
		if e.Kind == alertExposure {
			got = append(got, e.Title)
			if e.Session != "S" || !strings.Contains(e.Detail, "Run /compact") {
				t.Errorf("event %+v", e)
			}
		}
	}
	want := []string{"Claude Code holds 520k, Burst sends 135k", "Claude Code holds 720k, Burst sends 135k", "Claude Code holds 510k, Burst sends 135k"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("exposure alerts = %q, want %q", got, want)
	}
	if len(st.notices) != 3 || !strings.Contains(st.notices[0], "without Burst it all goes uncached") {
		t.Fatalf("session lines = %q", st.notices)
	}
}
