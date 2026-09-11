package keyexchange

import (
	"bytes"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/xtls/xray-core/proxy/olcrtc/olcrtclib/internal/crypto"
)

// This file plugs the exchange into the tunnel. Both types here implement
// muxconn.Keys, so from the data path's point of view nothing has changed:
// records are still sealed and opened by something holding a key. The
// difference is that the key arrives on the wire rather than from the config.
//
// The exchange frames travel on the same channel as everything else and are
// recognised by failing to open as data records — which costs nothing, because
// muxconn already drops records it cannot open (a room full of unrelated
// participants guarantees a steady supply of those) and only logs a
// rate-limited summary.

// ErrNotNegotiated is returned when a record has to be sealed before a session
// key exists. On the client that means the exchange has not finished; on the
// server it means a peer was written to before it ever said hello, which is a
// bug rather than a race.
var ErrNotNegotiated = errors.New("olcrtc: session key not negotiated yet")

// errConsumed marks a record that belonged to the exchange itself. muxconn
// drops whatever it cannot open, so returning an error here is exactly the
// behaviour we want; this one merely says so precisely.
var errConsumed = errors.New("olcrtc: key exchange record consumed")

// negotiateTimeout bounds how long a client's first write waits for the
// exchange to complete. It is generous because bringing a WebRTC carrier up
// through an SFU legitimately takes tens of seconds, and failing a connection
// that would have worked is worse than a slow one.
const negotiateTimeout = 45 * time.Second

// retryInterval is how often the client re-sends its opening frame while
// waiting. The first frames across a freshly negotiated media path are
// routinely lost — the SFU may still be wiring up the route — and a handshake
// that gives up on one lost packet would make the tunnel unreliable for
// reasons that have nothing to do with the peer.
const retryInterval = 2 * time.Second

// plane holds one channel's send path. A tunnel has two — bulk data and the
// isolated control channel — and the exchange has to answer on whichever one
// it was addressed over.
type plane struct {
	aad     []byte
	send    func([]byte) error
	canSend func() bool
}

// ClientKeys negotiates a session key against a server public key and then
// behaves as an ordinary key set.
//
// It is created once per carrier. A reconnect builds a new carrier and calls
// Reset, so every session gets a key of its own — which is the point: the
// server has forgotten the old one, and reusing it would be both broken and,
// were it to work, a loss of forward secrecy.
type ClientKeys struct {
	serverPub string
	now       func() time.Time

	mu        sync.Mutex
	planes    []plane
	initiator *Initiator
	started   bool
	keys      *crypto.KeySet
	failure   error
	ready     chan struct{}
	stop      chan struct{}
}

// NewClientKeys returns a Keys that will negotiate against serverPublicKey.
// The key is parsed eagerly so a typo in the config fails at startup rather
// than on the first connection.
func NewClientKeys(serverPublicKey string) (*ClientKeys, error) {
	if _, err := ParsePublicKey(serverPublicKey); err != nil {
		return nil, err
	}
	return &ClientKeys{
		serverPub: serverPublicKey,
		now:       time.Now,
		ready:     make(chan struct{}),
		stop:      make(chan struct{}),
	}, nil
}

// Bind records a channel the exchange may speak on. Called once per muxconn.
func (c *ClientKeys) Bind(aad []byte, send func([]byte) error, canSend func() bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, p := range c.planes {
		if bytes.Equal(p.aad, aad) {
			return // same plane rebound; the first one still works
		}
	}
	c.planes = append(c.planes, plane{aad: aad, send: send, canSend: canSend})
}

// Reset discards the negotiated key so the next write starts a fresh exchange.
// The carrier calls it when it rebuilds itself.
func (c *ClientKeys) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stop != nil {
		close(c.stop)
	}
	c.planes = nil
	c.initiator = nil
	c.started = false
	c.keys = nil
	c.failure = nil
	c.ready = make(chan struct{})
	c.stop = make(chan struct{})
}

// SealInto protects an outbound record, waiting for the exchange if it has not
// finished. The wait is what makes the rest of the stack oblivious: smux opens
// its first stream the moment the carrier is up and simply blocks here until
// there is a key to use.
func (c *ClientKeys) SealInto(dst, plaintext, aad []byte) ([]byte, error) {
	keys, err := c.await()
	if err != nil {
		return nil, err
	}
	return keys.SealInto(dst, plaintext, aad)
}

// OpenInto opens an inbound record, or consumes the server's answer if the
// exchange is still in flight.
func (c *ClientKeys) OpenInto(dst, record, aad []byte) ([]byte, error) {
	c.mu.Lock()
	keys, initiator := c.keys, c.initiator
	c.mu.Unlock()

	if keys != nil {
		return keys.OpenInto(dst, record, aad)
	}
	if initiator == nil {
		return nil, ErrNotNegotiated
	}

	sessionKey, err := initiator.Complete(record)
	if err != nil {
		// Not our answer: another participant's traffic, or noise. Dropping it
		// is right, and saying nothing about it is right too.
		return nil, errConsumed
	}
	set, err := crypto.NewKeySet(sessionKey, crypto.Client)
	if err != nil {
		c.fail(fmt.Errorf("derive session keys: %w", err))
		return nil, errConsumed
	}
	c.succeed(set)
	return nil, errConsumed
}

