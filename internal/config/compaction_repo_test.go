package config

import "testing"

func TestCompactionForRepo(t *testing.T) {
	c := CompactionConfig{CompactAtTokens: 300_000, WarnAtPercent: 80, RepoOverrides: []RepoCompaction{
		{Repo: "/src/big/", CompactAtTokens: 600_000},
		{Repo: "/src/never", Off: true},
		{Repo: "/src/empty"}, // neither a limit nor off: ignored
	}}
	for _, tc := range []struct {
		root       string
		at, warn   int64
		overridden bool
	}{
		{"/src/big", 600_000, 480_000, true},
		{"/src/never", NeverTokens, NeverTokens, true},
		{"/src/other", 300_000, 240_000, false},
		{"/src/big/sub", 300_000, 240_000, false}, // a path inside is not the repository
		{"/src/empty", 300_000, 240_000, false},
		{"", 300_000, 240_000, false},
	} {
		got, o := c.ForRepo(tc.root)
		if got.CompactAtTokens != tc.at || got.WarnAtTokens != tc.warn || (o != nil) != tc.overridden {
			t.Errorf("%q: at=%d warn=%d override=%v, want %d %d %v", tc.root, got.CompactAtTokens, got.WarnAtTokens, o != nil, tc.at, tc.warn, tc.overridden)
		}
	}
	if got, _ := (CompactionConfig{}).ForRepo("/x"); got.CompactAtTokens != DefaultCompactionCompactAt {
		t.Errorf("defaults still apply: %d", got.CompactAtTokens)
	}
}
