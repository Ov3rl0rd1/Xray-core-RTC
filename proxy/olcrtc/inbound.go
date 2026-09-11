package olcrtc

import (
	"context"
	"encoding/base64"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/xtls/xray-core/app/dispatcher"
	"github.com/xtls/xray-core/app/tariff"
	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/errors"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/net/cnc"
	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/common/session"
	"github.com/xtls/xray-core/common/uuid"
	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/features/routing"
	oltunnel "github.com/xtls/xray-core/proxy/olcrtc/olcrtclib/pkg/olcrtc/tunnel"
	"github.com/xtls/xray-core/transport"
	"github.com/xtls/xray-core/transport/internet/stat"
)

// Server is the olcrtc inbound handler. It is self-driven: rather than binding a
// socket, it joins the configured room, accepts tunnel streams, and dispatches
// each CONNECT target through Xray's router. It implements proxy.SelfDrivenInbound.
//
// It also implements proxy.UserManager (backed by [Validator]) so users can be
// added/removed at runtime via HandlerService (AlterInbound), and proxy.Inbound
// (with stub methods) so the command service can reach that UserManager.
type Server struct {
	config     *ServerConfig
	tag        string
	dispatcher routing.Dispatcher
	validator  *Validator

	// rooms decides which carrier room each run uses; see rooms.go.
	rooms *roomPool
	// roomUp is whether the current run has seen a client, so a room is
	// reported up once rather than on every connection.
	roomUp atomic.Bool
}

// NewServer creates an olcrtc inbound handler from config. It captures the
// inbound tag from ctx (set by the handler manager) and resolves the router.
func NewServer(ctx context.Context, config *ServerConfig) (*Server, error) {
	cooldown, err := optionalDuration(config.GetRoomCooldown(), "roomCooldown")
	if err != nil {
		return nil, err
	}
	s := &Server{
		config:    config,
		validator: NewValidator(),
		rooms:     newRoomPool(config.GetRoomId(), config.GetFallbackRooms(), cooldown),
	}
	if inbound := session.InboundFromContext(ctx); inbound != nil {
		s.tag = inbound.Tag
	}
	if err := core.RequireFeatures(ctx, func(d routing.Dispatcher) error {
		s.dispatcher = d
		return nil
	}); err != nil {
		return nil, err
	}
	return s, nil
}

// Rooms reports what the inbound has learned about its carrier rooms, for
// diagnostics.
func (s *Server) Rooms() []roomState { return s.rooms.snapshot() }

// Serve brings up the server carrier and blocks until ctx is cancelled. Each
// accepted tunnel stream is authenticated by authHook and its target dispatched
// through Xray's router; the resulting link is piped against the stream.
//
// A maxSessionDuration is applied by bounding this call's context. Returning
// hands control back to SelfDrivenInboundHandler, whose restart loop brings a
// fresh carrier up — which is exactly what a planned rebuild is, and needs no
// rotation machinery of its own.
func (s *Server) Serve(ctx context.Context) error {
	cfg, err := serverConfig(s.config)
	if err != nil {
		return err
	}
	cfg.AuthHook = s.authHook

	maxSession, err := optionalDuration(s.config.GetMaxSessionDuration(), "maxSessionDuration")
	if err != nil {
		return err
	}
	if maxSession > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, maxSession)
		defer cancel()
	}

	// Which room this attempt uses. The handler calls Serve again when it
	// returns, so working down the list is a matter of answering differently
	// each time rather than looping here.
	room, switched := s.rooms.next()
	if room != "" {
		cfg.RoomURL = room
	}
	if switched {
		errors.LogWarning(ctx, "olcrtc: switching to room ", room)
		tariff.PublishRoom(tariff.EventKind_EVENT_ROOM_SWITCHED, s.tag, room, "")
	}

	// A session opening means the room carried a client end to end, which is
	// the only evidence that actually settles whether a room works.
	cfg.OnSessionOpen = func(string, string, map[string]any) { s.roomWorked(ctx, room) }

	dial := func(dctx context.Context, addr string, port int, sessionID string) (net.Conn, error) {
		return s.dispatch(dctx, addr, port, sessionID)
	}

	started := time.Now()
	runErr := oltunnel.NewWithDial(cfg, dial).Run(ctx)
	s.roomEnded(ctx, room, time.Since(started), runErr)
	if runErr != nil {
		return errors.New("olcrtc inbound ended").Base(runErr)
	}
	return nil
}

