package keyexchange

import (
	"bytes"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/xtls/xray-core/proxy/olcrtc/olcrtclib/internal/crypto"
	"github.com/xtls/xray-core/proxy/olcrtc/olcrtclib/internal/logger"
)

// This file plugs the exchange into the tunnel. Both types here implement
// muxconn.Keys, so from the data path's point of view nothing has changed:
// records are still sealed and opened by something holding a key. The
// difference is that the key arrives on the wire rather than from the config.
//
// The exchange frames travel on the same channels as everything else and are
// recognised by failing to open as data records.
//
// # Which channel
//
// A transport with an isolated control channel (vp8channel) runs the exchange
// there and nowhere else. That is not a preference, it is the only thing that
// works: such a client discards everything arriving on its data channel until
// the handshake has told it which participant is the server, and the handshake
// needs the key this exchange produces. The control channel is the one the
// transport delivers before that point, because it is the one upstream runs its
// handshake over. A transport with a single channel simply uses that.
//
// # Retransmission
//
// The first frames across a freshly negotiated media path are routinely lost,
// so the client resends its opening frame until it is answered. It resends the
// same frame, not a new one. The server keeps one key per peer, so every fresh
// opening frame would replace it, and a client that accepted the answer to an
// earlier one would be left holding a key the server had already discarded. On
// a path whose round trip exceeds the resend interval that would never
// converge. With one frame per attempt there is only one key either side can
// arrive at: the server answers a copy it has already seen by repeating its
// answer, without touching the key.

// ErrNotNegotiated is returned when a record has to be sealed before a session
// key exists. On the client that means the exchange has not finished; on the
// server it means a peer was written to before it ever said hello, which is a
// bug rather than a race.
var ErrNotNegotiated = errors.New("olcrtc: session key not negotiated yet")

// consumedError marks a record that belonged to the exchange itself. muxconn
// drops whatever OpenInto refuses; ConsumedRecord tells it that this refusal is
// not a decryption failure, so a successful exchange does not show up in the
// log as one.
type consumedError struct{}

func (consumedError) Error() string        { return "olcrtc: key exchange record consumed" }
func (consumedError) ConsumedRecord() bool { return true }

var errConsumed error = consumedError{}

// negotiateTimeout bounds how long a client's first write waits for the
// exchange to complete.
//
// The carrier is already up when the exchange starts, so this only has to cover
// the first frames finding their way through the SFU, which resends handle.
// What decides the value is what sits above it: the handshake opens its stream
// through smux, which gives up after 30 seconds with a bare "timeout". Failing
// here first turns that into an error that says what is actually wrong.
const negotiateTimeout = 25 * time.Second

// retryInterval is how often the client resends its opening frame while
// waiting for the answer.
const retryInterval = 2 * time.Second

// readyPoll is how often the client looks again for a channel that will take
// the opening frame before it has managed to send it at all. Much shorter than
// retryInterval: nothing has been sent yet, so waiting only adds latency.
const readyPoll = 50 * time.Millisecond

// plane holds one channel's send path. A tunnel has up to two — bulk data and
// the isolated control channel.
type plane struct {
	aad     []byte
	control bool
	send    func([]byte) error
	canSend func() bool
}

func (p plane) ready() bool { return p.canSend == nil || p.canSend() }

// bindPlane records a channel, ignoring a second binding of the same one.
func bindPlane(planes []plane, next plane) []plane {
	for _, p := range planes {
		if bytes.Equal(p.aad, next.aad) {
			return planes
		}
	}
	return append(planes, next)
}

