package session

import (
	"context"

	"github.com/xtls/xray-core/common/net"
)

// fork: lets whoever accepted a connection learn where routing sent it.
//
// The outbound tag is decided deep inside the dispatcher, after sniffing, on a
// path the inbound only waits on. Reading Outbound.Tag from the inbound's side
// would race with the dispatcher writing it — and a torn string read is a crash,
// not a wrong answer. The dispatcher instead calls NotifyRouted at the moment it
// has decided, on its own goroutine, and the observer copies what it needs.
//
// Used by the TUN inbound's connection list (proxy/tun/fork_tracker.go), which
// is what lets a client show which application's connection went through the
// proxy and which went direct.

// RouteObserver receives the outbound a connection was given, and the target
// as routing saw it: the sniffed domain when there was one, else the address.
type RouteObserver func(outboundTag string, target net.Destination)

type routeObserverKey struct{}

// ContextWithRouteObserver returns a context whose connection reports its route
// to f.
func ContextWithRouteObserver(ctx context.Context, f RouteObserver) context.Context {
	return context.WithValue(ctx, routeObserverKey{}, f)
}

// NotifyRouted tells the context's observer, if any, where ob was sent.
func NotifyRouted(ctx context.Context, ob *Outbound) {
	f, ok := ctx.Value(routeObserverKey{}).(RouteObserver)
	if !ok || f == nil || ob == nil {
		return
	}
	target := ob.Target
	if ob.RouteTarget.IsValid() {
		target = ob.RouteTarget
	}
	f(ob.Tag, target)
}
