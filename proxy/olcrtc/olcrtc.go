// Package olcrtc integrates the olcRTC encrypted TCP-over-WebRTC tunnel
// (github.com/openlibrecommunity/olcrtc) into Xray as a proxy protocol.
//
// It provides:
//
//   - an outbound (ClientConfig / "olcrtc"): each outbound connection is
//     multiplexed as a stream over a shared WebRTC carrier to a room, and the
//     olcrtc server on the other side exits to the internet.
//   - an inbound (ServerConfig / "olcrtc"): a self-driven handler that joins the
//     same room, accepts tunnel streams and dispatches their targets through
//     Xray's router (so routing, DNS, sniffing and stats all apply).
//
// Traffic is disguised as an ordinary video call on an allowed SFU service
// (Yandex Telemost, WbStream) and additionally encrypted end-to-end. The
// server holds a long-term X25519 key pair and clients carry only its public
// half; every connection negotiates a session key of its own before anything
// else flows, so participants sharing a room cannot read each other.
//
// The library itself is vendored under olcrtclib and kept in step with its
// upstream by fork/bin/olcrtc; this package talks to it only through the
// public API under olcrtclib/pkg.
package olcrtc

import (
	"context"
	"time"

	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/errors"
	"github.com/xtls/xray-core/proxy"
	olclient "github.com/xtls/xray-core/proxy/olcrtc/olcrtclib/pkg/olcrtc/client"
	oltunnel "github.com/xtls/xray-core/proxy/olcrtc/olcrtclib/pkg/olcrtc/tunnel"
)

// Compile-time guarantees that the inbound Server satisfies the interfaces the
// handler manager and command service rely on: it is self-driven, exposes a
// (stub) proxy.Inbound so GetInbound works, and manages users.
var (
	_ proxy.SelfDrivenInbound = (*Server)(nil)
	_ proxy.Inbound           = (*Server)(nil)
	_ proxy.UserManager       = (*Server)(nil)
)

func init() {
	// Outbound (client).
	common.Must(common.RegisterConfig((*ClientConfig)(nil), func(ctx context.Context, config interface{}) (interface{}, error) {
		return NewClient(ctx, config.(*ClientConfig))
	}))
	// Inbound (server). The handler is self-driven: it dials out to the carrier
	// rather than listening on a socket, so mark its config type accordingly.
	common.Must(common.RegisterConfig((*ServerConfig)(nil), func(ctx context.Context, config interface{}) (interface{}, error) {
		return NewServer(ctx, config.(*ServerConfig))
	}))
	proxy.RegisterSelfDrivenInbound((*ServerConfig)(nil))
}

// clientConfig maps the outbound proto config onto the library's client config.
func clientConfig(c *ClientConfig) (olclient.Config, error) {
	liveness, err := liveness(c.GetLivenessInterval(), c.GetLivenessTimeout(), int(c.GetLivenessFailures()))
	if err != nil {
		return olclient.Config{}, err
	}
	return olclient.Config{
		Transport:        c.GetTransport(),
		Provider:         c.GetProvider(),
		RoomURL:          c.GetRoomId(),
		ServerPublicKey:  c.GetPublicKey(),
		DNSServer:        c.GetDnsServer(),
		ProviderToken:    c.GetAuthToken(),
		Engine:           c.GetEngine(),
		URL:              c.GetUrl(),
		Token:            c.GetToken(),
		TransportOptions: clientTransportOptions(c),
		Liveness:         olclient.LivenessConfig(liveness),
		DeviceID:         c.GetDeviceId(),
		DeviceIDPath:     c.GetDeviceIdPath(),
		Claims:           clientClaims(c),
	}, nil
}

// serverConfig maps the inbound proto config onto the library's server config.
func serverConfig(c *ServerConfig) (oltunnel.Config, error) {
	liveness, err := liveness(c.GetLivenessInterval(), c.GetLivenessTimeout(), int(c.GetLivenessFailures()))
	if err != nil {
		return oltunnel.Config{}, err
	}
	return oltunnel.Config{
		Transport:        c.GetTransport(),
		Provider:         c.GetProvider(),
		RoomURL:          c.GetRoomId(),
		PrivateKey:       c.GetPrivateKey(),
		DNSServer:        c.GetDnsServer(),
		ProviderToken:    c.GetAuthToken(),
		Engine:           c.GetEngine(),
		URL:              c.GetUrl(),
		Token:            c.GetToken(),
		TransportOptions: serverTransportOptions(c),
		Liveness:         liveness,
	}, nil
}

