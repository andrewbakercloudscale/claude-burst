package router

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// queryUDP sends one A query to a plain DNS server and parses the answer by
// hand. The standard library's resolver cannot be used for this: it reads
// /etc/hosts first, which is exactly the file pointing the host at us, and
// golang.org/x/net/dns/dnsmessage would be this repo's first dependency.
//
// A reply the server cut short to fit a datagram is asked for again over TCP,
// as the protocol intends.
func queryUDP(ctx context.Context, server, host string) ([]string, time.Duration, error) {
	msg, id, err := aQuery(host)
	if err != nil {
		return nil, 0, err
	}
	d := net.Dialer{Timeout: 3 * time.Second}
	conn, err := d.DialContext(ctx, "udp", server)
	if err != nil {
		return nil, 0, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(dnsDeadline(ctx))
	if _, err := conn.Write(msg); err != nil {
		return nil, 0, err
	}
	buf := make([]byte, 1500)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			return nil, 0, err
		}
		addrs, ttl, err := parseAResponse(buf[:n], id, host)
		switch {
		case errors.Is(err, errNotOurReply):
			// Keep listening until the deadline: a forged datagram must be
			// able neither to supply the answer nor to end the wait for the
			// real one.
			continue
		case errors.Is(err, errTruncatedReply):
			return queryTCP(ctx, server, host)
		}
		return addrs, ttl, err
	}
}

