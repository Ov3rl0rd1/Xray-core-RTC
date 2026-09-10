package client

import (
	"context"
	"fmt"
	"net"

	"github.com/xtls/xray-core/proxy/olcrtc/olcrtclib/internal/client"
	"github.com/xtls/xray-core/proxy/olcrtc/olcrtclib/internal/control"
)

// This file belongs to the fork, not to olcrtc upstream. It is listed in
// fork/olcrtc-manifest.txt so a resync keeps it.
//
// Upstream's Client runs a SOCKS5 listener and is driven by whatever connects
// to it. An Xray outbound has already parsed its target — routing chose the
// destination before the proxy was even reached — so it needs the carrier with
// nothing in front of it. This exposes exactly that, reusing the Config and the
// conversion upstream already has.

// Tunnel is a live carrier with no local listener. Open one stream per target
// with DialContext; it is safe to call from many goroutines at once.
type Tunnel struct {
	t *client.Tunnel
}

// StartTunnel brings up the carrier described by cfg and returns a Tunnel ready
// to dial. It keeps running, reconnecting as needed, until Close is called or
// ctx is cancelled.
func StartTunnel(ctx context.Context, cfg Config) (*Tunnel, error) {
	RegisterDefaults()
	t, err := client.StartTunnel(ctx, toClientConfig(cfg))
	if err != nil {
		return nil, fmt.Errorf("client: %w", err)
	}
	return &Tunnel{t: t}, nil
}

// DialContext opens a stream to addr:port and returns a net.Conn carrying the
// tunnelled bytes. Closing it closes that stream alone.
func (t *Tunnel) DialContext(ctx context.Context, addr string, port int) (net.Conn, error) {
	return t.t.DialContext(ctx, addr, port)
}

// Status returns the latest control-stream health snapshot.
func (t *Tunnel) Status() HealthStatus { return t.t.Status() }

// Close tears the tunnel down and releases the carrier.
func (t *Tunnel) Close() error { return t.t.Close() }

// compile-time check that the public health type still aliases the internal one.
var _ control.Status = HealthStatus{}
