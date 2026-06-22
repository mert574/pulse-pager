package checker

import (
	"fmt"
	"net"
	"syscall"
)

// isBlockedIP reports whether an IP is in a range we refuse to check when
// BlockPrivateNetworks is on: loopback, link-local (v4 and v6), and RFC1918
// private ranges. IsPrivate covers RFC1918 (10/8, 172.16/12, 192.168/16) plus
// the IPv6 unique local range, which is the private equivalent there.
func isBlockedIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return true
	}
	if ip.IsPrivate() {
		return true
	}
	return false
}

// resolveAndCheck looks up the host and returns an error if any resolved IP is
// in a blocked range. We pre-resolve so we can return the clean blocked_target
// reason without sending a single byte. If the host is already a literal IP,
// LookupIP returns it directly. A resolve failure surfaces as an error too,
// which the caller turns into a connection error (we could not reach the name).
func resolveAndCheck(host string) error {
	ips, err := net.LookupIP(host)
	if err != nil {
		return err
	}
	for _, ip := range ips {
		if isBlockedIP(ip) {
			return fmt.Errorf("host %q resolves to blocked address %s", host, ip)
		}
	}
	return nil
}

// dialControl is the net.Dialer Control callback. It runs after the OS has
// resolved and is about to connect to a concrete address, so it closes the
// TOCTOU gap: even if DNS changed between our pre-resolve and the actual dial,
// we still refuse the connection if the real connected IP is blocked.
func dialControl(network, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		// Without a parseable host:port we cannot verify the target, so refuse.
		return fmt.Errorf("cannot parse dial address %q: %w", address, err)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		// Control is called with a resolved IP literal, so a non-IP here is
		// unexpected. Refuse rather than connect to something we cannot check.
		return fmt.Errorf("dial address %q is not an IP", host)
	}
	if isBlockedIP(ip) {
		return fmt.Errorf("dial to blocked address %s refused", ip)
	}
	return nil
}