// liveness parses the control-stream tuning, which the JSON config carries as
// Go duration strings. Empty values leave the library's defaults in place.
func liveness(interval, timeout string, failures int) (oltunnel.LivenessConfig, error) {
	cfg := oltunnel.LivenessConfig{Failures: failures}
	var err error
	if cfg.Interval, err = optionalDuration(interval, "livenessInterval"); err != nil {
		return cfg, err
	}
	if cfg.Timeout, err = optionalDuration(timeout, "livenessTimeout"); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func optionalDuration(value, field string) (time.Duration, error) {
	if value == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(value)
	if err != nil {
		return 0, errors.New("olcrtc: ", field, ": ", value, " is not a duration").Base(err)
	}
	if d < 0 {
		return 0, errors.New("olcrtc: ", field, " must not be negative")
	}
	return d, nil
}

// Transport tuning is now one value rather than three, so only the options for
// the transport actually in use are built. An unrecognised transport yields nil
// and the library reports it, rather than this package having to know the list.
func clientTransportOptions(c *ClientConfig) olclient.TransportOptions {
	switch c.GetTransport() {
	case transportVP8:
		return olclient.VP8Options{FPS: int(c.GetVp8Fps()), BatchSize: int(c.GetVp8BatchSize())}
	case transportSEI:
		return olclient.SEIOptions{
			FPS:          int(c.GetSeiFps()),
			BatchSize:    int(c.GetSeiBatchSize()),
			FragmentSize: int(c.GetSeiFragmentSize()),
			AckTimeoutMS: int(c.GetSeiAckTimeoutMs()),
		}
	case transportVideo:
		return olclient.VideoOptions{
			Width:      int(c.GetVideoWidth()),
			Height:     int(c.GetVideoHeight()),
			FPS:        int(c.GetVideoFps()),
			QRSize:     int(c.GetVideoQrSize()),
			QRRecovery: c.GetVideoQrRecovery(),
			Codec:      c.GetVideoCodec(),
			TileModule: int(c.GetVideoTileModule()),
			TileRS:     int(c.GetVideoTileRs()),
		}
	default:
		return nil
	}
}

func serverTransportOptions(c *ServerConfig) oltunnel.TransportOptions {
	switch c.GetTransport() {
	case transportVP8:
		return oltunnel.VP8Options{FPS: int(c.GetVp8Fps()), BatchSize: int(c.GetVp8BatchSize())}
	case transportSEI:
		return oltunnel.SEIOptions{
			FPS:          int(c.GetSeiFps()),
			BatchSize:    int(c.GetSeiBatchSize()),
			FragmentSize: int(c.GetSeiFragmentSize()),
			AckTimeoutMS: int(c.GetSeiAckTimeoutMs()),
		}
	case transportVideo:
		return oltunnel.VideoOptions{
			Width:      int(c.GetVideoWidth()),
			Height:     int(c.GetVideoHeight()),
			FPS:        int(c.GetVideoFps()),
			QRSize:     int(c.GetVideoQrSize()),
			QRRecovery: c.GetVideoQrRecovery(),
			Codec:      c.GetVideoCodec(),
			TileModule: int(c.GetVideoTileModule()),
			TileRS:     int(c.GetVideoTileRs()),
		}
	default:
		return nil
	}
}

// clientClaims carries the client's credential to the server's auth hook.
//
// It travels in the handshake on the first smux stream, which is already
// encrypted with the session key the exchange produced — so unlike a
// credential placed in the opening frame, it stays secret even from someone
// who later obtains the server's private key.
func clientClaims(c *ClientConfig) map[string]any {
	if c.GetUuid() == "" {
		return nil
	}
	return map[string]any{claimUUID: c.GetUuid()}
}

// claimUUID is the handshake claim naming the subscription a client belongs
// to. The server resolves it against its user list.
const claimUUID = "uuid"

// Transport names, as they appear in the JSON config.
const (
	transportVP8   = "vp8channel"
	transportSEI   = "seichannel"
	transportVideo = "videochannel"
)
