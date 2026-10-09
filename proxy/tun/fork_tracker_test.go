package tun

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/xtls/xray-core/common/buf"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/session"
)

// fakePacketConn stands in for udpConn: a net.Conn that also moves whole
// buffers, which is what carries each packet's own destination.
type fakePacketConn struct {
	net.Conn
	closed bool
	in     chan buf.MultiBuffer
}

func (f *fakePacketConn) ReadMultiBuffer() (buf.MultiBuffer, error) {
	mb, ok := <-f.in
	if !ok {
		return nil, io.EOF
	}
	return mb, nil
}
func (f *fakePacketConn) WriteMultiBuffer(mb buf.MultiBuffer) error { buf.ReleaseMulti(mb); return nil }
func (f *fakePacketConn) Close() error                             { f.closed = true; return nil }

func TestTrackerKeepsThePacketInterface(t *testing.T) {
	src := net.UDPDestination(net.ParseAddress("198.18.0.1"), 50000)
	dst := net.UDPDestination(net.ParseAddress("1.2.3.4"), 27015)
	inner := &fakePacketConn{in: make(chan buf.MultiBuffer, 1)}

	_, conn, untrack := trackConnection(context.Background(), func() {}, inner, src, dst)
	defer untrack()

	// buf.NewReader takes the MultiBuffer path only if the wrapper still has it;
	// losing it would address every reply to the first destination.
	if _, ok := conn.(buf.Reader); !ok {
		t.Fatal("wrapped UDP connection lost buf.Reader")
	}
	if _, ok := conn.(buf.Writer); !ok {
		t.Fatal("wrapped UDP connection lost buf.Writer")
	}

	b := buf.New()
	b.Write(make([]byte, 100))
	inner.in <- buf.MultiBuffer{b}
	mb, err := conn.(buf.Reader).ReadMultiBuffer()
	if err != nil || mb.Len() != 100 {
		t.Fatalf("read %d, %v", mb.Len(), err)
	}
	buf.ReleaseMulti(mb)

	snap := find(t, src)
	if snap.Up != 100 || snap.Network != "udp" {
		t.Fatalf("snapshot %+v", snap)
	}
}

func TestTrackerReportsTheRouteAndCloses(t *testing.T) {
	src := net.TCPDestination(net.ParseAddress("198.18.0.1"), 40001)
	dst := net.TCPDestination(net.ParseAddress("5.6.7.8"), 443)
	cancelled := false
	inner := &fakePacketConn{in: make(chan buf.MultiBuffer)}

	ctx, _, untrack := trackConnection(context.Background(), func() { cancelled = true }, inner, src, dst)
	defer untrack()

	ob := &session.Outbound{Tag: "direct", Target: net.TCPDestination(net.ParseAddress("example.com"), 443)}
	session.NotifyRouted(ctx, ob)

	snap := find(t, src)
	if snap.Outbound != "direct" || snap.Target != "example.com:443" {
		t.Fatalf("route not recorded: %+v", snap)
	}
	if snap.Started > time.Now().UnixMilli() {
		t.Fatal("start time in the future")
	}

	if n := CloseConnections([]uint64{snap.ID}, false); n != 1 {
		t.Fatalf("closed %d", n)
	}
	if !cancelled || !inner.closed {
		t.Fatal("close did not cancel and close the connection")
	}
	if n := CloseConnections([]uint64{snap.ID}, false); n != 0 {
		t.Fatalf("closed twice: %d", n)
	}
}

func find(t *testing.T, src net.Destination) ConnectionInfo {
	t.Helper()
	for _, c := range Snapshot() {
		if c.Source == src.NetAddr() {
			return c
		}
	}
	t.Fatalf("%v not tracked", src)
	return ConnectionInfo{}
}
