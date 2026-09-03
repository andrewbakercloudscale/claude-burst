package main

import (
	"log"
	"net"
	"os/exec"
	"strings"
)

// peerLoggingListener wraps the gateway's raw net.Listener and, for every
// newly accepted connection, immediately shells out to lsof to identify
// which local process owns the other end of the loopback socket -- logged
// before a TLS handshake can fail and tear the connection down.
//
// Diagnostic only, opt-in via CLAUDE_BURST_LOG_TLS_PEERS, built to
// root-cause the still-open TLS handshake-error storm documented in
// INVESTIGATION-TLS-STORM.md. dtrace's syscall provider -- that
// investigation's own documented next step -- turned out to be fully
// blocked by SIP on the machine where this was needed ("does not match any
// probes. System Integrity Protection is on"), with no unprivileged
// fallback. An earlier attempt at a tight lsof polling loop also failed,
// for a different reason: "the loopback TLS-reject-and-close cycle is
// almost certainly faster than shell-loop polling resolution." This wins
// that same race by construction rather than by polling faster: identify()
// starts the instant Accept() returns a connection, not on some
// independent timer that has to get lucky.
//
// lsof needs no special privilege to see the current user's own sockets,
// and scoping the query to the exact ephemeral port from RemoteAddr()
// (rather than an unscoped snapshot of everything) is what makes a single
// lsof invocation, run once per connection, fast enough to matter.
type peerLoggingListener struct {
	net.Listener
	logger *log.Logger
}

func (l *peerLoggingListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return conn, err
	}
	// Async: lsof takes a few ms, and Accept() must return immediately so
	// the listener keeps accepting the next connection. The connection
	// itself is untouched by this -- identify() only inspects the process
	// table, never conn's data.
	go l.identify(conn.RemoteAddr().String())
	return conn, nil
}

func (l *peerLoggingListener) identify(remote string) {
	_, port, err := net.SplitHostPort(remote)
	if err != nil {
		l.logger.Printf("peer-log: could not parse remote addr %q: %v", remote, err)
		return
	}
	// -sTCP:ESTABLISHED scopes to a live connection so a stale listening
	// socket on the same port number (impossible here, but cheap to rule
	// out) can't produce a false match.
	out, err := exec.Command("lsof", "-n", "-P", "-iTCP:"+port, "-sTCP:ESTABLISHED").CombinedOutput()
	trimmed := strings.TrimSpace(string(out))
	if err != nil && trimmed == "" {
		l.logger.Printf("peer-log: lsof found nothing for 127.0.0.1:%s (already closed?): %v", port, err)
		return
	}
	l.logger.Printf("peer-log: connection from 127.0.0.1:%s -- lsof: %s", port, strings.Join(strings.Split(trimmed, "\n"), " | "))
}
