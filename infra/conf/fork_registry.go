package conf

import (
	"strings"

	"github.com/xtls/xray-core/common"
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
