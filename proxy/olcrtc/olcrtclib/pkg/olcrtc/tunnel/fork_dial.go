package tunnel

import (
	"context"
	"net"

	"github.com/xtls/xray-core/proxy/olcrtc/olcrtclib/internal/server"
)

// This file belongs to the fork, not to olcrtc upstream. It is listed in
// fork/olcrtc-manifest.txt so a resync keeps it.
//
// It exists as a separate entry point rather than another field on Config so
// that upstream's tunnel.go stays untouched: adding a field would mean editing
// both the struct and toServerConfig, and every edit inside an upstream file is
// something a future resync can argue with. NewWithDial reuses toServerConfig
// and sets the one thing it does not know about.

// DialFunc establishes the egress connection for one tunnel stream. It gets the
// target from the client's CONNECT request and the sessionID that AuthHook
// returned, which is how a host application attributes the connection to a user.
type DialFunc func(ctx context.Context, addr string, port int, sessionID string) (net.Conn, error)

// NewWithDial returns a server that hands every tunnelled target to dial
// instead of exiting to the internet itself.
//
// That is what an embedded server wants: inside Xray the target has to go
// through the router, so routing rules, DNS, sniffing, per-user statistics and
// the chosen egress outbound all apply exactly as they do for any other
// inbound. A nil dial gives the ordinary server, dialling for itself.
func NewWithDial(cfg Config, dial DialFunc) *Server {
	RegisterDefaults()
	return &Server{
		cfg: cfg,
		run: func(ctx context.Context, sc server.Config) error {
			sc.DialHook = server.DialFunc(dial)
			return server.Run(ctx, sc)
		},
	}
}
