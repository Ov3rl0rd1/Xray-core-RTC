package conf

import (
	"strings"

	tariffservice "github.com/xtls/xray-core/app/tariff/command"
	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/serial"
)

// This file holds every fork-local addition to the JSON config registry.
//
// Registering from here instead of editing the literals in xray.go keeps
// xray.go byte-identical to upstream, so merges from XTLS/Xray-core never
// conflict when upstream adds a protocol of its own. Add fork protocols here.

// portlessInbounds lists protocols whose inbound drives its own connection out
// to a carrier rather than listening on a socket, so InboundDetourConfig.Build
// must not require a port for them.
var portlessInbounds = map[string]bool{
	"olcrtc": true,
}

// isPortlessInbound reports whether the protocol runs without a listening port.
func isPortlessInbound(protocol string) bool {
	return portlessInbounds[strings.ToLower(protocol)]
}

func init() {
	common.Must(inboundConfigLoader.cache.RegisterCreator("olcrtc", func() interface{} { return new(OLCRTCServerConfig) }))
	common.Must(outboundConfigLoader.cache.RegisterCreator("olcrtc", func() interface{} { return new(OLCRTCClientConfig) }))
}

// forkAPIServices maps the fork's own API service names onto their configs.
//
// APIConfig.Build walks a hardcoded switch over upstream's services and
// silently ignores anything it does not recognise, so this is appended after
// that loop rather than added to it — one line inside xray.go's neighbour,
// with the names themselves living here where merges cannot reach them.
func forkAPIServices(names []string) []*serial.TypedMessage {
	var out []*serial.TypedMessage
	for _, name := range names {
		switch strings.ToLower(name) {
		case "tariffservice":
			out = append(out, serial.ToTypedMessage(&tariffservice.Config{}))
		}
	}
	return out
}
