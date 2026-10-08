package autocompact

import (
	"path/filepath"
	"testing"
	"time"
)

func TestTheSizeInForceIsOnRecordFromTheMomentItChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sizes.jsonl")
	t0 := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	var s Sizes
	n, err := s.Record(path, t0, map[string]SizeChange{"": {At: 300_000, Source: "start"}, "/r/a": {At: 200_000, Source: "learned"}})
	if err != nil || n != 2 {
		t.Fatalf("first record: %d, %v", n, err)
	}
	// The same sizes again are no change; a moved one and a new repository are.
	if n, _ := s.Record(path, t0.Add(30*time.Minute), map[string]SizeChange{"": {At: 300_000}, "/r/a": {At: 200_000}}); n != 0 {
		t.Fatalf("nothing changed, %d recorded", n)
	}
	if n, _ := s.Record(path, t0.Add(time.Hour), map[string]SizeChange{"": {At: 300_000}, "/r/a": {At: 175_000}, "/r/b": {At: 0, Source: "off"}}); n != 2 {
		t.Fatalf("two changed, %d recorded", n)
	}
	for _, got := range []Sizes{s, ReadSizes(path)} {
		at := func(root string, d time.Duration) int64 {
			t.Helper()
			v, ok := got.At(root, t0.Add(d))
			if !ok {
				t.Fatalf("%s after %v: not on record", root, d)
			}
			return v
		}
		if _, ok := got.At("/r/a", t0.Add(-time.Second)); ok {
			t.Fatal("a size from before the record began")
		}
		if at("/r/a", 0) != 200_000 || at("/r/a", 59*time.Minute) != 200_000 || at("/r/a", time.Hour) != 175_000 {
			t.Fatal("/r/a: 200k, then 175k from the hour")
		}
		// A repository with no size of its own had the one for the rest,
		// until it got its own.
		if at("/r/b", 10*time.Minute) != 300_000 || at("/r/b", 2*time.Hour) != 0 || at("/r/new", time.Hour) != 300_000 {
			t.Fatal("/r/b: the rest's 300k, then never; an unknown repository: the rest's")
		}
		if got.Len() != 4 || len(got.Roots()) != 2 {
			t.Fatalf("%d changes over %v", got.Len(), got.Roots())
		}
	}
}
