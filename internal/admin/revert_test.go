package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
	"github.com/andrewbakercloudscale/claude-burst/internal/tlsca"
)

func postRevert(t *testing.T, s *Server) (*httptest.ResponseRecorder, revertResponse) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/revert", nil)
	req.Header.Set(mutationHeader, "1")
	req.Host = "127.0.0.1:7788"
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	var out revertResponse
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decoding revert response %q: %v", rec.Body.String(), err)
		}
	}
	return rec, out
}

// TestRevertRunsRollbackScript is the test this change exists for.
//
// On 2026-09-07 the transparent redirect was left half-installed -- /etc/hosts
// still pointed api.anthropic.com at 127.0.0.1 while the pf rdr rule had gone
// missing -- and every process on the Mac got "connection refused". Revert
// could not fix that: it edited settings.json and the CA bundle in-process and
// only PRINTED the sudo command for the machine-wide half. The one thing that
// did recover the machine, scripts/rollback.sh, had to be found and run by
// hand. So the button must now actually run it.
func TestRevertRunsRollbackScript(t *testing.T) {
	s, scripts := newInstallServer(t)
	if err := os.WriteFile(filepath.Join(scripts, "rollback.sh"), []byte("#!/bin/zsh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	var launched string
	orig := launchTerminal
	launchTerminal = func(p string) error { launched = p; return nil }
	t.Cleanup(func() { launchTerminal = orig })

	rec, out := postRevert(t, s)
	if rec.Code != http.StatusOK {
		t.Fatalf("revert returned %d: %s", rec.Code, rec.Body.String())
	}
	if out.Script == "" || launched != out.Script {
		t.Fatalf("expected the generated script to be opened in Terminal; launched=%q script=%q", launched, out.Script)
	}
	body, err := os.ReadFile(out.Script)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "rollback.sh") {
		t.Errorf("generated script does not run rollback.sh:\n%s", body)
	}
	// A second implementation of the recovery path is how one of them ends up
	// wrong, so assert the script delegates rather than reimplementing the
	// steps rollback.sh owns.
	for _, forbidden := range []string{"/etc/hosts", "pfctl", "security delete-certificate"} {
		if strings.Contains(string(body), forbidden) && !strings.Contains(string(body), "echo") {
			t.Errorf("generated script reimplements %q instead of delegating to rollback.sh", forbidden)
		}
	}
	// rollback.sh exits 2 when it cannot verify direct connectivity -- exactly
	// the run whose output must not disappear with the window.
	if !strings.Contains(string(body), "Press any key to close this window") {
		t.Errorf("generated script does not hold the Terminal window open:\n%s", body)
	}
}

// TestRevertReportsScriptWhenTerminalFails: the script exists and is runnable
// even when the window never opened, so the response must still name it rather
// than reading as "nothing happened".
func TestRevertReportsScriptWhenTerminalFails(t *testing.T) {
	s, scripts := newInstallServer(t)
	if err := os.WriteFile(filepath.Join(scripts, "rollback.sh"), []byte("#!/bin/zsh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	orig := launchTerminal
	launchTerminal = func(string) error { return os.ErrPermission }
	t.Cleanup(func() { launchTerminal = orig })

	rec, out := postRevert(t, s)
	if rec.Code != http.StatusOK {
		t.Fatalf("revert returned %d: %s", rec.Code, rec.Body.String())
	}
	if out.Script == "" {
		t.Fatal("response did not name the generated script")
	}
	if !strings.Contains(out.Detail, out.Script) {
		t.Errorf("detail should tell the user to run the script themselves, got %q", out.Detail)
	}
}

// TestRevertFallbackKeepsCAWhileHostsRedirectStands.
//
// The in-process fallback (no scripts/ directory) removed the CA from the
// trust bundle whether or not /etc/hosts still redirected at the gateway.
// That ordering makes a broken machine worse rather than better: requests
// keep arriving here and now fail TLS instead of being proxied -- the state
// interceptActive itself calls "worse than not intercepting". Observed live
// on 2026-09-07, one click before the redirect could be removed.
func TestRevertFallbackKeepsCAWhileHostsRedirectStands(t *testing.T) {
	s := newTestServer(t)
	// rootHelper points at a path that does not exist, so scriptsDir() fails
	// and handleRevert takes the in-process fallback.
	if _, ok := s.scriptsDir(); ok {
		t.Fatal("test setup: expected no scripts directory")
	}

	home := os.Getenv("HOME")
	bundle := filepath.Join(home, ".claude", "certs", "node-extra-ca-certs.pem")
	if err := os.MkdirAll(filepath.Dir(bundle), 0o700); err != nil {
		t.Fatal(err)
	}
	// Name the bundle in config.json rather than relying on the default,
	// which prefers $NODE_EXTRA_CA_CERTS -- on a developer machine that
	// points at the REAL bundle, and this test removes what it finds.
	cfgDir := filepath.Join(home, ".config", "claude-burst")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Intercept.Mode = config.InterceptTransparent
	cfg.Intercept.CABundle = bundle
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	// Written through tlsca itself rather than with a hand-typed marker: a
	// literal that drifts from beginMarker would make this test pass by
	// asserting nothing.
	if err := tlsca.EnsureInBundle(bundle, []byte("-----BEGIN CERTIFICATE-----\nx\n-----END CERTIFICATE-----\n")); err != nil {
		t.Fatal(err)
	}

	rec, out := postRevert(t, s)
	if rec.Code != http.StatusOK {
		t.Fatalf("revert returned %d: %s", rec.Code, rec.Body.String())
	}

	// Whether /etc/hosts on the machine running this test has a redirect is
	// not ours to control, so assert the invariant that must hold either way:
	// the CA is removed only when nothing is being redirected at us.
	kept := strings.Contains(strings.Join(out.Steps, "\n"), "left the local CA in place")
	after, errRead := os.ReadFile(bundle)
	if errRead != nil {
		t.Fatal(errRead)
	}
	stillTrusted := tlsca.HasBlock(string(after))
	if kept != stillTrusted {
		t.Errorf("response says CA kept=%v but the bundle says trusted=%v", kept, stillTrusted)
	}
	if out.NeedsRoot != kept {
		t.Errorf("needs_root=%v should match the outstanding hosts redirect (kept=%v)", out.NeedsRoot, kept)
	}
}
