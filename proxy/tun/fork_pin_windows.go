//go:build windows

package tun

import (
	"encoding/binary"
	"net"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

const soProtocolInfo = 0x2005 // SO_PROTOCOL_INFOW

// pinTargets lists the address families the socket really has. Asked of the
// socket rather than read off the network name: Go opens "udp" on a wildcard
// address as a dual-stack AF_INET6 socket and calls it "udp6", and olcRTC's
// sockets arrive with no name at all.
func pinTargets(network, address string, fd uintptr) []pinTarget {
	h := windows.Handle(fd)
	v4 := pinTarget{family: familyIPv4, pin: func(iface *net.Interface) error { return pin4(h, iface.Index) }}
	v6 := pinTarget{family: familyIPv6, pin: func(iface *net.Interface) error { return pin6(h, iface.Index) }}

	family := socketFamily(h)
	if family == 0 {
		switch {
		case strings.HasSuffix(network, "4"):
			family = windows.AF_INET
		case strings.HasSuffix(network, "6"):
			family = windows.AF_INET6
		}
	}
	switch family {
	case windows.AF_INET:
		return []pinTarget{v4}
	case windows.AF_INET6:
		// IPPROTO_IP options are invalid on a socket that is IPv6 only.
		if only, err := windows.GetsockoptInt(h, windows.IPPROTO_IPV6, windows.IPV6_V6ONLY); err == nil && only != 0 {
			return []pinTarget{v6}
		}
		return []pinTarget{v6, v4}
	}
	// Not an IP socket (a unix one): nothing to pin and nothing that could be
	// routed into the TUN. Upstream's setinterface panics here.
	return nil
}

func socketFamily(h windows.Handle) int {
	var info windows.WSAProtocolInfo
	size := int32(unsafe.Sizeof(info))
	if err := windows.Getsockopt(h, windows.SOL_SOCKET, soProtocolInfo, (*byte)(unsafe.Pointer(&info)), &size); err != nil {
		return 0
	}
	return int(info.AddressFamily)
}

// pin6 takes the interface index in host byte order.
func pin6(h windows.Handle, index int) error {
	if err := windows.SetsockoptInt(h, windows.IPPROTO_IPV6, IPV6_UNICAST_IF, index); err != nil {
		return err
	}
	// Multicast follows where the socket has the option (UDP); the unicast pin
	// is the one that decides.
	_ = windows.SetsockoptInt(h, windows.IPPROTO_IPV6, windows.IPV6_MULTICAST_IF, index)
	return nil
}

// pin4 takes it in network byte order.
func pin4(h windows.Handle, index int) error {
	var be [4]byte
	binary.BigEndian.PutUint32(be[:], uint32(index))
	value := int(*(*uint32)(unsafe.Pointer(&be[0])))
	if err := windows.SetsockoptInt(h, windows.IPPROTO_IP, IP_UNICAST_IF, value); err != nil {
		return err
	}
	_ = windows.SetsockoptInt(h, windows.IPPROTO_IP, windows.IP_MULTICAST_IF, value)
	return nil
}
