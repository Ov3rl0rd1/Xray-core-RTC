package vp8channel

// This file belongs to the fork, not to olcrtc upstream. It is listed in
// fork/olcrtc-manifest.txt so a resync keeps it.
//
// It runs the real server and the real client over real vp8channel transports
// with only the SFU simulated, because that is the level at which the key
// exchange first failed: every unit test of the exchange passed, and the tunnel
// still deadlocked, since this transport discards a client's data channel until
// the handshake has latched the server — a rule no fake transport modelled.

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xtls/xray-core/proxy/olcrtc/olcrtclib/internal/client"
	"github.com/xtls/xray-core/proxy/olcrtc/olcrtclib/internal/engine"
	enginebuiltin "github.com/xtls/xray-core/proxy/olcrtc/olcrtclib/internal/engine/builtin"
	"github.com/xtls/xray-core/proxy/olcrtc/olcrtclib/internal/server"
	"github.com/xtls/xray-core/proxy/olcrtc/olcrtclib/internal/transport"
)

// simRoom forwards every sample a participant writes to every other
// participant, which is what an SFU does with a published video track. Delivery
// is lossless and in order; KCP's own retransmission is not under test here.
type simRoom struct {
	mu      sync.Mutex
	members []*simMember
}

type simMember struct {
	tr    *streamTransport
	inbox chan []byte
	done  chan struct{}
}

func (r *simRoom) join(tr *streamTransport) {
	m := &simMember{tr: tr, inbox: make(chan []byte, 8192), done: make(chan struct{})}
	tr.sampleWriter = func(sample []byte) bool {
		r.broadcast(m, sample)
		return true
	}
	r.mu.Lock()
	r.members = append(r.members, m)
	r.mu.Unlock()
	go func() {
		for {
			select {
			case frame := <-m.inbox:
				tr.handleIncomingFrame(frame)
			case <-m.done:
				return
			}
		}
	}()
}

func (r *simRoom) broadcast(from *simMember, sample []byte) {
	r.mu.Lock()
	members := append([]*simMember(nil), r.members...)
	r.mu.Unlock()
	for _, m := range members {
		if m == from {
			continue
		}
		select {
		case m.inbox <- bytes.Clone(sample):
		default: // a receiver that cannot keep up loses frames, as it would at an SFU
		}
	}
}

func (r *simRoom) size() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.members)
}

func (r *simRoom) close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, m := range r.members {
		close(m.done)
	}
	r.members = nil
}

// registerSimRoom makes a transport name and a provider name that, used
// together, put every link created with them into one simulated room.
func registerSimRoom(t *testing.T) (transportName, providerName string, room *simRoom) {
	t.Helper()
	room = &simRoom{}
	t.Cleanup(room.close)

	suffix := strings.NewReplacer("/", "-", " ", "-").Replace(t.Name())
	transportName = "vp8sim-" + suffix
	providerName = "vp8sim-provider-" + suffix

	enginebuiltin.Register(providerName, func(context.Context, enginebuiltin.Config) (engine.Session, error) {
		return &fakeEngineSession{stream: &fakeVideoStream{canSend: true}}, nil
	})
	transport.Register(transportName, func(ctx context.Context, cfg transport.Config) (transport.Transport, error) {
		tr, err := New(ctx, cfg)
		if err != nil {
			return nil, err
		}
		st, ok := tr.(*streamTransport)
		if !ok {
			return nil, fmt.Errorf("vp8channel.New returned %T", tr)
		}
		room.join(st)
		return tr, nil
	})
	return transportName, providerName, room
}

func simKeyPair(t *testing.T) (private, public string) {
	t.Helper()
	key, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	enc := base64.RawURLEncoding
	return enc.EncodeToString(key.Bytes()), enc.EncodeToString(key.PublicKey().Bytes())
}

// echoEgress stands in for the internet: every tunnelled connection is
// answered by whoever wrote into it.
func echoEgress(context.Context, string, int, string) (net.Conn, error) {
	near, far := net.Pipe()
	go func() {
		defer far.Close()
		_, _ = io.Copy(far, far)
	}()
	return near, nil
}