// roomWorked records, once per run, that this room carried a client.
func (s *Server) roomWorked(ctx context.Context, room string) {
	if room == "" || !s.roomUp.CompareAndSwap(false, true) {
		return
	}
	s.rooms.succeeded(room)
	errors.LogInfo(ctx, "olcrtc: room ", room, " is carrying clients")
	tariff.PublishRoom(tariff.EventKind_EVENT_ROOM_UP, s.tag, room, "")
}

// roomEnded records how a run finished, which is what decides whether the next
// one uses the same room.
//
// Cancellation is not the room's fault: a planned rebuild or a shutdown says
// nothing about whether the room works, and counting it would rotate away from
// a perfectly good one every maxSessionDuration.
func (s *Server) roomEnded(ctx context.Context, room string, ranFor time.Duration, err error) {
	wasUp := s.roomUp.Swap(false)
	if room == "" || ctx.Err() != nil {
		return
	}
	s.rooms.failed(room, ranFor, err)
	if !wasUp {
		detail := "carrier did not hold"
		if err != nil {
			detail = err.Error()
		}
		errors.LogWarning(ctx, "olcrtc: room ", room, " failed after ", ranFor.Round(time.Second), ": ", detail)
		tariff.PublishRoom(tariff.EventKind_EVENT_ROOM_DOWN, s.tag, room, detail)
	}
}

// authHook authorises a client handshake.
//
// Two identities arrive, and they mean different things. The claim named
// "uuid" is the *credential*: it says which subscription this is, and the
// inbound resolves it against its user list. deviceID is the *machine*: it
// says which of that subscription's devices is calling, and is what device
// caps and the fair split between devices are counted over.
//
// Both travel inside the handshake on the first smux stream, which by then is
// already encrypted with the session key the exchange produced — so neither is
// recoverable from recorded traffic, even by someone who later obtains the
// server's private key.
//
// With no users registered the inbound is open: anyone who completed the key
// exchange gets in, which is the right behaviour for a server whose panel has
// not provisioned it yet. Once a single user exists, the list is enforced.
func (s *Server) authHook(deviceID string, claims map[string]any) (string, error) {
	if s.validator.Empty() {
		return encodeSessionID("", 0, deviceID), nil
	}
	credential, _ := claims[claimUUID].(string)
	u := s.validator.Get(credential)
	if u == nil {
		// The reason reaches the client verbatim; keep it uninformative.
		return "", errors.New("unauthorized")
	}
	return encodeSessionID(u.Email, u.Level, deviceID), nil
}

// dispatch routes a single tunnel target through Xray and returns a net.Conn
// whose Read side is the target's downlink and whose Write side is its uplink.
// The authenticated user (decoded from sessionID) is attached to the inbound
// session so routing, per-user stats, online tracking and speed limits apply.
func (s *Server) dispatch(ctx context.Context, addr string, port int, sessionID string) (net.Conn, error) {
	dest := net.TCPDestination(net.ParseAddress(addr), net.Port(port))

	inb := &session.Inbound{
		Tag:    s.tag,
		Source: net.TCPDestination(net.AnyIP, 0),
	}
	email, level, device := decodeSessionID(sessionID)
	if email != "" {
		// Attaching the user makes the dispatcher key per-user traffic stats,
		// online tracking, quotas and the speed limit by email — across every
		// device and every protocol.
		inb.User = &protocol.MemoryUser{Email: email, Level: level}
	}
	ctx = session.ContextWithInbound(ctx, inb)

	content := new(session.Content)
	if device != "" {
		// olcRTC connections carry no source address, so without this every one
		// of a user's devices would look like the same device to the fair split
		// and to device caps. The handshake knows which machine this is, so it
		// says so rather than leaving it to be guessed.
		content.SetAttribute(dispatcher.DeviceAttribute, device)
	}
	ctx = session.ContextWithContent(ctx, content)

	errors.LogInfo(ctx, "olcrtc: dispatching tunnel target ", dest)
	link, err := s.dispatcher.Dispatch(ctx, dest)
	if err != nil {
		return nil, errors.New("olcrtc: dispatch ", dest, " failed").Base(err)
	}

	return cnc.NewConnection(
		cnc.ConnectionInputMulti(link.Writer),
		cnc.ConnectionOutputMulti(link.Reader),
		cnc.ConnectionOnClose(&linkCloser{link: link}),
	), nil
}

