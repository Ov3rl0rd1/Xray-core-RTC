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

import (
	"context"
	"net"
	"net/netip"
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
)

// bindOutbounds makes u the interface every outbound socket is pinned to, for as
// long as this TUN runs.
func bindOutbounds(u *InterfaceUpdater) {
	updater = u // upstream's global, read by the route-change callbacks
	u.Update()
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

// BindSocket pins one socket to the current physical interface, or to loopback
// when there is none. Exported for transports that open sockets outside the
// core's dialer (olcRTC's WebRTC stack).
func BindSocket(network, address string, c syscall.RawConn, u *InterfaceUpdater) error {
	if u == nil {
		if u = activeBind.Load(); u == nil {
			return nil
		}
	}
	iface := u.Get()
	if iface == nil {
		// The route callback may simply not have run yet; ask once more.
		u.Update()
		iface = u.Get()
	}
	refused := iface == nil
	if refused {
		iface = loopbackInterface()
		if iface == nil {
			return errors.New("[tun] no physical interface and no loopback to refuse with")
		}
	}
	var setErr error
	if err := c.Control(func(fd uintptr) {
		setErr = setinterface(network, address, fd, iface)
	}); err != nil {
		return err
	}
	if refused {
		return errors.New("[tun] no physical interface; refusing ", address, " rather than looping it into the TUN")
	}
	return setErr
}

func loopbackInterface() *net.Interface {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	for i := range ifaces {
		if ifaces[i].Flags&net.FlagLoopback != 0 {
			return &ifaces[i]
		}
	}
	return nil
}

// BindFD is BindSocket for code that only has the descriptor (olcRTC's
// protector callback). It reports false when the socket was refused — pinned to
// loopback because no physical interface exists — so the caller can fail the
// dial early instead of waiting for it to time out. With no TUN running it does
// nothing and reports true.
func BindFD(fd uintptr) bool {
	u := activeBind.Load()
	if u == nil {
		return true
	}
	iface := u.Get()
	if iface == nil {
		u.Update()
		iface = u.Get()
	}
	refused := iface == nil
	if refused {
		if iface = loopbackInterface(); iface == nil {
			return false
		}
	}
	// "tcp6" selects both address families' options on every platform that
	// distinguishes them. The family the socket does not have fails, and that is
	// expected, so the result is not an error signal.
	_ = setinterface("tcp6", "", fd, iface)
	return !refused
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
