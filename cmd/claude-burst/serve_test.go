package main

import (
	"bytes"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
	"github.com/andrewbakercloudscale/claude-burst/internal/logline"
	"github.com/andrewbakercloudscale/claude-burst/internal/tlsca"
)

// loadConfigJSON is config.Load on a config.json holding body, under a HOME
// of its own.
func loadConfigJSON(t *testing.T, body string) (config.Config, error) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	if err := config.EnsureDir(); err != nil {
		t.Fatal(err)
	}
	p, err := config.ConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return config.Load()
}

// A keep-awake setting config.Load rejects is not something a request needs:
// the gateway starts with the default in its place and names it. It used to
// stop the start, and with it every Claude Code session.
func TestABadKeepAwakeSettingDoesNotStopTheGateway(t *testing.T) {
	for name, c := range map[string]struct {
		body  string
		check func(config.Config) bool
	}{
		"power": {`{"keep_awake_lid_closed_power":"sometimes"}`,
			func(c config.Config) bool { return c.KeepAwakeLidClosedPower == config.KeepAwakeOnAC }},
		"idle minutes": {`{"keep_awake_idle_minutes":99999}`,
			func(c config.Config) bool {
				return c.KeepAwakeIdleMinutes == config.Default().KeepAwakeIdleMinutes
			}},
	} {
		t.Run(name, func(t *testing.T) {
			cfg, err := loadConfigJSON(t, c.body)
			if err == nil {
				t.Fatal("setup: config.Load must reject this file")
			}
			fixed, found, ok := startableConfig(cfg, err)
			if !ok || len(found) != 1 {
				t.Fatalf("want it startable with one problem named, got ok=%v %+v", ok, found)
			}
			if !c.check(fixed) {
				t.Fatalf("the rejected setting must be back at its default: %+v", fixed)
			}
			if !strings.Contains(found[0].Detail, "keep_awake") {
				t.Fatalf("the problem must name the setting: %q", found[0].Detail)
			}
			if fixed.Listen == "" || fixed.Primary.BaseURL == "" {
				t.Fatalf("the rest of the configuration must be what Load made of the file: %+v", fixed)
			}
		})
	}
}

// What cannot be guessed around still stops the start: a file that does not
// parse (with or without a bad keep-awake value in it), a value of the wrong
// type, an intercept mode nobody knows.
func TestAConfigThatCannotBeUsedStillStopsTheGateway(t *testing.T) {
	for name, body := range map[string]string{
		"not JSON":                `{"listen": "127.0.0.1:7777",`,
		"wrong type":              `{"keep_awake_idle_minutes":"ten"}`,
		"wrong type and bad mode": `{"max_request_mb":"big","keep_awake_lid_closed_power":"sometimes"}`,
		"unknown intercept mode":  `{"intercept":{"mode":"sideways"}}`,
		"bad mode and bad power":  `{"intercept":{"mode":"sideways"},"keep_awake_lid_closed_power":"sometimes"}`,
	} {
		t.Run(name, func(t *testing.T) {
			cfg, err := loadConfigJSON(t, body)
			if err == nil {
				t.Fatal("setup: config.Load must reject this file")
			}
			if _, found, ok := startableConfig(cfg, err); ok {
				t.Fatalf("started on a configuration that cannot be trusted (%v), naming %+v", err, found)
			}
		})
	}
	if _, _, ok := startableConfig(config.Default(), errors.New("something Load never said before")); ok {
		t.Fatal("an error this does not recognise must not be started through")
	}
}

