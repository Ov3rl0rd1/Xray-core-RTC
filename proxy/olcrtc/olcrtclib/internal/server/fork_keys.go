package server

import (
	"fmt"
	"sync"

	"github.com/xtls/xray-core/proxy/olcrtc/olcrtclib/internal/crypto"
	"github.com/xtls/xray-core/proxy/olcrtc/olcrtclib/internal/keyexchange"
	"github.com/xtls/xray-core/proxy/olcrtc/olcrtclib/internal/logger"
	"github.com/xtls/xray-core/proxy/olcrtc/olcrtclib/internal/muxconn"
	"github.com/xtls/xray-core/proxy/olcrtc/olcrtclib/internal/tunnelcore"
)

// This file belongs to the fork, not to olcrtc upstream. It is listed in
// fork/olcrtc-manifest.txt so a resync keeps it.
//
// Upstream protects the tunnel with one key derived from a pre-shared secret
// every client in the room holds. That has a consequence which rules out the
// cheap way to serve several users — putting them in one room instead of one
// room each — because everybody in the room can read everybody else's traffic.
//
// Here each peer negotiates a key of its own against the server's static
// public key, so the room becomes a shared carrier rather than a shared
// secret. keysFor is the whole seam: the call sites inside upstream's server
// ask it for a peer's keys instead of reaching for one shared set.

// keyStore hands out one key set per peer and remembers them for as long as
// the peer is around.
type keyStore struct {
	responder *keyexchange.Responder

	mu   sync.Mutex
	keys map[string]*keyexchange.ServerKeys
}

func newKeyStore(responder *keyexchange.Responder) *keyStore {
	return &keyStore{responder: responder, keys: make(map[string]*keyexchange.ServerKeys)}
}

// serverKeys chooses how this server protects its tunnel.
//
// PrivateKey selects the key exchange: each peer gets a session key of its
// own, negotiated against this key's public half. KeyHex is upstream's
// pre-shared secret, kept working so upstream's own tests keep testing it —
// nothing in this fork's configuration can select it.
func serverKeys(cfg Config) (muxconn.Keys, *keyStore, error) {
	if cfg.PrivateKey != "" {
		responder, err := keyexchange.NewResponder(cfg.PrivateKey)
		if err != nil {
			return nil, nil, fmt.Errorf("setup server key: %w", err)
		}
		logger.Infof("server public key: %s", responder.PublicKey())
		return nil, newKeyStore(responder), nil
	}
	keys, err := tunnelcore.SetupKeySet(cfg.KeyHex, crypto.Server)
	if err != nil {
		return nil, nil, fmt.Errorf("setup key set: %w", err)
	}
	return keys, nil, nil
}

// keysFor returns the key set for one peer, creating it on first sight.
//
// peerID is empty on transports that cannot tell participants apart, which
// collapses to a single session — the same thing upstream has.
func (s *Server) keysFor(peerID string) muxconn.Keys {
	if s.keyStore == nil {
		return s.keys // upstream's shared secret; one key for the whole room
	}
	return s.keyStore.get(peerID)
}

func (k *keyStore) get(peerID string) *keyexchange.ServerKeys {
	k.mu.Lock()
	defer k.mu.Unlock()
	keys := k.keys[peerID]
	if keys == nil {
		keys = keyexchange.NewServerKeys(k.responder)
		k.keys[peerID] = keys
	}
	return keys
}

// forgetKeys drops a peer's key set once the peer is gone, so a room that has
// seen a lot of churn does not accumulate one per participant that ever
// joined. A peer that comes back simply negotiates again.
func (s *Server) forgetKeys(peerID string) {
	if s.keyStore == nil {
		return
	}
	s.keyStore.mu.Lock()
	defer s.keyStore.mu.Unlock()
	delete(s.keyStore.keys, peerID)
}

// PublicKey returns the key clients have to be configured with, or empty when
// this server runs on upstream's shared secret instead. Logged at startup so
// an operator can see what a server is actually serving without going back to
// the config that produced it.
func (s *Server) PublicKey() string {
	if s.keyStore == nil {
		return ""
	}
	return s.keyStore.responder.PublicKey()
}
