package tun

// fork: the list of connections the TUN is carrying, for the client's UI.
//
// A user of a split tunnel needs two things the core alone knows: which of
// their applications' connections went through the proxy and which went
// direct, and a way to make a connection pick up a rule they just changed. The
// TUN inbound sees every connection's source (the application's own socket, by
// which the host maps it to a process) and its lifetime; the dispatcher reports
// where routing sent it (common/session/fork_routed.go). Closing one tears it
// down from both ends, so the application reconnects — and is routed afresh.
//
// Cost when nobody asks: an entry in a map per live connection and two atomic
// adds per buffer. Nothing is resolved or formatted until Snapshot is called.

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xtls/xray-core/common/buf"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/session"
)

type tracked struct {
	id      uint64
	network net.Network
	src     net.Destination
	dst     net.Destination
	started time.Time

	up, down atomic.Int64
	outbound atomic.Pointer[string]
	target   atomic.Pointer[string]

	cancel context.CancelFunc
	conn   net.Conn
	closed atomic.Bool
}

var (
	trackMu  sync.Mutex
	trackSeq uint64
	trackAll = map[uint64]*tracked{}
)

// trackConnection registers conn and returns the context and connection the
// handler must use from here on, plus the function that unregisters it.
func trackConnection(ctx context.Context, cancel context.CancelFunc, conn net.Conn, src, dst net.Destination) (context.Context, net.Conn, func()) {
	e := &tracked{network: dst.Network, src: src, dst: dst, started: time.Now(), cancel: cancel, conn: conn}

	trackMu.Lock()
	trackSeq++
	e.id = trackSeq
	trackAll[e.id] = e
	trackMu.Unlock()

	ctx = session.ContextWithRouteObserver(ctx, func(tag string, target net.Destination) {
		e.outbound.Store(&tag)
		t := target.NetAddr()
		e.target.Store(&t)
	})

	untrack := func() {
		trackMu.Lock()
		delete(trackAll, e.id)
		trackMu.Unlock()
	}

	// The UDP side is a packet connection whose buffers carry their own
	// destination — that is what full-cone NAT is made of — so the wrapper must
	// keep the MultiBuffer interface, or every reply would be addressed to the
	// first destination.
	if pc, ok := conn.(packetConn); ok {
		return ctx, &countedPacketConn{packetConn: pc, e: e}, untrack
	}
	return ctx, &countedConn{Conn: conn, e: e}, untrack
}

type packetConn interface {
	net.Conn
	buf.Reader
	buf.Writer
}

type countedConn struct {
	net.Conn
	e *tracked
}

func (c *countedConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	c.e.up.Add(int64(n))
	return n, err
}

func (c *countedConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	c.e.down.Add(int64(n))
	return n, err
}

type countedPacketConn struct {
	packetConn
	e *tracked
}

func (c *countedPacketConn) ReadMultiBuffer() (buf.MultiBuffer, error) {
	mb, err := c.packetConn.ReadMultiBuffer()
	c.e.up.Add(int64(mb.Len()))
	return mb, err
}

func (c *countedPacketConn) WriteMultiBuffer(mb buf.MultiBuffer) error {
	n := mb.Len()
	err := c.packetConn.WriteMultiBuffer(mb)
	if err == nil {
		c.e.down.Add(int64(n))
	}
	return err
}

func (c *countedPacketConn) Read(b []byte) (int, error) {
	n, err := c.packetConn.Read(b)
	c.e.up.Add(int64(n))
	return n, err
}

func (c *countedPacketConn) Write(b []byte) (int, error) {
	n, err := c.packetConn.Write(b)
	c.e.down.Add(int64(n))
	return n, err
}

// ConnectionInfo is one live connection, as Snapshot reports it.
type ConnectionInfo struct {
	ID       uint64 `json:"id"`
	Network  string `json:"net"`
	Source   string `json:"src"`
	Dest     string `json:"dst"`
	Target   string `json:"target,omitempty"`
	Outbound string `json:"outbound,omitempty"`
	Up       int64  `json:"up"`
	Down     int64  `json:"down"`
	Started  int64  `json:"startedMs"`
}

// Snapshot lists the live connections.
func Snapshot() []ConnectionInfo {
	trackMu.Lock()
	entries := make([]*tracked, 0, len(trackAll))
	for _, e := range trackAll {
		entries = append(entries, e)
	}
	trackMu.Unlock()

	out := make([]ConnectionInfo, 0, len(entries))
	for _, e := range entries {
		info := ConnectionInfo{
			ID:      e.id,
			Network: e.network.SystemString(),
			Source:  e.src.NetAddr(),
			Dest:    e.dst.NetAddr(),
			Up:      e.up.Load(),
			Down:    e.down.Load(),
			Started: e.started.UnixMilli(),
		}
		if p := e.outbound.Load(); p != nil {
			info.Outbound = *p
		}
		if p := e.target.Load(); p != nil && *p != info.Dest {
			info.Target = *p
		}
		out = append(out, info)
	}
	return out
}

// SnapshotJSON is Snapshot for the C ABI.
func SnapshotJSON() string {
	b, err := json.Marshal(Snapshot())
	if err != nil {
		return "[]"
	}
	return string(b)
}

// CloseConnections tears down the connections with the given ids (all of them
// when ids is empty and all is true) and reports how many it closed. The
// application on the other end sees its connection end and, typically,
// reconnects — which routes it under the rules as they are now.
func CloseConnections(ids []uint64, all bool) int {
	trackMu.Lock()
	var victims []*tracked
	if all {
		for _, e := range trackAll {
			victims = append(victims, e)
		}
	} else {
		for _, id := range ids {
			if e, ok := trackAll[id]; ok {
				victims = append(victims, e)
			}
		}
	}
	trackMu.Unlock()

	n := 0
	for _, e := range victims {
		if !e.closed.CompareAndSwap(false, true) {
			continue
		}
		e.cancel()
		_ = e.conn.Close()
		n++
	}
	return n
}