// --- proxy.Inbound (stub) --------------------------------------------------
//
// olcrtc has no socket listener, so these satisfy proxy.Inbound only so the
// command service (AlterInbound) can reach the proxy.UserManager below via
// SelfDrivenInboundHandler.GetInbound(). Process is never invoked.

// Network implements proxy.Inbound.
func (s *Server) Network() []net.Network { return []net.Network{net.Network_TCP} }

// Process implements proxy.Inbound. It is never called for a self-driven inbound.
func (s *Server) Process(context.Context, net.Network, stat.Connection, routing.Dispatcher) error {
	return errors.New("olcrtc inbound is self-driven and has no socket listener")
}

// --- proxy.UserManager -----------------------------------------------------

// AddUser implements proxy.UserManager.
func (s *Server) AddUser(_ context.Context, u *protocol.MemoryUser) error {
	return s.validator.Add(u)
}

// RemoveUser implements proxy.UserManager.
func (s *Server) RemoveUser(_ context.Context, email string) error {
	return s.validator.Del(email)
}

// GetUser implements proxy.UserManager.
func (s *Server) GetUser(_ context.Context, email string) *protocol.MemoryUser {
	return s.validator.GetByEmail(email)
}

// GetUsers implements proxy.UserManager.
func (s *Server) GetUsers(_ context.Context) []*protocol.MemoryUser {
	return s.validator.GetAll()
}

// GetUsersCount implements proxy.UserManager.
func (s *Server) GetUsersCount(_ context.Context) int64 {
	return s.validator.GetCount()
}

// --- sessionID codec -------------------------------------------------------
//
// The vendored server hands the AuthHook-returned sessionID to the dial hook
// for every tunnel stream of a connection, so it doubles as a stateless
// carrier for who that connection belongs to:
//
//	u2:<base64url(email)>:<level>:<base64url(device)>:<random>
//
// The trailing random keeps each connection's sessionID unique, which the
// library relies on to tell peers apart; the prefix lets dispatch recover the
// identity with no shared map and no lock on the connection path.

const sessionUserPrefix = "u2:"

func encodeSessionID(email string, level uint32, device string) string {
	id := uuid.New()
	if email == "" && device == "" {
		return "anon:" + id.String()
	}
	return sessionUserPrefix +
		base64.RawURLEncoding.EncodeToString([]byte(email)) + ":" +
		strconv.FormatUint(uint64(level), 10) + ":" +
		base64.RawURLEncoding.EncodeToString([]byte(device)) + ":" + id.String()
}

func decodeSessionID(sid string) (email string, level uint32, device string) {
	if !strings.HasPrefix(sid, sessionUserPrefix) {
		return "", 0, ""
	}
	parts := strings.SplitN(strings.TrimPrefix(sid, sessionUserPrefix), ":", 4)
	if len(parts) != 4 {
		return "", 0, ""
	}
	rawEmail, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", 0, ""
	}
	rawDevice, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return "", 0, ""
	}
	lvl, _ := strconv.ParseUint(parts[1], 10, 32)
	return string(rawEmail), uint32(lvl), string(rawDevice)
}

// linkCloser tears down a dispatched link when the tunnel conn is closed.
type linkCloser struct {
	link *transport.Link
}

func (l *linkCloser) Close() error {
	common.Interrupt(l.link.Reader)
	common.Close(l.link.Writer)
	return nil
}
