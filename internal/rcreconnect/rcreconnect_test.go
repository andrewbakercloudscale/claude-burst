package rcreconnect

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The screen of a session that gave up, as wordpress-cyber-devtools showed
// it on 2026-11-05.
const gaveUpScreen = `⏺ Remote Control disconnected — the connection to the Remote Control
  server kept dropping after each reconnect — run /remote-control to
  reconnect
──────────────────────────────────────────────────────────
❯
──────────────────────────────────────────────────────────
  ⏵⏵ auto mode on (shift+tab to cycle)`

type rig struct {
	t       *testing.T
	dir     string
	now     time.Time
	net     bool
	idle    time.Duration
	screen  string
	typed   []string
	logs    []string
	w       *Watcher
	typeErr error
}

func newRig(t *testing.T) *rig {
	r := &rig{t: t, dir: t.TempDir(), now: time.Date(2026, 11, 5, 20, 0, 0, 0, time.Local), net: true,
		idle: 10 * time.Minute, screen: gaveUpScreen}
	r.w = New(Deps{
		SessionsDir: r.dir,
		Alive:       func(int) bool { return true },
		NetworkUp:   func() bool { return r.net },
		HandsOff:    func() (time.Duration, error) { return r.idle, nil },
		Screen:      func(string, int) (string, error) { return r.screen, nil },
		Type: func(id, text string) error {
			if r.typeErr != nil {
				return r.typeErr
			}
			r.typed = append(r.typed, id+" "+text)
			return nil
		},
		Now:  func() time.Time { return r.now },
		Logf: func(f string, a ...any) { r.logs = append(r.logs, fmt.Sprintf(f, a...)) },
	})
	return r
}

func (r *rig) session(pid int, status, bridge string, idleFor time.Duration) {
	b, _ := json.Marshal(Session{Pid: pid, ID: fmt.Sprintf("sess-%d", pid), Name: "proj", Status: status,
		StatusUpdatedAt: r.now.Add(-idleFor).UnixMilli(), Bridge: bridge})
	if err := os.WriteFile(filepath.Join(r.dir, fmt.Sprintf("%d.json", pid)), b, 0o600); err != nil {
		r.t.Fatal(err)
	}
}

func TestTypesIntoASessionThatGaveUp(t *testing.T) {
	r := newRig(t)
	r.session(1, "idle", "", 5*time.Minute)
	r.w.Tick()
	if len(r.typed) != 1 || r.typed[0] != "sess-1 /remote-control" {
		t.Fatalf("typed %q", r.typed)
	}
	// Bridge back: logged, and the state forgotten.
	r.session(1, "idle", "session_x", 5*time.Minute)
	r.w.Tick()
	if !strings.Contains(strings.Join(r.logs, "\n"), "connected again") {
		t.Fatalf("logs %q", r.logs)
	}
}

func TestNeverTypesWhenItIsNotSafe(t *testing.T) {
	cases := []struct {
		name string
		set  func(r *rig)
	}{
		{"connected", func(r *rig) { r.session(1, "idle", "session_x", 5*time.Minute) }},
		{"busy", func(r *rig) { r.session(1, "busy", "", 5*time.Minute) }},
		{"only just idle", func(r *rig) { r.session(1, "idle", "", 10*time.Second) }},
		{"network down", func(r *rig) { r.session(1, "idle", "", 5*time.Minute); r.net = false }},
		{"someone at the Mac", func(r *rig) { r.session(1, "idle", "", 5*time.Minute); r.idle = 20 * time.Second }},
		{"turned off on purpose: no disconnect line", func(r *rig) {
			r.session(1, "idle", "", 5*time.Minute)
			r.screen = "❯ \n"
		}},
		{"reconnected since", func(r *rig) {
			r.session(1, "idle", "", 5*time.Minute)
			r.screen = gaveUpScreen + "\n/remote-control is active · Continue here\n❯ \n"
		}},
		{"half typed prompt", func(r *rig) {
			r.session(1, "idle", "", 5*time.Minute)
			r.screen = strings.Replace(gaveUpScreen, "❯", "❯ fix the build", 1)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t)
			c.set(r)
			r.w.Tick()
			if len(r.typed) != 0 {
				t.Fatalf("typed %q", r.typed)
			}
		})
	}
}

func TestAttemptsBackOff(t *testing.T) {
	r := newRig(t)
	r.session(1, "idle", "", 5*time.Minute)
	r.w.Tick() // attempt 1
	r.now = r.now.Add(time.Minute)
	r.session(1, "idle", "", 5*time.Minute)
	r.w.Tick() // too soon
	r.now = r.now.Add(90 * time.Second)
	r.session(1, "idle", "", 5*time.Minute)
	r.w.Tick() // 2.5m after: attempt 2
	if len(r.typed) != 2 {
		t.Fatalf("want 2 attempts, got %d", len(r.typed))
	}
	r.now = r.now.Add(4 * time.Minute)
	r.session(1, "idle", "", 5*time.Minute)
	r.w.Tick() // 4m after the second, wait is 5m
	if len(r.typed) != 2 {
		t.Fatalf("third attempt came early")
	}
}

func TestOneSessionPerTickAndFailureLogged(t *testing.T) {
	r := newRig(t)
	r.session(1, "idle", "", 5*time.Minute)
	r.session(2, "idle", "", 5*time.Minute)
	r.w.Tick()
	if len(r.typed) != 1 {
		t.Fatalf("typed into %d sessions in one tick", len(r.typed))
	}
	r.w.Tick()
	if len(r.typed) != 2 {
		t.Fatalf("second session not reached on the next tick: %q", r.typed)
	}
	r2 := newRig(t)
	r2.typeErr = errors.New("no answer from Ghostty Dock in 5s")
	r2.session(1, "idle", "", 5*time.Minute)
	r2.w.Tick()
	if !strings.Contains(strings.Join(r2.logs, "\n"), "could not type") {
		t.Fatalf("failure not logged: %q", r2.logs)
	}
}

func TestPromptEmpty(t *testing.T) {
	for in, want := range map[string]bool{
		"❯ \n":                    true,
		"❯\n":                     true,
		"  ❯   │\n":               true,
		"❯ /remote-control\n❯ \n": true, // history above, empty input below
		"❯ \n❯ hello\n":           false,
		"no prompt here\n":        false,
	} {
		if got := PromptEmpty(in); got != want {
			t.Errorf("%q: got %v want %v", in, got, want)
		}
	}
}

func TestDockCall(t *testing.T) {
	dir := t.TempDir()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			m, _ := filepath.Glob(filepath.Join(dir, "req-*.json"))
			if len(m) == 1 {
				b, _ := os.ReadFile(m[0])
				var req map[string]any
				_ = json.Unmarshal(b, &req)
				if req["op"] != "send" || req["text"] != "/remote-control" || req["enter"] != true {
					t.Errorf("request %v", req)
				}
				id := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(m[0]), "req-"), ".json")
				_ = os.Remove(m[0])
				_ = os.WriteFile(filepath.Join(dir, "res-"+id+".json"), []byte(`{"ok":true}`), 0o600)
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Error("no request written")
	}()
	if err := dockCall(dir, map[string]any{"op": "send", "target": "x", "text": "/remote-control", "enter": true}, nil); err != nil {
		t.Fatal(err)
	}
	<-done
	if err := dockCall(filepath.Join(dir, "missing"), map[string]any{}, nil); err == nil {
		t.Fatal("no Dock must be an error")
	}
}
