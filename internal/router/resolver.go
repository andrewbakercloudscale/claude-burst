package router

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// interceptResolver dials the upstream host while /etc/hosts points that same
// hostname at this gateway.
//
// THE PROBLEM IT SOLVES. Transparent intercept mode adds
// "127.0.0.1 api.anthropic.com" to /etc/hosts so Claude Code connects here
// believing it reached Anthropic. That entry is machine-wide, so it applies to
// this process too: the gateway's own upstream request would resolve to
// 127.0.0.1 and it would call itself, forever. Go's resolver consults
// /etc/hosts in BOTH its cgo and pure-Go modes, so setting PreferGo does not
// avoid this.
//
// The fix is to resolve the intercepted hostname over DNS-over-HTTPS, which
// never consults /etc/hosts, then dial the returned address directly while
// leaving TLS ServerName as the real hostname so certificate verification is
// unchanged. The DoH endpoint is itself resolved normally -- it must not be a
// host we redirect, or the loop simply moves.
//
// Only the intercepted hostname is treated this way. Every other destination
// (Together AI, Bedrock, ...) uses the standard dialer, since redirecting one
// name is no reason to route the rest through a third party's resolver.
type interceptResolver struct {
	host     string // the one hostname to resolve via DoH
	endpoint string // DoH JSON endpoint
	pinned   string // optional fixed IP, skips DoH entirely
	dialer   *net.Dialer
	client   *http.Client // plain client; must NOT use this resolver

	mu    sync.Mutex
	cache map[string]dnsEntry
	now   func() time.Time // swappable for tests
}

type dnsEntry struct {
	addrs   []string
	expires time.Time
}

// Cache floors. A CDN can advertise very short TTLs; re-resolving on every
// request would put a DoH round-trip in front of each call, and never
// re-resolving would pin us to an address that has moved.
const (
	minDNSTTL = 30 * time.Second
	maxDNSTTL = 5 * time.Minute
)

func newInterceptResolver(host, endpoint, pinned string) *interceptResolver {
	return &interceptResolver{
		host:     strings.ToLower(host),
		endpoint: endpoint,
		pinned:   pinned,
		dialer:   &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second},
		client:   &http.Client{Timeout: 10 * time.Second},
		cache:    map[string]dnsEntry{},
		now:      time.Now,
	}
}

// DialContext is installed as the Transport's DialContext.
func (r *interceptResolver) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return r.dialer.DialContext(ctx, network, addr)
	}
	if !strings.EqualFold(host, r.host) {
		return r.dialer.DialContext(ctx, network, addr)
	}
	if r.pinned != "" {
		return r.dialer.DialContext(ctx, network, net.JoinHostPort(r.pinned, port))
	}

	addrs, err := r.lookup(ctx, host)
	if err != nil {
		// Deliberately no fallback to the system resolver: that would resolve
		// via /etc/hosts, connect to ourselves, and produce a confusing hang
		// instead of a clear error.
		return nil, fmt.Errorf("resolve %s over DoH (%s): %w", host, r.endpoint, err)
	}

	var lastErr error
	for _, ip := range addrs {
		conn, err := r.dialer.DialContext(ctx, network, net.JoinHostPort(ip, port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("dial %s (DoH gave %v): %w", host, addrs, lastErr)
}

func (r *interceptResolver) lookup(ctx context.Context, host string) ([]string, error) {
	r.mu.Lock()
	if e, ok := r.cache[host]; ok && r.now().Before(e.expires) {
		addrs := e.addrs
		r.mu.Unlock()
		return addrs, nil
	}
	r.mu.Unlock()

	addrs, ttl, err := r.queryDoH(ctx, host)
	if err != nil {
		return nil, err
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("no A records for %s", host)
	}
	if ttl < minDNSTTL {
		ttl = minDNSTTL
	}
	if ttl > maxDNSTTL {
		ttl = maxDNSTTL
	}

	r.mu.Lock()
	r.cache[host] = dnsEntry{addrs: addrs, expires: r.now().Add(ttl)}
	r.mu.Unlock()
	return addrs, nil
}

type dohAnswer struct {
	Name string `json:"name"`
	Type int    `json:"type"`
	TTL  int    `json:"TTL"`
	Data string `json:"data"`
}

type dohResponse struct {
	Status int         `json:"Status"`
	Answer []dohAnswer `json:"Answer"`
}

// queryDoH uses the JSON DoH API rather than wire-format DNS: it needs only
// net/http and encoding/json, keeping this repo dependency-free.
func (r *interceptResolver) queryDoH(ctx context.Context, host string) ([]string, time.Duration, error) {
	u, err := url.Parse(r.endpoint)
	if err != nil {
		return nil, 0, fmt.Errorf("bad DoH endpoint %q: %w", r.endpoint, err)
	}
	q := u.Query()
	q.Set("name", host)
	q.Set("type", "A")
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/dns-json")

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, 0, fmt.Errorf("DoH HTTP %d", resp.StatusCode)
	}

	var out dohResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, 0, fmt.Errorf("decode DoH response: %w", err)
	}
	if out.Status != 0 {
		return nil, 0, fmt.Errorf("DoH status %d", out.Status)
	}

	var addrs []string
	ttl := int(maxDNSTTL / time.Second)
	for _, a := range out.Answer {
		if a.Type != 1 { // A records only; CNAMEs in the chain are not addresses
			continue
		}
		if net.ParseIP(a.Data) == nil {
			continue
		}
		// A poisoned or misconfigured answer pointing back at loopback would
		// recreate the very loop this type exists to prevent.
		if ip := net.ParseIP(a.Data); ip.IsLoopback() {
			return nil, 0, fmt.Errorf("DoH returned loopback %s for %s -- refusing (this would loop back into the gateway)", a.Data, host)
		}
		addrs = append(addrs, a.Data)
		if a.TTL > 0 && a.TTL < ttl {
			ttl = a.TTL
		}
	}
	return addrs, time.Duration(ttl) * time.Second, nil
}
