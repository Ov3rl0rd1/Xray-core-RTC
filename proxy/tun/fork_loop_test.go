package tun

import (
	"context"
	"net"
	"net/netip"
	"syscall"
	"testing"

	xnet "github.com/xtls/xray-core/common/net"
)

// dialAsCore opens a TCP connection the way the core's dialer does: every
// controller is run on the socket before it is bound.
func dialAsCore(t *testing.T, g *loopGuard, address string) net.Conn {
	t.Helper()
	d := &net.Dialer{Control: func(network, _ string, c syscall.RawConn) error {
		g.register(network, c)
		return nil
	}}
	conn, err := d.DialContext(context.Background(), "tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	return conn
}

func listenAsCore(t *testing.T, g *loopGuard, network, address string) net.PacketConn {
	t.Helper()
	lc := &net.ListenConfig{Control: func(network, _ string, c syscall.RawConn) error {
		g.register(network, c)
		return nil
	}}
	pc, err := lc.ListenPacket(context.Background(), network, address)
	if err != nil {
		t.Skipf("%s %s: %v", network, address, err)
	}
	return pc
}

func echoListener(t *testing.T) net.Listener {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			defer c.Close()
		}
	}()
	return l
}

func addrPort(a net.Addr) netip.AddrPort {
	ap, _ := netip.ParseAddrPort(a.String())
	return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
}

// The loop, as the TUN sees it: a connection arrives whose source is the local
// address of a socket the core itself opened a moment ago.
func TestAConnectionFromTheCoresOwnSocketIsALoop(t *testing.T) {
	g := &loopGuard{}
	conn := dialAsCore(t, g, echoListener(t).Addr().String())
	defer conn.Close()

	if !g.owns(false, addrPort(conn.LocalAddr())) {
		t.Fatalf("the core's own socket %s was not recognised", conn.LocalAddr())
	}
}

// Every application's connection reaches the TUN from the TUN's own address;
// only the port tells them apart. Another program's socket must pass.
func TestAnotherProgramsSocketIsNotALoop(t *testing.T) {
	g := &loopGuard{}
	l := echoListener(t)
	own := dialAsCore(t, g, l.Addr().String())
	defer own.Close()

	theirs, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer theirs.Close()

	if g.owns(false, addrPort(theirs.LocalAddr())) {
		t.Fatal("a socket the core did not open was taken for its own")
	}
	if g.owns(true, addrPort(own.LocalAddr())) {
		t.Fatal("a UDP source matched the core's TCP socket on the same port")
	}
}

// A connected TCP socket is known by its exact address: the core's socket on
// the physical interface and an application's socket on the TUN address may
// carry the same port number.
func TestTheSamePortOnAnotherAddressIsNotALoop(t *testing.T) {
	g := &loopGuard{}
	conn := dialAsCore(t, g, echoListener(t).Addr().String())
	defer conn.Close()

	local := addrPort(conn.LocalAddr())
	elsewhere := netip.AddrPortFrom(netip.MustParseAddr("198.18.0.1"), local.Port())
	if g.owns(false, elsewhere) {
		t.Fatalf("%s matched the core's socket at %s", elsewhere, local)
	}
}

// The port is free for anyone the moment the socket closes; a stale entry
// would drop that application's connection.
func TestAClosedSocketIsForgotten(t *testing.T) {
	g := &loopGuard{}
	conn := dialAsCore(t, g, echoListener(t).Addr().String())
	local := addrPort(conn.LocalAddr())
	if !g.owns(false, local) {
		t.Fatal("not recognised while open")
	}
	conn.Close()
	if g.owns(false, local) {
		t.Fatal("still recognised after it was closed")
	}
}

// The dialer's UDP sockets are bound to the wildcard address and never
// connected, so the port is all there is to know them by — on whichever
// address Windows sends from. A dual-stack socket holds the port for both
// families; an IPv4 one only for IPv4.
func TestAnUnconnectedUDPSocketIsKnownByItsPort(t *testing.T) {
	g := &loopGuard{}

	dual := listenAsCore(t, g, "udp", "[::]:0")
	defer dual.Close()
	port := addrPort(dual.LocalAddr()).Port()
	sources := []string{"198.18.0.1", "10.1.0.6"}
	if addrPort(dual.LocalAddr()).Addr().Is6() {
		sources = append(sources, "fdfe:dcba:9876::1")
	} else {
		t.Log("no IPv6 on this machine: the wildcard socket is IPv4 only, its IPv6 side is not checked")
	}
	for _, from := range sources {
		if !g.owns(true, netip.AddrPortFrom(netip.MustParseAddr(from), port)) {
			t.Errorf("a datagram from %s port %d was not recognised as the core's own", from, port)
		}
	}
	if g.owns(false, netip.AddrPortFrom(netip.MustParseAddr("198.18.0.1"), port)) {
		t.Error("a TCP source matched the core's UDP socket")
	}

	v4 := listenAsCore(t, g, "udp4", "0.0.0.0:0")
	defer v4.Close()
	port4 := addrPort(v4.LocalAddr()).Port()
	if !g.owns(true, netip.AddrPortFrom(netip.MustParseAddr("198.18.0.1"), port4)) {
		t.Error("an IPv4 datagram from the IPv4 socket's port was not recognised")
	}
	if g.owns(true, netip.AddrPortFrom(netip.MustParseAddr("fdfe:dcba:9876::1"), port4)) {
		t.Error("an IPv6 source matched a socket that has no IPv6")
	}
}

// One entry per socket ever opened would be a leak the size of the session.
func TestClosedSocketsDoNotAccumulate(t *testing.T) {
	g := &loopGuard{}
	l := echoListener(t)
	for i := 0; i < 2000; i++ {
		dialAsCore(t, g, l.Addr().String()).Close()
	}
	if n := g.size(); n > 600 {
		t.Fatalf("%d sockets remembered after 2000 were opened and closed", n)
	}
}

// What the handler asks: is this connection one to drop.
func TestTheHandlerRefusesALoopedConnectionAndCountsIt(t *testing.T) {
	before := LoopsRefused()
	conn := dialAsCore(t, &coreSockets, echoListener(t).Addr().String())
	defer conn.Close()

	local := addrPort(conn.LocalAddr())
	source := xnet.TCPDestination(xnet.IPAddress(local.Addr().AsSlice()), xnet.Port(local.Port()))
	destination := xnet.TCPDestination(xnet.ParseAddress("2001:db8::1"), 443)
	if !refuseLooped(source, destination) {
		t.Fatal("a connection from the core's own socket was let through")
	}
	if LoopsRefused() != before+1 {
		t.Fatalf("refused %d, want %d", LoopsRefused(), before+1)
	}

	other := xnet.TCPDestination(xnet.ParseAddress("198.18.0.1"), xnet.Port(local.Port()))
	if refuseLooped(other, destination) {
		t.Fatal("an application's connection was refused")
	}
	domain := xnet.TCPDestination(xnet.ParseAddress("example.com"), 443)
	if refuseLooped(domain, destination) {
		t.Fatal("a source that is not an address was refused")
	}
}
