package olcrtc

import (
	olclient "github.com/xtls/xray-core/proxy/olcrtc/olcrtclib/pkg/olcrtc/client"
	"github.com/xtls/xray-core/proxy/tun"
)

// While the core's own TUN is up with outbound binding, the default route points
// into it. The core's dialer pins its sockets to the physical interface, but
// olcRTC's WebRTC stack opens its own — signalling over WebSocket, ICE over UDP —
// and those would enter the TUN they are meant to carry, looping forever. The
// library has a socket hook for exactly this (it is how Android's VpnService
// protect() is wired); it is installed here for as long as the binding lasts,
// and removed after, so a server-side olcRTC or a build without a TUN behaves
// exactly as before.
func init() {
	tun.OnOutboundBinding(func(active bool) {
		if !active {
			olclient.SetSocketProtector(nil)
			return
		}
		olclient.SetSocketProtector(func(fd int) bool {
			return tun.BindFD(uintptr(fd))
		})
	})
}
