package all

// Fork-local distro registrations.
//
// Keeping these out of all.go leaves that file byte-identical to upstream, so
// merges from XTLS/Xray-core never conflict on the import block. Register fork
// proxies, transports and commands here.

import (
	// Proxies
	_ "github.com/xtls/xray-core/proxy/olcrtc"

	// The per-user policy and usage store's management API. Listing
	// "TariffService" in api.services is what creates the store and attaches
	// it to the dispatcher, so a server that does not ask for the API keeps
	// the plain level-based plans and carries none of this.
	_ "github.com/xtls/xray-core/app/tariff/command"
)
