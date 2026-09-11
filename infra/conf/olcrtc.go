package conf

import (
	"github.com/xtls/xray-core/common/errors"
	"github.com/xtls/xray-core/proxy/olcrtc"
	"google.golang.org/protobuf/proto"
)

// OLCRTCVP8Config tunes the vp8channel transport.
type OLCRTCVP8Config struct {
	FPS       int32 `json:"fps"`
	BatchSize int32 `json:"batchSize"`
}

// OLCRTCSEIConfig tunes the seichannel transport.
type OLCRTCSEIConfig struct {
	FPS          int32 `json:"fps"`
	BatchSize    int32 `json:"batchSize"`
	FragmentSize int32 `json:"fragmentSize"`
	AckTimeoutMs int32 `json:"ackTimeoutMs"`
}

// OLCRTCVideoConfig tunes the videochannel transport.
type OLCRTCVideoConfig struct {
	Width      int32  `json:"width"`
	Height     int32  `json:"height"`
	FPS        int32  `json:"fps"`
	Bitrate    string `json:"bitrate"`
	HW         string `json:"hw"`
	QRSize     int32  `json:"qrSize"`
	QRRecovery string `json:"qrRecovery"`
	Codec      string `json:"codec"`
	TileModule int32  `json:"tileModule"`
	TileRS     int32  `json:"tileRs"`
}

// olcrtcCommon holds the fields shared by the olcrtc inbound and outbound JSON
// configs.
type olcrtcCommon struct {
	Provider  string `json:"provider"`
	Transport string `json:"transport"`
	RoomID    string `json:"roomId"`
	DNSServer string `json:"dnsServer"`
	AuthToken string `json:"authToken"`

	Engine string `json:"engine"`
	URL    string `json:"url"`
	Token  string `json:"token"`

	VP8   *OLCRTCVP8Config   `json:"vp8"`
	SEI   *OLCRTCSEIConfig   `json:"sei"`
	Video *OLCRTCVideoConfig `json:"video"`

	LivenessInterval   string `json:"livenessInterval"`
	LivenessTimeout    string `json:"livenessTimeout"`
	LivenessFailures   int32  `json:"livenessFailures"`
	MaxSessionDuration string `json:"maxSessionDuration"`
}

// OLCRTCServerConfig is the JSON config for the olcrtc inbound (server).
type OLCRTCServerConfig struct {
	olcrtcCommon
	// PrivateKey is the server's long-term X25519 key, as produced by
	// `xray x25519`. Clients carry only its public half.
	PrivateKey string `json:"privateKey"`
}

// OLCRTCClientConfig is the JSON config for the olcrtc outbound (client).
type OLCRTCClientConfig struct {
	olcrtcCommon
	// PublicKey is the server's long-term X25519 public key. Holding it proves
	// nothing — it is not a secret — so a leaked client config does not let
	// anyone read anyone else's traffic, which a shared room key did.
	PublicKey string `json:"publicKey"`
	// UUID is the subscription this client belongs to, resolved against the
	// inbound's user list.
	UUID string `json:"uuid"`
	// DeviceID names the machine, so one subscription used from several
	// devices can be counted and capped. Generated and persisted at
	// DeviceIDPath when left empty.
	DeviceID     string `json:"deviceId"`
	DeviceIDPath string `json:"deviceIdPath"`
}

// supportedTransports are the carriers this deployment will accept.
//
// The vendored library implements two more, and they are left compiled in
// because excluding them saves 1.3 MB of a 50 MB binary and would cost a
// rewrite of upstream's validation tests — a bad trade for a fork that has to
// resync. What is worth having is this: neither of them can work here, and a
// config that selects one should say so at startup rather than fail somewhere
// inside a provider handshake.
//
//   - datachannel needs a WebRTC data channel, and neither provider offers
//     one: wbstream issues guest tokens with canPublishData=false, telemost
//     does not expose it at all.
//   - videochannel renders bytes as QR or tile images through ffmpeg, which
//     is not in the runtime image, and is upstream's slowest transport by its
//     own measurements.
//
// Add a name here to allow it; nothing else needs to change.
var supportedTransports = map[string]bool{
	"vp8channel": true,
	"seichannel": true,
}

func (c *olcrtcCommon) validate() error {
	if c.Provider == "" {
		return errors.New("olcrtc: provider is required (telemost, wbstream or none)")
	}
	if c.Transport == "" {
		return errors.New("olcrtc: transport is required (vp8channel or seichannel)")
	}
	if !supportedTransports[c.Transport] {
		return errors.New("olcrtc: transport ", c.Transport,
			" is not supported by this build (use vp8channel or seichannel)")
	}
	return nil
}

