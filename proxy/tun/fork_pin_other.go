//go:build !windows

package tun

import "net"

// pinTargets: off Windows the platform pins the socket as a whole
// (SO_BINDTODEVICE, IP_BOUND_IF), so there is one target and no family to tell
// apart.
func pinTargets(network, address string, fd uintptr) []pinTarget {
	return []pinTarget{{family: wholeSocket, pin: func(iface *net.Interface) error {
		return setinterface(network, address, fd, iface)
	}}}
}
