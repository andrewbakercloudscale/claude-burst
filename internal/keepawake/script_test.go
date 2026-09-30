package keepawake

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// desired runs the real root script's no-root "desired" check against a
// temporary state directory, with the lid state given.
func desired(t *testing.T, dir, lid string) string {
	t.Helper()
	cmd := exec.Command("zsh", "../../scripts/lid-awake-root.sh", "desired", "always")
	cmd.Env = append(os.Environ(), "CLAUDE_BURST_ROOT_STATE_DIR="+dir, "CLAUDE_BURST_TEST_LID="+lid)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("desired: %v: %s", err, out)
	}
	return strings.TrimSpace(string(out))
}

// The idle window: awake with the lid shut for IDLE minutes after the lid
// was last open or Claude Code was last used, then normal sleep.
func TestIdleWindow(t *testing.T) {
	dir := t.TempDir()
	lidopen := filepath.Join(dir, "lid-awake.lidopen")
	if got := desired(t, dir, "shut"); got != "1" {
		t.Fatalf("no window set: always awake, got %s", got)
	}
	os.WriteFile(filepath.Join(dir, "lid-awake.idle"), []byte("30\n"), 0o644)
	os.WriteFile(lidopen, nil, 0o644)
	if got := desired(t, dir, "shut"); got != "1" {
		t.Errorf("lid shut a moment ago: still awake, got %s", got)
	}
	old := time.Now().Add(-2 * time.Hour)
	os.Chtimes(lidopen, old, old)
	if got := desired(t, dir, "shut"); got != "0" {
		t.Errorf("shut for 2 hours, nothing used: sleep allowed, got %s", got)
	}
	// Recent activity in a file outside ~/.config/claude-burst is ignored:
	// the root daemon reads only the gateway's own file.
	bogus := filepath.Join(dir, "last-activity")
	os.WriteFile(bogus, nil, 0o644)
	os.WriteFile(filepath.Join(dir, "lid-awake.activity"), []byte(bogus+"\n"), 0o644)
	if got := desired(t, dir, "shut"); got != "0" {
		t.Errorf("activity path not allowed: ignored, got %s", got)
	}
	if got := desired(t, dir, "open"); got != "1" {
		t.Errorf("lid open: in use, got %s", got)
	}
	if got := desired(t, dir, "shut"); got != "1" {
		t.Errorf("lid just closed after being open: new window, got %s", got)
	}
}
