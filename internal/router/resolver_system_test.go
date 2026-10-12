package router

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// systemSeam stands in for `scutil --dns`: it hands back servers and counts
// how often it was consulted.
func systemSeam(calls *int32, servers ...string) func(context.Context) ([]string, error) {
	return func(context.Context) ([]string, error) {
		atomic.AddInt32(calls, 1)
		return servers, nil
	}
}

// The network's own resolvers are the least trusted source and are asked
// last: not while a DoH endpoint answers, and not while a public DNS server
// does.
func TestSystemResolversAreNotAskedWhileAPublicOneAnswers(t *testing.T) {
	sys, sysQueries := udpDNSServer(t, [4]byte{160, 79, 104, 99})
	var discovered int32

	good := dohServer(t, dohResponse{Answer: []dohAnswer{{Name: "api.anthropic.com", Type: 1, TTL: 60, Data: "160.79.104.10"}}}, nil)
	defer good.Close()
	r := newInterceptResolver("api.anthropic.com", good.URL, "")
	r.systemServers = systemSeam(&discovered, sys)
	if addrs, err := r.lookup(context.Background(), "api.anthropic.com"); err != nil || addrs[0] != "160.79.104.10" {
		t.Fatalf("lookup = %v, %v; want the DoH answer", addrs, err)
	}

	public, _ := udpDNSServer(t, [4]byte{160, 79, 104, 11})
	r = newInterceptResolver("api.anthropic.com", resettingServer(t).URL, "")
	r.udpServers = []string{public}
	r.systemServers = systemSeam(&discovered, sys)
	if addrs, err := r.lookup(context.Background(), "api.anthropic.com"); err != nil || addrs[0] != "160.79.104.11" {
		t.Fatalf("lookup = %v, %v; want the public DNS answer", addrs, err)
	}

	if discovered != 0 || sysQueries() != 0 {
		t.Fatalf("this network's resolvers were looked up %d times and asked %d times while a public resolver answered",
			discovered, sysQueries())
	}
}

// A network that blocks every public resolver, DoH and port 53 alike, used to
// leave the gateway with no way to find Anthropic. The resolver the network
// handed out is asked then, and its answer used.
func TestSystemResolverAnswersWhenEveryPublicOneFails(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadUDP := pc.LocalAddr().String()
	pc.Close()
	sys, sysQueries := udpDNSServer(t, [4]byte{160, 79, 104, 10})
	var discovered int32

	r := newInterceptResolver("api.anthropic.com", resettingServer(t).URL, "")
	r.fallbacks = []string{resettingServer(t).URL}
	r.udpServers = []string{deadUDP}
	// The dead public server is listed by the network too: it has had its
	// turn and must not be asked a second time.
	r.systemServers = systemSeam(&discovered, deadUDP, sys)
	r.localAddrs = func() []net.IP { return []net.IP{net.ParseIP("192.168.1.20")} }

	addrs, err := r.lookup(context.Background(), "api.anthropic.com")
	if err != nil || len(addrs) != 1 || addrs[0] != "160.79.104.10" {
		t.Fatalf("lookup = %v, %v; want this network's resolver's answer", addrs, err)
	}
	if discovered != 1 || sysQueries() != 1 {
		t.Fatalf("discovery ran %d times, the resolver saw %d queries; want 1 and 1", discovered, sysQueries())
	}
}

// An answer that is this Mac sends the gateway's own request back into the
// gateway. From the network's resolver that is loopback, 0.0.0.0, or one of
// this Mac's interface addresses; each is refused, and the next server asked.
func TestSystemResolverAnswerPointingAtThisMacIsRefused(t *testing.T) {
	own := net.ParseIP("192.168.1.20")
	for name, ip := range map[string][4]byte{
		"loopback":            {127, 0, 0, 1},
		"unspecified address": {0, 0, 0, 0},
		"address of this Mac": {192, 168, 1, 20},
	} {
		t.Run(name, func(t *testing.T) {
			bad, _ := udpDNSServer(t, ip)
			var discovered int32
			r := newInterceptResolver("api.anthropic.com", resettingServer(t).URL, "")
			r.systemServers = systemSeam(&discovered, bad)
			r.localAddrs = func() []net.IP { return []net.IP{own} }

			addrs, err := r.lookup(context.Background(), "api.anthropic.com")
			var le *LookupError
			if !errors.As(err, &le) || len(addrs) != 0 {
				t.Fatalf("lookup = %v, %v; want a LookupError and no address", addrs, err)
			}
			if !strings.Contains(err.Error(), "refusing") || !strings.Contains(err.Error(), "this network's resolver") {
				t.Errorf("error does not say what was refused, or from where: %v", err)
			}

			good, _ := udpDNSServer(t, [4]byte{160, 79, 104, 10})
			r = newInterceptResolver("api.anthropic.com", resettingServer(t).URL, "")
			r.systemServers = systemSeam(&discovered, bad, good)
			r.localAddrs = func() []net.IP { return []net.IP{own} }
			addrs, err = r.lookup(context.Background(), "api.anthropic.com")
			if err != nil || len(addrs) != 1 || addrs[0] != "160.79.104.10" {
				t.Fatalf("lookup = %v, %v; want the next server's answer", addrs, err)
			}
		})
	}
}

