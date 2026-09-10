// Package keyexchange establishes a per-connection session key between an
// olcrtc client and server, replacing the shared room key the tunnel used to
// be encrypted with.
//
// # Why the shared key had to go
//
// One XChaCha20 key per room, held by every client, had three consequences.
// A key leaked by one client compromised every other. Rotating it meant
// reissuing every client's configuration at once. And — the one that blocks
// putting several users in a single room, which is far cheaper than running a
// room per user — everyone in the room could decrypt everyone else's traffic.
//
// # What replaces it
//
// The server holds a long-term X25519 key pair; clients are configured with
// only its public half, the same shape as REALITY and WireGuard. A key
// exchange runs over two raw transport frames before anything else, and every
// connection ends up with a session key of its own:
//
//	client                                        server
//	  │  e_pub ‖ AEAD(k_init){version, timestamp}  │
//	  │ ─────────────────────────────────────────► │  only the holder of the
//	  │                                            │  static key can open this
//	  │  s_epub ‖ AEAD(k_reply){version}           │
//	  │ ◄───────────────────────────────────────── │
//	  │                                            │
//	  ▼ both derive k_session; everything after is encrypted with it
//
//	es        = X25519(e_priv,  S_pub)      what the static key buys
//	ee        = X25519(e_priv,  s_epub)     what makes it forward-secret
//	k_init    = HKDF(es,      "olcrtc-ke-init-v1",    e_pub)
//	k_reply   = HKDF(es ‖ ee, "olcrtc-ke-reply-v1",   e_pub ‖ s_epub)
//	k_session = HKDF(es ‖ ee, "olcrtc-ke-session-v1", e_pub ‖ s_epub)
//
// To anyone else in the room both frames are indistinguishable from random:
// they cannot derive k_init without the server's static private key, so they
// can neither read the exchange nor answer it. That is the same property the
// room key used to provide against stray peers, without the key being a secret
// anyone has to distribute.
//
// Compromising the server's static key later does not open recorded sessions:
// that yields es, but k_session also needs ee, which requires an ephemeral
// private key both sides discard.
//
// # What is deliberately not here
//
// The exchange carries no identity at all. Which user this is — their UUID,
// their device — travels in olcrtc's existing handshake on the first smux
// stream, which by then is already encrypted with k_session and therefore
// forward-secret. Putting the credential in the first frame instead, as a
// 0-RTT design would, would mean anyone who ever obtains the server's static
// key could go back through recorded traffic and read every user's UUID.
//
// The cost is that an unauthenticated peer can complete the exchange before
// being turned away. That is one X25519 operation per attempt, and it is what
// TLS does too.
package keyexchange

import (
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"golang.org/x/crypto/chacha20poly1305"
)

// Version is the exchange's wire version. It travels inside the encrypted
// payload rather than in the clear, so both frames stay indistinguishable from
// random to an observer — and so a future hybrid (adding ML-KEM alongside
// X25519) can be negotiated without the frame layout giving anything away.
const Version = 1

// KeySize is the length of an X25519 key, public or private.
const KeySize = 32

// Sizes of the two frames. Both are fixed, which lets a receiver reject
// anything of the wrong length before doing any cryptography.
const (
	initPayloadSize  = 1 + 8 // version, unix seconds
	replyPayloadSize = 1     // version

	// InitSize is the length of the client's first frame.
	InitSize = KeySize + initPayloadSize + chacha20poly1305.Overhead
	// ReplySize is the length of the server's answer.
	ReplySize = KeySize + replyPayloadSize + chacha20poly1305.Overhead
)

// MaxClockSkew bounds how far a client's timestamp may be from the server's.
//
// It is the only replay protection here, and it is enough: replaying a
// recorded first frame makes the server generate a *fresh* ephemeral, so the
// resulting session key is one the replayer cannot derive. The window exists
// to bound the pointless work, not to prevent a break.
const MaxClockSkew = 5 * time.Minute

// Errors reported by this package. They are deliberately coarse on the server
// side: a peer that fails to authenticate learns only that it failed.
var (
	// ErrBadKey is returned when a key is not a valid X25519 key.
	ErrBadKey = errors.New("olcrtc: invalid X25519 key")
	// ErrBadFrame is returned when a frame is the wrong size or does not
	// decrypt. Both are the same answer to anyone watching.
	ErrBadFrame = errors.New("olcrtc: not a key exchange frame")
	// ErrVersion is returned when the peer speaks a version we do not.
	ErrVersion = errors.New("olcrtc: unsupported key exchange version")
	// ErrClockSkew is returned when a client's timestamp is too far off.
	ErrClockSkew = errors.New("olcrtc: key exchange timestamp outside the accepted window")
)

