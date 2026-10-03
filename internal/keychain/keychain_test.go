package keychain

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// stubDir holds one security stub for the whole package: macOS scans every
// new executable on its first run, which costs about half a second, so the
// stub is written once and each test sets what it answers.
var stubDir string

func TestMain(m *testing.M) {
	d, err := os.MkdirTemp("", "keychain-stub")
	if err != nil {
		panic(err)
	}
	stubDir = d
	body := "#!/bin/sh\nd=\"$(dirname \"$0\")\"\necho \"$*\" >> \"$d/calls\"\ncat \"$d/out\"\nexit $(cat \"$d/code\")\n"
	if err := os.WriteFile(filepath.Join(d, "security"), []byte(body), 0o755); err != nil {
		panic(err)
	}
	code := m.Run()
	os.RemoveAll(d)
	os.Exit(code)
}

// stubSecurity points the package at the stub, which logs its argv, prints
// out and exits with code. It returns a func reading the calls so far.
func stubSecurity(t *testing.T, out string, code int) func() []string {
	t.Helper()
	for name, v := range map[string]string{"out": out, "code": strconv.Itoa(code), "calls": ""} {
		if err := os.WriteFile(filepath.Join(stubDir, name), []byte(v), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	old := securityPath
	securityPath = filepath.Join(stubDir, "security")
	t.Cleanup(func() { securityPath = old })
	return func() []string {
		b, _ := os.ReadFile(filepath.Join(stubDir, "calls"))
		if s := strings.TrimSpace(string(b)); s != "" {
			return strings.Split(s, "\n")
		}
		return nil
	}
}

// TestLoadUsesCallerSpecifiedEnvVar is a regression test: Load used to
// hardcode checking AWS_BEARER_TOKEN_BEDROCK regardless of which service was
// passed, so a second secret (e.g. a Together AI key) sharing this function
// would silently receive the Bedrock env var's value instead of its own.
func TestLoadUsesCallerSpecifiedEnvVar(t *testing.T) {
	calls := stubSecurity(t, "from-keychain", 0)
	t.Setenv("TOGETHER_API_KEY", "test-together-key")
	t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "test-bedrock-key")

	v, err := Load("claude-burst-together", "TOGETHER_API_KEY")
	if err != nil {
		t.Fatal(err)
	}
	if v != "test-together-key" {
		t.Fatalf("Load returned %q, want the TOGETHER_API_KEY value, not the Bedrock one", v)
	}
	if c := calls(); c != nil {
		t.Fatalf("the env var was set: the Keychain must not be asked, got %q", c)
	}
}

func TestLoadDoesNotCrossContaminateBetweenEnvVars(t *testing.T) {
	t.Setenv("TOGETHER_API_KEY", "")
	t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "test-bedrock-key")

	// An empty envVar makes Load fall through to the Keychain lookup, which
	// is stubbed to "not found" (exit 44): the answer must not depend on
	// whether this Mac happens to hold a real credential, and the test must
	// never read the real login Keychain.
	calls := stubSecurity(t, "security: SecKeychainSearchCopyNext: The specified item could not be found in the keychain.", 44)
	const noSuchService = "claude-burst-test-nonexistent-service"

	// Requesting the Together env var must never fall back to a DIFFERENT
	// provider's env var just because that one happens to be set.
	_, err := Load(noSuchService, "TOGETHER_API_KEY")
	if err == nil {
		t.Fatal("expected an error when TOGETHER_API_KEY is unset, even though AWS_BEARER_TOKEN_BEDROCK is set")
	}
	c := calls()
	if len(c) != 1 || !strings.HasPrefix(c[0], "find-generic-password ") || !strings.HasSuffix(c[0], "-s "+noSuchService+" -w") {
		t.Fatalf("the stub security was not the one asked: %q", c)
	}
}

func TestLoadFromKeychainTrimsAndRejectsEmpty(t *testing.T) {
	t.Setenv("SOME_KEY", "")
	calls := stubSecurity(t, "  sk-test\n", 0)
	if v, err := Load("svc", "SOME_KEY"); err != nil || v != "sk-test" {
		t.Fatalf("Load = %q, %v", v, err)
	}
	if len(calls()) != 1 {
		t.Fatal("stub not called")
	}
	stubSecurity(t, "   ", 0)
	if _, err := Load("svc", "SOME_KEY"); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("an empty stored key must be an error, got %v", err)
	}
}

