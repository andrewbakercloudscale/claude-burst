package router

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"syscall"
	"time"
)

// netProbe is one measurement of local network health. It is taken once per
// transport failure and used twice: written to the log, and consulted to
// decide whether failing over could possibly help.
type netProbe struct {
	ifaces []string
	dnsOK  bool
	dnsErr error
	dnsDur time.Duration
	// webDown: names resolve, but an HTTPS request to a host unrelated to
	// any provider failed too. A phone out of data does exactly this: DNS
	// keeps answering and every connection is reset (2026-10-04 00:23, a
	// failover to Together, which was reset the same way). Zero value is
	// "not known to be down", so a probe that never ran blocks nothing.
	webDown bool
	webErr  error
}

// controlWebURL is the HTTPS control request: Apple's captive-portal check,
// tiny, and served by nobody Burst routes to. HTTPS, so a carrier cannot
// answer it with a page of its own.
var controlWebURL = "https://captive.apple.com/hotspot-detect.html"

// The control request's answer is kept briefly: a burst of failures on a
// dead network would otherwise each wait out its own timeout.
var (
	webProbeMu  sync.Mutex
	webProbeAt  time.Time
	webProbeErr error
)

const webProbeTTL = 10 * time.Second

func probeWeb() error {
	webProbeMu.Lock()
	defer webProbeMu.Unlock()
	if time.Since(webProbeAt) < webProbeTTL {
		return webProbeErr
	}
	// Through the same proxy as Anthropic traffic: on a network where only
	// the corporate proxy gets out, a direct check always fails and every
	// error would be blamed on this Mac's network.
	c := &http.Client{Timeout: 4 * time.Second, Transport: &http.Transport{Proxy: upstreamProxy}}
	resp, err := c.Get(controlWebURL)
	if err == nil {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
	}
	webProbeAt, webProbeErr = time.Now(), err
	return err
}

func probeNetwork() netProbe {
	var p netProbe
	if addrs, ierr := net.InterfaceAddrs(); ierr == nil {
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok || ipnet.IP.IsLoopback() {
				continue
			}
			p.ifaces = append(p.ifaces, ipnet.IP.String())
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	_, p.dnsErr = net.DefaultResolver.LookupHost(ctx, "www.apple.com")
	p.dnsDur = time.Since(start)
	p.dnsOK = p.dnsErr == nil
	if p.dnsOK {
		p.webErr = probeWeb()
		p.webDown = p.webErr != nil
	}
	return p
}

// snapshotRepeat is how long an unchanged network state is logged in short
// form: a burst of failures on one network logs its interfaces and DNS
// once, then only each failure's own error.
const snapshotRepeat = time.Minute

// logSnapshot logs np for a transport failure, in full when the network
// state differs from the last full one or that is snapshotRepeat old.
func (s *Server) logSnapshot(rid, route string, triggerErr error, np netProbe) {
	state := np.state()
	s.snapMu.Lock()
	same := state == s.snapState && time.Since(s.snapAt) < snapshotRepeat
	if !same {
		s.snapState, s.snapAt = state, time.Now()
	}
	at := s.snapAt
	s.snapMu.Unlock()
	if same {
		// At most one short line a minute: the full one above already says
		// what the network looks like, and the request's own lines carry
		// its error.
		if ok, skipped := s.repeats.allow("snapshot unchanged", time.Now()); ok {
			s.logger.Printf("req=%s network-snapshot route=%s trigger_err=%q (network unchanged since %s)%s",
				rid, route, triggerErr, at.Format("15:04:05"), andMore(skipped))
		}
		return
	}
	s.logger.Printf("req=%s %s", rid, np.snapshot(route, triggerErr))
}

// state is the snapshot without its timing, for telling a changed network
// from the same one.
func (p netProbe) state() string {
	return strings.Join(p.ifaces, ",") + "|" + fmt.Sprint(p.dnsOK) + "|" + fmt.Sprint(p.webDown)
}

func (p netProbe) snapshot(route string, triggerErr error) string {
	ifaceState := "NONE (network interface appears down)"
	if len(p.ifaces) > 0 {
		ifaceState = strings.Join(p.ifaces, ",")
	}
	dnsState := fmt.Sprintf("ok (%s)", p.dnsDur.Round(time.Millisecond))
	if p.dnsErr != nil {
		dnsState = fmt.Sprintf("FAILED (%s): %v", p.dnsDur.Round(time.Millisecond), p.dnsErr)
	}
	webState := "ok"
	if p.webDown {
		webState = fmt.Sprintf("FAILED: %v", p.webErr)
	} else if !p.dnsOK {
		webState = "not tried"
	}
	return fmt.Sprintf("network-snapshot route=%s trigger_err=%q local_ifaces=%s control_dns=%s control_https=%s",
		route, triggerErr, ifaceState, dnsState, webState)
}

// peerAnswered reports whether err proves something on the far side of the
// network actually replied. A refused or reset connection is a peer speaking;
// a timeout or an EOF is silence, and silence is the ambiguous case where the
// probe above is worth consulting.
func peerAnswered(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET)
}

// safeToResend says whether a transport error left the request certainly
// unrun on the server, so replaying it cannot double-charge or double-run:
// a write-side failure, a failed lookup, or a timeout before the request
// was sent (connecting, or the TLS handshake). A timeout waiting for the
// response headers is NOT safe: the whole request was sent and the model
// may be generating it, so a resend runs (and charges) it again, and five
// of them turned one slow reply into minutes. A read-side reset is not safe
// either.
func safeToResend(err error) bool {
	if isStaleWriteFailure(err) {
		return true
	}
	// A failed lookup never opened a connection, so nothing was sent.
	var lookupErr *LookupError
	if errors.As(err, &lookupErr) {
		return true
	}
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" {
		return true
	}
	return strings.Contains(err.Error(), "TLS handshake timeout")
}

// deadConnection reports a primary error that most likely means the pooled
// connection died under the request (a network switch: the local address is
// gone, or HTTP/2 never answers on it). The request may have reached
// Anthropic, so it is resent once on a fresh connection and never laddered:
// one resend can at worst run a turn twice on the subscription, which costs
// plan usage, while giving up costs a failover to a paid provider.
func deadConnection(err error) bool {
	if errors.Is(err, syscall.EADDRNOTAVAIL) {
		return true
	}
	return strings.Contains(err.Error(), "timeout awaiting response headers")
}

// isStaleWriteFailure reports whether err is a WRITE onto a connection that
// had already died, which is what a laptop moving between networks produces:
// the kept-alive connection to Anthropic is still in the pool, the interface
// it rode on is gone, and the first write onto it fails with EPIPE.
//
// It is scoped to writes on purpose. A failed write means the server cannot
// have received the whole request, so sending it again on a fresh connection
// cannot run the same generation twice. A read-side reset or EOF gives no such
// guarantee and is deliberately not retried.
func isStaleWriteFailure(err error) bool {
	var op *net.OpError
	if !errors.As(err, &op) || op.Op != "write" {
		return false
	}
	return errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNRESET)
}
