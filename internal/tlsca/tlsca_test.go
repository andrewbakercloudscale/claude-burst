package tlsca

import (
	"crypto/x509"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const host = "api.anthropic.com"

func TestLoadOrCreateMintsUsableLeaf(t *testing.T) {
	dir := t.TempDir()
	leaf, caPEM, err := LoadOrCreate(dir, host)
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	if leaf == nil || leaf.Leaf == nil {
		t.Fatal("no leaf returned")
	}

	// The leaf must actually chain to the returned CA and match the host --
	// otherwise Claude Code sees a TLS error rather than the gateway.
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("returned CA PEM is not parseable")
	}
	if _, err := leaf.Leaf.Verify(x509.VerifyOptions{DNSName: host, Roots: pool}); err != nil {
		t.Fatalf("leaf does not verify against returned CA: %v", err)
	}
}

func TestLoadOrCreateIsStableAcrossCalls(t *testing.T) {
	dir := t.TempDir()
	first, firstCA, err := LoadOrCreate(dir, host)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, secondCA, err := LoadOrCreate(dir, host)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if string(firstCA) != string(secondCA) {
		t.Error("CA regenerated on reload; the bundle entry would go stale every restart")
	}
	if first.Leaf.SerialNumber.Cmp(second.Leaf.SerialNumber) != 0 {
		t.Error("leaf regenerated on reload")
	}
}

func TestLoadOrCreateReissuesForDifferentHost(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := LoadOrCreate(dir, host); err != nil {
		t.Fatalf("first: %v", err)
	}
	leaf, caPEM, err := LoadOrCreate(dir, "api.example.com")
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if err := leaf.Leaf.VerifyHostname("api.example.com"); err != nil {
		t.Fatalf("leaf not reissued for new host: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)
	if _, err := leaf.Leaf.Verify(x509.VerifyOptions{DNSName: "api.example.com", Roots: pool}); err != nil {
		t.Fatalf("reissued leaf does not verify: %v", err)
	}
}

func TestKeyPermissions(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := LoadOrCreate(dir, host); err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	_, caKey, _, leafKey := Files(dir)
	for _, p := range []string{caKey, leafKey} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatalf("stat %s: %v", p, err)
		}
		if perm := info.Mode().Perm(); perm != 0600 {
			t.Errorf("%s has mode %04o, want 0600", filepath.Base(p), perm)
		}
	}
}

// --- bundle handling ---------------------------------------------------------
//
// These matter more than they look: the bundle on a corporate machine holds
// the employer's CA certificates, and mangling it breaks everything that
// machine talks to, not just this tool.

const corporate = `-----BEGIN CERTIFICATE-----
CORPORATEONE
-----END CERTIFICATE-----
-----BEGIN CERTIFICATE-----
CORPORATETWO
-----END CERTIFICATE-----
`

func TestEnsureInBundlePreservesExistingCerts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bundle.pem")
	if err := os.WriteFile(path, []byte(corporate), 0600); err != nil {
		t.Fatal(err)
	}
	if err := EnsureInBundle(path, []byte("-----BEGIN CERTIFICATE-----\nOURS\n-----END CERTIFICATE-----\n")); err != nil {
		t.Fatalf("EnsureInBundle: %v", err)
	}
	got, _ := os.ReadFile(path)
	if !strings.Contains(string(got), "CORPORATEONE") || !strings.Contains(string(got), "CORPORATETWO") {
		t.Fatal("existing corporate certificates were lost")
	}
	if !strings.Contains(string(got), "OURS") {
		t.Fatal("our CA was not added")
	}
}

func TestEnsureInBundleIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bundle.pem")
	os.WriteFile(path, []byte(corporate), 0600)
	ca := []byte("-----BEGIN CERTIFICATE-----\nOURS\n-----END CERTIFICATE-----\n")

	for i := 0; i < 3; i++ {
		if err := EnsureInBundle(path, ca); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
	got, _ := os.ReadFile(path)
	if n := strings.Count(string(got), "OURS"); n != 1 {
		t.Fatalf("CA appears %d times after 3 runs, want 1", n)
	}
	if n := strings.Count(string(got), beginMarker); n != 1 {
		t.Fatalf("%d marker blocks after 3 runs, want 1", n)
	}
}

func TestRemoveFromBundleRestoresOriginalExactly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bundle.pem")
	os.WriteFile(path, []byte(corporate), 0600)
	if err := EnsureInBundle(path, []byte("-----BEGIN CERTIFICATE-----\nOURS\n-----END CERTIFICATE-----\n")); err != nil {
		t.Fatal(err)
	}
	if err := RemoveFromBundle(path); err != nil {
		t.Fatalf("RemoveFromBundle: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != corporate {
		t.Errorf("bundle not restored byte-for-byte.\n got: %q\nwant: %q", got, corporate)
	}
}

func TestRemoveFromBundleMissingFileIsNotAnError(t *testing.T) {
	if err := RemoveFromBundle(filepath.Join(t.TempDir(), "absent.pem")); err != nil {
		t.Errorf("want nil for absent bundle, got %v", err)
	}
}

func TestStripBlock(t *testing.T) {
	block := BundleBlock([]byte("-----BEGIN CERTIFICATE-----\nX\n-----END CERTIFICATE-----\n"))

	t.Run("no block present", func(t *testing.T) {
		got, changed := StripBlock(corporate)
		if changed || got != corporate {
			t.Errorf("unchanged input was modified: changed=%v", changed)
		}
	})

	t.Run("block at end", func(t *testing.T) {
		got, changed := StripBlock(corporate + block)
		if !changed {
			t.Fatal("block not detected")
		}
		if got != corporate {
			t.Errorf("got %q, want %q", got, corporate)
		}
	})

	t.Run("block in middle", func(t *testing.T) {
		got, changed := StripBlock(corporate + block + "trailing\n")
		if !changed {
			t.Fatal("block not detected")
		}
		if got != corporate+"trailing\n" {
			t.Errorf("got %q", got)
		}
	})

	t.Run("begin without end is left alone", func(t *testing.T) {
		// Truncated file: guessing where the block ends could delete a
		// corporate certificate, so we must refuse instead.
		input := corporate + beginMarker + "\nhalf a cert\n"
		got, changed := StripBlock(input)
		if changed {
			t.Error("stripped a block with no end marker")
		}
		if got != input {
			t.Error("input modified")
		}
	})
}
