package tlsca

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"testing"
	"time"
)

// The CA must not be able to vouch for any other site, even with its key in
// hand: a certificate for another name that it signs fails verification.
func TestCAIsNameConstrainedToTheInterceptedHost(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := LoadOrCreate(dir, host); err != nil {
		t.Fatal(err)
	}
	caCertPath, caKeyPath, _, _ := Files(dir)
	ca, key, _, err := loadCA(caCertPath, caKeyPath)
	if err != nil || ca == nil {
		t.Fatalf("loadCA: %v", err)
	}
	if !constrainedTo(ca, host) {
		t.Fatalf("CA constraint: critical=%v permitted=%v", ca.PermittedDNSDomainsCritical, ca.PermittedDNSDomains)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	for name, wantOK := range map[string]bool{host: true, "github.com": false, "evil.anthropic.com.attacker.net": false} {
		leaf := signLeaf(t, ca, key, name)
		_, err := leaf.Verify(x509.VerifyOptions{DNSName: name, Roots: roots})
		if (err == nil) != wantOK {
			t.Errorf("%s: verify err=%v, want ok=%v", name, err, wantOK)
		}
	}
}

// An install from before the constraint has an unconstrained CA, trusted by
// every running Claude Code session. Migrating must give a constrained CA,
// delete the old key, and serve a chain that BOTH the old CA (sessions
// started earlier) and the new one (sessions started later) accept.
func TestMigratingAnUnconstrainedCAKeepsOldAndNewClientsWorking(t *testing.T) {
	dir := t.TempDir()
	caCertPath, caKeyPath, _, _ := Files(dir)
	legacy, legacyKey := writeLegacyCA(t, caCertPath, caKeyPath)

	leaf, caPEM, err := LoadOrCreate(dir, host)
	if err != nil {
		t.Fatal(err)
	}
	newCA, newKey, _, err := loadCA(caCertPath, caKeyPath)
	if err != nil || newCA == nil || !constrainedTo(newCA, host) {
		t.Fatalf("after migration the CA is not constrained: %v", err)
	}
	if newKey.Equal(legacyKey) {
		t.Fatal("the unconstrained key is still on disk")
	}
	if len(leaf.Certificate) != 2 {
		t.Fatalf("chain has %d certificates, want leaf + bridge", len(leaf.Certificate))
	}
	bridge, err := x509.ParseCertificate(leaf.Certificate[1])
	if err != nil {
		t.Fatal(err)
	}
	inter := x509.NewCertPool()
	inter.AddCert(bridge)

	oldRoots := x509.NewCertPool()
	oldRoots.AddCert(legacy)
	if _, err := leaf.Leaf.Verify(x509.VerifyOptions{DNSName: host, Roots: oldRoots, Intermediates: inter}); err != nil {
		t.Errorf("a session that trusts only the old CA rejects the chain: %v", err)
	}
	newRoots := x509.NewCertPool()
	newRoots.AppendCertsFromPEM(caPEM)
	if _, err := leaf.Leaf.Verify(x509.VerifyOptions{DNSName: host, Roots: newRoots, Intermediates: inter}); err != nil {
		t.Errorf("a session that trusts the new CA rejects the chain: %v", err)
	}
	if _, legacyPath := migrationFiles(dir); !fileExists(legacyPath) {
		t.Error("the old certificate was not kept for trust removal")
	}

	// Stable afterwards: a second start neither migrates again nor drops
	// the bridge while it is valid.
	again, _, err := LoadOrCreate(dir, host)
	if err != nil || len(again.Certificate) != 2 {
		t.Fatalf("second start: err=%v chain=%d", err, len(again.Certificate))
	}
}

func writeLegacyCA(t *testing.T, certPath, keyPath string) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "claude-burst local CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(365 * 24 * time.Hour),
		KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true, IsCA: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kd, _ := x509.MarshalECPrivateKey(key)
	os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0644)
	os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kd}), 0600)
	c, _ := x509.ParseCertificate(der)
	return c, key
}

func signLeaf(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, name string) *x509.Certificate {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(7), Subject: pkix.Name{CommonName: name}, DNSNames: []string{name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(der)
	return c
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }
