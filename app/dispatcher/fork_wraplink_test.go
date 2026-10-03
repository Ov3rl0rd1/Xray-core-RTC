package dispatcher

import (
	"context"
	"io"
	"sync"
	"testing"

	"github.com/xtls/xray-core/common/buf"
	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/common/session"
	"github.com/xtls/xray-core/common/shaper"
	"github.com/xtls/xray-core/features/policy"
	"github.com/xtls/xray-core/transport"
)

// VLESS and Hysteria hand their link to DispatchLink whole, and DispatchLink
// goes through WrapLink, never getLink. The per-user hook used to live in
// getLink alone, so plans, quotas and accounting applied to every protocol
// except the two that carry nearly all the traffic. These tests drive WrapLink.

type recordingMeter struct {
	mu       sync.Mutex
	counted  map[shaper.Direction]int64
	blocked  bool
	released bool
}

func (m *recordingMeter) Count(dir shaper.Direction, n int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.counted == nil {
		m.counted = map[shaper.Direction]int64{}
	}
	m.counted[dir] += n
}
func (m *recordingMeter) Blocked() bool       { return m.blocked }
func (m *recordingMeter) BlockReason() string { return "test" }
func (m *recordingMeter) Release()            { m.released = true }
func (m *recordingMeter) Wait(context.Context, shaper.Direction, int) error {
	return nil
}

func withMeter(t *testing.T, m *recordingMeter) {
	t.Helper()
	SetUsageTracker(func(string, uint32, string, string) UsageMeter { return m })
	t.Cleanup(func() { SetUsageTracker(nil) })
}

// A user's inbound session as VLESS sets it up for Vision, just before it
// dispatches: CanSpliceCopy 2 means "may be spliced once the inner TLS is up".
func visionCtx() (context.Context, *session.Inbound) {
	inb := &session.Inbound{
		Tag:           "vless-in",
		User:          &protocol.MemoryUser{Email: "u@example"},
		CanSpliceCopy: 2,
	}
	return session.ContextWithInbound(context.Background(), inb), inb
}

type sliceReader struct{ chunks []buf.MultiBuffer }

func (r *sliceReader) ReadMultiBuffer() (buf.MultiBuffer, error) {
	if len(r.chunks) == 0 {
		return nil, io.EOF
	}
	mb := r.chunks[0]
	r.chunks = r.chunks[1:]
	return mb, nil
}

func bytesMB(n int) buf.MultiBuffer {
	b := buf.New()
	b.Extend(int32(n))
	return buf.MultiBuffer{b}
}

type discardWriter struct{ n int64 }

func (w *discardWriter) WriteMultiBuffer(mb buf.MultiBuffer) error {
	w.n += int64(mb.Len())
	buf.ReleaseMulti(mb)
	return nil
}

func TestWrapLinkMetersBothDirections(t *testing.T) {
	m := &recordingMeter{}
	withMeter(t, m)
	ctx, _ := visionCtx()

	link := WrapLink(ctx, policy.DefaultManager{}, nil, &transport.Link{
		Reader: &sliceReader{chunks: []buf.MultiBuffer{bytesMB(1000), bytesMB(500)}},
		Writer: &discardWriter{},
	})

	for {
		mb, err := link.Reader.ReadMultiBuffer()
		buf.ReleaseMulti(mb)
		if err != nil {
			break
		}
	}
	if err := link.Writer.WriteMultiBuffer(bytesMB(4000)); err != nil {
		t.Fatal(err)
	}

	if got := m.counted[shaper.Up]; got != 1500 {
		t.Errorf("uplink counted %d bytes, want 1500 — the reader side is the uplink here", got)
	}
	if got := m.counted[shaper.Down]; got != 4000 {
		t.Errorf("downlink counted %d bytes, want 4000", got)
	}
}

// The reader must stay inside the timeout wrapper: the sniffer type-asserts
// buf.TimeoutReader on the link it gets, and anything else panics.
func TestWrapLinkKeepsTheTimeoutReaderOutermost(t *testing.T) {
	withMeter(t, &recordingMeter{})
	ctx, _ := visionCtx()

	link := WrapLink(ctx, policy.DefaultManager{}, nil, &transport.Link{Reader: &sliceReader{}, Writer: &discardWriter{}})

	if _, ok := link.Reader.(buf.TimeoutReader); !ok {
		t.Fatalf("WrapLink returned a %T reader; sniffing needs a buf.TimeoutReader", link.Reader)
	}
}

func TestWrapLinkRefusesABlockedUsersUplink(t *testing.T) {
	withMeter(t, &recordingMeter{blocked: true})
	ctx, _ := visionCtx()

	link := WrapLink(ctx, policy.DefaultManager{}, nil, &transport.Link{
		Reader: &sliceReader{chunks: []buf.MultiBuffer{bytesMB(100)}},
		Writer: &discardWriter{},
	})

	if _, err := link.Reader.ReadMultiBuffer(); err == nil {
		t.Fatal("a blocked user's upload was let through")
	}
	if err := link.Writer.WriteMultiBuffer(bytesMB(100)); err == nil {
		t.Fatal("a blocked user's download was let through")
	}
}

// Vision hands a connection to splice(2) once the inner TLS is up, and from
// then on nothing in user space sees a byte: measured on a real Vision link
// before this, a 5 MB/s plan ran at 400 MB/s and the ledger recorded only the
// handshake. A limited or metered user must never be promoted to splice.
func TestWrapLinkKeepsALimitedUserOutOfSplice(t *testing.T) {
	withMeter(t, &recordingMeter{})
	ctx, inb := visionCtx()

	WrapLink(ctx, policy.DefaultManager{}, nil, &transport.Link{Reader: &sliceReader{}, Writer: &discardWriter{}})

	if inb.CanSpliceCopy != 3 {
		t.Fatalf("CanSpliceCopy = %d after WrapLink, want 3 (never splice)", inb.CanSpliceCopy)
	}
}

// With nothing installed — no policy store, no stats, tiers off — the hook
// must cost nothing, and that includes leaving splice alone.
func TestWrapLinkLeavesAnUnmanagedUserAlone(t *testing.T) {
	SetUsageTracker(nil)
	defer restoreTiers(tiersEnabled)
	tiersEnabled = false
	ctx, inb := visionCtx()
	inner := &sliceReader{}

	link := WrapLink(ctx, policy.DefaultManager{}, nil, &transport.Link{Reader: inner, Writer: &discardWriter{}})

	if inb.CanSpliceCopy != 2 {
		t.Fatalf("CanSpliceCopy = %d for an unmanaged user, want it untouched (2)", inb.CanSpliceCopy)
	}
	if tw, ok := link.Reader.(*buf.TimeoutWrapperReader); !ok || tw.Reader != inner {
		t.Fatal("an unmanaged user's reader was wrapped")
	}
}
