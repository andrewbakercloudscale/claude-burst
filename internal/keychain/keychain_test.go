package keychain

import (
	"testing"
	"time"
)

// TestLoadUsesCallerSpecifiedEnvVar is a regression test: Load used to
// hardcode checking AWS_BEARER_TOKEN_BEDROCK regardless of which service was
// passed, so a second secret (e.g. a Together AI key) sharing this function
// would silently receive the Bedrock env var's value instead of its own.
func TestLoadUsesCallerSpecifiedEnvVar(t *testing.T) {
	t.Setenv("TOGETHER_API_KEY", "test-together-key")
	t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "test-bedrock-key")

	v, err := Load("claude-burst-together", "TOGETHER_API_KEY")
	if err != nil {
		t.Fatal(err)
	}
	if v != "test-together-key" {
		t.Fatalf("Load returned %q, want the TOGETHER_API_KEY value, not the Bedrock one", v)
	}
}

func TestLoadDoesNotCrossContaminateBetweenEnvVars(t *testing.T) {
	t.Setenv("TOGETHER_API_KEY", "")
	t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "test-bedrock-key")

	// An empty envVar makes Load fall through to the macOS Keychain lookup
	// (see keychain.go's Load), so the service name here must not collide
	// with anything a real machine might actually have stored -- using the
	// real "claude-burst-together" service name made this test depend on
	// whether THIS Mac happened to have that credential saved from prior
	// real use, which is exactly the kind of environment-dependent flake
	// that must never gate a deploy.
	const noSuchService = "claude-burst-test-nonexistent-service"

	// Requesting the Together env var must never fall back to a DIFFERENT
	// provider's env var just because that one happens to be set.
	_, err := Load(noSuchService, "TOGETHER_API_KEY")
	if err == nil {
		t.Fatal("expected an error when TOGETHER_API_KEY is unset, even though AWS_BEARER_TOKEN_BEDROCK is set")
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
