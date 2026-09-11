package keyexchange

import (
	"bytes"
	"errors"
	"sync"
	"testing"
	"time"
)

// wire couples a client's and a server's key handling the way muxconn does:
// every record one side seals is handed to the other's OpenInto, and the
// exchange's own frames ride the same path as everything else.
//
// It stands in for the transport rather than mocking the key types, because
// the property worth testing is exactly that the two ends converge through the
// ordinary record path without anything above them knowing.
type wire struct {
	t *testing.T

	mu       sync.Mutex
	toServer [][]byte
	toClient [][]byte
	dropped  int
}

const (
	dataAAD    = "olcrtc/muxconn/v2/data"
	controlAAD = "olcrtc/muxconn/v2/control"
)

func newWire(t *testing.T) *wire { return &wire{t: t} }

func (w *wire) clientSend(b []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.toServer = append(w.toServer, bytes.Clone(b))
	return nil
}

func (w *wire) serverSend(b []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.toClient = append(w.toClient, bytes.Clone(b))
	return nil
}

func (w *wire) takeToServer() [][]byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := w.toServer
	w.toServer = nil
	return out
}

func (w *wire) takeToClient() [][]byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := w.toClient
	w.toClient = nil
	return out
}

// pair builds a client and a server wired to each other, bound on both planes
// exactly as muxconn binds them.
func pair(t *testing.T) (*ClientKeys, *ServerKeys, *wire) {
	t.Helper()
	private, public := keyPair(t)

	client, err := NewClientKeys(public)
	if err != nil {
		t.Fatal(err)
	}
	responder, err := NewResponder(private)
	if err != nil {
		t.Fatal(err)
	}
	server := NewServerKeys(responder)

	w := newWire(t)
	ready := func() bool { return true }
	client.Bind([]byte(dataAAD), w.clientSend, ready)
	client.Bind([]byte(controlAAD), w.clientSend, ready)
	server.Bind([]byte(dataAAD), w.serverSend, ready)
	server.Bind([]byte(controlAAD), w.serverSend, ready)
	return client, server, w
}

// pump carries whatever is queued in both directions until the client has a
// key or the deadline passes. It is the test's stand-in for the transport
// delivering records.
func pump(t *testing.T, client *ClientKeys, server *ServerKeys, w *wire, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatal("the exchange never completed")
		}
		moved := false
		for _, frame := range w.takeToServer() {
			_, _ = server.OpenInto(nil, frame, []byte(dataAAD))
			moved = true
		}
		for _, frame := range w.takeToClient() {
			_, _ = client.OpenInto(nil, frame, []byte(dataAAD))
			moved = true
		}
		if !moved {
			time.Sleep(time.Millisecond)
		}
	}
}

// TestSealAndOpenAcrossTheExchange is the whole feature in one test: a client
// with only the server's public key writes a record, and the server reads it.
func TestSealAndOpenAcrossTheExchange(t *testing.T) {
	client, server, w := pair(t)

	payload := []byte("the quick brown fox")
	var sealed []byte
	var sealErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		sealed, sealErr = client.SealInto(nil, payload, []byte(dataAAD))
	}()

	pump(t, client, server, w, func() bool {
		select {
		case <-done:
			return true
		default:
			return false
		}
	})
	<-done
	if sealErr != nil {
		t.Fatal(sealErr)
	}

	opened, err := server.OpenInto(nil, sealed, []byte(dataAAD))
	if err != nil {
		t.Fatalf("the server could not open the client's first record: %v", err)
	}
	if !bytes.Equal(opened, payload) {
		t.Fatalf("opened %q, want %q", opened, payload)
	}

	// And back the other way.
	reply := []byte("jumps over the lazy dog")
	sealedReply, err := server.SealInto(nil, reply, []byte(dataAAD))
	if err != nil {
		t.Fatal(err)
	}
	openedReply, err := client.OpenInto(nil, sealedReply, []byte(dataAAD))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(openedReply, reply) {
		t.Fatalf("opened %q, want %q", openedReply, reply)
	}
}

// TestTwoClientsCannotReadEachOther is the property the shared room key made
// impossible, and the reason several users can now share one room.
func TestTwoClientsCannotReadEachOther(t *testing.T) {
	private, public := keyPair(t)
	responder, err := NewResponder(private)
	if err != nil {
		t.Fatal(err)
	}

	seal := func() []byte {
		client, err := NewClientKeys(public)
		if err != nil {
			t.Fatal(err)
		}
		server := NewServerKeys(responder)
		w := newWire(t)
		ready := func() bool { return true }
		client.Bind([]byte(dataAAD), w.clientSend, ready)
		server.Bind([]byte(dataAAD), w.serverSend, ready)

		var sealed []byte
		done := make(chan struct{})
		go func() {
			defer close(done)
			sealed, _ = client.SealInto(nil, []byte("private to this session"), []byte(dataAAD))
		}()
		pump(t, client, server, w, func() bool {
			select {
			case <-done:
				return true
			default:
				return false
			}
		})
		<-done
		return sealed
	}

	first, second := seal(), seal()
	if bytes.Equal(first, second) {
		t.Fatal("two sessions produced identical records; the key is not per-session")
	}

	// Now give one session's server keys the other session's record. Both
	// negotiated against the same server, which is exactly the situation two
	// users in one room are in.
	other := NewServerKeys(responder)
	if _, err := other.OpenInto(nil, first, []byte(dataAAD)); err == nil {
		t.Fatal("a record from another session was readable")
	}
}

