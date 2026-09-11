package keyexchange

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"testing"
	"time"
)

var now = time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

func keyPair(t *testing.T) (private, public string) {
	t.Helper()
	private, public, err := GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	return private, public
}

// exchange runs a full handshake and returns both sides' session keys.
func exchange(t *testing.T) (clientKey, serverKey []byte) {
	t.Helper()
	private, public := keyPair(t)

	initiator, init, err := NewInitiator(public, now)
	if err != nil {
		t.Fatal(err)
	}
	responder, err := NewResponder(private)
	if err != nil {
		t.Fatal(err)
	}
	reply, serverKey, err := responder.Accept(init, now)
	if err != nil {
		t.Fatal(err)
	}
	clientKey, err = initiator.Complete(reply)
	if err != nil {
		t.Fatal(err)
	}
	return clientKey, serverKey
}

func TestBothSidesDeriveTheSameKey(t *testing.T) {
	clientKey, serverKey := exchange(t)
	if !bytes.Equal(clientKey, serverKey) {
		t.Fatalf("session keys differ:\n client %x\n server %x", clientKey, serverKey)
	}
	if len(clientKey) != KeySize {
		t.Fatalf("session key is %d bytes, want %d", len(clientKey), KeySize)
	}
}

// TestEveryConnectionGetsItsOwnKey is the property that lets several users
// share one room: with the old shared room key they could all decrypt each
// other.
func TestEveryConnectionGetsItsOwnKey(t *testing.T) {
	private, public := keyPair(t)
	responder, err := NewResponder(private)
	if err != nil {
		t.Fatal(err)
	}

	seen := map[string]bool{}
	for i := 0; i < 8; i++ {
		_, init, err := NewInitiator(public, now)
		if err != nil {
			t.Fatal(err)
		}
		_, key, err := responder.Accept(init, now)
		if err != nil {
			t.Fatal(err)
		}
		if seen[string(key)] {
			t.Fatal("two connections to the same server derived the same session key")
		}
		seen[string(key)] = true
	}
}

