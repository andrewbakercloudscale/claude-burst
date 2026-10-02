// Package tlsca manages the local certificate authority used by transparent
// intercept mode.
//
// In that mode /etc/hosts points api.anthropic.com at the gateway, so the
// gateway must present a certificate for that name which Claude Code will
// accept. It therefore mints its own CA, keeps it on disk, and signs a leaf
// for the intercepted host. The CA is added to the NODE_EXTRA_CA_CERTS bundle
// that Claude Code already reads (see EnsureInBundle).
//
// Scope of the private key: the CA carries a critical X.509 name constraint
// permitting only the intercepted host (api.anthropic.com and names under
// it), so even someone who reads the key cannot mint a certificate that a
// verifier will accept for any other site. Until 2026-10-02 it had no
// constraint: the dashboard install trusts the CA system-wide, so a copy of
// that key could have impersonated any website to every app on the Mac. The
// key is written 0600 in a 0700 directory and never leaves the machine.
//
// Migrating an unconstrained CA (see migrateLegacy) keeps running Claude
// Code sessions working: Node reads its CA bundle once at startup, so a
// session started before the change still trusts only the old CA. For a
// short transition the gateway also presents a bridge certificate, the new
// CA's public key signed by the old CA, after which the old key is deleted.
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
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/atomicfile"
)

const (
	caValidity   = 10 * 365 * 24 * time.Hour
	leafValidity = 397 * 24 * time.Hour
	// bridgeValidity is how long sessions started before a migration keep
	// working without a restart.
	bridgeValidity = 30 * 24 * time.Hour

	// Reissue a leaf before it actually expires. Without this the gateway
	// would keep serving a valid-looking cert until the moment it went stale,
	// then fail every request at once with a TLS error that looks like a
	// network fault rather than an expiry.
	renewBefore = 30 * 24 * time.Hour

	beginMarker = "# BEGIN claude-burst CA -- managed by claude-burst, do not edit inside this block"
	endMarker   = "# END claude-burst CA"
)

// Migration files: the bridge certificate presented during a transition, and
// the old CA's certificate, kept (without its key) so the system-wide trust
// can be found and removed.
func migrationFiles(dir string) (bridgeCert, legacyCert string) {
	return filepath.Join(dir, "bridge-cert.pem"), filepath.Join(dir, "ca-legacy-cert.pem")
}

// Files returns the on-disk locations under dir.
func Files(dir string) (caCert, caKey, leafCert, leafKey string) {
	return filepath.Join(dir, "ca-cert.pem"),
		filepath.Join(dir, "ca-key.pem"),
		filepath.Join(dir, "leaf-cert.pem"),
		filepath.Join(dir, "leaf-key.pem")
}

// LoadOrCreate returns a leaf certificate for host, minting the CA and/or the
// leaf if they are absent or close to expiry. It also returns the CA in PEM
// form so the caller can put it in a trust bundle.
func LoadOrCreate(dir, host string) (*tls.Certificate, []byte, error) {
	if host == "" {
		return nil, nil, fmt.Errorf("tlsca: empty host")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, nil, fmt.Errorf("tlsca: create %s: %w", dir, err)
	}

	caCertPath, caKeyPath, leafCertPath, leafKeyPath := Files(dir)

	caCert, caKey, caPEM, err := loadCA(caCertPath, caKeyPath)
	if err != nil {
		return nil, nil, err
	}
	if caCert != nil && !constrainedTo(caCert, host) {
		old, oldKey := caCert, caKey
		caCert, caKey, caPEM, err = createCA(caCertPath, caKeyPath, host)
		if err != nil {
			return nil, nil, err
		}
		if len(old.PermittedDNSDomains) == 0 {
			if err := migrateLegacy(dir, old, oldKey, caCert, caKey); err != nil {
				return nil, nil, err
			}
		}
	}
	if caCert == nil {
		caCert, caKey, caPEM, err = createCA(caCertPath, caKeyPath, host)
		if err != nil {
			return nil, nil, err
		}
	}

	leaf, err := loadLeaf(leafCertPath, leafKeyPath, host, caCert)
	if err != nil {
		return nil, nil, err
	}
	if leaf == nil {
		leaf, err = createLeaf(leafCertPath, leafKeyPath, host, caCert, caKey)
		if err != nil {
			return nil, nil, err
		}
	}
	if bridge := loadBridge(dir, caCert); bridge != nil {
		chained := *leaf
		chained.Certificate = append(append([][]byte{}, leaf.Certificate...), bridge.Raw)
		leaf = &chained
	}
	return leaf, caPEM, nil
}

