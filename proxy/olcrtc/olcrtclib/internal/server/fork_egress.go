package server

import (
	"context"
	"net"
)

// This file belongs to the fork, not to olcrtc upstream. It is listed in
// fork/olcrtc-manifest.txt so a resync keeps it.
//
// Upstream's server exits to the internet itself, with a net.Dialer or an
// upstream SOCKS proxy. Running inside Xray, it must not: the whole point of
// the olcrtc inbound is that a tunnelled target goes through Xray's router, so
// that routing rules, DNS, sniffing, per-user statistics and the chosen egress
// outbound all apply exactly as they do for any other inbound.
//
// DialHook is that seam. The three lines it needs inside upstream's files are
// in fork/olcrtc-patches/.

// DialFunc establishes the egress connection for one tunnel stream.
//
// It receives the target from the client's CONNECT request together with the
// sessionID the handshake's AuthHook returned, which is what lets the host
// application attribute the connection to a user — Xray encodes the
// authenticated identity into that sessionID and decodes it here.
type DialFunc func(ctx context.Context, addr string, port int, sessionID string) (net.Conn, error)

// dialFor routes one stream's egress, preferring the host application's hook
// over the built-in dialer.
func (s *Server) dialFor(ctx context.Context, request ConnectRequest, sessionID string) (net.Conn, error) {
	if s.dialHook == nil {
		return s.dial(request)
	}
	if err := request.validate(); err != nil {
		return nil, err
	}
	return s.dialHook(ctx, request.Addr, request.Port, sessionID)
}