// TestServerAnswersOnTheChannelItWasAskedOn matters because on video-paced
// carriers the control channel comes up well before the data channel does; an
// answer on the wrong one would never arrive.
func TestServerAnswersOnTheChannelItWasAskedOn(t *testing.T) {
	private, public := keyPair(t)
	responder, err := NewResponder(private)
	if err != nil {
		t.Fatal(err)
	}
	server := NewServerKeys(responder)

	var dataSent, controlSent int
	server.Bind([]byte(dataAAD), func([]byte) error { dataSent++; return nil }, nil)
	server.Bind([]byte(controlAAD), func([]byte) error { controlSent++; return nil }, nil)

	_, frame, err := NewInitiator(public, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.OpenInto(nil, frame, []byte(controlAAD)); !errors.Is(err, errConsumed) {
		t.Fatalf("OpenInto = %v, want the frame consumed", err)
	}
	if controlSent != 1 || dataSent != 0 {
		t.Fatalf("answered on data=%d control=%d, want the control channel only", dataSent, controlSent)
	}
}

// TestReHandshakeReplacesTheKey covers a client reconnecting: the server has to
// accept a fresh exchange from a peer it already has a key for, and switch to
// the new one.
func TestReHandshakeReplacesTheKey(t *testing.T) {
	client, server, w := pair(t)

	seal := func() []byte {
		var sealed []byte
		done := make(chan struct{})
		go func() {
			defer close(done)
			sealed, _ = client.SealInto(nil, []byte("payload"), []byte(dataAAD))
		}()
		pump(t, client, server, w, func() bool {
			select {
			case <-done:
				return true
			default:
				return false
			}
		})
		<-done
		return sealed
	}

	first := seal()
	if _, err := server.OpenInto(nil, first, []byte(dataAAD)); err != nil {
		t.Fatal(err)
	}

	// The carrier is rebuilt; the client starts over.
	client.Reset()
	client.Bind([]byte(dataAAD), w.clientSend, func() bool { return true })
	second := seal()
	if _, err := server.OpenInto(nil, second, []byte(dataAAD)); err != nil {
		t.Fatalf("the server did not accept the peer's second exchange: %v", err)
	}
	if bytes.Equal(first, second) {
		t.Fatal("the second session reused the first session's key")
	}
}

// TestSealFailsWithoutAnAnswer checks that a client talking to a server that
// is not there gives up rather than blocking a connection forever.
func TestSealFailsWithoutAnAnswer(t *testing.T) {
	_, public := keyPair(t)
	client, err := NewClientKeys(public)
	if err != nil {
		t.Fatal(err)
	}
	client.Bind([]byte(dataAAD), func([]byte) error { return nil }, func() bool { return true })

	// Shrink the budget rather than waiting out the real one.
	client.mu.Lock()
	stop := client.stop
	client.mu.Unlock()
	go func() {
		time.Sleep(50 * time.Millisecond)
		close(stop)
	}()

	if _, err := client.SealInto(nil, []byte("x"), []byte(dataAAD)); !errors.Is(err, ErrNotNegotiated) {
		t.Fatalf("SealInto = %v, want ErrNotNegotiated", err)
	}
}

// TestServerRefusesToSpeakFirst pins down that the server never writes before a
// peer has said hello. A call that got that far would mean a bug elsewhere, and
// silently sealing with a key that does not exist is not an option.
func TestServerRefusesToSpeakFirst(t *testing.T) {
	private, _ := keyPair(t)
	responder, err := NewResponder(private)
	if err != nil {
		t.Fatal(err)
	}
	server := NewServerKeys(responder)
	if _, err := server.SealInto(nil, []byte("x"), []byte(dataAAD)); !errors.Is(err, ErrNotNegotiated) {
		t.Fatalf("SealInto = %v, want ErrNotNegotiated", err)
	}
	if server.Negotiated() {
		t.Fatal("a server that has seen nothing reports a negotiated session")
	}
}

// TestNoiseFromTheRoomIsDropped: a room is full of participants whose traffic
// is nothing to do with us, and none of it should look like an exchange or an
// error worth escalating.
func TestNoiseFromTheRoomIsDropped(t *testing.T) {
	private, _ := keyPair(t)
	responder, err := NewResponder(private)
	if err != nil {
		t.Fatal(err)
	}
	server := NewServerKeys(responder)

	for _, size := range []int{0, 1, 57, 64, 1500} {
		noise := bytes.Repeat([]byte{0xAB}, size)
		if _, err := server.OpenInto(nil, noise, []byte(dataAAD)); !errors.Is(err, errConsumed) {
			t.Errorf("noise of %d bytes: err = %v, want it quietly dropped", size, err)
		}
	}
}