// await returns the negotiated keys, starting and driving the exchange if
// nobody has yet.
func (c *ClientKeys) await() (*crypto.KeySet, error) {
	c.mu.Lock()
	if c.keys != nil {
		defer c.mu.Unlock()
		return c.keys, nil
	}
	if c.failure != nil {
		defer c.mu.Unlock()
		return nil, c.failure
	}
	ready, stop := c.ready, c.stop
	if !c.started {
		c.started = true
		c.mu.Unlock()
		go c.drive(ready, stop)
	} else {
		c.mu.Unlock()
	}

	select {
	case <-ready:
	case <-time.After(negotiateTimeout):
		c.fail(fmt.Errorf("%w: no answer in %s", ErrNotNegotiated, negotiateTimeout))
	case <-stop:
		return nil, ErrNotNegotiated
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.keys == nil {
		if c.failure != nil {
			return nil, c.failure
		}
		return nil, ErrNotNegotiated
	}
	return c.keys, nil
}

// drive sends the opening frame and keeps resending it until an answer lands.
func (c *ClientKeys) drive(ready, stop chan struct{}) {
	for {
		if err := c.sendInit(); err != nil {
			c.fail(err)
			return
		}
		select {
		case <-ready:
			return
		case <-stop:
			return
		case <-time.After(retryInterval):
		}
	}
}

// sendInit builds a fresh opening frame and puts it on a channel that will
// take it. A new frame each time rather than a resend: each carries its own
// ephemeral, so a server that answered a frame we never saw simply has a
// session nobody claims, and the next attempt is unaffected.
func (c *ClientKeys) sendInit() error {
	initiator, frame, err := NewInitiator(c.serverPub, c.now())
	if err != nil {
		return err
	}

	c.mu.Lock()
	planes := append([]plane(nil), c.planes...)
	c.initiator = initiator
	c.mu.Unlock()

	if len(planes) == 0 {
		return nil // nothing bound yet; the next tick will find one
	}
	// Prefer a channel that says it is ready. On video-paced carriers the
	// control channel comes up well before the publisher side of the data
	// channel does, and sending into one that is not ready just loses the
	// frame.
	for _, p := range planes {
		if p.canSend == nil || p.canSend() {
			if err := p.send(frame); err == nil {
				return nil
			}
		}
	}
	return nil // every channel refused; try again on the next tick
}

func (c *ClientKeys) succeed(set *crypto.KeySet) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.keys != nil {
		return
	}
	c.keys = set
	close(c.ready)
}

func (c *ClientKeys) fail(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.keys != nil || c.failure != nil {
		return
	}
	c.failure = err
}

// ServerKeys answers exchanges from one peer and then behaves as an ordinary
// key set.
//
// There is one per peer, which is what stops several users sharing a room from
// reading each other's traffic — the thing a single room-wide key made
// impossible.
type ServerKeys struct {
	responder *Responder
	now       func() time.Time

	mu     sync.Mutex
	planes []plane
	keys   *crypto.KeySet
}

// NewServerKeys returns a Keys that answers exchanges with the server's static
// private key.
func NewServerKeys(responder *Responder) *ServerKeys {
	return &ServerKeys{responder: responder, now: time.Now}
}

// Bind records a channel the exchange may answer on.
func (s *ServerKeys) Bind(aad []byte, send func([]byte) error, canSend func() bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.planes {
		if bytes.Equal(p.aad, aad) {
			return
		}
	}
	s.planes = append(s.planes, plane{aad: aad, send: send, canSend: canSend})
}

// Negotiated reports whether this peer has completed an exchange.
func (s *ServerKeys) Negotiated() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.keys != nil
}

// SealInto protects an outbound record. The server never speaks first, so a
// call before the peer has handshaked is a programming error rather than a
// race, and is reported as one.
func (s *ServerKeys) SealInto(dst, plaintext, aad []byte) ([]byte, error) {
	s.mu.Lock()
	keys := s.keys
	s.mu.Unlock()
	if keys == nil {
		return nil, ErrNotNegotiated
	}
	return keys.SealInto(dst, plaintext, aad)
}

// OpenInto opens an inbound record, falling back to treating it as the opening
// frame of an exchange.
//
// The order matters. Trying the session key first keeps the cost of a data
// record at exactly what it was; only a record that fails to open — noise from
// the room, or a peer starting over after a reconnect — pays for an X25519
// operation. A peer that re-handshakes therefore replaces its key without any
// explicit teardown, which is what a carrier rebuild looks like from here.
func (s *ServerKeys) OpenInto(dst, record, aad []byte) ([]byte, error) {
	s.mu.Lock()
	keys := s.keys
	s.mu.Unlock()

	if keys != nil {
		if out, err := keys.OpenInto(dst, record, aad); err == nil {
			return out, nil
		}
	}

	reply, sessionKey, err := s.responder.Accept(record, s.now())
	if err != nil {
		if keys != nil {
			// A record we could not open and that is not an exchange either.
			// Report the original failure shape so muxconn's accounting still
			// means what it says.
			return nil, err
		}
		return nil, errConsumed
	}

	set, keyErr := crypto.NewKeySet(sessionKey, crypto.Server)
	if keyErr != nil {
		return nil, errConsumed
	}

	s.mu.Lock()
	s.keys = set
	planes := append([]plane(nil), s.planes...)
	s.mu.Unlock()

	// Answer on the channel the opening frame arrived over. Anything else and
	// a client whose control plane is up but whose data plane is not would
	// never hear back.
	for _, p := range planes {
		if bytes.Equal(p.aad, aad) {
			_ = p.send(reply)
			return nil, errConsumed
		}
	}
	for _, p := range planes {
		if err := p.send(reply); err == nil {
			break
		}
	}
	return nil, errConsumed
}
