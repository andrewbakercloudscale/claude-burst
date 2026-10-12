package tlsca

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// A gateway that stays up must not serve its leaf past expiry: once the leaf
// is within renewBefore of expiring, the next handshake gets a new one, signed
// by the same CA, and the one after that gets the same new one.
func TestLeafIsReissuedBeforeItExpires(t *testing.T) {
	dir := t.TempDir()
	clock := time.Now()
	l, caPEM, err := newLeaf(dir, host, func() time.Time { return clock })
	if err != nil {
		t.Fatal(err)
	}
	first, err := l.GetCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(leafValidity - renewBefore - time.Hour)
	if same, _ := l.GetCertificate(nil); same != first {
		t.Fatal("reissued a leaf that was not yet due")
	}

	clock = clock.Add(2 * time.Hour) // now inside the renewal margin
	var logged []string
	l.Logf = func(f string, a ...any) { logged = append(logged, f) }
	second, err := l.GetCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}
	if second == first || second.Leaf.SerialNumber.Cmp(first.Leaf.SerialNumber) == 0 {
		t.Fatal("a leaf inside the renewal margin was served again")
	}
	if !second.Leaf.NotAfter.After(clock.Add(renewBefore)) {
		t.Fatalf("the new leaf expires %s, no later than the old one needed", second.Leaf.NotAfter)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(caPEM)
	if _, err := second.Leaf.Verify(x509.VerifyOptions{DNSName: host, Roots: roots, CurrentTime: clock}); err != nil {
		t.Fatalf("the new leaf does not chain to the CA sessions already trust: %v", err)
	}
	if len(logged) != 1 {
		t.Errorf("a reissue should be logged once, got %d lines", len(logged))
	}
	if third, _ := l.GetCertificate(nil); third != second {
		t.Error("reissued again on the next handshake")
	}
}

// With the CA key gone the leaf cannot be reissued. The gateway must keep
// serving the one it has (still valid for the margin), must not mint a new
// CA that no running session trusts, and must not retry on every handshake.
func TestLeafKeepsServingWhenItCannotBeReissued(t *testing.T) {
	dir := t.TempDir()
	clock := time.Now()
	l, caPEM, err := newLeaf(dir, host, func() time.Time { return clock })
	if err != nil {
		t.Fatal(err)
	}
	first, _ := l.GetCertificate(nil)
	caCertPath, caKeyPath, _, _ := Files(dir)
	if err := os.Remove(caKeyPath); err != nil {
		t.Fatal(err)
	}
	var logged []string
	l.Logf = func(f string, a ...any) { logged = append(logged, f) }

	clock = clock.Add(leafValidity - renewBefore + time.Hour)
	for i := 0; i < 3; i++ {
		got, err := l.GetCertificate(nil)
		if err != nil || got != first {
			t.Fatalf("handshake %d: got %v, %v; want the leaf it already had", i, got, err)
		}
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "could not be reissued") {
		t.Fatalf("want one warning for three handshakes, got %q", logged)
	}
	if onDisk, _ := os.ReadFile(caCertPath); string(onDisk) != string(caPEM) {
		t.Fatal("a running gateway replaced the CA")
	}
	if fileExists(caKeyPath) {
		t.Fatal("a running gateway minted a new CA key")
	}

	clock = clock.Add(reissueRetry + time.Minute)
	l.GetCertificate(nil)
	if len(logged) != 2 {
		t.Errorf("the reissue should be tried again after %s, got %d warnings", reissueRetry, len(logged))
	}
}

// Handshakes arrive on many goroutines. Run with -race.
func TestLeafIsSafeForConcurrentHandshakes(t *testing.T) {
	var mu sync.Mutex
	clock := time.Now()
	now := func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
	l, _, err := newLeaf(t.TempDir(), host, now)
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	clock = clock.Add(leafValidity - time.Hour)
	mu.Unlock()
	serials := make([]string, 16)
	var wg sync.WaitGroup
	for i := range serials {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c, err := l.GetCertificate(nil)
			if err != nil {
				t.Error(err)
				return
			}
			serials[i] = c.Leaf.SerialNumber.String()
		}(i)
	}
	wg.Wait()
	for _, s := range serials {
		if s != serials[0] {
			t.Fatal("concurrent handshakes each reissued the leaf")
		}
	}
}

