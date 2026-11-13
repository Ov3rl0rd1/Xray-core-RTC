package tun

// fork: outbound interface binding for a core that is started and stopped many
// times inside one host process (libxray).
//
// Upstream registered a dialer controller on every Handler.Start and never
// removed it. internet.Controllers is a process-wide slice, so in an embedded
// host every reconnect added one more: measured on the bench, 7 controllers
// after 6 restarts, each run on every socket the core opens. They also kept
// binding sockets to the old TUN's notion of "the physical interface" after the
// TUN was gone.
//
// And when no physical interface could be found the controller returned
// without binding. With the default route pointing into the TUN, an unbound
// socket is routed straight back into it: the core's own connection to the node
// enters the tunnel it is meant to carry, and every turn of that loop opens
// another one. Controller errors are only logged by the dialer, so refusing has
// to be done by binding: a socket pinned to the loopback interface cannot reach
// anything remote, and the dial fails where it would otherwise have looped.
//
// The same holds when the interface is there but will not take the socket, or
// one address family of it — see fork_pin.go. Nothing leaves this file unpinned
// without a Warning saying so.

import (
	"context"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/xtls/xray-core/common/errors"
	"github.com/xtls/xray-core/transport/internet"
)

var (
	bindOnce   sync.Once
	activeBind atomic.Pointer[InterfaceUpdater]
	bindWarned bindSaid
)

// bindOutbounds makes u the interface every outbound socket is pinned to, for as
// long as this TUN runs.
func bindOutbounds(u *InterfaceUpdater) {
	updater = u // upstream's global, read by the route-change callbacks
	u.Update()
	bindWarned.reset()
	watchOwnSockets() // and should the pin not hold, the loop is seen: fork_loop.go
	activeBind.Store(u)
	notifyBinding(true)
	bindOnce.Do(func() {
		if err := internet.RegisterDialerController(bindController); err != nil {
			errors.LogWarningInner(context.Background(), err, "[tun] outbound binding unavailable")
		}
	})
}

// unbindOutbounds stops pinning. The updater itself stays reachable: a route
// callback already in flight may still call it.
func unbindOutbounds(u *InterfaceUpdater) {
	if u != nil && activeBind.CompareAndSwap(u, nil) {
		notifyBinding(false)
	}
}

// BoundInterface reports the interface outbound sockets are being pinned to,
// or nil when no TUN with outbound binding is running.
func BoundInterface() *net.Interface {
	if u := activeBind.Load(); u != nil {
		return u.Get()
	}
	return nil
}

func bindController(network, address string, c syscall.RawConn) error {
	u := activeBind.Load()
	if u == nil {
		return nil // no TUN: the route table is the truth
	}
	if addrPort, err := netip.ParseAddrPort(address); err == nil && addrPort.Addr().IsLoopback() {
		return nil
	}
	if strings.HasPrefix(strings.ToLower(address), "localhost:") {
		return nil
	}
	return BindSocket(network, address, c, u)
}

// BindSocket pins one socket to the current physical interface. What cannot be
// pinned there — everything when there is no such interface, one address family
// when the interface will not take it — is pinned to loopback instead, and the
// dial that needed it gets an error. Exported for transports that open sockets
// outside the core's dialer (olcRTC's WebRTC stack).
func BindSocket(network, address string, c syscall.RawConn, u *InterfaceUpdater) error {
	if u == nil {
		if u = activeBind.Load(); u == nil {
			return nil
		}
	}
	iface := physicalInterface(u)
	var out pinOutcome
	if err := c.Control(func(fd uintptr) {
		out = pinEach(pinTargets(network, address, fd), iface, loopbackInterface())
	}); err != nil {
		return err
	}
	announce(out, iface)
	return out.verdict(network, address, iface)
}

func physicalInterface(u *InterfaceUpdater) *net.Interface {
	iface := u.Get()
	if iface == nil {
		// The route callback may simply not have run yet; ask once more.
		u.Update()
		iface = u.Get()
	}
	return iface
}

// announce says at Warning, once per interface and family, what was refused or
// left open. The dialer logs a controller's error at Info, which a client that
// runs the core at "warning" never sees — and this is the line that explains
// why a direct connection does not work.
func announce(out pinOutcome, iface *net.Interface) {
	if len(out.refused) == 0 && len(out.open) == 0 {
		return
	}
	index, name := 0, "no physical interface"
	if iface != nil {
		index, name = iface.Index, iface.Name+" (#"+strconv.Itoa(iface.Index)+")"
	}
	for _, family := range out.open {
		if bindWarned.first(index, "open "+family) {
			errors.LogWarningInner(context.Background(), out.cause, "[tun] ", familyList([]string{family}),
				" of the core's sockets can be pinned neither to ", name,
				" nor to loopback: such a socket follows the route table, and what it carries may come back into the TUN")
		}
	}
	if iface == nil {
		if len(out.refused) > 0 && bindWarned.first(0, "none") {
			errors.LogWarning(context.Background(), "[tun] no physical interface to pin the core's sockets to; they are refused rather than looped into the TUN")
		}
		return
	}
	for _, family := range out.refused {
		if !bindWarned.first(index, family) {
			continue
		}
		if family == wholeSocket {
			errors.LogWarningInner(context.Background(), out.cause, "[tun] the core's sockets cannot be pinned to ", name,
				"; direct connections are refused while it is the way out, rather than looped into the TUN")
			continue
		}
		errors.LogWarningInner(context.Background(), out.cause, "[tun] ", family, " cannot be pinned to ", name,
			": the interface does not carry ", family, " (on Windows, the protocol is unticked on the adapter). Direct ", family,
			" connections are refused while it is the way out, rather than looped into the TUN; the other address family is not affected")
	}
}

// BindFD is BindSocket for code that only has the descriptor (olcRTC's
// protector callback). It reports false when the socket was refused — nothing
// of it could be pinned to a physical interface — so the caller can fail the
// dial early instead of waiting for it to time out. With no TUN running it does
// nothing and reports true.
func BindFD(fd uintptr) bool {
	u := activeBind.Load()
	if u == nil {
		return true
	}
	iface := physicalInterface(u)
	targets := pinTargets("", "", fd)
	if len(targets) == 0 {
		return true // not an IP socket: nothing to pin, nothing that could loop
	}
	out := pinEach(targets, iface, loopbackInterface())
	announce(out, iface)
	return iface != nil && out.usable(len(targets))
}

var (
	bindHooksMu sync.Mutex
	bindHooks   []func(active bool)
)

// OnOutboundBinding registers f to hear when outbound binding starts and stops.
// For transports with their own socket factory; call from init.
func OnOutboundBinding(f func(active bool)) {
	bindHooksMu.Lock()
	bindHooks = append(bindHooks, f)
	bindHooksMu.Unlock()
}

func notifyBinding(active bool) {
	bindHooksMu.Lock()
	hooks := append([]func(bool){}, bindHooks...)
	bindHooksMu.Unlock()
	for _, f := range hooks {
		f(active)
	}
}