// scutil missing, hanging or failing must cost nothing but this fallback: the
// lookup fails the way it did before, as a LookupError naming every attempt,
// and an address already held is still used.
func TestSystemResolverDiscoveryFailingIsHarmless(t *testing.T) {
	failing := func(context.Context) ([]string, error) {
		return nil, errors.New("scutil --dns: exec: not found")
	}
	first := resettingServer(t)
	r := newInterceptResolver("api.anthropic.com", first.URL, "")
	r.systemServers = failing

	_, err := r.lookup(context.Background(), "api.anthropic.com")
	var le *LookupError
	if !errors.As(err, &le) {
		t.Fatalf("err = %v (%T), want a *LookupError", err, err)
	}
	for _, want := range []string{first.URL, "system resolvers: scutil --dns"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name %s: %v", want, err)
		}
	}

	r = newInterceptResolver("api.anthropic.com", first.URL, "")
	r.systemServers = failing
	r.cache["api.anthropic.com"] = dnsEntry{addrs: []string{"160.79.104.10"}} // expired
	addrs, err := r.lookup(context.Background(), "api.anthropic.com")
	if err != nil || len(addrs) != 1 || addrs[0] != "160.79.104.10" {
		t.Fatalf("lookup = %v, %v; want the address already held", addrs, err)
	}
}

// No more than systemServerLimit of the network's servers are asked: each
// dead one costs three seconds of a lookup that has already spent most of its
// budget on the public resolvers.
func TestSystemResolversAskedAreBounded(t *testing.T) {
	var servers []string
	var counts []func() int
	for i := 0; i < systemServerLimit+2; i++ {
		addr, n := emptyDNSServer(t)
		servers = append(servers, addr)
		counts = append(counts, n)
	}
	var discovered int32
	r := newInterceptResolver("api.anthropic.com", resettingServer(t).URL, "")
	r.systemServers = systemSeam(&discovered, servers...)
	if _, err := r.lookup(context.Background(), "api.anthropic.com"); err == nil {
		t.Fatal("want an error when no server has a record")
	}
	asked := 0
	for _, n := range counts {
		asked += n()
	}
	if asked != systemServerLimit {
		t.Fatalf("%d of this network's resolvers were asked, want %d", asked, systemServerLimit)
	}
}

// emptyDNSServer answers every query with no records.
func emptyDNSServer(t *testing.T) (addr string, queries func() int) {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	var n int32
	go func() {
		buf := make([]byte, 512)
		for {
			m, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			atomic.AddInt32(&n, 1)
			reply := append([]byte(nil), buf[:m]...)
			reply[2] |= 0x80 // the query, marked as a response
			_, _ = pc.WriteTo(reply, from)
		}
	}()
	return pc.LocalAddr().String(), func() int { return int(atomic.LoadInt32(&n)) }
}

// `scutil --dns` as a Mac on a VPN prints it: the same server under several
// resolvers and again under the scoped queries, an mDNS resolver with no
// server, and an IPv6 link-local server that is only reachable with its
// interface.
func TestParseScutilDNS(t *testing.T) {
	out := `DNS configuration

resolver #1
  search domain[0] : corp.example
  nameserver[0] : 10.20.0.1
  nameserver[1] : 10.20.0.2
  flags    : Supplemental, Request A records
  reach    : 0x00030002 (Reachable,Local Address,Directly Reachable Address)
  order    : 102600

resolver #2
  nameserver[0] : 1.1.1.1
  nameserver[1] : fe80::1%en0
  nameserver[2] : not-an-address
  if_index : 11 (en0)
  flags    : Request A records

resolver #3
  domain   : local
  options  : mdns
  timeout  : 5
  order    : 300000

DNS configuration (for scoped queries)

resolver #1
  search domain[0] : corp.example
  nameserver[0] : 10.20.0.1
  nameserver[1] : 2001:db8::53
  if_index : 11 (en0)
`
	want := []string{"10.20.0.1:53", "10.20.0.2:53", "1.1.1.1:53", "[fe80::1%en0]:53", "[2001:db8::53]:53"}
	if got := parseScutilDNS(out); !reflect.DeepEqual(got, want) {
		t.Fatalf("parseScutilDNS =\n  %v\nwant\n  %v", got, want)
	}
	if got := parseScutilDNS("scutil: command produced nothing useful"); len(got) != 0 {
		t.Fatalf("unrecognised output gave servers: %v", got)
	}
}