// A panic in a background loop is logged with its stack and the loop is run
// again after the pause; one that returns is finished.
func TestBackgroundRestartsALoopThatPanicked(t *testing.T) {
	var paused []time.Duration
	old := backgroundPause
	backgroundPause = func(d time.Duration) { paused = append(paused, d) }
	t.Cleanup(func() { backgroundPause = old })

	var buf bytes.Buffer
	runs := 0
	background(log.New(&buf, "", 0), "network watcher", time.Minute, func() {
		runs++
		if runs < 3 {
			var none []int
			_ = none[runs] // a real runtime panic, not one made with panic()
		}
	})
	if runs != 3 {
		t.Fatalf("ran %d times, want 3: twice into a panic, then to its end", runs)
	}
	if len(paused) != 2 || paused[0] != time.Minute {
		t.Fatalf("paused %v, want a minute after each panic", paused)
	}
	logged := buf.String()
	if strings.Count(logged, "network watcher PANIC") != 2 || !strings.Contains(logged, "index out of range") {
		t.Fatalf("each panic must be logged with what it was:\n%s", logged)
	}
	if !strings.Contains(logged, "goroutine ") || !strings.Contains(logged, "serve_test.go") {
		t.Fatalf("the stack must be in the log:\n%s", logged)
	}
	if logline.Level(logged) != logline.Error {
		t.Fatal("the line must read as an error to whoever filters the log by level")
	}
}

// With no restart asked for, the panic is caught and reported and the loop
// is left stopped. It must not take the process down either way.
func TestBackgroundDoesNotRestartWhenToldNotTo(t *testing.T) {
	old := backgroundPause
	backgroundPause = func(time.Duration) { t.Error("nothing to pause for") }
	t.Cleanup(func() { backgroundPause = old })
	var buf bytes.Buffer
	runs := 0
	background(log.New(&buf, "", 0), "shutdown drain", 0, func() {
		runs++
		panic("gone")
	})
	if runs != 1 || !strings.Contains(buf.String(), "shutdown drain PANIC err=gone (it is not restarted)") {
		t.Fatalf("runs=%d log:\n%s", runs, buf.String())
	}
}

// testLeaf is a real leaf from tlsca, with its CA, in a directory of its own.
func testLeaf(t *testing.T) (*tls.Certificate, []byte) {
	t.Helper()
	leaf, caPEM, err := tlsca.LoadOrCreate(t.TempDir(), "api.anthropic.com")
	if err != nil {
		t.Fatal(err)
	}
	return leaf, caPEM
}

// A gateway that outlives its leaf: nothing is asked for while the leaf has
// more than 30 days left, and inside them the next one is fetched and
// served. Before, the leaf loaded at start was served until the process
// ended, expired or not.
func TestLeafIsReplacedBeforeItExpires(t *testing.T) {
	first, caPEM := testLeaf(t)
	next, _ := testLeaf(t)
	expires := leafExpiry(first)
	if time.Until(expires) < 300*24*time.Hour {
		t.Fatalf("setup: a new leaf should last about 397 days, this one ends %s", expires)
	}

	now := time.Now()
	loads := 0
	var newCAs int
	var buf bytes.Buffer
	l := &leafSource{
		leaf: first, caPEM: caPEM, now: func() time.Time { return now }, logger: log.New(&buf, "", 0),
		load:  func() (*tls.Certificate, []byte, error) { loads++; return next, caPEM, nil },
		newCA: func([]byte) { newCAs++ },
	}
	serving := func() *tls.Certificate {
		t.Helper()
		c, err := l.get(nil)
		if err != nil || c == nil {
			t.Fatalf("a handshake must always get a certificate: %v", err)
		}
		return c
	}

	if serving() != first || loads != 0 {
		t.Fatalf("a leaf with a year left must be served as it is, without asking (%d loads)", loads)
	}
	now = expires.Add(-leafRenewWithin - time.Hour)
	if serving() != first || loads != 0 {
		t.Fatalf("an hour before the renewal time nothing is asked (%d loads)", loads)
	}
	now = expires.Add(-leafRenewWithin + time.Hour)
	if serving() != next || loads != 1 {
		t.Fatalf("inside its last 30 days the leaf must be replaced (%d loads)", loads)
	}
	if newCAs != 0 {
		t.Fatal("the new leaf is from the same CA: there is nothing new for a session to trust")
	}
	if !strings.Contains(buf.String(), "now serving a new one") {
		t.Fatalf("the change of certificate must be in the log:\n%s", buf.String())
	}
	// The new leaf has a year again, so nothing more is asked.
	if serving() != next || loads != 1 {
		t.Fatalf("asked again for a leaf that is new (%d loads)", loads)
	}
}

