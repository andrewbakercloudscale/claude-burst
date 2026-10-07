package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The run before this one ended in a panic: its first line is found. Once
// a start line follows it, it is not reported again.
func TestPreviousCrashIsFoundOnceAndDated(t *testing.T) {
	p := filepath.Join(t.TempDir(), "launchd.err.log")
	write := func(s string) {
		f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		f.WriteString(s)
	}
	if got := previousCrash(p); got != "" {
		t.Fatalf("no file: %q", got)
	}
	write("2026/10/07 10:00:00 http: TLS handshake error from 127.0.0.1:1: EOF\n")
	if got := previousCrash(p); got != "" {
		t.Fatalf("a handshake line is not a crash: %q", got)
	}
	write("panic: runtime error: invalid memory address or nil pointer dereference\n[signal SIGSEGV: segmentation violation]\n\ngoroutine 1 [running]:\nmain.serve()\n")
	if got := previousCrash(p); !strings.HasPrefix(got, "panic: runtime error") {
		t.Fatalf("the panic's first line: %q", got)
	}
	line := startLine("1.2.3", time.Date(2026, 10, 7, 11, 30, 0, 0, time.FixedZone("SAST", 2*3600)))
	if !strings.Contains(line, "1.2.3 started 2026-10-07 11:30:00 +02:00 pid ") {
		t.Fatalf("the start line must carry the version and local time with its zone: %q", line)
	}
	write(line + "\n2026/10/07 11:30:01 http: TLS handshake error from 127.0.0.1:2: EOF\n")
	if got := previousCrash(p); got != "" {
		t.Fatalf("a crash before the last start was reported again: %q", got)
	}
	write("fatal error: concurrent map writes\n")
	if got := previousCrash(p); got != "fatal error: concurrent map writes" {
		t.Fatalf("a runtime fault: %q", got)
	}
}

// Over its limit the file keeps its end, whole lines only, in place.
func TestTrimStderrLogKeepsTheEnd(t *testing.T) {
	p := filepath.Join(t.TempDir(), "launchd.err.log")
	var b strings.Builder
	for i := 0; i < 2000; i++ {
		b.WriteString("line ")
		b.WriteString(strings.Repeat("x", 40))
		b.WriteString("\n")
	}
	b.WriteString("the last line\n")
	os.WriteFile(p, []byte(b.String()), 0o600)
	if trimmed, _ := trimStderrLog(p, 1<<20, 1000); trimmed {
		t.Fatal("under the limit: left alone")
	}
	trimmed, err := trimStderrLog(p, 10_000, 1000)
	if err != nil || !trimmed {
		t.Fatalf("trimmed=%v err=%v", trimmed, err)
	}
	got, _ := os.ReadFile(p)
	lines := strings.Split(strings.TrimRight(string(got), "\n"), "\n")
	if len(got) > 1200 || !strings.HasPrefix(lines[0], "(cut back from ") || lines[len(lines)-1] != "the last line" {
		t.Fatalf("%d bytes, first %q, last %q", len(got), lines[0], lines[len(lines)-1])
	}
	for _, l := range lines[1 : len(lines)-1] {
		if !strings.HasPrefix(l, "line x") {
			t.Fatalf("a cut line was kept: %q", l)
		}
	}
}
