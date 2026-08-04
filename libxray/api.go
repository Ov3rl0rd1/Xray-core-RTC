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
	"strings"
	"sync"

	"github.com/xtls/xray-core/common/errors"
	"github.com/xtls/xray-core/common/platform"
	core "github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/infra/conf/serial"

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

// main is required for -buildmode=c-shared and never runs.
func main() {}
