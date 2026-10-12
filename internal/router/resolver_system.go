package router

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os/exec"
	"strings"
	"time"
)

// The last resolvers asked are the ones this Mac's network handed out.
//
// Every resolver before them is public: Cloudflare and Google, by name, by IP
// and on port 53. A network that lets none of those out (many corporate
// networks allow DNS to their own servers only) left the gateway unable to
// find Anthropic at all, on exactly the networks where it could have been
// found by asking the server next door.
//
// This is not "falling back to the system resolver", which DialContext rules
// out. The system resolver reads /etc/hosts, where transparent mode points
// the intercepted host at this gateway. Here the network's servers are sent
// the query directly on port 53, by queryUDP, so no hosts file and no
// per-host override on this Mac is consulted.
//
// They are asked last, and not remembered as preferred, because they are the
// least trusted source: a captive portal or a filtering resolver answers with
// its own address. What they say is used only to open a connection that must
// then present Anthropic's certificate, so a wrong answer fails the request
// and cannot read it. The one answer that would do harm is an address of this
// Mac, which would send the gateway's request back into the gateway, and that
// is refused: loopback and 0.0.0.0 by parseAResponse, and any address of this
// Mac's own interfaces here. A resolver running on this Mac that serves
// /etc/hosts (dnsmasq does by default) answers 127.0.0.1 and is refused the
// same way.

// systemServerLimit bounds how many of the network's servers are asked. Each
// can take three seconds to time out, and by the time they are reached the
// public resolvers have used much of lookupBudget.
const systemServerLimit = 4

// querySystem asks each of the network's DNS servers in turn and returns the
// first usable answer, or one line per failure for queryAll's error.
func (r *interceptResolver) querySystem(ctx context.Context, host string) ([]string, time.Duration, []string) {
	servers, err := r.systemServers(ctx)
	if err != nil {
		// scutil missing, slow or failing costs nothing but this fallback.
		return nil, 0, []string{"system resolvers: " + err.Error()}
	}
	asked := map[string]bool{}
	for _, s := range r.udpServers {
		asked[s] = true // a public server the network also lists has had its turn
	}
	var errs []string
	tried := 0
	for _, srv := range servers {
		if asked[srv] {
			continue
		}
		if ctx.Err() != nil || tried == systemServerLimit {
			break
		}
		tried++
		addrs, ttl, err := queryUDP(ctx, srv, host)
		if err == nil {
			err = r.refuseOwnAddress(addrs, host)
		}
		if err == nil && len(addrs) > 0 {
			return addrs, ttl, nil
		}
		if err == nil {
			err = fmt.Errorf("no A records")
		}
		errs = append(errs, "dns://"+srv+" (this network's resolver): "+err.Error())
	}
	return nil, 0, errs
}

// refuseOwnAddress rejects an answer naming one of this Mac's own interface
// addresses. parseAResponse has already refused loopback; this is the same
// loop by another address (192.168.1.20 is as much this Mac as 127.0.0.1 is).
func (r *interceptResolver) refuseOwnAddress(addrs []string, host string) error {
	if r.localAddrs == nil {
		return nil
	}
	own := r.localAddrs()
	for _, a := range addrs {
		ip := net.ParseIP(a)
		for _, mine := range own {
			if ip != nil && ip.Equal(mine) {
				return fmt.Errorf("DNS returned %s for %s, an address of this Mac -- refusing (this would loop back into the gateway)", a, host)
			}
		}
	}
	return nil
}

// interfaceAddrs is this Mac's own addresses, read at the moment of asking
// because they change with the network.
func interfaceAddrs() []net.IP {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var ips []net.IP
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			ips = append(ips, n.IP)
		}
	}
	return ips
}

// systemDNSServers asks macOS which DNS servers are configured. `scutil
// --dns` only reads: it is what the system's own resolver would use, and
// /etc/resolv.conf is not, on a Mac with a VPN or more than one interface.
func systemDNSServers(ctx context.Context) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/sbin/scutil", "--dns").Output()
	if err != nil {
		return nil, fmt.Errorf("scutil --dns: %w", err)
	}
	return parseScutilDNS(string(out)), nil
}

// parseScutilDNS picks the nameserver lines out of `scutil --dns`, in the
// order printed (which is the order macOS prefers them), each once, as
// host:53. The same server is listed under every search domain and again
// under "DNS configuration (for scoped queries)".
func parseScutilDNS(out string) []string {
	var servers []string
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		key, val, ok := strings.Cut(line, " : ")
		if !ok || !strings.HasPrefix(strings.TrimSpace(key), "nameserver[") {
			continue
		}
		// ParseAddr, not ParseIP: an IPv6 link-local server carries its
		// interface (fe80::1%en0) and cannot be reached without it.
		addr, err := netip.ParseAddr(strings.TrimSpace(val))
		if err != nil {
			continue
		}
		hp := net.JoinHostPort(addr.String(), "53")
		if !seen[hp] {
			seen[hp] = true
			servers = append(servers, hp)
		}
	}
	return servers
}
