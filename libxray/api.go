// Command libxray builds Xray-core as a C shared library (libxray.so /
// xray.dll) for embedding in a host application over its C ABI -- P/Invoke
// from .NET, JNI from Android, and so on. See export.go for the exported
// surface and libxray/README.md for the calling convention.
//
// This file holds the logic and stays free of cgo, so the package still
// compiles and vets with CGO_ENABLED=0.
package main

import (
	"os"
	"runtime/debug"
	"strings"
	"sync"

	"github.com/xtls/xray-core/common/errors"
	"github.com/xtls/xray-core/common/pause"
	"github.com/xtls/xray-core/common/platform"
	core "github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/infra/conf/serial"
	"github.com/xtls/xray-core/transport/internet/hysteria"

	// Registers the feature set this library exposes.
	_ "github.com/xtls/xray-core/main/distro/lib"
)

var (
	mu       sync.Mutex
	instance *core.Instance

	errMu   sync.Mutex
	lastErr string
)

// setLastError records err for retrieval through XrayLastError. Callers get a
// return code from every entry point; this is how they get the detail.
func setLastError(err error) {
	errMu.Lock()
	defer errMu.Unlock()
	if err == nil {
		lastErr = ""
		return
	}
	lastErr = err.Error()
}

func takeLastError() string {
	errMu.Lock()
	defer errMu.Unlock()
	return lastErr
}

// loadConfig parses a JSON config into the core's protobuf form.
func loadConfig(configJSON string) (*core.Config, error) {
	if strings.TrimSpace(configJSON) == "" {
		return nil, errors.New("empty config")
	}
	return serial.LoadJSONConfig(strings.NewReader(configJSON))
}

// startInstance builds and starts an instance from a JSON config. It is an
// error to start while another instance is running: stop it first, so a caller
// that loses track of state cannot leak a running core.
func startInstance(configJSON string) error {
	mu.Lock()
	defer mu.Unlock()

	if instance != nil {
		return errors.New("xray is already running")
	}

	config, err := loadConfig(configJSON)
	if err != nil {
		return err
	}
	server, err := core.New(config)
	if err != nil {
		return err
	}
	if err := server.Start(); err != nil {
		server.Close()
		return err
	}

	instance = server
	return nil
}

// stopInstance shuts the running instance down. Stopping while nothing runs is
// not an error, so callers can stop unconditionally on teardown.
func stopInstance() error {
	mu.Lock()
	defer mu.Unlock()

	if instance == nil {
		return nil
	}
	err := instance.Close()
	instance = nil
	return err
}

func isRunning() bool {
	mu.Lock()
	defer mu.Unlock()
	return instance != nil
}

// testConfig reports whether a config parses and builds every handler, without
// starting anything. This is what `xray run -test` does.
func testConfig(configJSON string) error {
	config, err := loadConfig(configJSON)
	if err != nil {
		return err
	}
	server, err := core.New(config)
	if err != nil {
		return err
	}
	return server.Close()
}

// setAssetPath points the core at the directory holding geoip.dat and
// geosite.dat. Routing rules that use geoip:/geosite: need it, and the default
// -- the executable's directory -- is not a useful location for a library
// loaded into someone else's process.
func setAssetPath(path string) error {
	return os.Setenv(platform.AssetLocation, path)
}

func version() string {
	return core.VersionStatement()[0]
}

// resetConnections tears down pooled transport sessions so the next dial builds
// fresh ones, and reports how many were closed.
//
// This is the answer to a network handover. When a device moves between Wi-Fi
// and mobile, every established session is dead the instant the old link goes
// away — but nothing in the stack is told so. QUIC will sit on the dead path
// until its idle timeout, which is minutes on a mobile network, and to the user
// that is a VPN which is connected and carries nothing until they toggle it off
// and on. Closing the sessions is what makes the next request rebuild on the
// new path immediately.
//
// It is deliberately far cheaper than the alternative the host would otherwise
// reach for: stopping and restarting the whole instance drops the TUN, renews
// every handler and re-runs the connect path, where this keeps the tunnel
// standing and only invalidates what the handover actually invalidated.
//
// Safe to call at any time, including with no instance running: an empty pool
// closes nothing and returns 0. Only the hysteria pool is covered today, which
// is where the long-lived sessions are — VLESS over TCP is dialled per request
// and mux is not enabled by the client this library was built for.
func resetConnections() int {
	return hysteria.CloseAllClients()
}

// forceGc returns freed memory to the operating system.
//
// Go keeps pages it has released rather than handing them back, which is the
// right default for a server and the wrong one for a library inside a mobile
// app: RSS grows steadily across a session measured in weeks, and a large
// resident process is the obvious thing for the OOM killer to reach for. The
// host calls this from its own low-memory callback.
//
// Asynchronous on purpose. FreeOSMemory forces a stop-the-world collection, and
// the host's low-memory callback is on a thread that must not block.
func forceGc() {
	go debug.FreeOSMemory()
}

// sleep pauses background housekeeping. See package pause for what that covers
// and what it costs; in short, the core keeps carrying traffic and stops waking
// the device up to tidy structures nothing is touching.
func sleep() {
	pause.Pause()
}

// wake undoes sleep. Idempotent, and safe without a matching sleep.
func wake() {
	pause.Resume()
}

// isPaused reports whether housekeeping is currently paused. Diagnostics only —
// the host uses it to confirm its Doze wiring actually reaches the core.
func isPaused() bool {
	return pause.IsPaused()
}

// main is required for -buildmode=c-shared and never runs.
func main() {}