func (c *olcrtcCommon) vp8() (int32, int32) {
	if c.VP8 == nil {
		return 0, 0
	}
	return c.VP8.FPS, c.VP8.BatchSize
}

func (c *olcrtcCommon) sei() (int32, int32, int32, int32) {
	if c.SEI == nil {
		return 0, 0, 0, 0
	}
	return c.SEI.FPS, c.SEI.BatchSize, c.SEI.FragmentSize, c.SEI.AckTimeoutMs
}

func (c *olcrtcCommon) video() OLCRTCVideoConfig {
	if c.Video == nil {
		return OLCRTCVideoConfig{}
	}
	return *c.Video
}

// Build implements Buildable.
func (c *OLCRTCServerConfig) Build() (proto.Message, error) {
	if err := c.validate(); err != nil {
		return nil, err
	}
	vfps, vbatch := c.vp8()
	sfps, sbatch, sfrag, sack := c.sei()
	v := c.video()
	if c.PrivateKey == "" {
		return nil, errors.New("olcrtc: privateKey is required (generate one with `xray x25519`)")
	}
	return &olcrtc.ServerConfig{
		Provider:           c.Provider,
		Transport:          c.Transport,
		RoomId:             c.RoomID,
		PrivateKey:         c.PrivateKey,
		DnsServer:          c.DNSServer,
		AuthToken:          c.AuthToken,
		Engine:             c.Engine,
		Url:                c.URL,
		Token:              c.Token,
		Vp8Fps:             vfps,
		Vp8BatchSize:       vbatch,
		SeiFps:             sfps,
		SeiBatchSize:       sbatch,
		SeiFragmentSize:    sfrag,
		SeiAckTimeoutMs:    sack,
		VideoWidth:         v.Width,
		VideoHeight:        v.Height,
		VideoFps:           v.FPS,
		VideoBitrate:       v.Bitrate,
		VideoHw:            v.HW,
		VideoQrSize:        v.QRSize,
		VideoQrRecovery:    v.QRRecovery,
		VideoCodec:         v.Codec,
		VideoTileModule:    v.TileModule,
		VideoTileRs:        v.TileRS,
		LivenessInterval:   c.LivenessInterval,
		LivenessTimeout:    c.LivenessTimeout,
		LivenessFailures:   c.LivenessFailures,
		MaxSessionDuration: c.MaxSessionDuration,
	}, nil
}

// Build implements Buildable.
func (c *OLCRTCClientConfig) Build() (proto.Message, error) {
	if err := c.validate(); err != nil {
		return nil, err
	}
	vfps, vbatch := c.vp8()
	sfps, sbatch, sfrag, sack := c.sei()
	v := c.video()
	if c.PublicKey == "" {
		return nil, errors.New("olcrtc: publicKey is required (the server's public key)")
	}
	return &olcrtc.ClientConfig{
		Provider:           c.Provider,
		Transport:          c.Transport,
		RoomId:             c.RoomID,
		PublicKey:          c.PublicKey,
		DnsServer:          c.DNSServer,
		AuthToken:          c.AuthToken,
		Engine:             c.Engine,
		Url:                c.URL,
		Token:              c.Token,
		Vp8Fps:             vfps,
		Vp8BatchSize:       vbatch,
		SeiFps:             sfps,
		SeiBatchSize:       sbatch,
		SeiFragmentSize:    sfrag,
		SeiAckTimeoutMs:    sack,
		VideoWidth:         v.Width,
		VideoHeight:        v.Height,
		VideoFps:           v.FPS,
		VideoBitrate:       v.Bitrate,
		VideoHw:            v.HW,
		VideoQrSize:        v.QRSize,
		VideoQrRecovery:    v.QRRecovery,
		VideoCodec:         v.Codec,
		VideoTileModule:    v.TileModule,
		VideoTileRs:        v.TileRS,
		LivenessInterval:   c.LivenessInterval,
		LivenessTimeout:    c.LivenessTimeout,
		LivenessFailures:   c.LivenessFailures,
		MaxSessionDuration: c.MaxSessionDuration,
		DeviceId:           c.DeviceID,
		DeviceIdPath:       c.DeviceIDPath,
		Uuid:               c.UUID,
	}, nil
}
