// Package publicnet keeps outbound dials to public addresses.
package publicnet

import (
	"fmt"
	"net/netip"
	"syscall"
)

// sharedAddressSpace is carrier-grade NAT space (RFC 6598), which IsPrivate does not cover.
var sharedAddressSpace = netip.MustParsePrefix("100.64.0.0/10")

// RefusePrivate is a net.Dialer Control that refuses every address that is not public unicast.
func RefusePrivate(_, address string, _ syscall.RawConn) error {
	addrPort, err := netip.ParseAddrPort(address)
	if err != nil {
		return err
	}
	ip := addrPort.Addr().Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || sharedAddressSpace.Contains(ip) {
		return fmt.Errorf("refusing %s: not a public address", ip)
	}
	return nil
}
