//go:build !windows

package tun

import (
	"net/netip"
	"syscall"
)

// socketName is the socket's local address; port 0 until the socket is bound.
func socketName(fd uintptr) (netip.AddrPort, bool) {
	sa, err := syscall.Getsockname(int(fd))
	if err != nil {
		return netip.AddrPort{}, false
	}
	switch a := sa.(type) {
	case *syscall.SockaddrInet4:
		return netip.AddrPortFrom(netip.AddrFrom4(a.Addr), uint16(a.Port)), true
	case *syscall.SockaddrInet6:
		return netip.AddrPortFrom(netip.AddrFrom16(a.Addr), uint16(a.Port)), true
	}
	return netip.AddrPort{}, false
}
