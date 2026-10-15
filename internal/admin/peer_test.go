package admin

import (
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"
)

// servePeerChecked serves h the way the dashboard is served, on a real
// loopback port, and returns its address.
func servePeerChecked(t *testing.T, h http.Handler) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := NewHTTPServer(h)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().String()
}

func peerGet(t *testing.T, url string) (int, string) {
	t.Helper()
	tr := &http.Transport{}
	defer tr.CloseIdleConnections()
	resp, err := (&http.Client{Transport: tr}).Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func peerAPI() http.Handler {
	mux := http.NewServeMux()
	ok := func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "answered") }
	mux.HandleFunc("/", ok)
	mux.HandleFunc("/api/log", ok)
	mux.HandleFunc(peerOpenPath, ok)
	return guard("", mux)
}

// Another account's process gets nothing from the API, and the page shell
// and the root guard's alert route still answer.
func TestAnotherAccountIsRefusedTheAPI(t *testing.T) {
	asked := 0
	old := peerOwner
	peerOwner = func(remote, local string) peerVerdict { asked++; return peerForeign }
	t.Cleanup(func() { peerOwner = old })
	addr := servePeerChecked(t, peerAPI())

	if code, body := peerGet(t, "http://"+addr+"/api/log"); code != http.StatusForbidden || strings.Contains(body, "answered") {
		t.Errorf("another account read the API: %d %q", code, body)
	}
	if asked != 1 {
		t.Fatalf("the owner of the connection was asked for %d time(s), want 1: the refusal above proves nothing", asked)
	}
	for _, open := range []string{"/", peerOpenPath} {
		if code, _ := peerGet(t, "http://"+addr+open); code != http.StatusOK {
			t.Errorf("%s: got %d, want it answered whoever asks", open, code)
		}
	}
}

// When the socket list cannot be read the dashboard still answers: the
// console has to stay reachable, and another account cannot cause that.
func TestAnUnknownCallerIsLetIn(t *testing.T) {
	old := peerOwner
	peerOwner = func(remote, local string) peerVerdict { return peerUnknown }
	t.Cleanup(func() { peerOwner = old })
	addr := servePeerChecked(t, peerAPI())
	if code, _ := peerGet(t, "http://"+addr+"/api/log"); code != http.StatusOK {
		t.Errorf("got %d with the caller unknown, want 200", code)
	}
}

// The real kernel list, on a real connection: this test's own request is
// ours, and the same question about a caller nobody holds is not.
func TestTheKernelNamesThisProcessAsTheCaller(t *testing.T) {
	var verdicts []peerVerdict
	old := peerOwner
	peerOwner = func(remote, local string) peerVerdict {
		v := socketPeerOwner(remote, local)
		verdicts = append(verdicts, v)
		return v
	}
	t.Cleanup(func() { peerOwner = old })
	addr := servePeerChecked(t, peerAPI())
	if code, body := peerGet(t, "http://"+addr+"/api/log"); code != http.StatusOK {
		t.Fatalf("this process was refused its own dashboard: %d %q", code, body)
	}
	if len(verdicts) != 1 || verdicts[0] != peerOurs {
		t.Fatalf("the kernel's answers were %v, want one peerOurs", verdicts)
	}

	// A connection held open, judged from its own two ends.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	s, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	list, err := tcpSockets()
	if err != nil {
		t.Fatal(err)
	}
	remote, local := s.RemoteAddr().String(), s.LocalAddr().String()
	if v, err := judgePeer(list, remote, local, os.Getuid()); v != peerOurs || err != nil {
		t.Errorf("this account's caller: %v %v, want peerOurs", v, err)
	}
	if v, err := judgePeer(list, remote, local, os.Getuid()+1); v != peerUnknown || err == nil {
		t.Errorf("judged as another account, the gateway's own end is not its own, so the list cannot be trusted: got %v %v, want peerUnknown", v, err)
	}
}

