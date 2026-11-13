package metrics

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// The benchmark history is shaped like the real one these scans were
// measured against on 1 Nov 2026, 162,000 events from 3,809 sessions over 14
// days: what a scan costs is in how many events it reads and how many
// sessions it keys.
const (
	benchEvents   = 170_000
	benchSessions = 3_800
	benchDays     = 14
)

// The history is written once for every benchmark in a run and removed by
// TestMain when the run ends, so a run leaves nothing in the temp directory.
var (
	benchOnce sync.Once
	benchPath string
	benchErr  error
)

func TestMain(m *testing.M) {
	code := m.Run()
	if benchPath != "" {
		os.RemoveAll(filepath.Dir(benchPath))
	}
	os.Exit(code)
}

func benchHistory(b *testing.B) string {
	b.Helper()
	benchOnce.Do(func() {
		dir, err := os.MkdirTemp("", "metrics-bench-")
		if err != nil {
			benchErr = err
			return
		}
		benchPath = filepath.Join(dir, "metrics.jsonl")
		benchErr = writeBenchHistory(benchPath)
	})
	if benchErr != nil {
		b.Fatal(benchErr)
	}
	return benchPath
}

func writeBenchHistory(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriterSize(f, 1<<20)
	defer w.Flush()
	rng := rand.New(rand.NewPCG(1, 2))
	end := time.Now()
	span := benchDays * 24 * time.Hour
	step := span / benchEvents
	models := []string{"claude-opus-4-7", "claude-sonnet-4-6", "claude-haiku-4-5", "anthropic.claude-sonnet-4-6-v1:0"}
	notes := []string{"", "", "", "", "", "", "", "prune", "client cancelled", "failover: rate limit"}
	ctx := make([]int64, benchSessions)
	enc := json.NewEncoder(w)
	for i := range benchEvents {
		s := rng.IntN(benchSessions)
		secondary := rng.IntN(10) == 0
		e := Event{
			Time:       end.Add(-span + time.Duration(i)*step),
			RequestID:  fmt.Sprintf("req_%016x", rng.Uint64()),
			SessionID:  fmt.Sprintf("%08x-1111-4222-8333-%012x", s, s),
			Route:      "anthropic",
			Slot:       "primary",
			Model:      models[rng.IntN(3)],
			HTTPStatus: 200,
			DurationMS: int64(300 + rng.IntN(20000)),
			Note:       notes[rng.IntN(len(notes))],
		}
		e.Destination = "https://api.anthropic.com/v1/messages"
		if secondary {
			e.Route, e.Slot, e.Model = "bedrock", "secondary", models[3]
			e.Destination = "https://bedrock-runtime.eu-west-1.amazonaws.com/model/x/invoke"
			e.PrunedBytes = int64(rng.IntN(200_000))
			e.PrunedToolResults = int64(rng.IntN(8))
		}
		if rng.IntN(25) == 0 {
			e.HTTPStatus = 429
		}
		ctx[s] += int64(2_000 + rng.IntN(6_000))
		if ctx[s] > 900_000 {
			ctx[s] = 40_000
			e.CompactedMessages = int64(10 + rng.IntN(60))
			e.CompactedBytes = int64(rng.IntN(400_000))
		}
		e.CacheReadTokens = ctx[s]
		e.CacheWriteTokens = int64(rng.IntN(8_000))
		e.InputTokens = int64(rng.IntN(400))
		e.OutputTokens = int64(rng.IntN(3_000))
		e.APIEquivalentUSD = float64(e.OutputTokens)/1e6*15 + float64(e.CacheReadTokens)/1e6*1.5
		if rng.IntN(50) == 0 {
			e.PricingUnknown = true
			e.APIEquivalentUSD = 0
		}
		if err := enc.Encode(e); err != nil {
			return err
		}
	}
	return nil
}

func benchRepoOf(session string) (string, string) {
	return "proj", "/work/proj"
}

func BenchmarkSummarizeTotal(b *testing.B) {
	path := benchHistory(b)
	if _, err := Summarize(path, time.Time{}); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := Summarize(path, time.Time{}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSummarizeToday(b *testing.B) {
	path := benchHistory(b)
	since := time.Now().Add(-24 * time.Hour)
	if _, err := Summarize(path, since); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := Summarize(path, since); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkEfficiencySince(b *testing.B) {
	path := benchHistory(b)
	since := time.Now().Add(-30 * 24 * time.Hour)
	if _, err := EfficiencySince(path, since); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := EfficiencySince(path, since); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCompactionStatsSince(b *testing.B) {
	path := benchHistory(b)
	since := time.Now().Add(-30 * 24 * time.Hour)
	if _, err := CompactionStatsSince(path, since); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := CompactionStatsSince(path, since); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCompactionStrategies(b *testing.B) {
	path := benchHistory(b)
	since := time.Now().Add(-30 * 24 * time.Hour)
	run := func() {
		if _, err := CompactionStrategiesTracked(path, since, 500_000, 5*time.Minute, benchRepoOf, nil); err != nil {
			b.Fatal(err)
		}
	}
	run()
	b.ReportAllocs()
	for b.Loop() {
		run()
	}
}

func BenchmarkUsage7d(b *testing.B) {
	path := benchHistory(b)
	now := time.Now()
	f := UsageFilter{
		From:  time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local).AddDate(0, 0, -6),
		To:    now,
		Daily: true,
	}
	run := func() {
		if _, err := Usage(path, f, func(string) string { return "proj" }); err != nil {
			b.Fatal(err)
		}
	}
	run()
	b.ReportAllocs()
	for b.Loop() {
		run()
	}
}

func BenchmarkDaily14(b *testing.B) {
	path := benchHistory(b)
	run := func() {
		if _, err := Daily(path, 14); err != nil {
			b.Fatal(err)
		}
	}
	run()
	b.ReportAllocs()
	for b.Loop() {
		run()
	}
}

func BenchmarkColdLoad(b *testing.B) {
	src := benchHistory(b)
	data, err := os.ReadFile(src)
	if err != nil {
		b.Fatal(err)
	}
	dir := b.TempDir()
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		i++
		p := filepath.Join(dir, fmt.Sprintf("m%d.jsonl", i))
		b.StopTimer()
		if err := os.WriteFile(p, data, 0o600); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		if _, err := Summarize(p, time.Time{}); err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		os.Remove(p)
		if _, err := Summarize(p, time.Time{}); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
	}
}
