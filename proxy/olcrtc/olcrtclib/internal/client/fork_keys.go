package client

import (
	"fmt"

	"github.com/xtls/xray-core/proxy/olcrtc/olcrtclib/internal/crypto"
	"github.com/xtls/xray-core/proxy/olcrtc/olcrtclib/internal/keyexchange"
	"github.com/xtls/xray-core/proxy/olcrtc/olcrtclib/internal/muxconn"
	"github.com/xtls/xray-core/proxy/olcrtc/olcrtclib/internal/tunnelcore"
)

// This file belongs to the fork, not to olcrtc upstream. It is listed in
// fork/olcrtc-manifest.txt so a resync keeps it.

// clientKeys chooses how this client protects its tunnel.
//
// ServerPublicKey selects the key exchange: the client carries only the
// server's public half and negotiates a session key of its own, so a leaked
// client configuration reveals nothing about anyone else's traffic. KeyHex is
// upstream's pre-shared secret, kept working so upstream's own tests keep
// testing it — nothing in this fork's configuration can select it.
func clientKeys(cfg Config) (muxconn.Keys, error) {
	if cfg.ServerPublicKey != "" {
		keys, err := keyexchange.NewClientKeys(cfg.ServerPublicKey)
		if err != nil {
			return nil, fmt.Errorf("setup server key: %w", err)
		}
		return keys, nil
	}
	keys, err := tunnelcore.SetupKeySet(cfg.KeyHex, crypto.Client)
	if err != nil {
		return nil, fmt.Errorf("setup key set: %w", err)
	}
	return keys, nil
}

// resetKeys starts a fresh exchange when the carrier is rebuilt.
//
// A reconnect is a new session as far as the server is concerned: it has
// forgotten the old key along with the peer that held it. Reusing ours would
// leave the two sides unable to talk, and were it somehow to work it would
// hand every session after the first the same key — losing the forward secrecy
// that having a per-session key is for.
//
// A pre-shared key set has nothing to reset, so this is a no-op for it.
func (c *Client) resetKeys() {
	if k, ok := c.keys.(*keyexchange.ClientKeys); ok {
		k.Reset()
	}
}
