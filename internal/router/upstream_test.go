package router

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/tlsca"
)

// A corporate egress proxy is honoured; one on loopback (Burst's own
// address in older installs) is not, or the gateway would call itself.
func TestUpstreamProxySkipsLoopback(t *testing.T) {
	for _, tc := range []struct{ proxy, want string }{
		{"http://proxy.corp.example:8080", "proxy.corp.example:8080"},
		{"http://127.0.0.1:7777", ""},
		{"http://[::1]:7777", ""},
		{"http://localhost:7777", ""},
	} {
		u, _ := url.Parse(tc.proxy)
		got := ""
		if v := withoutLoopback(u); v != nil {
			got = v.Host
		}
		if got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.proxy, got, tc.want)
		}
	}
	if withoutLoopback(nil) != nil {
		t.Error("no proxy must stay no proxy")
	}
}

// A corporate CA in the NODE_EXTRA_CA_CERTS bundle is trusted for
// upstreams (an internal Portkey gateway); Burst's own block is not.
func TestUpstreamRootsTrustTheBundleButNotBurstsOwnCA(t *testing.T) {
	corp := selfSigned(t, "Corp Root")
	burst := selfSigned(t, "claude-burst local CA")
	bundle := filepath.Join(t.TempDir(), "bundle.pem")
	content := string(pemOf(corp)) + tlsca.BundleBlock(pemOf(burst))
	os.WriteFile(bundle, []byte(content), 0600)

	pool := upstreamRoots(bundle, nil)
	if _, err := corp.Verify(x509.VerifyOptions{Roots: pool}); err != nil {
		t.Errorf("corporate CA from the bundle not trusted: %v", err)
	}
	if _, err := burst.Verify(x509.VerifyOptions{Roots: pool}); err == nil {
		t.Error("Burst's own CA was trusted for upstreams")
	}
	if upstreamRoots(filepath.Join(t.TempDir(), "missing.pem"), nil) == nil {
		t.Error("a missing bundle must fall back to the system store")
	}
}

func selfSigned(t *testing.T, cn string) *x509.Certificate {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true, IsCA: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(der)
	return c
}

func pemOf(c *x509.Certificate) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})
}
