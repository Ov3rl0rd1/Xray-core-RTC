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
// It models the one transport rule the exchange has to live with, taken from
// vp8channel: a client discards everything on its data channel until it has
// confirmed which participant is the server, and it can only do that after the
// handshake — which needs the key. The control channel is delivered from the
// start. An exchange that answers on the data channel therefore never lands,
// which is the failure the first live test ran into.
type wire struct {
	t *testing.T

	mu        sync.Mutex
	toServer  []record
	toClient  []record
	confirmed bool // the client has latched onto the server

	sentData, sentControl int // client frames, by channel
	droppedAtClient       int
}

type record struct {
	control bool
	data    []byte
}

const (
	dataAAD    = "olcrtc/muxconn/v2/data"
	controlAAD = "olcrtc/muxconn/v2/control"
)

func aadOf(control bool) []byte {
	if control {
		return []byte(controlAAD)
	}
	return []byte(dataAAD)
}

func newWire(t *testing.T) *wire { return &wire{t: t} }

func (w *wire) clientSender(control bool) func([]byte) error {
	return func(b []byte) error {
		w.mu.Lock()
		defer w.mu.Unlock()
		if control {
			w.sentControl++
		} else {
			w.sentData++
		}
		w.toServer = append(w.toServer, record{control: control, data: bytes.Clone(b)})
		return nil
	}
}

func (w *wire) serverSender(control bool) func([]byte) error {
	return func(b []byte) error {
		w.mu.Lock()
		defer w.mu.Unlock()
		w.toClient = append(w.toClient, record{control: control, data: bytes.Clone(b)})
		return nil
	}
}

func (w *wire) confirm() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.confirmed = true
}

func (w *wire) take() (toServer, toClient []record) {
	w.mu.Lock()
	defer w.mu.Unlock()
	toServer, toClient = w.toServer, w.toClient
	w.toServer, w.toClient = nil, nil
	var delivered []record
	for _, r := range toClient {
		if !r.control && !w.confirmed {
			w.droppedAtClient++
			continue
		}
		delivered = append(delivered, r)
	}
	return toServer, delivered
}

func ready() bool { return true }

// bindClient binds both channels in the order the client's link does: data
// first, control second. That order is what made the first send go out on the
// data channel.
func (w *wire) bindClient(c *ClientKeys) {
	c.Bind(aadOf(false), false, w.clientSender(false), ready)
	c.Bind(aadOf(true), true, w.clientSender(true), ready)
}

func (w *wire) bindServer(s *ServerKeys) {
	s.Bind(aadOf(false), false, w.serverSender(false), ready)
	s.Bind(aadOf(true), true, w.serverSender(true), ready)
}

// pair builds a client and a server wired to each other on both channels.
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
	server := NewServerKeys(responder, "test")

	w := newWire(t)
	w.bindClient(client)
	w.bindServer(server)
	return client, server, w
}

// pump carries whatever is queued in both directions until done reports true
// or the deadline passes. It is the test's stand-in for the transport.
func pump(t *testing.T, client *ClientKeys, server *ServerKeys, w *wire, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("the exchange never completed (client sent data=%d control=%d, %d answers dropped at the client)",
				w.sentData, w.sentControl, w.droppedAtClient)
		}
		toServer, toClient := w.take()
		for _, r := range toServer {
			_, _ = server.OpenInto(nil, r.data, aadOf(r.control))
		}
		for _, r := range toClient {
			_, _ = client.OpenInto(nil, r.data, aadOf(r.control))
		}
		if len(toServer)+len(toClient) == 0 {
			time.Sleep(time.Millisecond)
		}
	}
}

// sealThroughExchange seals one record on the client, pumping the wire until
// the exchange lets it through.
func sealThroughExchange(t *testing.T, client *ClientKeys, server *ServerKeys, w *wire, payload []byte) []byte {
	t.Helper()
	var sealed []byte
	var sealErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		sealed, sealErr = client.SealInto(nil, payload, aadOf(true))
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
	return sealed
}

