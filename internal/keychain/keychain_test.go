package keychain

import "testing"

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
