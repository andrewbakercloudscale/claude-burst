package logline

import (
	"bytes"
	"log"
	"strings"
	"testing"
	"time"
)

func TestEveryLineHasItsTimeZoneAndLevel(t *testing.T) {
	var buf bytes.Buffer
	at := time.Date(2026, 10, 7, 11, 30, 5, 123_000_000, time.FixedZone("SAST", 2*3600))
	l := log.New(&Writer{W: &buf, Now: func() time.Time { return at }}, "", 0)
	l.Printf("req=abc ok route=anthropic status=200")
	l.Printf("req=abc failover route=anthropic -> together reason=%q", "five_hour")
	l.Printf("req=abc PANIC method=%q", "POST")
	l.Printf(`req=abc upstream_error err="Get \"https://cloudflare-dns.com/dns-query?name=api.anthropic.com&type=A\": timeout"`)
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	want := []string{
		"2026-10-07T11:30:05.123+02:00 level=info req=abc ok route=anthropic status=200",
		`2026-10-07T11:30:05.123+02:00 level=warn req=abc failover route=anthropic -> together reason="five_hour"`,
		`2026-10-07T11:30:05.123+02:00 level=error req=abc PANIC method="POST"`,
		`2026-10-07T11:30:05.123+02:00 level=warn req=abc upstream_error err="Get \"https://cloudflare-dns.com/dns-query\": timeout"`,
	}
	if len(lines) != len(want) {
		t.Fatalf("%d lines:\n%s", len(lines), buf.String())
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Errorf("line %d\n got %s\nwant %s", i, lines[i], want[i])
		}
	}
	for _, l := range lines {
		if ts, ok := ParseStamp(l); !ok || !ts.Equal(at) {
			t.Errorf("the stamp must read back: %v %v", ts, ok)
		}
	}
	if ts, ok := ParseStamp("2026/10/07 11:30:05 req=abc"); !ok || ts.Hour() != 11 {
		t.Errorf("a line from before the change must still read: %v %v", ts, ok)
	}
	if _, ok := ParseStamp("goroutine 1 [running]:"); ok {
		t.Error("a line with no time has none")
	}
}

func TestStripQueriesLeavesTheRestAlone(t *testing.T) {
	for in, want := range map[string]string{
		"no url here? really":                                  "no url here? really",
		"GET https://a.example/p?x=1&y=2 failed":               "GET https://a.example/p failed",
		`err="Post \"http://h:1/v1/messages?beta=true\": EOF"`: `err="Post \"http://h:1/v1/messages\": EOF"`,
		"two https://a/b?c=d and https://e/f?g=h":              "two https://a/b and https://e/f",
		"https://api.anthropic.com/v1/messages":                "https://api.anthropic.com/v1/messages",
	} {
		if got := StripQueries(in); got != want {
			t.Errorf("%q\n got %q\nwant %q", in, got, want)
		}
	}
}
