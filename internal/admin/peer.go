package admin

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
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
// for Unix sockets), so the question goes to lsof, which without root lists
// only the caller's own processes: a connection it lists is ours, and one
// it does not list is somebody else's. It is asked once per connection, on
// the first API call, and a browser keeps its connections open.

// peerVerdict is what is known about the process at the far end.
type peerVerdict int

const (
	// peerUnknown: lsof could not be run. Let in, and said in the log: the
	// console must stay reachable when a tool is missing, and another
	// account cannot make lsof fail.
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

// peerOwner is a variable so tests can answer for lsof.
var peerOwner = lsofPeerOwner

var peerUnknownLogged sync.Once

func lsofPeerOwner(remote, local string) peerVerdict {
	_, port, err := net.SplitHostPort(remote)
	if err != nil {
		return peerUnknown
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/sbin/lsof", "-nP", "-a", "-iTCP:"+port, "-sTCP:ESTABLISHED", "-Fun").Output()
	if lsofLists(out, remote, local, os.Getuid()) {
		return peerOurs
	}
	// lsof exits 1 when it lists nothing, which is the answer for another
	// account's connection. Anything else means it was not asked at all.
	var exit *exec.ExitError
	if err != nil && (!errors.As(err, &exit) || ctx.Err() != nil) {
		peerUnknownLogged.Do(func() {
			log.Printf("admin: cannot tell which account is calling the dashboard (lsof: %v), so callers are not checked", err)
		})
		return peerUnknown
	}
	return peerForeign
}

// lsofLists reports whether lsof's field output has the connection from
// remote to local, held by a process of uid. The gateway's own end of the
// same connection is listed too, the other way round, and is not a match.
func lsofLists(out []byte, remote, local string, uid int) bool {
	want, owner := "n"+remote+"->"+local, -1
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "p"):
			owner = -1
		case strings.HasPrefix(line, "u"):
			if n, err := strconv.Atoi(line[1:]); err == nil {
				owner = n
			}
		case line == want && owner == uid:
			return true
		}
	}
	return false
}
