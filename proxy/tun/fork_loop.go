package tun

// fork: a connection that reaches the TUN from one of the core's own sockets is
// dropped.
//
// Everything the core sends out is supposed to leave by the physical interface
// (fork_bind.go). When a socket does not — the pin failed, Windows ignored it
// (forwarding is on for the interface: Mobile Hotspot, Internet Connection
// Sharing), nothing was pinned at all — its traffic follows the routes into the
// TUN, the TUN hands it to the dispatcher like any application's, the
// dispatcher sends it out again on a new socket, and that one comes back too.
// Each turn costs a socket and a port. A user's machine went through all 16 384
// of Windows' ephemeral ports in 80 seconds on one datagram, and then nothing
// on it could open a connection until the sockets idled out.
//
// fork_pin.go closes the door that failure came through. This is for the next
// one: whatever the cause, the loop needs a connection to arrive at the TUN
// from a socket the core opened, and that can be seen.
//
// How it is seen. The dialer runs its controllers on every socket before it is
// bound; the socket's handle is kept, not its address, which does not exist
// yet. A socket is bound before its first packet can leave — explicitly on
// Windows, by connect elsewhere — so by the time a connection from it reaches
// the TUN, asking the socket for its name gives at least the port. That makes
// the check exact rather than a race between the dial returning and the TUN
// accepting: no turn of the loop gets through.
//
// What it costs: one handle remembered per live socket, one getsockname per
// socket, a map lookup per TUN connection.

import (
	"context"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/xtls/xray-core/common/errors"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/transport/internet"
)

type socketState int

const (
	socketClosed socketState = iota
	socketUnbound
	socketBound
)

type ownSocket struct {
	raw syscall.RawConn
	udp bool
}

// name asks the socket where it is. Control fails once the socket is closed,
// which is the only notice of that there is: the connection belongs to whoever
// dialled it and cannot be wrapped (they assert its concrete type).
func (s *ownSocket) name() (local netip.AddrPort, state socketState) {
	state = socketClosed
	_ = s.raw.Control(func(fd uintptr) {
		state = socketUnbound
		if ap, ok := socketName(fd); ok && ap.Port() != 0 {
			local, state = netip.AddrPortFrom(ap.Addr().Unmap().WithZone(""), ap.Port()), socketBound
		}
	})
	return local, state
}

type loopPort struct {
	udp  bool
	port uint16
}

// loopGuard knows the sockets the core has open.
type loopGuard struct {
	mu      sync.Mutex
	pending []*ownSocket              // registered before they were bound
	byPort  map[loopPort][]*ownSocket // bound, by protocol and local port
	bound   int
	sweepAt int
}

const loopSweepFloor = 256

// register remembers a socket the core is about to use. Called from a dialer
// controller, so before the socket has an address.
func (g *loopGuard) register(network string, raw syscall.RawConn) {
	udp := strings.HasPrefix(network, "udp")
	if !udp && !strings.HasPrefix(network, "tcp") {
		return
	}
	g.mu.Lock()
	g.pending = append(g.pending, &ownSocket{raw: raw, udp: udp})
	if len(g.pending)+g.bound >= max(g.sweepAt, loopSweepFloor) {
		g.sweepLocked()
	}
	g.mu.Unlock()
}

// owns reports whether source is the local end of a socket the core has open.
func (g *loopGuard) owns(udp bool, source netip.AddrPort) bool {
	source = netip.AddrPortFrom(source.Addr().Unmap().WithZone(""), source.Port())

	g.mu.Lock()
	defer g.mu.Unlock()
	g.settleLocked()

	key := loopPort{udp, source.Port()}
	list := g.byPort[key]
	found := false
	kept := list[:0]
	for _, s := range list {
		// Asked again rather than remembered: the socket may have been closed, and
		// a TCP socket that was mid-connect when it was first seen has its real
		// address only now.
		local, state := s.name()
		if state != socketBound || local.Port() != source.Port() {
			g.bound--
			continue
		}
		kept = append(kept, s)
		if sameEnd(local.Addr(), source.Addr()) {
			found = true
		}
	}
	g.storeLocked(key, kept)
	return found
}

