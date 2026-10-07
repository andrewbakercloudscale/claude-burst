package touchid

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The child is this test binary playing the prompt: BURST_TEST_PROMPT says
// how it ends.
func TestMain(m *testing.M) {
	switch os.Getenv("BURST_TEST_PROMPT") {
	case "":
		os.Exit(m.Run())
	case "ok":
		os.Exit(helperOK)
	case "denied":
		os.Stdout.WriteString("User canceled authentication\n")
		os.Exit(helperDenied)
	case "segv":
		// What happened on 2026-09-08, in the gateway itself.
		syscall.Kill(os.Getpid(), syscall.SIGSEGV)
		time.Sleep(5 * time.Second)
	case "exit1":
		os.Stdout.WriteString("something that looks like an answer\n")
		os.Exit(1)
	case "hang":
		time.Sleep(time.Minute)
	}
	os.Exit(99)
}

func playPrompt(t *testing.T, how string) {
	t.Helper()
	old := helperCmd
	helperCmd = func(ctx context.Context, reason string) (*exec.Cmd, error) {
		cmd := exec.CommandContext(ctx, os.Args[0])
		cmd.Env = append(os.Environ(), "BURST_TEST_PROMPT="+how)
		return cmd, nil
	}
	t.Cleanup(func() { helperCmd = old })
}

func TestIsolatedPromptPassesTheAnswerOn(t *testing.T) {
	playPrompt(t, "ok")
	if err := AuthenticateIsolated("reveal the key"); err != nil {
		t.Fatalf("an authenticated owner was refused: %v", err)
	}
	playPrompt(t, "denied")
	if err := AuthenticateIsolated("reveal the key"); err == nil || err.Error() != "User canceled authentication" {
		t.Fatalf("a cancel must come back as the system's own message, got %v", err)
	}
}

// The point of the child: it dies of a fault, this process does not, and
// the answer is no.
func TestACrashInThePromptIsARefusalNotACrash(t *testing.T) {
	for _, how := range []string{"segv", "exit1"} {
		playPrompt(t, how)
		err := AuthenticateIsolated("reveal the key")
		if !errors.Is(err, ErrPromptCrashed) {
			t.Errorf("%s: got %v, want ErrPromptCrashed", how, err)
		}
		if err != nil && strings.Contains(err.Error(), "looks like an answer") {
			t.Errorf("%s: a crashed child's output was passed on: %v", how, err)
		}
	}
}

func TestAPromptThatNeverEndsTimesOut(t *testing.T) {
	old := helperWait
	helperWait = 300 * time.Millisecond
	t.Cleanup(func() { helperWait = old })
	playPrompt(t, "hang")
	start := time.Now()
	err := AuthenticateIsolated("reveal the key")
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("got %v, want a timeout", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("waited %v for a child that was to be stopped", time.Since(start))
	}
}

func TestAPromptThatCannotStartIsARefusal(t *testing.T) {
	old := helperCmd
	helperCmd = func(ctx context.Context, reason string) (*exec.Cmd, error) {
		return exec.CommandContext(ctx, "/nonexistent/claude-burst"), nil
	}
	t.Cleanup(func() { helperCmd = old })
	if err := AuthenticateIsolated("reveal the key"); err == nil {
		t.Fatal("no prompt must mean no")
	}
}

func TestTheHelperRefusesToRunWithoutAReason(t *testing.T) {
	if RunHelper(nil) == helperOK || RunHelper([]string{""}) == helperOK || RunHelper([]string{"a", "b"}) == helperOK {
		t.Fatal("the helper answered yes with nothing to ask")
	}
}
