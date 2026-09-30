package router

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
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
func queryUDP(ctx context.Context, server, host string) ([]string, time.Duration, error) {
	id := uint16(rand.Intn(1 << 16))
	msg := make([]byte, 12, 64)
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
	msg = append(msg, 0, 0, 1, 0, 1) // root, type A, class IN

	d := net.Dialer{Timeout: 3 * time.Second}
	conn, err := d.DialContext(ctx, "udp", server)
	if err != nil {
		return nil, 0, err
	}
	defer conn.Close()
	deadline := time.Now().Add(3 * time.Second)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	_ = conn.SetDeadline(deadline)
	if _, err := conn.Write(msg); err != nil {
		return nil, 0, err
	}
	buf := make([]byte, 1500)
	n, err := conn.Read(buf)
	if err != nil {
		return nil, 0, err
	}
	return parseAResponse(buf[:n], id, host)
}

// parseAResponse extracts the A records from a DNS reply to query id.
func parseAResponse(b []byte, id uint16, host string) ([]string, time.Duration, error) {
	if len(b) < 12 {
		return nil, 0, errors.New("short DNS reply")
	}
	if binary.BigEndian.Uint16(b[0:]) != id {
		return nil, 0, errors.New("DNS reply id mismatch")
	}
	if rcode := b[3] & 0x0f; rcode != 0 {
		return nil, 0, fmt.Errorf("DNS rcode %d", rcode)
	}
	qd := int(binary.BigEndian.Uint16(b[4:]))
	an := int(binary.BigEndian.Uint16(b[6:]))
	off := 12
	for i := 0; i < qd; i++ {
		var err error
		if off, err = skipName(b, off); err != nil {
			return nil, 0, err
		}
		off += 4
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
			if ip.IsLoopback() {
				return nil, 0, fmt.Errorf("DNS returned loopback %s for %s -- refusing (this would loop back into the gateway)", ip, host)
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
			if ip := net.ParseIP(a); ip != nil && !ip.IsLoopback() {
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
