// Package tlsca manages the local certificate authority used by transparent
// intercept mode.
//
// In that mode /etc/hosts points api.anthropic.com at the gateway, so the
// gateway must present a certificate for that name which Claude Code will
// accept. It therefore mints its own CA, keeps it on disk, and signs a leaf
// for the intercepted host. The CA is added to the NODE_EXTRA_CA_CERTS bundle
// that Claude Code already reads (see EnsureInBundle).
//
// Scope of the private key: it can impersonate exactly the hosts it issues
// leaves for, to processes that trust the bundle. It is written 0600 in a 0700
// directory and never leaves the machine.
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
)

const (
	caValidity   = 10 * 365 * 24 * time.Hour
	leafValidity = 397 * 24 * time.Hour

	// Reissue a leaf before it actually expires. Without this the gateway
	// would keep serving a valid-looking cert until the moment it went stale,
	// then fail every request at once with a TLS error that looks like a
	// network fault rather than an expiry.
	renewBefore = 30 * 24 * time.Hour

	beginMarker = "# BEGIN claude-burst CA -- managed by claude-burst, do not edit inside this block"
	endMarker   = "# END claude-burst CA"
)

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
	if caCert == nil {
		caCert, caKey, caPEM, err = createCA(caCertPath, caKeyPath)
		if err != nil {
			return nil, nil, err
		}
	}

	leaf, err := loadLeaf(leafCertPath, leafKeyPath, host)
	if err != nil {
		return nil, nil, err
	}
	if leaf == nil {
		leaf, err = createLeaf(leafCertPath, leafKeyPath, host, caCert, caKey)
		if err != nil {
			return nil, nil, err
		}
	}
	return leaf, caPEM, nil
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
	return cert, key, certPEM, nil
}

func createCA(certPath, keyPath string) (*x509.Certificate, *ecdsa.PrivateKey, []byte, error) {
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
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("tlsca: create CA cert: %w", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := writeFile(certPath, certPEM, 0644); err != nil {
		return nil, nil, nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("tlsca: marshal CA key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := writeFile(keyPath, keyPEM, 0600); err != nil {
		return nil, nil, nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("tlsca: parse new CA cert: %w", err)
	}
	return cert, key, certPEM, nil
}

func loadLeaf(certPath, keyPath, host string) (*tls.Certificate, error) {
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
	if err := os.WriteFile(path, data, perm); err != nil {
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
