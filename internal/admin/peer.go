package admin

import (
	"context"
	"encoding/binary"
	"errors"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"sync"
	"syscall"
	"time"
)

// The dashboard has no login: it is reached from this Mac alone, and the
// guard keeps web pages out. That left every other account on the Mac, since
// loopback is shared between them: a second user could read conversations
// and change settings with one curl. So the API also asks whose process is
// on the other end of the connection, and answers only the account the
// gateway runs as.
//
// macOS has no call that names the owner of a TCP peer (LOCAL_PEERCRED is
// for Unix sockets), so the answer comes from the kernel's list of TCP
// sockets, the one netstat reads, which any account may read and which
// carries the uid that opened each socket. It is asked once per connection,
// on the first API call, and a browser keeps its connections open. It used
// to be asked of lsof, which walks every process on the Mac: about a second
// of CPU each time, and with the usage panels opening a connection per poll
// that kept the gateway near 70% of a core and the Mac hot.

// peerVerdict is what is known about the process at the far end.
type peerVerdict int

const (
	// peerUnknown: the socket list could not be read or made no sense. Let
	// in, and said in the log: the console must stay reachable, and another
	// account cannot make the kernel's list unreadable.
	peerUnknown peerVerdict = iota
	peerOurs
	peerForeign
)

// peerOpenPath is the one API route answered whoever asks: the pf guard
// runs as root and posts its alerts here, and all it can do is raise a
// notice.
const peerOpenPath = "/api/alert"

type peerKey struct{}

// peer is one connection's answer, looked up on first use.
type peer struct {
	once          sync.Once
	remote, local string
	verdict       peerVerdict
}

// NewHTTPServer is the server both the dashboard and the support console
// are served from: it notes each connection so the guard can ask who owns
// the far end. A handler served any other way is not checked, which is how
// the tests call it.
func NewHTTPServer(h http.Handler) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			return context.WithValue(ctx, peerKey{}, &peer{remote: c.RemoteAddr().String(), local: c.LocalAddr().String()})
		},
	}
}

// foreignPeer reports a request from a process of another account.
func foreignPeer(r *http.Request) bool {
	p, _ := r.Context().Value(peerKey{}).(*peer)
	if p == nil {
		return false
	}
	p.once.Do(func() { p.verdict = peerOwner(p.remote, p.local) })
	return p.verdict == peerForeign
}

// peerOwner is a variable so tests can answer for the kernel.
var peerOwner = socketPeerOwner

var peerUnknownLogged sync.Once

// tcpSockets reads the kernel's TCP socket list; a variable so a test can
// hand it a list that makes no sense.
var tcpSockets = func() ([]byte, error) {
	s, err := syscall.Sysctl("net.inet.tcp.pcblist_n")
	return []byte(s), err
}

func socketPeerOwner(remote, local string) peerVerdict {
	list, err := tcpSockets()
	if err == nil {
		var v peerVerdict
		if v, err = judgePeer(list, remote, local, os.Getuid()); err == nil {
			return v
		}
	}
	peerUnknownLogged.Do(func() {
		log.Printf("admin: cannot tell which account is calling the dashboard (%v), so callers are not checked", err)
	})
	return peerUnknown
}

// tcpSocket is what is needed of one entry in the kernel's list.
type tcpSocket struct {
	local, remote netip.AddrPort
	uid           int
}

// Record kinds and offsets in net.inet.tcp.pcblist_n, from xnu's
// xinpcb_n and xsocket_n (bsd/netinet/in_pcb.h, bsd/sys/socketvar.h),
// which are packed to 4 bytes. Each socket is an xinpcb_n followed by its
// xsocket_n and further records; every record starts with its length and
// kind and is padded to 8 bytes.
const (
	xsoSocket = 0x001
	xsoInpcb  = 0x010

	inpFport, inpLport = 16, 18
	inpVflag           = 44
	inpFaddr, inpLaddr = 48, 64 // in6_addr; an IPv4 address is its last 4 bytes
	inpLen             = 80
	sockUID            = 64
	sockLen            = 68
	inpIPv4            = 0x1
)

// parseTCPSockets reads the list. It fails on anything it does not
// understand, rather than guessing at a layout a new macOS has changed.
func parseTCPSockets(b []byte) ([]tcpSocket, error) {
	if len(b) < 4 {
		return nil, errors.New("TCP socket list is empty")
	}
	var out []tcpSocket
	var cur *tcpSocket
	off := int(binary.LittleEndian.Uint32(b))
	for off+8 <= len(b) {
		n := int(binary.LittleEndian.Uint32(b[off:]))
		kind := binary.LittleEndian.Uint32(b[off+4:])
		if n < 8 || off+n > len(b) {
			break // the trailer, which repeats the header
		}
		rec := b[off : off+n]
		switch kind {
		case xsoInpcb:
			if n < inpLen {
				return nil, errors.New("TCP socket list has records shorter than expected")
			}
			addr := func(at int) netip.Addr {
				a := netip.AddrFrom16([16]byte(rec[at : at+16]))
				if rec[inpVflag]&inpIPv4 != 0 {
					return netip.AddrFrom4([4]byte(rec[at+12 : at+16]))
				}
				return a
			}
			out = append(out, tcpSocket{
				local:  netip.AddrPortFrom(addr(inpLaddr), binary.BigEndian.Uint16(rec[inpLport:])),
				remote: netip.AddrPortFrom(addr(inpFaddr), binary.BigEndian.Uint16(rec[inpFport:])),
				uid:    -1,
			})
			cur = &out[len(out)-1]
		case xsoSocket:
			if n < sockLen {
				return nil, errors.New("TCP socket list has records shorter than expected")
			}
			if cur != nil {
				cur.uid = int(binary.LittleEndian.Uint32(rec[sockUID:]))
				cur = nil
			}
		}
		off += (n + 7) &^ 7
	}
	return out, nil
}

// judgePeer finds the caller's end of the connection from remote to local
// and says whether uid holds it. The gateway's own end must be in the list
// as well, under uid: when it is not, the list was misread, and nobody is
// shut out on the strength of that.
func judgePeer(list []byte, remote, local string, uid int) (peerVerdict, error) {
	r, err1 := netip.ParseAddrPort(remote)
	l, err2 := netip.ParseAddrPort(local)
	if err := errors.Join(err1, err2); err != nil {
		return peerUnknown, err
	}
	r, l = netip.AddrPortFrom(r.Addr().Unmap(), r.Port()), netip.AddrPortFrom(l.Addr().Unmap(), l.Port())
	socks, err := parseTCPSockets(list)
	if err != nil {
		return peerUnknown, err
	}
	ownEnd, caller := false, false
	for _, s := range socks {
		sl, sr := netip.AddrPortFrom(s.local.Addr().Unmap(), s.local.Port()), netip.AddrPortFrom(s.remote.Addr().Unmap(), s.remote.Port())
		switch {
		case sl == l && sr == r && s.uid == uid:
			ownEnd = true
		case sl == r && sr == l:
			if s.uid == uid {
				caller = true
			}
		}
	}
	if !ownEnd {
		return peerUnknown, errors.New("the gateway's own end of the connection is not in the TCP socket list")
	}
	if caller {
		return peerOurs, nil
	}
	return peerForeign, nil
}
