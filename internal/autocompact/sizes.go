package autocompact

import (
	"encoding/json"
	"os"
	"sort"
	"time"
)

// The Compact at in force, on record. The learner takes a repository's size
// from a replay of its requests, and a replay is a model: to know how far
// it can be trusted, what it says the sizes in force should have cost has
// to be set beside what they did cost (metrics.StrategyTrack). That needs
// the size each repository had at each moment, which nothing else keeps:
// the learner's file holds only the latest.

// SizeChange is one repository's Compact at from Time on.
type SizeChange struct {
	Time time.Time `json:"time"`
	// Root is the repository; "" is every repository with no size of its
	// own (the fixed Compact at, or the intelligent mode's starting size).
	Root string `json:"root"`
	// At is the size in tokens, 0 when the repository is never compacted.
	At int64 `json:"at"`
	// Source is why: learned, override, start, fixed or off.
	Source string `json:"source,omitempty"`
}

// Sizes is every change on record, oldest first for each root.
type Sizes struct {
	byRoot map[string][]SizeChange
}

// ReadSizes reads the record. A missing file is an empty one.
func ReadSizes(path string) Sizes {
	s := Sizes{byRoot: map[string][]SizeChange{}}
	b, err := os.ReadFile(path)
	if err != nil {
		return s
	}
	start := 0
	for i := 0; i <= len(b); i++ {
		if i < len(b) && b[i] != '\n' {
			continue
		}
		var c SizeChange
		if json.Unmarshal(b[start:i], &c) == nil && !c.Time.IsZero() {
			s.byRoot[c.Root] = append(s.byRoot[c.Root], c)
		}
		start = i + 1
	}
	for _, l := range s.byRoot {
		sort.SliceStable(l, func(i, j int) bool { return l[i].Time.Before(l[j].Time) })
	}
	return s
}

// Len is how many changes are on record.
func (s Sizes) Len() int {
	n := 0
	for _, l := range s.byRoot {
		n += len(l)
	}
	return n
}

// Roots is every repository with a size of its own on record.
func (s Sizes) Roots() []string {
	var out []string
	for root := range s.byRoot {
		if root != "" {
			out = append(out, root)
		}
	}
	sort.Strings(out)
	return out
}

func last(l []SizeChange, at time.Time) (SizeChange, bool) {
	i := sort.Search(len(l), func(i int) bool { return l[i].Time.After(at) })
	if i == 0 {
		return SizeChange{}, false
	}
	return l[i-1], true
}

// At is the Compact at in force for the repository at root at a time: its
// own where it had one on record by then, else the one every repository
// without its own had. False when the record does not go back that far.
func (s Sizes) At(root string, at time.Time) (int64, bool) {
	if c, ok := last(s.byRoot[root], at); ok {
		return c.At, true
	}
	c, ok := last(s.byRoot[""], at)
	return c.At, ok
}

// Snapshot is a copy that later Records do not change.
func (s Sizes) Snapshot() Sizes {
	out := Sizes{byRoot: make(map[string][]SizeChange, len(s.byRoot))}
	for root, l := range s.byRoot {
		out.byRoot[root] = l[:len(l):len(l)]
	}
	return out
}

// Record puts on record, as of now, each size of inForce (by root, "" for
// the rest) that is not what the record already says, and appends them to
// the file at path ("" keeps them in memory). It returns how many changed.
func (s *Sizes) Record(path string, now time.Time, inForce map[string]SizeChange) (int, error) {
	if s.byRoot == nil {
		s.byRoot = map[string][]SizeChange{}
	}
	roots := make([]string, 0, len(inForce))
	for root := range inForce {
		roots = append(roots, root)
	}
	sort.Strings(roots)
	var lines []byte
	n := 0
	for _, root := range roots {
		c := inForce[root]
		c.Root, c.Time = root, now
		// "" sorts first, so a repository on the size the rest have is
		// compared with the rest's new one.
		if cur, ok := s.At(root, now); ok && cur == c.At {
			continue
		}
		s.byRoot[root] = append(s.byRoot[root], c)
		b, err := json.Marshal(c)
		if err != nil {
			return n, err
		}
		lines = append(append(lines, b...), '\n')
		n++
	}
	if path == "" || len(lines) == 0 {
		return n, nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return n, err
	}
	defer f.Close()
	_, err = f.Write(lines)
	return n, err
}
