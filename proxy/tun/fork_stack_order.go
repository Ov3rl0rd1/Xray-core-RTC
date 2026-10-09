package tun

import (
	"github.com/xtls/xray-core/common/errors"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

// attachNIC gives the stack its NIC, which is the moment packets start to arrive:
// CreateNIC attaches the link endpoint, and the endpoint begins delivering at once
// (fdbased starts its processor goroutines, the Windows endpoint its reader).
//
// Upstream attached the NIC inside createStack and installed the TCP forwarder and
// the UDP handler afterwards. Packets arriving in between were delivered by those
// goroutines while the handlers were still being built and published — the race
// detector reports it on every start under traffic (NewForwarder, newListenContext,
// newUdpConnectionHandler, SetTransportProtocolHandler against DeliverTransportPacket)
// — and a packet that found no handler at all was answered by gVisor itself: a
// reset for TCP, ICMP port unreachable for UDP, which a Windows UDP socket reports
// as WSAECONNRESET. Attaching last makes the goroutine start the happens-before
// edge for everything Start set up.
func attachNIC(s *stack.Stack, ep stack.LinkEndpoint) error {
	if err := s.CreateNIC(defaultNIC, ep); err != nil {
		return errors.New(err.String())
	}

	s.SetRouteTable([]tcpip.Route{
		{Destination: header.IPv4EmptySubnet, NIC: defaultNIC},
		{Destination: header.IPv6EmptySubnet, NIC: defaultNIC},
	})

	if err := s.SetSpoofing(defaultNIC, true); err != nil {
		return errors.New(err.String())
	}
	if err := s.SetPromiscuousMode(defaultNIC, true); err != nil {
		return errors.New(err.String())
	}
	return nil
}
