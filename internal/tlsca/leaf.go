package tlsca

import (
	"crypto/tls"
	"fmt"
	"sync"
	"time"
)

// reissueRetry is how soon a failed reissue is tried again. The leaf being
// served is still valid for up to renewBefore at that point, so there is no
// hurry, and a full disk must not put a file write in front of every
// handshake.
const reissueRetry = time.Hour

// Leaf is the server certificate of a gateway that stays up: it hands out the
// leaf for host and signs a new one from the CA on disk once the current one
// is within renewBefore of expiring.
//
// LoadOrCreate alone checks the leaf once, when the gateway starts. A leaf
// lives 397 days and is reissued 30 days early, so a gateway that starts on a
// leaf with 31 days left and then runs for 32 would serve an expired
// certificate: every request failing at once with a TLS error that looks like
// a network fault, cured only by a restart.
//
// It never makes a CA. Sessions trust the CA that was in force when they
// started (see the package comment), so a running gateway that minted a new
// one would fail them all; if the CA cannot be used, the current leaf is kept
// and the reason is reported through Logf.
type Leaf struct {
	// Logf, when set, is told about a reissue and about one that failed.
	// Set it before the first handshake.
	Logf func(format string, args ...any)

	dir, host string
	now       func() time.Time // swappable for tests

	mu      sync.Mutex
	cur     *tls.Certificate
	retryAt time.Time // no reissue before this, after one failed
}

// NewLeaf is LoadOrCreate for a server that keeps running: it returns the
// same leaf and CA, with the leaf behind GetCertificate.
func NewLeaf(dir, host string) (*Leaf, []byte, error) {
	return newLeaf(dir, host, time.Now)
}

func newLeaf(dir, host string, now func() time.Time) (*Leaf, []byte, error) {
	cur, caPEM, err := loadOrCreate(dir, host, now())
	if err != nil {
		return nil, nil, err
	}
	return &Leaf{dir: dir, host: host, now: now, cur: cur}, caPEM, nil
}

// GetCertificate is for tls.Config.GetCertificate. It is safe for concurrent
// use and always returns a certificate: when a due reissue fails, the one it
// already has.
func (l *Leaf) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if now.Before(l.cur.Leaf.NotAfter.Add(-renewBefore)) || now.Before(l.retryAt) {
		return l.cur, nil
	}
	next, err := l.reissue(now)
	if err != nil {
		l.retryAt = now.Add(reissueRetry)
		l.logf("WARNING: the gateway's certificate for %s expires %s and could not be reissued: %v. Restarting the gateway reissues it",
			l.host, l.cur.Leaf.NotAfter.Local().Format("2 Jan 2006 15:04"), err)
		return l.cur, nil
	}
	l.cur = next
	l.logf("reissued the gateway's certificate for %s, valid until %s",
		l.host, next.Leaf.NotAfter.Local().Format("2 Jan 2006 15:04"))
	return l.cur, nil
}

// reissue signs a new leaf with the CA on disk, or takes one another process
// already wrote (`claude-burst status` and `enable` both call LoadOrCreate).
func (l *Leaf) reissue(now time.Time) (*tls.Certificate, error) {
	caCertPath, caKeyPath, leafCertPath, leafKeyPath := Files(l.dir)
	caCert, caKey, _, err := loadCA(caCertPath, caKeyPath, now)
	if err != nil {
		return nil, err
	}
	if caCert == nil {
		return nil, fmt.Errorf("tlsca: no usable CA in %s", l.dir)
	}
	if len(caCert.PermittedDNSDomains) > 0 && !constrainedTo(caCert, l.host) {
		return nil, fmt.Errorf("tlsca: the CA in %s may not sign for %s", l.dir, l.host)
	}
	leaf, err := loadLeaf(leafCertPath, leafKeyPath, l.host, caCert, now)
	if err != nil {
		return nil, err
	}
	if leaf != nil {
		return leaf, nil
	}
	return createLeaf(leafCertPath, leafKeyPath, l.host, caCert, caKey, now)
}

func (l *Leaf) logf(format string, args ...any) {
	if l.Logf != nil {
		l.Logf(format, args...)
	}
}
