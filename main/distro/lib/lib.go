// Package lib is the feature set linked into the c-shared library build in
// libxray/. It is main/distro/all minus the parts a client app has no use for:
// the CLI command tree (main/commands/all, which drags in the gRPC API client,
// certificate tooling and the whole cobra tree), the TOML and YAML config
// formats, and the loader that fetches config over http.
//
// Everything else registers itself as a side effect of infra/conf: the JSON
// config parser imports every proxy and transport package for its config
// types, and importing a package runs its init(), which is where handlers
// register. So the protocol set here is not narrower than distro/all's, and
// listing fewer proxies would not make it so -- a build that accepts JSON
// config necessarily links all of them. Cutting protocols would mean dropping
// JSON config support and having the caller pass a serialized protobuf
// core.Config instead.
//
// What is left below is the handful of registrations infra/conf does not
// already pull in.
package lib

import (
	// The following are necessary as they register handlers in their init functions.

	// Mandatory features. Can't remove unless there are replacements.
	_ "github.com/xtls/xray-core/app/dispatcher"
	_ "github.com/xtls/xray-core/app/proxyman/inbound"
	_ "github.com/xtls/xray-core/app/proxyman/outbound"

	// Fix dependency cycle caused by core import in internet package
	_ "github.com/xtls/xray-core/transport/internet/tagged/taggedimpl"

	// Transports that the JSON config parser does not reference by type.
	_ "github.com/xtls/xray-core/transport/internet/udp"
)