// startSimServer runs a server in the room and waits for it to join.
func startSimServer(t *testing.T, ctx context.Context, transportName, providerName string, room *simRoom, private string) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		done <- server.Run(ctx, server.Config{
			Transport:  transportName,
			Provider:   providerName,
			RoomURL:    "sim-room",
			ChannelID:  "sim",
			PrivateKey: private,
			DialHook:   echoEgress,
		})
	}()
	t.Cleanup(func() {
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("server did not stop")
		}
	})

	deadline := time.Now().Add(5 * time.Second)
	for room.size() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("server never joined the room")
		}
		select {
		case err := <-done:
			t.Fatalf("server stopped before joining: %v", err)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func startSimClient(t *testing.T, ctx context.Context, transportName, providerName, public, device string) *client.Tunnel {
	t.Helper()
	tun, err := client.StartTunnel(ctx, client.Config{
		Transport:       transportName,
		Provider:        providerName,
		RoomURL:         "sim-room",
		ChannelID:       "sim",
		ServerPublicKey: public,
		DeviceID:        device,
	})
	if err != nil {
		t.Fatalf("client %s: %v", device, err)
	}
	t.Cleanup(func() { _ = tun.Close() })
	return tun
}

func roundTrip(ctx context.Context, tun *client.Tunnel, payload []byte) error {
	conn, err := tun.DialContext(ctx, "example.com", 443)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if _, err := conn.Write(payload); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, got); err != nil {
		return fmt.Errorf("read: %w", err)
	}
	if !bytes.Equal(got, payload) {
		return fmt.Errorf("echo mismatch: got %d bytes that differ from the %d sent", len(got), len(payload))
	}
	return nil
}

// TestTunnelNegotiatesItsKeyOverVP8 is the live failure, reproduced: a client
// holding only the server's public key brings up a tunnel and carries data.
func TestTunnelNegotiatesItsKeyOverVP8(t *testing.T) {
	transportName, providerName, room := registerSimRoom(t)
	private, public := simKeyPair(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	serverCtx, stopServer := context.WithCancel(ctx)
	t.Cleanup(stopServer)

	startSimServer(t, serverCtx, transportName, providerName, room, private)
	tun := startSimClient(t, ctx, transportName, providerName, public, "alice-laptop")

	// Enough to span many KCP segments and several smux frames each way.
	payload := bytes.Repeat([]byte("olcrtc key exchange "), 16*1024)
	opCtx, opCancel := context.WithTimeout(ctx, 30*time.Second)
	defer opCancel()
	if err := roundTrip(opCtx, tun, payload); err != nil {
		t.Fatal(err)
	}
}

// TestTwoClientsShareOneRoom is what the per-connection key is for: two users
// in one room, each with a key of its own, both served.
func TestTwoClientsShareOneRoom(t *testing.T) {
	transportName, providerName, room := registerSimRoom(t)
	private, public := simKeyPair(t)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	serverCtx, stopServer := context.WithCancel(ctx)
	t.Cleanup(stopServer)

	startSimServer(t, serverCtx, transportName, providerName, room, private)

	devices := []string{"alice-laptop", "bob-phone"}
	tunnels := make([]*client.Tunnel, len(devices))
	var wg sync.WaitGroup
	for i, device := range devices {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tun, err := client.StartTunnel(ctx, client.Config{
				Transport:       transportName,
				Provider:        providerName,
				RoomURL:         "sim-room",
				ChannelID:       "sim",
				ServerPublicKey: public,
				DeviceID:        device,
			})
			if err != nil {
				t.Errorf("client %s: %v", device, err)
				return
			}
			tunnels[i] = tun
		}()
	}
	wg.Wait()
	for _, tun := range tunnels {
		if tun != nil {
			t.Cleanup(func() { _ = tun.Close() })
		}
	}
	if t.Failed() {
		return
	}

	opCtx, opCancel := context.WithTimeout(ctx, 30*time.Second)
	defer opCancel()
	errs := make([]error, len(tunnels))
	for i, tun := range tunnels {
		wg.Add(1)
		go func() {
			defer wg.Done()
			payload := bytes.Repeat([]byte(devices[i]+" "), 32*1024)
			errs[i] = roundTrip(opCtx, tun, payload)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("%s: %v", devices[i], err)
		}
	}
}

// TestAWrongPublicKeyFailsCleanly: a client configured for some other server
// must fail to connect, not hang and not get through.
func TestAWrongPublicKeyFailsCleanly(t *testing.T) {
	transportName, providerName, room := registerSimRoom(t)
	private, _ := simKeyPair(t)
	_, wrongPublic := simKeyPair(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	serverCtx, stopServer := context.WithCancel(ctx)
	t.Cleanup(stopServer)

	startSimServer(t, serverCtx, transportName, providerName, room, private)

	start := time.Now()
	tun, err := client.StartTunnel(ctx, client.Config{
		Transport:       transportName,
		Provider:        providerName,
		RoomURL:         "sim-room",
		ChannelID:       "sim",
		ServerPublicKey: wrongPublic,
		DeviceID:        "mallory",
	})
	if err == nil {
		_ = tun.Close()
		t.Fatal("a client with the wrong server key brought a tunnel up")
	}
	// The error is what an operator sees, so it has to point at the key rather
	// than at a stream timeout two layers up.
	if !strings.Contains(err.Error(), "publicKey") {
		t.Fatalf("error does not point at the key: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 35*time.Second {
		t.Fatalf("failing took %s", elapsed)
	}
}
