package config

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// The lost update this exists for: two writers load the same file, each
// changes its own field, and the later save drops the earlier change. The
// sleep inside fn holds each update open long enough that, without the lock,
// both would load before either saved.
func TestUpdateConcurrentWritersBothSurvive(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		errs <- Update(func(c *Config) error {
			time.Sleep(50 * time.Millisecond)
			c.ResetGraceSeconds = 42
			return nil
		})
	}()
	go func() {
		defer wg.Done()
		errs <- Update(func(c *Config) error {
			time.Sleep(50 * time.Millisecond)
			c.MaxRequestMB = 256
			return nil
		})
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.ResetGraceSeconds != 42 || got.MaxRequestMB != 256 {
		t.Fatalf("a concurrent update was lost: reset_grace=%d max_request_mb=%d", got.ResetGraceSeconds, got.MaxRequestMB)
	}
}

// The process mutex cannot protect against the CLI and the gateway, which
// are separate processes: only the flock does. Each child process adds its
// own pricing entry; every one must be there at the end.
func TestUpdateAcrossProcessesLosesNothing(t *testing.T) {
	if os.Getenv("CB_UPDATE_CHILD") != "" {
		return
	}
	home := t.TempDir()
	const n = 6
	cmds := make([]*exec.Cmd, n)
	for i := range cmds {
		cmd := exec.Command(os.Args[0], "-test.run=^TestUpdateChildProcess$")
		cmd.Env = append(os.Environ(), "HOME="+home, "CB_UPDATE_CHILD="+fmt.Sprint(i))
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		cmds[i] = cmd
	}
	for _, c := range cmds {
		if err := c.Wait(); err != nil {
			t.Fatalf("child failed: %v", err)
		}
	}
	t.Setenv("HOME", home)
	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if _, ok := got.Pricing[fmt.Sprintf("test-model-%d", i)]; !ok {
			t.Errorf("child %d's update was lost; pricing has %d entries", i, len(got.Pricing))
		}
	}
}

func TestUpdateChildProcess(t *testing.T) {
	id := os.Getenv("CB_UPDATE_CHILD")
	if id == "" {
		t.Skip("helper for TestUpdateAcrossProcessesLosesNothing")
	}
	err := Update(func(c *Config) error {
		time.Sleep(30 * time.Millisecond)
		c.Pricing["test-model-"+id] = ModelPrice{}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestUpdateErrorWritesNothing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := Update(func(c *Config) error { c.MaxRequestMB = 64; return nil }); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("refused")
	err := Update(func(c *Config) error { c.MaxRequestMB = 512; return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("got %v, want fn's error back", err)
	}
	if got, _ := Load(); got.MaxRequestMB != 64 {
		t.Fatalf("max_request_mb=%d after a refused update, want 64", got.MaxRequestMB)
	}
}

// A hand-edited config with a typo must come back as an error, not be
// replaced by defaults plus the one field the caller wanted to change.
func TestUpdateNeverOverwritesUnparseableConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := EnsureDir(); err != nil {
		t.Fatal(err)
	}
	p, _ := ConfigPath()
	broken := []byte("{\"listen\": \"127.0.0.1:7777\",,}\n")
	if err := os.WriteFile(p, broken, 0o600); err != nil {
		t.Fatal(err)
	}
	called := false
	err := Update(func(c *Config) error { called = true; return nil })
	if !IsUnreadable(err) {
		t.Fatalf("got %v, want an UnreadableError", err)
	}
	if called {
		t.Fatal("fn ran against a config that did not parse")
	}
	if b, _ := os.ReadFile(p); string(b) != string(broken) {
		t.Fatalf("the unparseable file was rewritten: %q", b)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(p), "config.json.lock")); err != nil {
		t.Fatalf("lock file missing: %v", err)
	}
}
