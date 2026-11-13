//go:build windows

package tun

import (
	"net/netip"

	"golang.org/x/sys/windows"
)

// socketName is the socket's local address. Windows refuses to name a socket
// that has not been bound, and names one that ConnectEx has not finished with
// by what it was bound to: the wildcard address and the real port.
func socketName(fd uintptr) (netip.AddrPort, bool) {
	sa, err := windows.Getsockname(windows.Handle(fd))
	if err != nil {
		return netip.AddrPort{}, false
	}
	switch a := sa.(type) {
	case *windows.SockaddrInet4:
		return netip.AddrPortFrom(netip.AddrFrom4(a.Addr), uint16(a.Port)), true
	case *windows.SockaddrInet6:
		return netip.AddrPortFrom(netip.AddrFrom16(a.Addr), uint16(a.Port)), true
	}
	return netip.AddrPort{}, false
}
