package touchid

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// A fault inside the system's authentication code is a signal, not a Go
// panic: recover() cannot catch it and neither can @try, so in the gateway's
// own process it ends the gateway and every Claude Code request in flight
// (2026-09-08, the one time it happened). The only containment is another
// process. AuthenticateIsolated runs the prompt in a child of this same
// binary, and a child that dies is a refusal: nothing is revealed and the
// gateway carries on.

// HelperCommand is the hidden subcommand the child runs.
const HelperCommand = "touchid-prompt"

// What the child exits with. Anything else, a signal included, is a crash.
const (
	helperOK     = 0
	helperDenied = 3
)

// helperWait is how long the parent waits for the child: the prompt's own
// limit, and a little for the process to start and stop.
var helperWait = (timeoutSeconds + 15) * time.Second

// helperCmd builds the child. A var for the tests.
var helperCmd = func(ctx context.Context, reason string) (*exec.Cmd, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return exec.CommandContext(ctx, exe, HelperCommand, reason), nil
}

// ErrPromptCrashed is returned when the child did not end with an answer.
var ErrPromptCrashed = errors.New("the authentication prompt stopped unexpectedly, so nothing was shown; try again")

// AuthenticateIsolated is Authenticate in a child process. It returns nil
// only when the child reported that the device owner authenticated.
func AuthenticateIsolated(reason string) error {
	ctx, cancel := context.WithTimeout(context.Background(), helperWait)
	defer cancel()
	cmd, err := helperCmd(ctx, reason)
	if err != nil {
		return fmt.Errorf("could not start the authentication prompt: %w", err)
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	err = cmd.Run()
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return errors.New("timed out waiting for authentication")
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == helperDenied {
		if msg := strings.TrimSpace(out.String()); msg != "" {
			return errors.New(msg)
		}
		return errors.New("authentication failed")
	}
	if errors.As(err, &ee) {
		return fmt.Errorf("%w (%s)", ErrPromptCrashed, ee.ProcessState.String())
	}
	return fmt.Errorf("could not start the authentication prompt: %w", err)
}

// RunHelper is the child's side: it shows the prompt and returns the exit
// code that says how it was answered.
func RunHelper(args []string) int {
	if len(args) != 1 || args[0] == "" {
		fmt.Fprintln(os.Stderr, "this command is run by the gateway, not by hand")
		return 2
	}
	if err := Authenticate(args[0]); err != nil {
		fmt.Println(err.Error())
		return helperDenied
	}
	return helperOK
}