// A leaf that cannot be reissued keeps being served, the failure is logged,
// and the question is not asked again on every handshake.
func TestLeafThatCannotBeReissuedIsStillServed(t *testing.T) {
	first, caPEM := testLeaf(t)
	now := leafExpiry(first).Add(-24 * time.Hour)
	loads := 0
	var buf bytes.Buffer
	l := &leafSource{
		leaf: first, caPEM: caPEM, now: func() time.Time { return now }, logger: log.New(&buf, "", 0),
		load: func() (*tls.Certificate, []byte, error) { loads++; return nil, nil, fmt.Errorf("disk full") },
	}
	for i := 0; i < 5; i++ {
		if c, err := l.get(nil); c != first || err != nil {
			t.Fatalf("handshake %d: %v %v", i, c, err)
		}
	}
	if loads != 1 || strings.Count(buf.String(), "error stage=tls_leaf") != 1 {
		t.Fatalf("asked %d times in one moment, want once:\n%s", loads, buf.String())
	}
	now = now.Add(leafRecheck + time.Minute)
	if _, _ = l.get(nil); loads != 2 {
		t.Fatalf("after %s it must ask again, loads=%d", leafRecheck, loads)
	}
}

// When the next leaf comes from another CA, sessions already open do not
// trust it, and that is said once: in the log and to whoever was given newCA.
func TestLeafFromANewCAIsAnnounced(t *testing.T) {
	first, caPEM := testLeaf(t)
	next, nextCA := testLeaf(t)
	now := leafExpiry(first).Add(-24 * time.Hour)
	var told [][]byte
	var buf bytes.Buffer
	l := &leafSource{
		leaf: first, caPEM: caPEM, now: func() time.Time { return now }, logger: log.New(&buf, "", 0),
		load:  func() (*tls.Certificate, []byte, error) { return next, nextCA, nil },
		newCA: func(pem []byte) { told = append(told, pem) },
	}
	if c, _ := l.get(nil); c != next {
		t.Fatal("the new leaf is the only one that will still be valid: it must be served")
	}
	if len(told) != 1 || !bytes.Equal(told[0], nextCA) || !strings.Contains(buf.String(), "signed by a new CA") {
		t.Fatalf("a new CA must be announced once (told %d times):\n%s", len(told), buf.String())
	}
}

// What the gateway writes to its stderr is dated like its log, and
// launchd.err.log is cut back while the gateway runs, not only at a start.
func TestStderrLogIsDatedAndBoundedWhileRunning(t *testing.T) {
	path := filepath.Join(t.TempDir(), "launchd.err.log")
	// As launchd opens it: for appending.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })

	w, ok := newStderrLog(f, path).(*logline.Writer)
	if !ok {
		t.Fatal("stderr lines must go through logline.Writer, which dates them")
	}
	sl := w.W.(*stderrLog)
	if sl.path != path {
		t.Fatal("stderr is this file: it must be recognised, or it is never cut back")
	}
	const max, keep = 600 << 10, 100 << 10
	sl.max, sl.keep = max, keep

	line := "http: TLS handshake error from 127.0.0.1:50000: EOF " + strings.Repeat("x", 200) + "\n"
	errLog := log.New(w, "", 0)
	for i := 0; i < 2*max/len(line); i++ {
		errLog.Print(line)
	}
	errLog.Print("the last line\n")

	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// It may run over by what is written between two looks.
	if st.Size() > max+stderrCheckEvery+int64(len(line)) {
		t.Fatalf("%d bytes written and the file is %d: it was not cut back while running (limit %d)", 2*max, st.Size(), max)
	}
	b, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	last := lines[len(lines)-1]
	if !strings.HasSuffix(last, "the last line") {
		t.Fatalf("the newest line must survive the cut: %q", last)
	}
	if _, ok := logline.ParseStamp(last); !ok || !strings.Contains(last, " level=") {
		t.Fatalf("a stderr line must start with the log's own timestamp: %q", last)
	}

	// A terminal, or any stderr that is not that file: nothing to cut back.
	other, err := os.Create(filepath.Join(t.TempDir(), "elsewhere"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { other.Close() })
	if got := newStderrLog(other, path).(*logline.Writer).W.(*stderrLog).path; got != "" {
		t.Fatalf("stderr is not launchd.err.log, yet %q would be cut back", got)
	}
}
