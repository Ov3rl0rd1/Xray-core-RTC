package all

// Fork-local distro registrations.
//
// Keeping these out of all.go leaves that file byte-identical to upstream, so
// merges from XTLS/Xray-core never conflict on the import block. Register fork
// proxies, transports and commands here.

import (
	// Proxies
	_ "github.com/xtls/xray-core/proxy/olcrtc"
)
