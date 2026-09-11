package client

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/xtls/xray-core/proxy/olcrtc/olcrtclib/internal/control"
	"github.com/xtls/xray-core/proxy/olcrtc/olcrtclib/internal/runtime"
)

// This file belongs to the fork, not to olcrtc upstream. It is listed in
// fork/olcrtc-manifest.txt so a resync keeps it.
//
// Upstream's client is a SOCKS5 listener: it accepts local connections, parses
// the SOCKS negotiation and dials the target through the carrier. An Xray
// outbound has already done all of that — routing has picked the destination
// and the proxy holds a target address — so it needs the carrier without the
// listener in front of it. That is the only thing here.

// Tunnel is a live client-side carrier session. It brings the carrier up,
// completes the handshake and runs the background control and reconnect loops,
// but starts no local listener: callers open one multiplexed stream per target
// through DialContext.
//
// A Tunnel is safe for concurrent use. DialContext may be called from many
// goroutines at once, each getting an independent smux stream over the shared
// carrier.
type Tunnel struct {
	c      *Client
	cancel context.CancelFunc
}

// StartTunnel brings up the carrier described by cfg and returns a Tunnel
// ready to dial. It keeps running, reconnecting as needed, until Close is
// called or the context passed here is cancelled.
//
// It mirrors RunWithAddress up to the point where that opens its listener; if
// upstream changes how a client is constructed, this is the function that has
// to follow.
func StartTunnel(ctx context.Context, cfg Config) (*Tunnel, error) {
	runCtx, cancel := context.WithCancel(ctx)

	keys, err := clientKeys(cfg)
	if err != nil {
		cancel()
		return nil, err
	}
	deviceID, err := resolveDeviceID(cfg.DeviceID, cfg.DeviceIDPath)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("resolve device id: %w", err)
	}

	c := &Client{
		keys: keys, deviceID: deviceID, claims: cfg.Claims, dnsServer: cfg.DNSServer,
		socksUser: cfg.SOCKSUser, socksPass: cfg.SOCKSPass,
		health: runtime.NewHealthTracker(cfg.OnHealth), sessionReady: make(chan struct{}),
	}
	if err := c.bringUpLink(runCtx, cfg, cancel); err != nil {
		c.shutdown()
		cancel()
		return nil, err
	}
	return &Tunnel{c: c, cancel: cancel}, nil
}

// sessionReadyTimeout bounds how long DialContext waits for a carrier that is
// mid-reconnect. Generous because a provider renegotiation legitimately takes
// tens of seconds, and failing a dial that would have succeeded is worse for a
// user than a slow one.
const sessionReadyTimeout = 60 * time.Second

// DialContext opens a stream to addr:port, sends the CONNECT request, waits for
// the server to acknowledge, and returns a net.Conn carrying the tunnelled
// bytes. Closing it closes that stream alone.
//
// If the carrier is rebuilding itself, this blocks until the session is ready
// again, the deadline passes, or ctx is cancelled — the same behaviour the
// SOCKS path has.
func (t *Tunnel) DialContext(ctx context.Context, addr string, port int) (net.Conn, error) {
	readyCtx, cancel := context.WithTimeout(ctx, sessionReadyTimeout)
	defer cancel()

	for {
		t.c.sessMu.RLock()
		sess, sid := t.c.session, t.c.sessionID
		t.c.sessMu.RUnlock()

		if sess != nil && !sess.IsClosed() && sid != "" {
			stream, err := sess.OpenStream()
			if err != nil {
				return nil, fmt.Errorf("open stream: %w", err)
			}
			if err := t.c.sendConnectRequest(stream, addr, port); err != nil {
				_ = stream.Close()
				return nil, fmt.Errorf("connect %s:%d: %w", addr, port, err)
			}
			return stream, nil // *smux.Stream is a net.Conn
		}

		select {
		case <-readyCtx.Done():
			return nil, fmt.Errorf("olcrtc tunnel not ready: %w", readyCtx.Err())
		case <-t.c.readyChannel():
			// The session came up or reconnected; look again.
		}
	}
}

// Status returns the latest control-stream health snapshot.
func (t *Tunnel) Status() control.Status { return t.c.Status() }

// Close tears the tunnel down and releases the carrier.
func (t *Tunnel) Close() error {
	t.cancel()
	t.c.shutdown()
	return nil
}
