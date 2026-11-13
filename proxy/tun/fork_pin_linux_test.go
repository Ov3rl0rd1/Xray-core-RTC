//go:build linux

package tun

import (
	"context"
	"errors"
	"net"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// The whole path on a real socket: an interface the kernel will not bind to
// must leave the socket bound to loopback, from where nothing reaches the
// default route, instead of unbound, where everything follows it.
func TestASocketThatCannotBePinnedIsSealedToLoopback(t *testing.T) {
	u := &InterfaceUpdater{tunIndex: -1, iface: &net.Interface{Index: 1 << 20, Name: "nosuchif0"}}
	lo := loopbackInterface()
	if lo == nil {
		t.Skip("no loopback interface")
	}

	var bindErr error
	var device string
	dialer := &net.Dialer{
		Timeout: 3 * time.Second,
		Control: func(network, address string, c syscall.RawConn) error {
			bindErr = BindSocket(network, address, c, u)
			_ = c.Control(func(fd uintptr) {
				device, _ = unix.GetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE)
			})
			return nil // as the core's dialer does: it only logs a controller's error
		},
	}
	conn, _ := dialer.DialContext(context.Background(), "udp", "192.0.2.1:9")
	if conn != nil {
		conn.Close()
	}
	if errors.Is(bindErr, syscall.EPERM) {
		t.Skip("binding to a device needs CAP_NET_RAW here")
	}
	if bindErr == nil {
		t.Fatal("BindSocket reported success for an interface that does not exist")
	}
	if device != lo.Name {
		t.Fatalf("the socket is bound to %q, want %q (bind error: %v)", device, lo.Name, bindErr)
	}
}
