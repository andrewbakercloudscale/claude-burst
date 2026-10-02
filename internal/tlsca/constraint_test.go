package tlsca

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
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
// every running Claude Code session. Starting the gateway must keep using
// it: on 2026-10-02 replacing it at start (with a "bridge" chain meant to
// keep old sessions working) failed every running session with
// SELF_SIGNED_CERT_IN_CHAIN. The leaf must chain to the existing CA alone,
// checked by real Node TLS when node is installed, since Go's verifier
// accepted the bridge chain that Node refused.
func TestStartingKeepsAnUnconstrainedCA(t *testing.T) {
	dir := t.TempDir()
	caCertPath, caKeyPath, _, _ := Files(dir)
	legacy, legacyKey := writeLegacyCA(t, caCertPath, caKeyPath)

	leaf, caPEM, err := LoadOrCreate(dir, host)
	if err != nil {
		t.Fatal(err)
	}
	ca, key, _, err := loadCA(caCertPath, caKeyPath)
	if err != nil || ca == nil || !key.Equal(legacyKey) {
		t.Fatalf("starting replaced the CA running sessions trust: %v", err)
	}
	if len(leaf.Certificate) != 1 {
		t.Fatalf("chain has %d certificates, want the leaf alone", len(leaf.Certificate))
	}
	if err := leaf.Leaf.CheckSignatureFrom(legacy); err != nil {
		t.Fatalf("leaf not signed by the existing CA: %v", err)
	}
	if Constrained(dir, host) {
		t.Error("Constrained says yes for an unconstrained CA")
	}
	nodeAccepts(t, dir, caPEM)
}

// Rotate gives a constrained CA, deletes the old key, keeps the old
// certificate for trust removal, and the new chain works in Node.
func TestRotateReplacesTheCA(t *testing.T) {
	dir := t.TempDir()
	caCertPath, caKeyPath, _, _ := Files(dir)
	_, legacyKey := writeLegacyCA(t, caCertPath, caKeyPath)
	if _, _, err := LoadOrCreate(dir, host); err != nil {
		t.Fatal(err)
	}
	caPEM, err := Rotate(dir, host)
	if err != nil {
		t.Fatal(err)
	}
	_, key, _, err := loadCA(caCertPath, caKeyPath)
	if err != nil || key == nil || key.Equal(legacyKey) {
		t.Fatalf("the old key survived rotation: %v", err)
	}
	if !Constrained(dir, host) {
		t.Error("rotated CA is not constrained")
	}
	if !fileExists(legacyFile(dir)) {
		t.Error("the old certificate was not kept for trust removal")
	}
	nodeAccepts(t, dir, caPEM)
}

// nodeAccepts serves dir's leaf chain over TLS and connects with Node
// trusting only caPEM, the way Claude Code does with NODE_EXTRA_CA_CERTS.
func nodeAccepts(t *testing.T, dir string, caPEM []byte) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Log("node not installed: Node TLS check skipped")
		return
	}
	leaf, _, err := LoadOrCreate(dir, host)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{*leaf}})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.(*tls.Conn).Handshake()
			c.Close()
		}
	}()
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	os.WriteFile(caFile, caPEM, 0600)
	script := `const tls=require("tls"),fs=require("fs");
const c=tls.connect({port:+process.argv[1],host:"127.0.0.1",servername:"` + host + `",ca:[fs.readFileSync(process.argv[2])]},()=>{console.log("OK");c.end()});
c.on("error",e=>{console.log(e.code||e.message);process.exit(1)});`
	out, err := exec.Command(node, "-e", script, fmt.Sprint(ln.Addr().(*net.TCPAddr).Port), caFile).CombinedOutput()
	if err != nil {
		t.Fatalf("Node rejected the chain: %s", out)
	}
}

func writeLegacyCA(t *testing.T, certPath, keyPath string) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		// The shape real installs have: same name as a new CA, pathlen:0.
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{Organization: []string{"claude-burst local intercept"}, CommonName: "claude-burst local CA"},
		NotBefore:    time.Now().Add(-time.Hour), NotAfter: time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true, IsCA: true, MaxPathLen: 0, MaxPathLenZero: true,
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