// Salts separate the three derived keys. Distinct salts mean that learning one
// of them says nothing about the others.
const (
	saltInit    = "olcrtc-ke-init-v1"
	saltReply   = "olcrtc-ke-reply-v1"
	saltSession = "olcrtc-ke-session-v1"
)

// GenerateKeyPair returns a fresh server key pair, encoded the way the rest of
// Xray encodes X25519 keys.
//
// `xray x25519` produces exactly this, so an operator provisioning a server
// does not need a new tool: the private half goes in the inbound's
// `privateKey`, the public half in every client's `publicKey`.
func GenerateKeyPair() (privateKey, publicKey string, err error) {
	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	enc := base64.RawURLEncoding
	return enc.EncodeToString(priv.Bytes()), enc.EncodeToString(priv.PublicKey().Bytes()), nil
}

// ParsePrivateKey decodes a server private key. base64 (raw URL or standard)
// and hex are all accepted, because operators paste keys from wherever they
// generated them and a rejected key is a server that will not start.
func ParsePrivateKey(s string) (*ecdh.PrivateKey, error) {
	raw, err := decodeKey(s)
	if err != nil {
		return nil, err
	}
	key, err := ecdh.X25519().NewPrivateKey(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBadKey, err)
	}
	return key, nil
}

// ParsePublicKey decodes a server public key, accepting the same encodings.
func ParsePublicKey(s string) (*ecdh.PublicKey, error) {
	raw, err := decodeKey(s)
	if err != nil {
		return nil, err
	}
	key, err := ecdh.X25519().NewPublicKey(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBadKey, err)
	}
	return key, nil
}

func decodeKey(s string) ([]byte, error) {
	if s == "" {
		return nil, fmt.Errorf("%w: empty", ErrBadKey)
	}
	for _, decode := range []func(string) ([]byte, error){
		base64.RawURLEncoding.DecodeString,
		base64.StdEncoding.DecodeString,
		base64.RawStdEncoding.DecodeString,
		hex.DecodeString,
	} {
		if raw, err := decode(s); err == nil && len(raw) == KeySize {
			return raw, nil
		}
	}
	return nil, fmt.Errorf("%w: %q is not %d bytes of base64 or hex", ErrBadKey, s, KeySize)
}

// Initiator is the client half of one exchange. It is single-use: a reconnect
// makes a new one, which is what gives each session its own key.
type Initiator struct {
	ephemeral *ecdh.PrivateKey
	serverPub *ecdh.PublicKey
	es        []byte
}

// NewInitiator starts an exchange against a server public key and returns the
// frame to send.
func NewInitiator(serverPublicKey string, now time.Time) (*Initiator, []byte, error) {
	pub, err := ParsePublicKey(serverPublicKey)
	if err != nil {
		return nil, nil, err
	}
	ephemeral, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	es, err := ephemeral.ECDH(pub)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %w", ErrBadKey, err)
	}

	i := &Initiator{ephemeral: ephemeral, serverPub: pub, es: es}
	ePub := ephemeral.PublicKey().Bytes()

	payload := make([]byte, initPayloadSize)
	payload[0] = Version
	binary.BigEndian.PutUint64(payload[1:], uint64(now.Unix()))

	aead, nonce, err := derive(i.es, saltInit, ePub)
	if err != nil {
		return nil, nil, err
	}
	frame := make([]byte, 0, InitSize)
	frame = append(frame, ePub...)
	frame = aead.Seal(frame, nonce, payload, ePub)
	return i, frame, nil
}

// Complete consumes the server's answer and returns the session key.
func (i *Initiator) Complete(reply []byte) ([]byte, error) {
	if len(reply) != ReplySize {
		return nil, ErrBadFrame
	}
	serverEphemeral, err := ecdh.X25519().NewPublicKey(reply[:KeySize])
	if err != nil {
		return nil, ErrBadFrame
	}
	ee, err := i.ephemeral.ECDH(serverEphemeral)
	if err != nil {
		return nil, ErrBadFrame
	}

	ePub := i.ephemeral.PublicKey().Bytes()
	sPub := serverEphemeral.Bytes()
	transcript := concat(ePub, sPub)
	shared := concat(i.es, ee)

	aead, nonce, err := derive(shared, saltReply, transcript)
	if err != nil {
		return nil, err
	}
	payload, err := aead.Open(nil, nonce, reply[KeySize:], sPub)
	if err != nil {
		return nil, ErrBadFrame
	}
	if len(payload) != replyPayloadSize {
		return nil, ErrBadFrame
	}
	if payload[0] != Version {
		return nil, fmt.Errorf("%w: server speaks v%d, we speak v%d", ErrVersion, payload[0], Version)
	}
	return sessionKey(shared, transcript)
}

