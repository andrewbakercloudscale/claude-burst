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

	// fallbacks are further DoH endpoints tried in order when endpoint
	// fails, and udpServers plain DNS servers tried after those. Both are
	// empty unless withFallbacks is called, so tests never reach the
	// internet. See withFallbacks for why they exist.
	fallbacks  []string
	udpServers []string
	// cachePath, when set, keeps the last good answer on disk so a restart
	// on a network that blocks every lookup still has an address to dial.
	cachePath string

	mu        sync.Mutex
	cache     map[string]dnsEntry
	preferred string           // the endpoint that last answered; tried first
	now       func() time.Time // swappable for tests
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

// defaultDoHFallbacks are addressed by IP, not name, on purpose. On
// 2026-09-30 a network started resetting every TLS connection whose SNI was
// cloudflare-dns.com or dns.google (curl exit 35 for both, while
// https://1.1.1.1/dns-query answered normally), so a named endpoint was the
// single point of failure that took the whole gateway down: every request
// to Anthropic got a 502 for as long as the Mac stayed on that network.
var defaultDoHFallbacks = []string{
	"https://1.1.1.1/dns-query",
	"https://8.8.8.8/resolve",
}

// defaultUDPServers are the last resort: plain DNS on port 53, which a
// network that filters DoH usually still lets through. Plain DNS queries
// never read /etc/hosts because they are sent by hand, not via the system
// resolver.
var defaultUDPServers = []string{"1.1.1.1:53", "8.8.8.8:53"}

// withFallbacks turns on the extra DoH endpoints, the plain DNS servers and
// the on-disk cache. Production only; tests build a bare resolver so they
// never leave the machine.
func (r *interceptResolver) withFallbacks(cachePath string) *interceptResolver {
	for _, e := range defaultDoHFallbacks {
		if e != r.endpoint {
			r.fallbacks = append(r.fallbacks, e)
		}
	}
	r.udpServers = append([]string(nil), defaultUDPServers...)
	r.cachePath = cachePath
	r.loadCache()
	return r
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

	addrs, ttl, err := r.queryAll(ctx, host)
	if err != nil {
		// Anthropic's address rarely changes, and the lookup fails mostly
		// while the Mac is changing networks. The last address it had is far
		// likelier to work than no address at all: on 2026-09-30 one reset
		// connection to the DoH server, with a good address in the cache
		// minutes stale, sent both sessions to the paid secondary.
		r.mu.Lock()
		e, ok := r.cache[host]
		r.mu.Unlock()
		if ok && len(e.addrs) > 0 {
			return e.addrs, nil
		}
		return nil, &LookupError{Host: host, Err: err}
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
	r.saveCache(host, addrs)
	return addrs, nil
}

// LookupError is a failed lookup, every resolver tried, with no address to
// fall back on. Nothing was sent, so the primary retry ladder resends it
// (safeToResend); if it still fails after that, it DOES count towards
// failover. Treating it as "this machine's network" and never failing over
// is what turned a DoH block on 2026-09-30 into a 502 on every request while
// Anthropic and the secondary were both reachable.
type LookupError struct {
	Host string
	Err  error
}

func (e *LookupError) Error() string { return "lookup " + e.Host + ": " + e.Err.Error() }
func (e *LookupError) Unwrap() error { return e.Err }

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

// queryAll asks each resolver in turn, starting with the one that answered
// last time, and returns the first answer. The error, when all fail, names
// every attempt: "the DoH server reset the connection" alone hid, on
// 2026-09-30, that it was the only server ever asked.
func (r *interceptResolver) queryAll(ctx context.Context, host string) ([]string, time.Duration, error) {
	endpoints := append([]string{r.endpoint}, r.fallbacks...)
	r.mu.Lock()
	pref := r.preferred
	r.mu.Unlock()
	if pref != "" {
		ordered := []string{pref}
		for _, e := range endpoints {
			if e != pref {
				ordered = append(ordered, e)
			}
		}
		endpoints = ordered
	}
	var errs []string
	var lastErr error
	for _, e := range endpoints {
		if ctx.Err() != nil {
			break
		}
		addrs, ttl, err := r.queryEndpoint(ctx, e, host)
		if err == nil && len(addrs) > 0 {
			r.mu.Lock()
			r.preferred = e
			r.mu.Unlock()
			return addrs, ttl, nil
		}
		if err == nil {
			err = fmt.Errorf("no A records")
		}
		lastErr = err
		errs = append(errs, e+": "+err.Error())
	}
	for _, srv := range r.udpServers {
		if ctx.Err() != nil {
			break
		}
		addrs, ttl, err := queryUDP(ctx, srv, host)
		if err == nil && len(addrs) > 0 {
			return addrs, ttl, nil
		}
		if err == nil {
			err = fmt.Errorf("no A records")
		}
		lastErr = err
		errs = append(errs, "dns://"+srv+": "+err.Error())
	}
	if len(errs) == 1 {
		// One resolver configured (tests, or no fallbacks): keep the bare
		// error so callers can still match on it.
		return nil, 0, lastErr
	}
	return nil, 0, fmt.Errorf("every resolver failed: %s", strings.Join(errs, "; "))
}

// queryDoH asks the configured endpoint only.
func (r *interceptResolver) queryDoH(ctx context.Context, host string) ([]string, time.Duration, error) {
	return r.queryEndpoint(ctx, r.endpoint, host)
}

// queryEndpoint uses the JSON DoH API rather than wire-format DNS: it needs only
// net/http and encoding/json, keeping this repo dependency-free.
func (r *interceptResolver) queryEndpoint(ctx context.Context, endpoint, host string) ([]string, time.Duration, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, 0, fmt.Errorf("bad DoH endpoint %q: %w", endpoint, err)
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
