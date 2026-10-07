package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andrewbakercloudscale/claude-burst/internal/notice"
)

func TestOnlyCommandsThatChangeSomethingAreAudited(t *testing.T) {
	for args, want := range map[string]string{
		"disable":                             "disable",
		"enable --mode transparent":           "enable --mode",
		"keychain-set --provider together":    "keychain-set --provider",
		"configure --secondary-key=sk-SECRET": "configure --secondary-key",
		"configure --secondary-key sk-SECRET": "configure --secondary-key",
		"codex enable":                        "codex enable",
		"codex disable":                       "codex disable",
		"codex status":                        "",
		"codex":                               "",
		"shunt guard":                         "",
		"shunt disable":                       "shunt disable",
		"status":                              "",
		"stats --days 7":                      "",
		"notice --kind x --title y":           "",
		"coord say hello":                     "",
		"serve":                               "",
		"force-secondary --for 30m":           "force-secondary --for",
		"restore-config":                      "restore-config",
		"":                                    "",
	} {
		if got := auditedCommand(strings.Fields(args)); got != want {
			t.Errorf("auditedCommand(%q) = %q, want %q", args, got, want)
		}
		if strings.Contains(auditedCommand(strings.Fields(args)), "SECRET") {
			t.Errorf("auditedCommand(%q) kept a flag's value", args)
		}
	}
}

func auditIn(home string) []notice.Event {
	return notice.ReadAudit(filepath.Join(home, ".config", "claude-burst", "audit.jsonl"), 50)
}

func TestACommandIsRecordedOnceWithHowItEnded(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cliAction = "disable"
	recordCLI(notice.Info, "done")
	recordCLI(notice.Warn, "failed: again") // already recorded: nothing
	cliAction = ""
	recordCLI(notice.Info, "done") // not an audited command: nothing

	evs := auditIn(home)
	if len(evs) != 1 {
		t.Fatalf("audit holds %d entries, want 1: %+v", len(evs), evs)
	}
	e := evs[0]
	if e.Kind != "action" || e.Title != "Command line: claude-burst disable" || e.Detail != "done" || e.Severity != notice.Info || !e.AuditOnly {
		t.Errorf("entry = %+v", e)
	}
}

// fatal exits, so it is run in a child: the failure must be in the audit
// with its reason before the process is gone.
func TestAFailedCommandIsRecordedAsFailed(t *testing.T) {
	if os.Getenv("BURST_TEST_FATAL") == "1" {
		cliAction = "ca-rotate"
		fatal(errors.New("a Claude Code session is running"))
		return
	}
	home := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=TestAFailedCommandIsRecordedAsFailed")
	cmd.Env = append(os.Environ(), "BURST_TEST_FATAL=1", "HOME="+home)
	if err := cmd.Run(); err == nil {
		t.Fatal("fatal must exit non-zero")
	}
	evs := auditIn(home)
	if len(evs) != 1 || evs[0].Severity != notice.Warn || evs[0].Detail != "failed: a Claude Code session is running" ||
		evs[0].Title != "Command line: claude-burst ca-rotate" {
		t.Fatalf("audit = %+v", evs)
	}
}

// The scripts write the audit with scripts/audit-add.sh, which must produce
// what the gateway reads: same file, same fields, a time it can parse.
func TestTheScriptsAuditEntryIsReadByTheGateway(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "sub", "audit.jsonl")
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("/bin/sh", append([]string{"../../scripts/audit-add.sh"}, args...)...)
		cmd.Env = append(os.Environ(), "CLAUDE_BURST_AUDIT_FILE="+file)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("audit-add.sh %v: %v\n%s", args, err, out)
		}
	}
	run("script", "warn", `Script: rollback.sh`, `failed (exit 2) "quoted" $HOME \ back`)
	run("watchdog", "nonsense", "Watchdog: killed a hung gateway")
	run("too", "few") // refused, and never an error for its caller

	// With Claude Burst's folder gone (uninstall --purge) nothing is written
	// and the folder is not put back.
	gone := filepath.Join(dir, "purged", "audit.jsonl")
	cmd := exec.Command("/bin/sh", "../../scripts/audit-add.sh", "script", "info", "Script: install.sh uninstall", "done")
	cmd.Env = append(os.Environ(), "CLAUDE_BURST_AUDIT_FILE="+gone)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("audit-add.sh with no folder: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Dir(gone)); err == nil {
		t.Error("audit-add.sh created the folder it was meant to leave alone")
	}

	evs := notice.ReadAudit(file, 10)
	if len(evs) != 2 {
		t.Fatalf("audit holds %d entries, want 2: %+v", len(evs), evs)
	}
	newest, first := evs[0], evs[1]
	if first.Kind != "script" || first.Severity != "warn" || first.Title != "Script: rollback.sh" ||
		first.Detail != `failed (exit 2) "quoted" $HOME \ back` || !first.AuditOnly {
		t.Errorf("first = %+v", first)
	}
	if first.At.IsZero() || first.TS == 0 || first.At.Unix() != first.TS || first.ID == "" {
		t.Errorf("first has no usable time or id: %+v", first)
	}
	if newest.Severity != "info" || newest.Detail != "" {
		t.Errorf("an unknown severity must become info, got %+v", newest)
	}
	if st, err := os.Stat(file); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("audit file mode = %v, %v, want 0600", st.Mode().Perm(), err)
	}

	// Past the limit the file moves aside and both are still read.
	if err := os.WriteFile(file, append(make([]byte, 0, notice.AuditMax+2), []byte(strings.Repeat(" ", notice.AuditMax+1)+"\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	run("script", "ok", "Script: repair.sh", "done")
	if _, err := os.Stat(file + ".1"); err != nil {
		t.Errorf("the full file was not moved aside: %v", err)
	}
	if evs := notice.ReadAudit(file, 10); len(evs) != 1 || evs[0].Title != "Script: repair.sh" {
		t.Errorf("after the move the audit reads %+v", evs)
	}
}