// constrainedTo says whether the CA may only sign for host: a critical name
// constraint that permits exactly host.
func constrainedTo(ca *x509.Certificate, host string) bool {
	return ca.PermittedDNSDomainsCritical && len(ca.PermittedDNSDomains) == 1 &&
		strings.EqualFold(ca.PermittedDNSDomains[0], host) &&
		len(ca.ExcludedDNSDomains) == 0
}

// migrateLegacy signs the new CA's public key with the old, unconstrained
// CA (the bridge), keeps the old certificate for trust removal, and deletes
// the old key. A session started before the migration still trusts only the
// old CA; served as leaf + bridge, its chain still reaches that CA.
func migrateLegacy(dir string, old *x509.Certificate, oldKey *ecdsa.PrivateKey, ca *x509.Certificate, caKey *ecdsa.PrivateKey) error {
	bridgePath, legacyPath := migrationFiles(dir)
	notAfter := time.Now().Add(bridgeValidity)
	if old.NotAfter.Before(notAfter) {
		notAfter = old.NotAfter
	}
	tmpl := *ca
	tmpl.NotAfter = notAfter
	serial, err := randomSerial()
	if err != nil {
		return err
	}
	tmpl.SerialNumber = serial
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, old, &caKey.PublicKey, oldKey)
	if err != nil {
		return fmt.Errorf("tlsca: create bridge cert: %w", err)
	}
	if err := writeFile(bridgePath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0644); err != nil {
		return err
	}
	if err := writeFile(legacyPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: old.Raw}), 0644); err != nil {
		return err
	}
	// The old key itself is gone already: createCA wrote the new key over it.
	return nil
}

// loadBridge returns the bridge certificate while it is still valid and
// still certifies the current CA's key; otherwise nil.
func loadBridge(dir string, ca *x509.Certificate) *x509.Certificate {
	bridgePath, _ := migrationFiles(dir)
	b, err := os.ReadFile(bridgePath)
	if err != nil {
		return nil
	}
	block, _ := pem.Decode(b)
	if block == nil {
		return nil
	}
	c, err := x509.ParseCertificate(block.Bytes)
	if err != nil || time.Now().After(c.NotAfter) {
		return nil
	}
	if pk, ok := c.PublicKey.(*ecdsa.PublicKey); !ok || !pk.Equal(ca.PublicKey) {
		return nil
	}
	return c
}

func loadCA(certPath, keyPath string) (*x509.Certificate, *ecdsa.PrivateKey, []byte, error) {
	certPEM, err := os.ReadFile(certPath)
	if os.IsNotExist(err) {
		return nil, nil, nil, nil
	}
	if err != nil {
		return nil, nil, nil, fmt.Errorf("tlsca: read %s: %w", certPath, err)
	}
	keyPEM, err := os.ReadFile(keyPath)
	if os.IsNotExist(err) {
		return nil, nil, nil, nil
	}
	if err != nil {
		return nil, nil, nil, fmt.Errorf("tlsca: read %s: %w", keyPath, err)
	}

	certBlock, _ := pem.Decode(certPEM)
	keyBlock, _ := pem.Decode(keyPEM)
	if certBlock == nil || keyBlock == nil {
		return nil, nil, nil, fmt.Errorf("tlsca: %s or %s is not valid PEM -- delete the directory to regenerate", certPath, keyPath)
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("tlsca: parse %s: %w", certPath, err)
	}
	key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("tlsca: parse %s: %w", keyPath, err)
	}
	// An expired CA is treated as absent so it is regenerated rather than
	// used to sign leaves nothing will accept.
	if time.Now().After(cert.NotAfter.Add(-renewBefore)) {
		return nil, nil, nil, nil
	}
	// A key that does not belong to the certificate (a crash between the
	// two writes) is treated as absent too.
	if pk, ok := cert.PublicKey.(*ecdsa.PublicKey); !ok || !pk.Equal(&key.PublicKey) {
		return nil, nil, nil, nil
	}
	return cert, key, certPEM, nil
}

