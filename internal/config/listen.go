package config

import (
	"net"
	"strings"
)

// LoopbackListen reports whether a listen address binds this Mac only:
// 127.0.0.0/8, ::1 or localhost, with a port. An address with no host
// (":7777") binds every interface and is not loopback, and neither is any
// other hostname, since what it resolves to is not this function's to know.
//
// Neither listener has a login (see SECURITY.md), so loopback is the whole
// of who may reach them. `configure` asks this before it stores an address.
func LoopbackListen(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