// TestSealAndOpenAcrossTheExchange is the whole feature in one test: a client
// with only the server's public key writes a record, and the server reads it —
// over a transport that will not deliver the data channel until afterwards.
func TestSealAndOpenAcrossTheExchange(t *testing.T) {
	client, server, w := pair(t)

	payload := []byte("the quick brown fox")
	sealed := sealThroughExchange(t, client, server, w, payload)

	opened, err := server.OpenInto(nil, sealed, aadOf(true))
	if err != nil {
		t.Fatalf("the server could not open the client's first record: %v", err)
	}
	if !bytes.Equal(opened, payload) {
		t.Fatalf("opened %q, want %q", opened, payload)
	}

	// The handshake is done; the client latches and the data channel opens.
	w.confirm()
	reply := []byte("jumps over the lazy dog")
	sealedReply, err := server.SealInto(nil, reply, aadOf(false))
	if err != nil {
		t.Fatal(err)
	}
	openedReply, err := client.OpenInto(nil, sealedReply, aadOf(false))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(openedReply, reply) {
		t.Fatalf("opened %q, want %q", openedReply, reply)
	}
}

// TestExchangeStaysOnTheControlChannel: the data channel was ready too, and was
// bound first. Using it is what deadlocked against a real SFU.
func TestExchangeStaysOnTheControlChannel(t *testing.T) {
	client, server, w := pair(t)
	sealThroughExchange(t, client, server, w, []byte("x"))

	if w.sentData != 0 {
		t.Fatalf("the client sent %d exchange frames on the data channel", w.sentData)
	}
	if w.sentControl == 0 {
		t.Fatal("the client never used the control channel")
	}
	if w.droppedAtClient != 0 {
		t.Fatalf("the server answered on the data channel %d times", w.droppedAtClient)
	}
}

// TestControlChannelIsWaitedFor: a control channel that is not ready yet is
// waited for, not bypassed through a data channel that is.
func TestControlChannelIsWaitedFor(t *testing.T) {
	private, public := keyPair(t)
	client, err := NewClientKeys(public)
	if err != nil {
		t.Fatal(err)
	}
	responder, err := NewResponder(private)
	if err != nil {
		t.Fatal(err)
	}
	server := NewServerKeys(responder, "")
	w := newWire(t)

	var controlUp sync.Mutex
	up := false
	controlReady := func() bool {
		controlUp.Lock()
		defer controlUp.Unlock()
		return up
	}
	client.Bind(aadOf(false), false, w.clientSender(false), ready)
	client.Bind(aadOf(true), true, w.clientSender(true), controlReady)
	w.bindServer(server)

	go func() {
		time.Sleep(150 * time.Millisecond)
		controlUp.Lock()
		up = true
		controlUp.Unlock()
	}()
	sealThroughExchange(t, client, server, w, []byte("x"))
	if w.sentData != 0 {
		t.Fatalf("the client fell back to the data channel %d times", w.sentData)
	}
}

