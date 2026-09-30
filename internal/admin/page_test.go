package admin

import (
	"regexp"
	"testing"
)

// Every id in the dashboard is unique. $(id) returns the first match, so a
// second element with the same id is silently unreachable: on 2026-09-30 the
// hotspot's "Check the internet every" field took the test dialog's id, and
// the Test button did nothing at all.
func TestPageIDsAreUnique(t *testing.T) {
	seen := map[string]int{}
	for _, m := range regexp.MustCompile(`\bid="([^"$]+)"`).FindAllSubmatch(indexHTML, -1) {
		seen[string(m[1])]++
	}
	if len(seen) < 100 {
		t.Fatalf("found only %d ids; the pattern no longer matches the page", len(seen))
	}
	for id, n := range seen {
		if n > 1 {
			t.Errorf("id %q is used %d times", id, n)
		}
	}
}