// queryTCP is queryUDP over TCP, where each message is preceded by its
// length. Only used when the UDP reply came back truncated.
func queryTCP(ctx context.Context, server, host string) ([]string, time.Duration, error) {
	msg, id, err := aQuery(host)
	if err != nil {
		return nil, 0, err
	}
	d := net.Dialer{Timeout: 3 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", server)
	if err != nil {
		return nil, 0, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(dnsDeadline(ctx))
	framed := make([]byte, 2, 2+len(msg))
	binary.BigEndian.PutUint16(framed, uint16(len(msg)))
	if _, err := conn.Write(append(framed, msg...)); err != nil {
		return nil, 0, err
	}
	var size [2]byte
	if _, err := io.ReadFull(conn, size[:]); err != nil {
		return nil, 0, err
	}
	reply := make([]byte, binary.BigEndian.Uint16(size[:]))
	if _, err := io.ReadFull(conn, reply); err != nil {
		return nil, 0, err
	}
	return parseAResponse(reply, id, host)
}

// dnsDeadline gives one plain DNS exchange three seconds, or what is left of
// the lookup if that is less.
func dnsDeadline(ctx context.Context) time.Time {
	deadline := time.Now().Add(3 * time.Second)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	return deadline
}

// aQuery builds an A query for host under a random id. The id comes from
// crypto/rand: it is one of the two things (with the source port) a forged
// reply has to guess, and math/rand, used here until 2026-10-12, is
// predictable from a few of its outputs.
func aQuery(host string) (msg []byte, id uint16, err error) {
	var raw [2]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return nil, 0, fmt.Errorf("DNS query id: %w", err)
	}
	id = binary.BigEndian.Uint16(raw[:])
	msg = make([]byte, 12, 64)
	binary.BigEndian.PutUint16(msg[0:], id)
	binary.BigEndian.PutUint16(msg[2:], 0x0100) // recursion desired
	binary.BigEndian.PutUint16(msg[4:], 1)      // one question
	for _, label := range strings.Split(strings.TrimSuffix(host, "."), ".") {
		if len(label) == 0 || len(label) > 63 {
			return nil, 0, fmt.Errorf("bad host name %q", host)
		}
		msg = append(msg, byte(len(label)))
		msg = append(msg, label...)
	}
	return append(msg, 0, 0, 1, 0, 1), id, nil // root, type A, class IN
}

var (
	// errNotOurReply is a datagram that does not answer the query sent: the
	// wrong id, not a response at all, or a different question.
	errNotOurReply = errors.New("DNS reply does not match the query")
	// errTruncatedReply is a reply the server cut short to fit UDP.
	errTruncatedReply = errors.New("truncated DNS reply")
)

// parseAResponse extracts the A records from a DNS reply to the A query for
// host sent under id. It trusts nothing in a reply until the id AND the
// question it echoes are the ones sent: the id alone is 16 bits.
func parseAResponse(b []byte, id uint16, host string) ([]string, time.Duration, error) {
	if len(b) < 12 {
		return nil, 0, errors.New("short DNS reply")
	}
	if binary.BigEndian.Uint16(b[0:]) != id {
		return nil, 0, fmt.Errorf("%w: id mismatch", errNotOurReply)
	}
	if b[2]&0x80 == 0 {
		return nil, 0, fmt.Errorf("%w: not a response", errNotOurReply)
	}
	if binary.BigEndian.Uint16(b[4:]) != 1 {
		return nil, 0, fmt.Errorf("%w: not one question", errNotOurReply)
	}
	an := int(binary.BigEndian.Uint16(b[6:]))
	name, off, err := questionName(b, 12)
	if err != nil {
		return nil, 0, err
	}
	if off+4 > len(b) {
		return nil, 0, errors.New("truncated DNS question")
	}
	if !strings.EqualFold(name, strings.TrimSuffix(host, ".")) ||
		binary.BigEndian.Uint16(b[off:]) != 1 || binary.BigEndian.Uint16(b[off+2:]) != 1 {
		return nil, 0, fmt.Errorf("%w: it answers a different question", errNotOurReply)
	}
	off += 4
	if rcode := b[3] & 0x0f; rcode != 0 {
		return nil, 0, fmt.Errorf("DNS rcode %d", rcode)
	}
	if b[2]&0x02 != 0 {
		return nil, 0, errTruncatedReply
	}
	var addrs []string
	ttl := uint32(maxDNSTTL / time.Second)
	for i := 0; i < an; i++ {
		var err error
		if off, err = skipName(b, off); err != nil {
			return nil, 0, err
		}
		if off+10 > len(b) {
			return nil, 0, errors.New("truncated DNS answer")
		}
		typ := binary.BigEndian.Uint16(b[off:])
		class := binary.BigEndian.Uint16(b[off+2:])
		t := binary.BigEndian.Uint32(b[off+4:])
		rdlen := int(binary.BigEndian.Uint16(b[off+8:]))
		off += 10
		if off+rdlen > len(b) {
			return nil, 0, errors.New("truncated DNS record")
		}
		if typ == 1 && class == 1 && rdlen == 4 {
			ip := net.IP(b[off : off+4])
			// Same guard as the DoH path: loopback would loop into us.
			if kind := selfAddress(ip); kind != "" {
				return nil, 0, fmt.Errorf("DNS returned %s %s for %s -- refusing (this would loop back into the gateway)", kind, ip, host)
			}
			addrs = append(addrs, ip.String())
			if t > 0 && t < ttl {
				ttl = t
			}
		}
		off += rdlen
	}
	return addrs, time.Duration(ttl) * time.Second, nil
}

// selfAddress names the kind of address that reaches this Mac whatever
// network it is on, "" for any other. 0.0.0.0 counts: connecting to it lands
// on this host, and it is what a blocking DNS server answers with.
func selfAddress(ip net.IP) string {
	switch {
	case ip.IsLoopback():
		return "loopback"
	case ip.IsUnspecified():
		return "the unspecified address"
	}
	return ""
}

// questionName reads the name in a reply's question section, which is the
// query's own bytes echoed back and so never compressed.
func questionName(b []byte, off int) (string, int, error) {
	var labels []string
	for {
		if off >= len(b) {
			return "", 0, errors.New("truncated DNS name")
		}
		l := int(b[off])
		off++
		if l == 0 {
			return strings.Join(labels, "."), off, nil
		}
		if l > 63 || off+l > len(b) {
			return "", 0, fmt.Errorf("%w: unreadable question", errNotOurReply)
		}
		labels = append(labels, string(b[off:off+l]))
		off += l
	}
}

// skipName steps over a possibly compressed DNS name.
func skipName(b []byte, off int) (int, error) {
	for {
		if off >= len(b) {
			return 0, errors.New("truncated DNS name")
		}
		l := int(b[off])
		switch {
		case l == 0:
			return off + 1, nil
		case l&0xc0 == 0xc0:
			return off + 2, nil
		default:
			off += 1 + l
		}
	}
}

// resolverCacheFile is the last good answer per host, kept on disk.
type resolverCacheFile map[string][]string

func resolverCachePath(statePath string) string {
	if statePath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(statePath), "resolver-cache.json")
}

// loadCache seeds the in-memory cache with the last good answers, already
// expired, so they are used only when every resolver fails. Without this, a
// restart on a network that blocks lookups had no address at all.
func (r *interceptResolver) loadCache() {
	if r.cachePath == "" {
		return
	}
	b, err := os.ReadFile(r.cachePath)
	if err != nil {
		return
	}
	var f resolverCacheFile
	if json.Unmarshal(b, &f) != nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for host, addrs := range f {
		var good []string
		for _, a := range addrs {
			if ip := net.ParseIP(a); ip != nil && selfAddress(ip) == "" {
				good = append(good, a)
			}
		}
		if len(good) > 0 {
			r.cache[host] = dnsEntry{addrs: good}
		}
	}
}

// saveCache writes the answer, only when it changed, so a healthy gateway
// does not rewrite the file every few minutes.
func (r *interceptResolver) saveCache(host string, addrs []string) {
	if r.cachePath == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	f := resolverCacheFile{}
	if b, err := os.ReadFile(r.cachePath); err == nil {
		_ = json.Unmarshal(b, &f)
	}
	if strings.Join(f[host], ",") == strings.Join(addrs, ",") {
		return
	}
	f[host] = addrs
	b, err := json.Marshal(f)
	if err != nil {
		return
	}
	tmp := r.cachePath + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, r.cachePath)
	}
}