// exchangePlanes returns the channels the exchange may use: the control
// channels when there are any, every channel otherwise. See "Which channel".
func exchangePlanes(planes []plane) []plane {
	var control []plane
	for _, p := range planes {
		if p.control {
			control = append(control, p)
		}
	}
	if len(control) > 0 {
		return control
	}
	return append([]plane(nil), planes...)
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
	initiator *Initiator // this attempt's; nil until the first send
	frame     []byte     // the opening frame initiator produced
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
func (c *ClientKeys) Bind(aad []byte, control bool, send func([]byte) error, canSend func() bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.planes = bindPlane(c.planes, plane{aad: aad, control: control, send: send, canSend: canSend})
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
	c.frame = nil
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
		// Not our answer: noise, or a server that holds a different key. The
		// caller counts it like any record that would not open.
		return nil, err
	}
	set, err := crypto.NewKeySet(sessionKey, crypto.Client)
	if err != nil {
		c.fail(fmt.Errorf("derive session keys: %w", err))
		return nil, errConsumed
	}
	c.succeed(initiator, set)
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

	timer := time.NewTimer(negotiateTimeout)
	defer timer.Stop()
	select {
	case <-ready:
	case <-timer.C:
		c.fail(fmt.Errorf("%w: the server did not answer within %s (check that publicKey matches the key the server logs at startup)",
			ErrNotNegotiated, negotiateTimeout))
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
	sends := 0
	for {
		sent, err := c.sendInit()
		if err != nil {
			c.fail(err)
			return
		}
		wait := readyPoll
		if sent {
			sends++
			if sends == 1 {
				logger.Debugf("olcrtc: key exchange started")
			}
			wait = retryInterval
		}
		timer := time.NewTimer(wait)
		select {
		case <-ready:
			timer.Stop()
			return
		case <-stop:
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// sendInit puts this attempt's opening frame on a channel that will take it,
// building the frame on first use. It reports whether any channel took it.
func (c *ClientKeys) sendInit() (bool, error) {
	c.mu.Lock()
	if c.keys != nil {
		c.mu.Unlock()
		return false, nil
	}
	if c.initiator == nil {
		initiator, frame, err := NewInitiator(c.serverPub, c.now())
		if err != nil {
			c.mu.Unlock()
			return false, err
		}
		c.initiator, c.frame = initiator, frame
	}
	frame := c.frame
	planes := exchangePlanes(c.planes)
	c.mu.Unlock()

	for _, p := range planes {
		if !p.ready() {
			continue
		}
		if err := p.send(frame); err == nil {
			return true, nil
		}
	}
	return false, nil
}

// succeed installs the session key, provided it answers this attempt's frame:
// a Reset in the meantime makes it an answer nobody is waiting for.
func (c *ClientKeys) succeed(initiator *Initiator, set *crypto.KeySet) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.keys != nil || c.initiator != initiator {
		return
	}
	c.keys = set
	close(c.ready)
	logger.Infof("olcrtc: session key negotiated")
}

func (c *ClientKeys) fail(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.keys != nil || c.failure != nil {
		return
	}
	c.failure = err
	logger.Warnf("olcrtc: key exchange failed: %v", err)
}

// ServerKeys answers exchanges from one peer and then behaves as an ordinary
// key set.
//
// There is one per peer, which is what stops several users sharing a room from
// reading each other's traffic — the thing a single room-wide key made
// impossible.
type ServerKeys struct {
	responder *Responder
	peer      string
	now       func() time.Time

	// exchange serialises answering, so two copies of one opening frame
	// arriving together cannot both reach Accept.
	exchange sync.Mutex

	mu     sync.Mutex
	planes []plane
	keys   *crypto.KeySet
	// frame and reply are the last exchange answered, kept so a client that
	// did not hear the answer can be given the same one again.
	frame []byte
	reply []byte
}

// NewServerKeys returns a Keys that answers exchanges with the server's static
// private key. peer names the participant in log lines and may be empty.
func NewServerKeys(responder *Responder, peer string) *ServerKeys {
	return &ServerKeys{responder: responder, peer: peer, now: time.Now}
}

// Bind records a channel the exchange may answer on.
func (s *ServerKeys) Bind(aad []byte, control bool, send func([]byte) error, canSend func() bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.planes = bindPlane(s.planes, plane{aad: aad, control: control, send: send, canSend: canSend})
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
// record at exactly what it was; only a record that fails to open pays for
// anything more, and only one of exactly the opening frame's size pays for an
// X25519 operation.
func (s *ServerKeys) OpenInto(dst, record, aad []byte) ([]byte, error) {
	s.mu.Lock()
	keys := s.keys
	s.mu.Unlock()

	var openErr error = ErrBadFrame
	if keys != nil {
		out, err := keys.OpenInto(dst, record, aad)
		if err == nil {
			return out, nil
		}
		openErr = err
	}
	if len(record) != InitSize {
		return nil, openErr
	}
	return nil, s.answer(record, aad)
}

// answer handles an opening frame: a copy of the last one gets the same answer
// again, a new one gets a new session.
func (s *ServerKeys) answer(frame, aad []byte) error {
	s.exchange.Lock()
	defer s.exchange.Unlock()

	s.mu.Lock()
	if s.frame != nil && bytes.Equal(frame, s.frame) {
		reply := s.reply
		s.mu.Unlock()
		s.send(reply, aad)
		return errConsumed
	}
	rekey := s.keys != nil
	s.mu.Unlock()

	reply, sessionKey, err := s.responder.Accept(frame, s.now())
	if err != nil {
		return err
	}
	set, err := crypto.NewKeySet(sessionKey, crypto.Server)
	if err != nil {
		return fmt.Errorf("derive session keys: %w", err)
	}

	s.mu.Lock()
	s.keys = set
	s.frame = bytes.Clone(frame)
	s.reply = reply
	s.mu.Unlock()

	if rekey {
		logger.Infof("olcrtc: session key renegotiated%s", s.label())
	} else {
		logger.Infof("olcrtc: session key negotiated%s", s.label())
	}
	s.send(reply, aad)
	return errConsumed
}

// send answers on the channel the opening frame arrived over, falling back to
// any other. The client only sends where it can also receive, so the first
// choice is the one that works; the fallback covers a channel that has since
// gone away.
func (s *ServerKeys) send(reply, aad []byte) {
	s.mu.Lock()
	planes := append([]plane(nil), s.planes...)
	s.mu.Unlock()

	for _, p := range planes {
		if !bytes.Equal(p.aad, aad) {
			continue
		}
		err := p.send(reply)
		if err == nil {
			return
		}
		logger.Debugf("olcrtc: key exchange answer%s not sent: %v", s.label(), err)
	}
	for _, p := range planes {
		if !bytes.Equal(p.aad, aad) && p.send(reply) == nil {
			return
		}
	}
	logger.Warnf("olcrtc: key exchange answer%s could not be sent on any channel", s.label())
}

func (s *ServerKeys) label() string {
	if s.peer == "" {
		return ""
	}
	return " peer=" + s.peer
}