// pcbList encodes sockets the way the kernel lists them: a header, then an
// xinpcb_n and an xsocket_n for each, then a trailer.
func pcbList(socks ...tcpSocket) []byte {
	b := make([]byte, 24)
	binary.LittleEndian.PutUint32(b, 24)
	for _, s := range socks {
		inp := make([]byte, 104)
		binary.LittleEndian.PutUint32(inp, 104)
		binary.LittleEndian.PutUint32(inp[4:], xsoInpcb)
		binary.BigEndian.PutUint16(inp[inpFport:], s.remote.Port())
		binary.BigEndian.PutUint16(inp[inpLport:], s.local.Port())
		if s.local.Addr().Is4() {
			inp[inpVflag] = inpIPv4
			copy(inp[inpFaddr+12:], s.remote.Addr().AsSlice())
			copy(inp[inpLaddr+12:], s.local.Addr().AsSlice())
		} else {
			inp[inpVflag] = 0x2
			copy(inp[inpFaddr:], s.remote.Addr().AsSlice())
			copy(inp[inpLaddr:], s.local.Addr().AsSlice())
		}
		so := make([]byte, 104)
		binary.LittleEndian.PutUint32(so, 104)
		binary.LittleEndian.PutUint32(so[4:], xsoSocket)
		binary.LittleEndian.PutUint32(so[sockUID:], uint32(s.uid))
		// A record of another kind between them, as the kernel sends.
		buf := make([]byte, 28) // padded to 32 below
		binary.LittleEndian.PutUint32(buf, 28)
		binary.LittleEndian.PutUint32(buf[4:], 0x002)
		b = append(b, inp...)
		b = append(b, so...)
		b = append(append(b, buf...), 0, 0, 0, 0)
	}
	return append(b, make([]byte, 24)...)
}

func TestTheCallersEndMustBeHeldByThisAccount(t *testing.T) {
	ap := netip.MustParseAddrPort
	const remote, local = "127.0.0.1:50001", "127.0.0.1:7788"
	gw := tcpSocket{local: ap(local), remote: ap(remote), uid: 501}
	caller := func(uid int) tcpSocket { return tcpSocket{local: ap(remote), remote: ap(local), uid: uid} }
	for _, c := range []struct {
		name  string
		socks []tcpSocket
		want  peerVerdict
	}{
		{"the caller, this account", []tcpSocket{gw, caller(501)}, peerOurs},
		{"the caller, root", []tcpSocket{gw, caller(0)}, peerForeign},
		{"the caller, another account, listed first", []tcpSocket{caller(502), gw}, peerForeign},
		{"no caller listed", []tcpSocket{gw}, peerForeign},
		{"a caller of ours to another port", []tcpSocket{gw, {local: ap(remote), remote: ap("127.0.0.1:7789"), uid: 501}}, peerForeign},
		{"no gateway end: the list was misread", []tcpSocket{caller(501)}, peerUnknown},
	} {
		got, _ := judgePeer(pcbList(c.socks...), remote, local, 501)
		if got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
	v6 := []tcpSocket{
		{local: ap("[::1]:7788"), remote: ap("[::1]:50001"), uid: 501},
		{local: ap("[::1]:50001"), remote: ap("[::1]:7788"), uid: 501},
	}
	if got, err := judgePeer(pcbList(v6...), "[::1]:50001", "[::1]:7788", 501); got != peerOurs {
		t.Errorf("over IPv6: got %v %v, want peerOurs", got, err)
	}
}

// A list that cannot be read shuts nobody out.
func TestAMisreadSocketListLetsCallersIn(t *testing.T) {
	old := tcpSockets
	t.Cleanup(func() { tcpSockets = old })
	for name, list := range map[string][]byte{
		"empty":        nil,
		"header only":  {24, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
		"short record": append([]byte{4, 0, 0, 0}, 16, 0, 0, 0, xsoInpcb, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0),
	} {
		tcpSockets = func() ([]byte, error) { return list, nil }
		if v := socketPeerOwner("127.0.0.1:50001", "127.0.0.1:7788"); v != peerUnknown {
			t.Errorf("%s: got %v, want peerUnknown", name, v)
		}
	}
}

// Checking a caller costs a millisecond or so, not the second lsof took,
// because the panels open a connection for every poll.
func TestCheckingACallerIsCheap(t *testing.T) {
	if testing.Short() {
		t.Skip("timing")
	}
	start := time.Now()
	for i := 0; i < 20; i++ {
		_ = socketPeerOwner("127.0.0.1:1", "127.0.0.1:2")
	}
	if per := time.Since(start) / 20; per > 50*time.Millisecond {
		t.Errorf("one check took %v", per)
	}
}
