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
// (Yandex Telemost, WbStream) and additionally encrypted with a shared
// XChaCha20-Poly1305 key.
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
		KeyHex:           c.GetKey(),
		DNSServer:        c.GetDnsServer(),
		ProviderToken:    c.GetAuthToken(),
		Engine:           c.GetEngine(),
		URL:              c.GetUrl(),
		Token:            c.GetToken(),
		TransportOptions: clientTransportOptions(c),
		Liveness:         olclient.LivenessConfig(liveness),
		DeviceID:         c.GetDeviceId(),
		DeviceIDPath:     c.GetDeviceIdPath(),
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
		KeyHex:           c.GetKey(),
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

// Transport names, as they appear in the JSON config.
const (
	transportVP8   = "vp8channel"
	transportSEI   = "seichannel"
	transportVideo = "videochannel"
)
