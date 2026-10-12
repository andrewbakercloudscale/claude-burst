package admin

import (
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
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

// When lsof cannot be run the dashboard still answers: the console has to
// stay reachable, and another account cannot cause that failure.
func TestAnUnknownCallerIsLetIn(t *testing.T) {
	old := peerOwner
	peerOwner = func(remote, local string) peerVerdict { return peerUnknown }
	t.Cleanup(func() { peerOwner = old })
	addr := servePeerChecked(t, peerAPI())
	if code, _ := peerGet(t, "http://"+addr+"/api/log"); code != http.StatusOK {
		t.Errorf("got %d with the caller unknown, want 200", code)
	}
}

// The real lsof, on a real connection: this test's own request is ours, and
// the same question about a connection that does not exist is not.
func TestLsofNamesThisProcessAsTheCaller(t *testing.T) {
	if _, err := os.Stat("/usr/sbin/lsof"); err != nil {
		t.Skip("no lsof here")
	}
	var verdicts []peerVerdict
	old := peerOwner
	peerOwner = func(remote, local string) peerVerdict {
		v := lsofPeerOwner(remote, local)
		verdicts = append(verdicts, v)
		return v
	}
	t.Cleanup(func() { peerOwner = old })
	addr := servePeerChecked(t, peerAPI())
	if code, body := peerGet(t, "http://"+addr+"/api/log"); code != http.StatusOK {
		t.Fatalf("this process was refused its own dashboard: %d %q", code, body)
	}
	if len(verdicts) != 1 || verdicts[0] != peerOurs {
		t.Fatalf("lsof's answers were %v, want one peerOurs", verdicts)
	}
	if v := lsofPeerOwner("127.0.0.1:1", addr); v != peerForeign {
		t.Errorf("a connection nobody here holds was judged %v, want peerForeign", v)
	}
}

func TestLsofListsOnlyTheCallersEndHeldByThisAccount(t *testing.T) {
	const remote, local = "127.0.0.1:50001", "127.0.0.1:7788"
	for _, c := range []struct {
		name, out string
		want      bool
	}{
		{"the caller, this account", "p10\nu501\nf5\nn127.0.0.1:50001->127.0.0.1:7788\n", true},
		{"only the gateway's own end", "p20\nu501\nf9\nn127.0.0.1:7788->127.0.0.1:50001\n", false},
		{"the caller, another account", "p10\nu502\nf5\nn127.0.0.1:50001->127.0.0.1:7788\n", false},
		{"another account, after one of ours", "p20\nu501\nf9\nn127.0.0.1:7788->127.0.0.1:50001\np10\nu0\nf5\nn127.0.0.1:50001->127.0.0.1:7788\n", false},
		{"a different connection on the same port", "p10\nu501\nf5\nn127.0.0.1:50001->127.0.0.1:7789\n", false},
		{"nothing listed", "", false},
	} {
		if got := lsofLists([]byte(c.out), remote, local, 501); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
