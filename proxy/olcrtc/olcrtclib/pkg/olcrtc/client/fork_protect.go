package client

import "github.com/xtls/xray-core/proxy/olcrtc/olcrtclib/internal/protect"

// This file belongs to the fork, not to olcrtc upstream. It is listed in
// fork/olcrtc-manifest.txt so a resync keeps it.
//
// SetSocketProtector exposes the library's process-wide socket hook, which
// upstream only reaches from its Android bindings. Every socket the carrier
// opens — signalling, the TURN/ICE UDP sockets pion creates — is handed to f
// before use; f returning false fails that socket. nil removes the hook.
func SetSocketProtector(f func(fd int) bool) {
	protect.SetProtector(f)
}