// A CA made now must refuse, at the verifier, a certificate for another name
// and a certificate for an IP address, and still vouch for the intercepted
// host. Before 2026-10-12 only DNS names were constrained, so the second of
// these verified.
func TestNewCARefusesOtherNamesAndEveryIPAddress(t *testing.T) {
	dir := t.TempDir()
	served, _, err := LoadOrCreate(dir, host)
	if err != nil {
		t.Fatal(err)
	}
	caCertPath, caKeyPath, _, _ := Files(dir)
	ca, key, _, err := loadCA(caCertPath, caKeyPath, time.Now())
	if err != nil || ca == nil {
		t.Fatalf("loadCA: %v", err)
	}
	if len(ca.ExtKeyUsage) != 1 || ca.ExtKeyUsage[0] != x509.ExtKeyUsageServerAuth {
		t.Errorf("CA extended key usage = %v, want server authentication only", ca.ExtKeyUsage)
	}
	if len(served.Leaf.ExtKeyUsage) != 1 || served.Leaf.ExtKeyUsage[0] != x509.ExtKeyUsageServerAuth {
		t.Errorf("leaf extended key usage = %v, want server authentication only", served.Leaf.ExtKeyUsage)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)

	if _, err := served.Leaf.Verify(x509.VerifyOptions{DNSName: host, Roots: roots}); err != nil {
		t.Errorf("the leaf the gateway serves no longer verifies: %v", err)
	}
	other := signLeaf(t, ca, key, "github.com")
	if _, err := other.Verify(x509.VerifyOptions{DNSName: "github.com", Roots: roots}); err == nil {
		t.Error("a certificate for another DNS name verified")
	}
	for _, ip := range []string{"203.0.113.5", "127.0.0.1", "2001:db8::1"} {
		c := signIPLeaf(t, ca, key, net.ParseIP(ip))
		if _, err := c.Verify(x509.VerifyOptions{DNSName: ip, Roots: roots}); err == nil {
			t.Errorf("a certificate for the IP address %s verified", ip)
		}
	}
	// The intercepted host beside an IP address is refused as well: the
	// permitted name does not carry a forbidden one in with it.
	mixed := signIPLeaf(t, ca, key, net.ParseIP("203.0.113.5"), host)
	if _, err := mixed.Verify(x509.VerifyOptions{DNSName: host, Roots: roots}); err == nil {
		t.Error("a certificate naming the host and an IP address verified")
	}
	// A certificate for anything but a TLS server is outside the CA's usage.
	if _, err := served.Leaf.Verify(x509.VerifyOptions{DNSName: host, Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning}}); err == nil {
		t.Error("the chain verified for code signing")
	}
}

// Unconstrained speaks up for a CA from before 2026-10-02 and names the real
// command, and says nothing for a constrained CA or for no CA at all.
func TestUnconstrainedWarnsOnlyForACAWithNoConstraint(t *testing.T) {
	empty := t.TempDir()
	if w := Unconstrained(empty, host); w != nil {
		t.Errorf("no CA yet: got %+v, want no warning", w)
	}
	fresh := t.TempDir()
	if _, _, err := LoadOrCreate(fresh, host); err != nil {
		t.Fatal(err)
	}
	if w := Unconstrained(fresh, host); w != nil {
		t.Errorf("constrained CA: got %+v, want no warning", w)
	}
	old := t.TempDir()
	caCertPath, caKeyPath, _, _ := Files(old)
	writeLegacyCA(t, caCertPath, caKeyPath)
	w := Unconstrained(old, host)
	if w == nil {
		t.Fatal("an unconstrained CA was kept with no warning")
	}
	for _, want := range []string{host, "claude-burst ca-rotate", "sudo scripts/trust-ca-systemwide.sh", old} {
		if !strings.Contains(w.Title+" "+w.Detail, want) {
			t.Errorf("warning does not mention %q: %s: %s", want, w.Title, w.Detail)
		}
	}
	if strings.Contains(w.Title+w.Detail, "\n") {
		t.Errorf("warning must be one plain line: %q", w.Detail)
	}
}

// signIPLeaf signs a certificate for an IP address, and for any DNS names
// given beside it.
func signIPLeaf(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, ip net.IP, names ...string) *x509.Certificate {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(8), Subject: pkix.Name{CommonName: ip.String()},
		IPAddresses: []net.IP{ip}, DNSNames: names,
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