func TestFramesAreFixedSize(t *testing.T) {
	private, public := keyPair(t)
	_, init, err := NewInitiator(public, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(init) != InitSize {
		t.Fatalf("init frame is %d bytes, want the advertised %d", len(init), InitSize)
	}
	responder, err := NewResponder(private)
	if err != nil {
		t.Fatal(err)
	}
	reply, _, err := responder.Accept(init, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(reply) != ReplySize {
		t.Fatalf("reply frame is %d bytes, want the advertised %d", len(reply), ReplySize)
	}
}

// TestAnotherServersKeyCannotOpenTheFrame is what stops a stray peer in the
// same room from answering — the property the shared room key used to provide.
func TestAnotherServersKeyCannotOpenTheFrame(t *testing.T) {
	_, public := keyPair(t)
	otherPrivate, _ := keyPair(t)

	_, init, err := NewInitiator(public, now)
	if err != nil {
		t.Fatal(err)
	}
	other, err := NewResponder(otherPrivate)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := other.Accept(init, now); !errors.Is(err, ErrBadFrame) {
		t.Fatalf("a different server opened the frame: err = %v", err)
	}
}

func TestGarbageIsRejectedWithoutDistinction(t *testing.T) {
	private, _ := keyPair(t)
	responder, err := NewResponder(private)
	if err != nil {
		t.Fatal(err)
	}

	cases := map[string][]byte{
		"empty":     {},
		"too short": make([]byte, InitSize-1),
		"too long":  make([]byte, InitSize+1),
		"right size, random": func() []byte {
			b := make([]byte, InitSize)
			if _, err := rand.Read(b); err != nil {
				t.Fatal(err)
			}
			return b
		}(),
		"all zeroes": make([]byte, InitSize),
	}
	for name, frame := range cases {
		if _, _, err := responder.Accept(frame, now); !errors.Is(err, ErrBadFrame) {
			t.Errorf("%s: err = %v, want ErrBadFrame — a peer must not learn why it failed", name, err)
		}
	}
}

func TestATamperedFrameIsRejected(t *testing.T) {
	private, public := keyPair(t)
	responder, err := NewResponder(private)
	if err != nil {
		t.Fatal(err)
	}
	_, init, err := NewInitiator(public, now)
	if err != nil {
		t.Fatal(err)
	}

	for i := range init {
		tampered := bytes.Clone(init)
		tampered[i] ^= 0x01
		if _, _, err := responder.Accept(tampered, now); err == nil {
			t.Fatalf("flipping bit 0 of byte %d was accepted", i)
		}
	}
}

func TestATamperedReplyIsRejected(t *testing.T) {
	private, public := keyPair(t)
	initiator, init, err := NewInitiator(public, now)
	if err != nil {
		t.Fatal(err)
	}
	responder, err := NewResponder(private)
	if err != nil {
		t.Fatal(err)
	}
	reply, _, err := responder.Accept(init, now)
	if err != nil {
		t.Fatal(err)
	}

	for i := range reply {
		tampered := bytes.Clone(reply)
		tampered[i] ^= 0x80
		if _, err := initiator.Complete(tampered); err == nil {
			t.Fatalf("the client accepted a reply with byte %d tampered", i)
		}
	}
}

// TestAReplyFromAnImpostorIsRejected covers the other direction: a peer that
// sees the client's frame in the room must not be able to answer it and take
// the session over.
//
// The impostor is given every advantage. It reads e_pub, which travels in the
// clear; it generates its own ephemeral and computes ee correctly, exactly as
// the real server would. The only thing it cannot compute is es, which needs
// the server's static private key — and that is enough.
func TestAReplyFromAnImpostorIsRejected(t *testing.T) {
	_, public := keyPair(t)
	initiator, init, err := NewInitiator(public, now)
	if err != nil {
		t.Fatal(err)
	}

	clientEphemeral, err := ecdh.X25519().NewPublicKey(init[:KeySize])
	if err != nil {
		t.Fatal(err)
	}
	impostorEphemeral, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ee, err := impostorEphemeral.ECDH(clientEphemeral)
	if err != nil {
		t.Fatal(err)
	}

	sPub := impostorEphemeral.PublicKey().Bytes()
	transcript := concat(clientEphemeral.Bytes(), sPub)
	// The best it can do is guess es. Any guess will do; it has 2^252 of them.
	guessedShared := concat(make([]byte, KeySize), ee)

	aead, nonce, err := derive(guessedShared, saltReply, transcript)
	if err != nil {
		t.Fatal(err)
	}
	forged := append([]byte{}, sPub...)
	forged = aead.Seal(forged, nonce, []byte{Version}, sPub)

	if _, err := initiator.Complete(forged); !errors.Is(err, ErrBadFrame) {
		t.Fatalf("the client accepted a reply from a peer without the server's key: err = %v", err)
	}
}

func TestStaleTimestampsAreRefused(t *testing.T) {
	private, public := keyPair(t)
	responder, err := NewResponder(private)
	if err != nil {
		t.Fatal(err)
	}

	for _, skew := range []time.Duration{MaxClockSkew + time.Minute, -MaxClockSkew - time.Minute} {
		_, init, err := NewInitiator(public, now.Add(-skew))
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := responder.Accept(init, now); !errors.Is(err, ErrClockSkew) {
			t.Errorf("skew %s: err = %v, want ErrClockSkew", skew, err)
		}
	}

	// Just inside the window must still work — clocks are never exact.
	_, init, err := NewInitiator(public, now.Add(-MaxClockSkew+time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := responder.Accept(init, now); err != nil {
		t.Fatalf("a client a few minutes out of step was refused: %v", err)
	}
}

// TestReplayingAFrameYieldsAKeyTheReplayerCannotDerive is why a replay cache is
// not needed: the server answers with a fresh ephemeral, so the session key is
// different and belongs to nobody.
func TestReplayingAFrameYieldsAKeyTheReplayerCannotDerive(t *testing.T) {
	private, public := keyPair(t)
	initiator, init, err := NewInitiator(public, now)
	if err != nil {
		t.Fatal(err)
	}
	responder, err := NewResponder(private)
	if err != nil {
		t.Fatal(err)
	}

	firstReply, firstKey, err := responder.Accept(init, now)
	if err != nil {
		t.Fatal(err)
	}
	_, replayKey, err := responder.Accept(init, now)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(firstKey, replayKey) {
		t.Fatal("a replayed frame produced the same session key")
	}

	// And the genuine client, which holds the ephemeral private key, still
	// completes against the reply meant for it.
	got, err := initiator.Complete(firstReply)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, firstKey) {
		t.Fatal("the genuine client derived a different key from its own reply")
	}
}

func TestKeysAreAcceptedInEveryEncodingOperatorsUse(t *testing.T) {
	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	raw := priv.Bytes()

	for name, encoded := range map[string]string{
		"base64 raw url":  base64.RawURLEncoding.EncodeToString(raw),
		"base64 standard": base64.StdEncoding.EncodeToString(raw),
		"base64 raw std":  base64.RawStdEncoding.EncodeToString(raw),
		"hex":             hex.EncodeToString(raw),
	} {
		got, err := ParsePrivateKey(encoded)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if !bytes.Equal(got.Bytes(), raw) {
			t.Errorf("%s: decoded to the wrong key", name)
		}
	}
}

func TestBadKeysAreRefusedClearly(t *testing.T) {
	for name, key := range map[string]string{
		"empty":     "",
		"too short": base64.RawURLEncoding.EncodeToString(make([]byte, 16)),
		"too long":  base64.RawURLEncoding.EncodeToString(make([]byte, 64)),
		"not a key": "this is not a key at all",
	} {
		if _, err := ParsePrivateKey(key); !errors.Is(err, ErrBadKey) {
			t.Errorf("private %s: err = %v, want ErrBadKey", name, err)
		}
		if _, err := ParsePublicKey(key); !errors.Is(err, ErrBadKey) {
			t.Errorf("public %s: err = %v, want ErrBadKey", name, err)
		}
	}
}

func TestResponderPublicKeyMatchesTheGeneratedOne(t *testing.T) {
	private, public := keyPair(t)
	responder, err := NewResponder(private)
	if err != nil {
		t.Fatal(err)
	}
	if got := responder.PublicKey(); got != public {
		t.Fatalf("responder reports %q, want %q", got, public)
	}
}

// TestExchangeCarriesNoIdentity pins the decision that the credential travels
// later, inside the session, where it is forward-secret. If a UUID ever ends up
// in the first frame, anyone who obtains the server's static key can read every
// user's identity out of recorded traffic.
func TestExchangeCarriesNoIdentity(t *testing.T) {
	private, public := keyPair(t)
	responder, err := NewResponder(private)
	if err != nil {
		t.Fatal(err)
	}
	_, init, err := NewInitiator(public, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := responder.Accept(init, now); err != nil {
		t.Fatal(err)
	}
	// The payload is exactly a version byte and a timestamp; there is no room
	// for anything else, and this size assertion is what keeps it that way.
	if initPayloadSize != 9 {
		t.Fatalf("the init payload grew to %d bytes — has an identity crept in?", initPayloadSize)
	}
}

func BenchmarkAccept(b *testing.B) {
	private, public, err := GenerateKeyPair()
	if err != nil {
		b.Fatal(err)
	}
	responder, err := NewResponder(private)
	if err != nil {
		b.Fatal(err)
	}
	_, init, err := NewInitiator(public, time.Now())
	if err != nil {
		b.Fatal(err)
	}
	at := time.Now()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := responder.Accept(init, at); err != nil {
			b.Fatal(err)
		}
	}
}