// Responder is the server half. One Responder serves every exchange, because
// the only long-lived thing it holds is the static key; each exchange's
// ephemeral is generated inside Accept.
type Responder struct {
	static *ecdh.PrivateKey
}

// NewResponder wraps a server private key.
func NewResponder(privateKey string) (*Responder, error) {
	key, err := ParsePrivateKey(privateKey)
	if err != nil {
		return nil, err
	}
	return &Responder{static: key}, nil
}

// PublicKey returns the key clients must be configured with.
func (r *Responder) PublicKey() string {
	return base64.RawURLEncoding.EncodeToString(r.static.PublicKey().Bytes())
}

// Accept opens a client's first frame and returns the answer to send back
// along with the session key.
//
// It is safe to call on anything that arrives, which is how it is used: the
// server tries a frame as data first and falls back to here, so a frame that
// is neither simply produces ErrBadFrame and is dropped. Nothing about the
// failure distinguishes a stray peer's noise from a wrong key.
func (r *Responder) Accept(frame []byte, now time.Time) (reply, key []byte, err error) {
	if len(frame) != InitSize {
		return nil, nil, ErrBadFrame
	}
	clientEphemeral, err := ecdh.X25519().NewPublicKey(frame[:KeySize])
	if err != nil {
		return nil, nil, ErrBadFrame
	}
	es, err := r.static.ECDH(clientEphemeral)
	if err != nil {
		return nil, nil, ErrBadFrame
	}
	ePub := clientEphemeral.Bytes()

	aead, nonce, err := derive(es, saltInit, ePub)
	if err != nil {
		return nil, nil, err
	}
	payload, err := aead.Open(nil, nonce, frame[KeySize:], ePub)
	if err != nil {
		// Either not a key exchange frame at all, or one meant for a
		// different server. Same answer either way.
		return nil, nil, ErrBadFrame
	}
	if len(payload) != initPayloadSize {
		return nil, nil, ErrBadFrame
	}
	if payload[0] != Version {
		return nil, nil, fmt.Errorf("%w: client speaks v%d, we speak v%d", ErrVersion, payload[0], Version)
	}
	sent := time.Unix(int64(binary.BigEndian.Uint64(payload[1:])), 0)
	if skew := now.Sub(sent); skew > MaxClockSkew || skew < -MaxClockSkew {
		return nil, nil, fmt.Errorf("%w: off by %s", ErrClockSkew, skew.Round(time.Second))
	}

	serverEphemeral, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	ee, err := serverEphemeral.ECDH(clientEphemeral)
	if err != nil {
		return nil, nil, ErrBadFrame
	}
	sPub := serverEphemeral.PublicKey().Bytes()
	transcript := concat(ePub, sPub)
	shared := concat(es, ee)

	replyAEAD, replyNonce, err := derive(shared, saltReply, transcript)
	if err != nil {
		return nil, nil, err
	}
	reply = make([]byte, 0, ReplySize)
	reply = append(reply, sPub...)
	reply = replyAEAD.Seal(reply, replyNonce, []byte{Version}, sPub)

	key, err = sessionKey(shared, transcript)
	if err != nil {
		return nil, nil, err
	}
	return reply, key, nil
}

// derive turns a shared secret into the AEAD and nonce for one frame.
//
// The nonce is derived rather than fixed at zero so that it is bound to the
// same transcript as the key: a peer that reused an ephemeral would repeat the
// pair rather than encrypt something new under an old nonce.
func derive(secret []byte, salt string, info []byte) (cipher.AEAD, []byte, error) {
	okm, err := hkdf.Key(sha256.New, secret, []byte(salt), string(info),
		chacha20poly1305.KeySize+chacha20poly1305.NonceSize)
	if err != nil {
		return nil, nil, err
	}
	c, err := chacha20poly1305.New(okm[:chacha20poly1305.KeySize])
	if err != nil {
		return nil, nil, err
	}
	return c, okm[chacha20poly1305.KeySize:], nil
}

// sessionKey derives the key everything after the exchange is encrypted with.
func sessionKey(shared, transcript []byte) ([]byte, error) {
	return hkdf.Key(sha256.New, shared, []byte(saltSession), string(transcript), KeySize)
}

func concat(a, b []byte) []byte {
	out := make([]byte, 0, len(a)+len(b))
	out = append(out, a...)
	return append(out, b...)
}