func createCA(certPath, keyPath, host string) (*x509.Certificate, *ecdsa.PrivateKey, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("tlsca: generate CA key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, nil, nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			Organization: []string{"claude-burst local intercept"},
			CommonName:   "claude-burst local CA",
		},
		NotBefore:             now.Add(-time.Hour), // tolerate small clock skew
		NotAfter:              now.Add(caValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
		// Only host and names under it: see the package comment.
		PermittedDNSDomainsCritical: true,
		PermittedDNSDomains:         []string{host},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("tlsca: create CA cert: %w", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("tlsca: marshal CA key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	// Key first: a crash between the two writes then leaves a key with the
	// old certificate, which loadCA rejects as a mismatch and regenerates,
	// never a new certificate beside a key that cannot sign for it.
	if err := writeFile(keyPath, keyPEM, 0600); err != nil {
		return nil, nil, nil, err
	}
	if err := writeFile(certPath, certPEM, 0644); err != nil {
		return nil, nil, nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("tlsca: parse new CA cert: %w", err)
	}
	return cert, key, certPEM, nil
}

func loadLeaf(certPath, keyPath, host string, ca *x509.Certificate) (*tls.Certificate, error) {
	certPEM, err := os.ReadFile(certPath)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("tlsca: read %s: %w", certPath, err)
	}
	keyPEM, err := os.ReadFile(keyPath)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("tlsca: read %s: %w", keyPath, err)
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		// Corrupt or mismatched pair: regenerate rather than fail hard.
		return nil, nil
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, nil
	}
	// Regenerate if it is for a different host or is nearing expiry.
	if time.Now().After(leaf.NotAfter.Add(-renewBefore)) {
		return nil, nil
	}
	if leaf.VerifyHostname(host) != nil {
		return nil, nil
	}
	// Signed by a different CA (the CA was just regenerated): reissue, or
	// the gateway would serve a chain no current trust store accepts.
	if leaf.CheckSignatureFrom(ca) != nil {
		return nil, nil
	}
	pair.Leaf = leaf
	return &pair, nil
}

func createLeaf(certPath, keyPath, host string, caCert *x509.Certificate, caKey *ecdsa.PrivateKey) (*tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("tlsca: generate leaf key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: host},
		DNSNames:              []string{host},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(leafValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("tlsca: create leaf cert: %w", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := writeFile(certPath, certPEM, 0644); err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("tlsca: marshal leaf key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := writeFile(keyPath, keyPEM, 0600); err != nil {
		return nil, err
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("tlsca: load new leaf: %w", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("tlsca: parse new leaf: %w", err)
	}
	pair.Leaf = leaf
	return &pair, nil
}

func randomSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	n, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, fmt.Errorf("tlsca: serial: %w", err)
	}
	return n, nil
}

func writeFile(path string, data []byte, perm os.FileMode) error {
	if err := atomicfile.Write(path, data, perm); err != nil {
		return fmt.Errorf("tlsca: write %s: %w", path, err)
	}
	// WriteFile only applies perm when creating; enforce it on rewrite too, so
	// a key that already existed with loose permissions gets tightened.
	if err := os.Chmod(path, perm); err != nil {
		return fmt.Errorf("tlsca: chmod %s: %w", path, err)
	}
	return nil
}

// --- trust bundle management -------------------------------------------------

// BundleBlock renders the marker-delimited block appended to a CA bundle.
func BundleBlock(caPEM []byte) string {
	body := strings.TrimRight(string(caPEM), "\n")
	return beginMarker + "\n" + body + "\n" + endMarker + "\n"
}

// StripBlock removes any claude-burst block from bundle contents, leaving the
// rest byte-for-byte intact. Returns the new contents and whether anything was
// removed. Exported (and pure) so it can be tested without touching real files
// -- this edits a file that may hold an employer's CA certificates, and
// corrupting it would break far more than this tool.
func StripBlock(contents string) (string, bool) {
	start := strings.Index(contents, beginMarker)
	if start < 0 {
		return contents, false
	}
	endIdx := strings.Index(contents[start:], endMarker)
	if endIdx < 0 {
		// Begin marker with no end: refuse to guess where it stops.
		return contents, false
	}
	end := start + endIdx + len(endMarker)
	if end < len(contents) && contents[end] == '\n' {
		end++
	}
	return contents[:start] + contents[end:], true
}

// HasBlock reports whether a claude-burst block is present.
func HasBlock(contents string) bool {
	return strings.Contains(contents, beginMarker) && strings.Contains(contents, endMarker)
}

// EnsureInBundle appends the CA to the bundle at path, replacing any block a
// previous run left. Existing content is preserved: on a corporate machine
// this file holds the employer's CAs and Claude Code will not reach anything
// without them.
func EnsureInBundle(path string, caPEM []byte) error {
	if path == "" {
		return fmt.Errorf("tlsca: no CA bundle path configured")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("tlsca: create %s: %w", filepath.Dir(path), err)
	}
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("tlsca: read %s: %w", path, err)
	}
	stripped, _ := StripBlock(string(existing))
	if stripped != "" && !strings.HasSuffix(stripped, "\n") {
		stripped += "\n"
	}
	return writeFile(path, []byte(stripped+BundleBlock(caPEM)), 0600)
}

// RemoveFromBundle removes the claude-burst block, leaving everything else.
func RemoveFromBundle(path string) error {
	existing, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("tlsca: read %s: %w", path, err)
	}
	stripped, changed := StripBlock(string(existing))
	if !changed {
		return nil
	}
	return writeFile(path, []byte(stripped), 0600)
}