// TestSingleChannelTransportUsesIt covers seichannel, which has no control
// channel and delivers its one channel from the start.
func TestSingleChannelTransportUsesIt(t *testing.T) {
	private, public := keyPair(t)
	client, err := NewClientKeys(public)
	if err != nil {
		t.Fatal(err)
	}
	responder, err := NewResponder(private)
	if err != nil {
		t.Fatal(err)
	}
	server := NewServerKeys(responder, "")
	w := newWire(t)
	w.confirm() // nothing gates a single-channel transport
	client.Bind(aadOf(false), false, w.clientSender(false), ready)
	server.Bind(aadOf(false), false, w.serverSender(false), ready)

	var sealed []byte
	done := make(chan struct{})
	go func() {
		defer close(done)
		sealed, _ = client.SealInto(nil, []byte("x"), aadOf(false))
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
	if _, err := server.OpenInto(nil, sealed, aadOf(false)); err != nil {
		t.Fatal(err)
	}
}

// TestRetransmissionsConvergeOnOneKey is the second half of what went wrong:
// answers that arrive after the client has already resent must not leave the
// two ends holding different keys.
func TestRetransmissionsConvergeOnOneKey(t *testing.T) {
	client, server, w := pair(t)

	// The client resends before anything comes back — a path slower than the
	// resend interval.
	for i := 0; i < 3; i++ {
		if _, err := client.sendInit(); err != nil {
			t.Fatal(err)
		}
	}
	toServer, _ := w.take()
	if len(toServer) != 3 {
		t.Fatalf("client sent %d frames, want 3", len(toServer))
	}
	for i := 1; i < len(toServer); i++ {
		if !bytes.Equal(toServer[i].data, toServer[0].data) {
			t.Fatal("a resend carried a different frame; every copy would rekey the server")
		}
	}
	for _, r := range toServer {
		if _, err := server.OpenInto(nil, r.data, aadOf(r.control)); !isConsumedErr(err) {
			t.Fatalf("server.OpenInto(copy) = %v, want the frame consumed", err)
		}
	}
	_, answers := w.take()
	if len(answers) != 3 {
		t.Fatalf("server sent %d answers, want one per copy", len(answers))
	}
	for i := 1; i < len(answers); i++ {
		if !bytes.Equal(answers[i].data, answers[0].data) {
			t.Fatal("the server answered a copy of the same frame differently")
		}
	}

	// Deliver them in reverse, as a reordering path might.
	for i := len(answers) - 1; i >= 0; i-- {
		_, _ = client.OpenInto(nil, answers[i].data, aadOf(answers[i].control))
	}
	sealed, err := client.SealInto(nil, []byte("after"), aadOf(true))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.OpenInto(nil, sealed, aadOf(true)); err != nil {
		t.Fatalf("client and server ended up with different keys: %v", err)
	}
}

// TestAReplayCannotCutAPeerOff: anyone in the room can copy a client's opening
// frame and send it again under that client's identity. It must not move the
// server off the key the client holds.
func TestAReplayCannotCutAPeerOff(t *testing.T) {
	client, server, w := pair(t)
	sealThroughExchange(t, client, server, w, []byte("first"))
	var firstFrame []byte
	client.mu.Lock()
	firstFrame = bytes.Clone(client.frame)
	client.mu.Unlock()

	// The client reconnects and negotiates again on the same server keys.
	client.Reset()
	w.bindClient(client)
	sealThroughExchange(t, client, server, w, []byte("second"))

	// Now the old frame is replayed.
	if _, err := server.OpenInto(nil, firstFrame, aadOf(true)); !errors.Is(err, ErrReplay) {
		t.Fatalf("replayed frame: err = %v, want ErrReplay", err)
	}
	sealed, err := client.SealInto(nil, []byte("still here"), aadOf(true))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.OpenInto(nil, sealed, aadOf(true)); err != nil {
		t.Fatalf("the replay moved the server off the client's key: %v", err)
	}

	// The same replay into another peer's keys is refused too: the memory
	// belongs to the server, not to one peer.
	other := NewServerKeys(server.responder, "other")
	if _, err := other.OpenInto(nil, firstFrame, aadOf(true)); !errors.Is(err, ErrReplay) {
		t.Fatalf("replay under another identity: err = %v, want ErrReplay", err)
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
		server := NewServerKeys(responder, "")
		w := newWire(t)
		w.bindClient(client)
		w.bindServer(server)
		return sealThroughExchange(t, client, server, w, []byte("private to this session"))
	}

	first, second := seal(), seal()
	if bytes.Equal(first, second) {
		t.Fatal("two sessions produced identical records; the key is not per-session")
	}

	// Now give one session's server keys the other session's record. Both
	// negotiated against the same server, which is exactly the situation two
	// users in one room are in.
	other := NewServerKeys(responder, "")
	if _, err := other.OpenInto(nil, first, aadOf(true)); err == nil {
		t.Fatal("a record from another session was readable")
	}
}

// TestServerAnswersOnTheChannelItWasAskedOn: the client asks only where it can
// hear the answer.
func TestServerAnswersOnTheChannelItWasAskedOn(t *testing.T) {
	private, public := keyPair(t)
	responder, err := NewResponder(private)
	if err != nil {
		t.Fatal(err)
	}
	server := NewServerKeys(responder, "")

	var dataSent, controlSent int
	server.Bind(aadOf(false), false, func([]byte) error { dataSent++; return nil }, nil)
	server.Bind(aadOf(true), true, func([]byte) error { controlSent++; return nil }, nil)

	_, frame, err := NewInitiator(public, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.OpenInto(nil, frame, aadOf(true)); !isConsumedErr(err) {
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

	first := sealThroughExchange(t, client, server, w, []byte("payload"))
	if _, err := server.OpenInto(nil, first, aadOf(true)); err != nil {
		t.Fatal(err)
	}

	// The carrier is rebuilt; the client starts over.
	client.Reset()
	w.bindClient(client)
	second := sealThroughExchange(t, client, server, w, []byte("payload"))
	if _, err := server.OpenInto(nil, second, aadOf(true)); err != nil {
		t.Fatalf("the server did not accept the peer's second exchange: %v", err)
	}
	if bytes.Equal(first, second) {
		t.Fatal("the second session reused the first session's key")
	}
}

// TestALateAnswerAfterResetIsIgnored: an answer to the attempt a Reset threw
// away must not be installed as the new attempt's key.
func TestALateAnswerAfterResetIsIgnored(t *testing.T) {
	client, server, w := pair(t)
	if _, err := client.sendInit(); err != nil {
		t.Fatal(err)
	}
	toServer, _ := w.take()
	for _, r := range toServer {
		_, _ = server.OpenInto(nil, r.data, aadOf(r.control))
	}
	_, answers := w.take()
	if len(answers) != 1 {
		t.Fatalf("got %d answers, want 1", len(answers))
	}

	client.Reset()
	w.bindClient(client)
	if _, err := client.sendInit(); err != nil {
		t.Fatal(err)
	}
	if _, err := client.OpenInto(nil, answers[0].data, aadOf(true)); err == nil || isConsumedErr(err) {
		t.Fatalf("stale answer: err = %v, want it refused as a record that does not open", err)
	}
	client.mu.Lock()
	negotiated := client.keys != nil
	client.mu.Unlock()
	if negotiated {
		t.Fatal("the answer to a discarded attempt was installed")
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
	client.Bind(aadOf(true), true, func([]byte) error { return nil }, ready)

	// Shrink the budget rather than waiting out the real one.
	client.mu.Lock()
	stop := client.stop
	client.mu.Unlock()
	go func() {
		time.Sleep(50 * time.Millisecond)
		close(stop)
	}()

	if _, err := client.SealInto(nil, []byte("x"), aadOf(true)); !errors.Is(err, ErrNotNegotiated) {
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
	server := NewServerKeys(responder, "")
	if _, err := server.SealInto(nil, []byte("x"), aadOf(false)); !errors.Is(err, ErrNotNegotiated) {
		t.Fatalf("SealInto = %v, want ErrNotNegotiated", err)
	}
	if server.Negotiated() {
		t.Fatal("a server that has seen nothing reports a negotiated session")
	}
}

// TestNoiseIsRefusedNotConsumed: noise must neither negotiate anything nor be
// hidden from muxconn's failure accounting — that count is how an operator
// finds a client configured with the wrong key.
func TestNoiseIsRefusedNotConsumed(t *testing.T) {
	private, _ := keyPair(t)
	responder, err := NewResponder(private)
	if err != nil {
		t.Fatal(err)
	}
	server := NewServerKeys(responder, "")

	for _, size := range []int{0, 1, InitSize, 64, 1500} {
		noise := bytes.Repeat([]byte{0xAB}, size)
		_, err := server.OpenInto(nil, noise, aadOf(true))
		if err == nil || isConsumedErr(err) {
			t.Errorf("noise of %d bytes: err = %v, want a failure muxconn counts", size, err)
		}
	}
	if server.Negotiated() {
		t.Fatal("noise negotiated a session")
	}
}

// TestAWrongKeySaysSo: a client configured with some other server's public key
// produces frames this server cannot open, and the error names that.
func TestAWrongKeySaysSo(t *testing.T) {
	private, _ := keyPair(t)
	_, otherPublic := keyPair(t)
	responder, err := NewResponder(private)
	if err != nil {
		t.Fatal(err)
	}
	server := NewServerKeys(responder, "")
	_, frame, err := NewInitiator(otherPublic, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.OpenInto(nil, frame, aadOf(true)); !errors.Is(err, ErrBadFrame) {
		t.Fatalf("frame for another server: err = %v, want ErrBadFrame", err)
	}
}

// isConsumedErr checks an error the way muxconn does.
func isConsumedErr(err error) bool {
	var consumed interface{ ConsumedRecord() bool }
	return errors.As(err, &consumed) && consumed.ConsumedRecord()
}