func TestStoreAndDelete(t *testing.T) {
	calls := stubSecurity(t, "", 0)
	if err := Store("svc", ""); err == nil || calls() != nil {
		t.Fatal("an empty key must be refused before the Keychain is touched")
	}
	if err := Store("svc", "v1"); err != nil {
		t.Fatal(err)
	}
	if c := calls(); len(c) != 1 || !strings.HasPrefix(c[0], "add-generic-password -U -a ") || !strings.HasSuffix(c[0], "-s svc -w v1") {
		t.Fatalf("store calls %q", c)
	}
	if err := Delete("svc"); err != nil {
		t.Fatal(err)
	}

	// Nothing stored is success: exit 44 is errSecItemNotFound.
	calls = stubSecurity(t, "not found", 44)
	if err := Delete("svc"); err != nil || len(calls()) != 1 {
		t.Fatalf("delete of nothing: %v", err)
	}
	// Any other failure is reported, never read as removed.
	stubSecurity(t, "User interaction is not allowed.", 36)
	if err := Delete("svc"); err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("a locked Keychain must fail the delete, got %v", err)
	}
	if err := Store("svc", "v"); err == nil {
		t.Fatal("a failed add must be an error")
	}
}

func TestDescribe(t *testing.T) {
	t.Setenv("DESC_KEY", "x")
	calls := stubSecurity(t, "", 0)
	if i := Describe("svc", "DESC_KEY"); !i.Present || i.Source != "environment" || calls() != nil {
		t.Fatalf("env var set: %+v", i)
	}
	t.Setenv("DESC_KEY", "")
	stubSecurity(t, `    "mdat"<timedate>=0x32303236303930383132333033345A00  "20260908123034Z\000"`, 0)
	if i := Describe("svc", "DESC_KEY"); !i.Present || i.Source != "keychain" || i.Modified.IsZero() {
		t.Fatalf("in the Keychain: %+v", i)
	}
	stubSecurity(t, "", 44)
	if i := Describe("svc", "DESC_KEY"); i.Present {
		t.Fatalf("nowhere: %+v", i)
	}
}

// TestParseKeychainDate pins the parse of `security`'s hex date attribute,
// and that it comes back as local time. A silent failure here degrades to
// "no timestamp" -- exactly the uninformative state the field was added to
// remove -- so it needs a test rather than a glance.
func TestParseKeychainDate(t *testing.T) {
	// Real `security find-generic-password` output, trimmed.
	out := []byte("keychain: \"/Users/x/Library/Keychains/login.keychain-db\"\n" +
		"    \"acct\"<blob>=\"x\"\n" +
		"    \"mdat\"<timedate>=0x32303236303930383132333033345A00  \"20260908123034Z\\000\"\n" +
		"    \"svce\"<blob>=\"claude-burst-together\"\n")
	got := parseKeychainDate(out)
	if got.IsZero() {
		t.Fatal("failed to parse the mdat attribute")
	}
	if !got.Equal(time.Date(2026, 9, 8, 12, 30, 34, 0, time.UTC)) {
		t.Errorf("parsed %v, want 2026-09-08 12:30:34 UTC", got.UTC())
	}
	// Keychain records UTC; this dashboard shows local time everywhere, and
	// a UTC timestamp beside local ones reads as hours stale.
	if got.Location() != time.Local {
		t.Errorf("returned location %v, want local", got.Location())
	}
}

// TestParseKeychainDateGarbage: no date attribute, or an unparseable one,
// must degrade to zero rather than to a wrong time or a panic.
func TestParseKeychainDateGarbage(t *testing.T) {
	for _, in := range []string{"", "no attributes here", `"mdat"<timedate>=0xZZZZ`, `"mdat"<timedate>=0x4142`} {
		if got := parseKeychainDate([]byte(in)); !got.IsZero() {
			t.Errorf("parseKeychainDate(%q) = %v, want zero", in, got)
		}
	}
}

// A security call that hangs (a locked Keychain's dialog) gives up at the
// timeout with an error that says why, instead of hanging its caller.
func TestLoadTimesOut(t *testing.T) {
	hang := filepath.Join(t.TempDir(), "security")
	if err := os.WriteFile(hang, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	oldPath, oldTimeout := securityPath, timeout
	securityPath, timeout = hang, 200*time.Millisecond
	t.Cleanup(func() { securityPath, timeout = oldPath, oldTimeout })
	t.Setenv("CLAUDE_BURST_TEST_UNSET_KEY", "")

	start := time.Now()
	_, err := Load("claude-burst-test", "CLAUDE_BURST_TEST_UNSET_KEY")
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("Load waited %s", took)
	}
	if err == nil || !strings.Contains(err.Error(), "did not answer in time") {
		t.Fatalf("want a timeout error, got %v", err)
	}
}