// A reply is trusted only when it echoes the question that was asked. The
// id alone is 16 bits, and a reply about another name, with the right id,
// used to be taken as the answer.
func TestParseAResponseRefusesAReplyToAnotherQuestion(t *testing.T) {
	reply := dnsReply(7, [4]byte{160, 79, 104, 10}) // answers api.anthropic.com
	if _, _, err := parseAResponse(reply, 7, "api.example.com"); !errors.Is(err, errNotOurReply) {
		t.Fatalf("a reply about another name: err = %v, want errNotOurReply", err)
	}
	if addrs, _, err := parseAResponse(reply, 7, "API.Anthropic.com."); err != nil || len(addrs) != 1 {
		t.Fatalf("case and a trailing dot must not matter: %v, %v", addrs, err)
	}
	query := append([]byte(nil), reply...)
	query[2] &^= 0x80
	if _, _, err := parseAResponse(query, 7, "api.anthropic.com"); !errors.Is(err, errNotOurReply) {
		t.Fatalf("a query is not a reply: err = %v, want errNotOurReply", err)
	}
	aaaa := append([]byte(nil), reply...)
	aaaa[12+19+1] = 28 // the question's type: AAAA
	if _, _, err := parseAResponse(aaaa, 7, "api.anthropic.com"); !errors.Is(err, errNotOurReply) {
		t.Fatalf("a reply to an AAAA question: err = %v, want errNotOurReply", err)
	}
	if _, _, err := parseAResponse(dnsReply(7, [4]byte{0, 0, 0, 0}), 7, "api.anthropic.com"); err == nil {
		t.Fatal("0.0.0.0 reaches this Mac and must be refused")
	}
}

// A datagram that does not match the query must neither be believed nor end
// the wait: the real reply, arriving after it, is the one used.
func TestQueryUDPIgnoresAForgedReplyAndWaitsForTheRealOne(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	go func() {
		buf := make([]byte, 512)
		m, from, err := pc.ReadFrom(buf)
		if err != nil || m < 12 {
			return
		}
		id := binary.BigEndian.Uint16(buf[0:])
		_, _ = pc.WriteTo(dnsReply(id+1, [4]byte{203, 0, 113, 66}), from) // a guess at the id
		_, _ = pc.WriteTo(dnsReply(id, [4]byte{160, 79, 104, 10}), from)
	}()
	addrs, _, err := queryUDP(context.Background(), pc.LocalAddr().String(), "api.anthropic.com")
	if err != nil || len(addrs) != 1 || addrs[0] != "160.79.104.10" {
		t.Fatalf("queryUDP = %v, %v; want the reply that matches the query", addrs, err)
	}
}

// A reply cut short to fit a datagram is fetched again over TCP.
func TestQueryUDPRetriesATruncatedReplyOverTCP(t *testing.T) {
	var tcp net.Listener
	var pc net.PacketConn
	for i := 0; i < 20; i++ { // the same port on both, as a DNS server has
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		if p, err := net.ListenPacket("udp", l.Addr().String()); err == nil {
			tcp, pc = l, p
			break
		}
		l.Close()
	}
	if tcp == nil {
		t.Skip("no port free on both TCP and UDP")
	}
	defer tcp.Close()
	defer pc.Close()
	go func() {
		buf := make([]byte, 512)
		m, from, err := pc.ReadFrom(buf)
		if err != nil || m < 12 {
			return
		}
		reply := dnsReply(binary.BigEndian.Uint16(buf[0:]), [4]byte{203, 0, 113, 66})
		reply[2] |= 0x02 // truncated: nothing in it is to be used
		_, _ = pc.WriteTo(reply, from)
	}()
	go func() {
		c, err := tcp.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		var size [2]byte
		if _, err := io.ReadFull(c, size[:]); err != nil {
			return
		}
		q := make([]byte, binary.BigEndian.Uint16(size[:]))
		if _, err := io.ReadFull(c, q); err != nil {
			return
		}
		reply := dnsReply(binary.BigEndian.Uint16(q[0:]), [4]byte{160, 79, 104, 10})
		binary.BigEndian.PutUint16(size[:], uint16(len(reply)))
		_, _ = c.Write(append(size[:], reply...))
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	addrs, _, err := queryUDP(ctx, pc.LocalAddr().String(), "api.anthropic.com")
	if err != nil || len(addrs) != 1 || addrs[0] != "160.79.104.10" {
		t.Fatalf("queryUDP = %v, %v; want the answer fetched over TCP", addrs, err)
	}
}

// Query ids come from crypto/rand. Sixty-four in a row being equal is what a
// fixed or unseeded source looks like, and has a chance of nothing otherwise.
func TestQueryIDsAreNotFixed(t *testing.T) {
	seen := map[uint16]bool{}
	for i := 0; i < 64; i++ {
		msg, id, err := aQuery("api.anthropic.com")
		if err != nil {
			t.Fatal(err)
		}
		if binary.BigEndian.Uint16(msg[0:]) != id {
			t.Fatal("the id returned is not the id in the query")
		}
		seen[id] = true
	}
	if len(seen) < 32 {
		t.Fatalf("64 queries used %d distinct ids", len(seen))
	}
}