// sameEnd compares a socket's own address with a connection's source. A socket
// on the wildcard address sends from whatever address the route gives it, so
// only the port can be held against it — for the families it has: 0.0.0.0 is an
// IPv4 socket, :: one that Go opens dual-stack.
func sameEnd(local, source netip.Addr) bool {
	if local.IsUnspecified() {
		return !local.Is4() || source.Is4()
	}
	return local == source
}

// settleLocked files the sockets that have been bound since they were registered.
func (g *loopGuard) settleLocked() {
	waiting := g.pending[:0]
	for _, s := range g.pending {
		local, state := s.name()
		switch state {
		case socketUnbound:
			waiting = append(waiting, s)
		case socketBound:
			key := loopPort{s.udp, local.Port()}
			if g.byPort == nil {
				g.byPort = map[loopPort][]*ownSocket{}
			}
			g.byPort[key] = append(g.byPort[key], s)
			g.bound++
		}
	}
	clear(g.pending[len(waiting):])
	g.pending = waiting
}

// sweepLocked forgets the sockets that have been closed. Run when the count
// has doubled since the last time, so the cost stays proportional to the dials.
func (g *loopGuard) sweepLocked() {
	g.settleLocked()
	for key, list := range g.byPort {
		kept := list[:0]
		for _, s := range list {
			if _, state := s.name(); state == socketBound {
				kept = append(kept, s)
			} else {
				g.bound--
			}
		}
		g.storeLocked(key, kept)
	}
	g.sweepAt = 2 * (len(g.pending) + g.bound)
}

func (g *loopGuard) storeLocked(key loopPort, list []*ownSocket) {
	if len(list) == 0 {
		delete(g.byPort, key)
		return
	}
	g.byPort[key] = list
}

// size is how many sockets are remembered.
func (g *loopGuard) size() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.pending) + g.bound
}

var (
	coreSockets  loopGuard
	loopOnce     sync.Once
	loopsRefused atomic.Uint64

	loopSaidMu   sync.Mutex
	loopSaidAt   time.Time
	loopSaidUpTo uint64
)

// watchOwnSockets has the dialer tell the guard about every socket it opens,
// from now until the process ends. Idempotent.
func watchOwnSockets() {
	loopOnce.Do(func() {
		if err := internet.RegisterDialerController(func(network, _ string, c syscall.RawConn) error {
			coreSockets.register(network, c)
			return nil
		}); err != nil {
			errors.LogWarningInner(context.Background(), err, "[tun] loop detection unavailable")
		}
	})
}

// LoopsRefused is how many connections the TUN has dropped because they came
// from the core's own sockets, since the process started.
func LoopsRefused() uint64 { return loopsRefused.Load() }

// refuseLooped reports whether the connection the TUN just accepted came from
// one of the core's own sockets, in which case the handler must drop it.
func refuseLooped(source, destination net.Destination) bool {
	watchOwnSockets()
	if !source.Address.Family().IsIP() {
		return false
	}
	addr, ok := netip.AddrFromSlice(source.Address.IP())
	if !ok || !coreSockets.owns(destination.Network == net.Network_UDP, netip.AddrPortFrom(addr, uint16(source.Port))) {
		return false
	}
	announceLoop(loopsRefused.Add(1), source, destination)
	return true
}

const loopSayEvery = 10 * time.Second

// announceLoop says it at Warning: the first time at once, then at most every
// ten seconds with the count, so a loop that keeps trying is visible without
// becoming the log.
func announceLoop(total uint64, source, destination net.Destination) {
	loopSaidMu.Lock()
	now := time.Now()
	if !loopSaidAt.IsZero() && now.Sub(loopSaidAt) < loopSayEvery {
		loopSaidMu.Unlock()
		return
	}
	since := total - loopSaidUpTo
	loopSaidAt, loopSaidUpTo = now, total
	loopSaidMu.Unlock()

	errors.LogWarning(context.Background(), "[tun] dropped a connection from the core's own socket ", source.NetAddr(),
		" to ", destination, ": it was routed back into the TUN instead of leaving by the physical interface (",
		since, " dropped since the last report, ", total, " in all)")
}
