// Package server implements the olcrtc tunnel server logic.
package server

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"

	"github.com/xtaci/smux"

	"github.com/xtls/xray-core/proxy/olcrtc/olcrtclib/internal/control"
	"github.com/xtls/xray-core/proxy/olcrtc/olcrtclib/internal/handshake"
	"github.com/xtls/xray-core/proxy/olcrtc/olcrtclib/internal/muxconn"
	"github.com/xtls/xray-core/proxy/olcrtc/olcrtclib/internal/runtime"
	"github.com/xtls/xray-core/proxy/olcrtc/olcrtclib/internal/transport"
	"github.com/xtls/xray-core/proxy/olcrtc/olcrtclib/internal/tunnelcore"
)

const connectCommand = "connect"

var (
	ErrKeyRequired         = runtime.ErrKeyRequired
	ErrKeySize             = runtime.ErrKeySize
	ErrSocks5AuthFailed    = errors.New("SOCKS5 auth failed")
	ErrSocks5ConnectFailed = errors.New("SOCKS5 connect failed")
	ErrInvalidTarget       = errors.New("invalid connect target")
)

// SessionOpenFunc is called after a successful handshake.
type SessionOpenFunc func(sessionID, deviceID string, claims map[string]any)

// SessionCloseFunc is called when a session is torn down.
type SessionCloseFunc func(sessionID, reason string)

// TrafficFunc is called once per tunnel stream after both copy loops finish.
type TrafficFunc func(sessionID, addr string, bytesIn, bytesOut uint64)

// HealthFunc is called when the server control health snapshot changes.
type HealthFunc func(control.Status)

// Server handles incoming tunnel connections and proxies their traffic.
type Server struct {
	baseCtx  context.Context //nolint:containedctx // server-lifetime context for reconnect goroutines
	ln       transport.Transport
	peerLn   transport.PeerTransport
	keys     muxconn.Keys
	keyStore *keyStore
	pair     *tunnelcore.SessionPair
	conn     *muxconn.Conn

	controlConn *muxconn.Conn
	session     *smux.Session
	controlSess *smux.Session
	controlStrm *smux.Stream
	controlStop context.CancelFunc
	sessMu      sync.RWMutex

	peerSessions map[string]*peerSession
	// peerLimitWarn rate-limits the peer-cap warning.
	peerLimitWarn atomic.Int64
	peersMu       sync.Mutex
	peerStats     map[string]peerStat
	reinstallMu   sync.Mutex
	wg            sync.WaitGroup
	authHook      handshake.AuthFunc
	onOpen        SessionOpenFunc
	onClose       SessionCloseFunc
	onTraffic     TrafficFunc
	dialHook      DialFunc
	deviceID      string
	sessionID     string

	dnsServer      string
	resolver       *net.Resolver
	socksProxyAddr string
	socksProxyPort int
	socksProxyUser string
	socksProxyPass string
	liveness       control.Config
	health         *runtime.HealthTracker
	state          stateGate
	done           chan struct{}
	doneOnce       sync.Once
}

// Config holds runtime configuration for [Run].
type Config struct {
	Transport string
	Provider  string
	RoomURL   string
	ChannelID string
	KeyHex    string
	// PrivateKey selects the fork's key exchange instead of a shared secret:
	// each peer negotiates a session key of its own against this key's public
	// half, so participants in one room can no longer read each other. Set it
	// and KeyHex is ignored. See fork_keys.go.
	PrivateKey       string
	DNSServer        string
	Resolver         *net.Resolver
	SOCKSProxyAddr   string
	SOCKSProxyPort   int
	SOCKSProxyUser   string
	SOCKSProxyPass   string
	TransportOptions transport.Options
	Engine           string
	URL              string
	Token            string
	ProviderToken    string
	Liveness         control.Config
	Traffic          transport.TrafficConfig
	AuthHook         handshake.AuthFunc
	// DialHook, when set, replaces the built-in egress dialer so a host
	// application can route the tunnel's exits through its own stack. See
	// fork_egress.go.
	DialHook       DialFunc
	OnSessionOpen  SessionOpenFunc
	OnSessionClose SessionCloseFunc
	OnTraffic      TrafficFunc
	OnHealth       HealthFunc
}

// Run starts the server with the given configuration.
func Run(ctx context.Context, cfg Config) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	keys, keyStore, err := serverKeys(cfg)
	if err != nil {
		return err
	}
	hook := cfg.AuthHook
	if hook == nil {
		hook = defaultAuthHook
	}
	onOpen := cfg.OnSessionOpen
	if onOpen == nil {
		onOpen = func(string, string, map[string]any) {}
	}
	onClose := cfg.OnSessionClose
	if onClose == nil {
		onClose = func(string, string) {}
	}
	onTraffic := cfg.OnTraffic
	if onTraffic == nil {
		onTraffic = func(string, string, uint64, uint64) {}
	}
	s := &Server{
		keys: keys, keyStore: keyStore, authHook: hook, onOpen: onOpen, onClose: onClose, onTraffic: onTraffic,
		dialHook:  cfg.DialHook,
		dnsServer: cfg.DNSServer, resolver: tunnelcore.Resolver(cfg.Resolver, cfg.DNSServer),
		socksProxyAddr: cfg.SOCKSProxyAddr, socksProxyPort: cfg.SOCKSProxyPort,
		socksProxyUser: cfg.SOCKSProxyUser, socksProxyPass: cfg.SOCKSProxyPass,
		liveness: cfg.Liveness, health: runtime.NewHealthTracker(cfg.OnHealth),
		peerSessions: make(map[string]*peerSession), peerStats: make(map[string]peerStat),
		done: make(chan struct{}),
	}
	defer func() {
		s.shutdown()
		s.wg.Wait()
	}()
	if err := s.bringUpLink(runCtx, cfg, cancel); err != nil {
		return err
	}
	go func() {
		<-runCtx.Done()
		s.closeSession()
	}()
	s.serve(runCtx)
	return nil
}
