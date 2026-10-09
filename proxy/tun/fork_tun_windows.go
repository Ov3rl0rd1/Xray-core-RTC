//go:build windows

package tun

// fork: the Windows TUN as it has to behave inside a host process that starts
// and stops the core many times over its life (libxray, P/Invoked by a GUI).
//
// Upstream's device was written for the xray executable, where Close is the
// last thing that happens before the process exits. Embedded, it is not, and
// two of its shortcuts become use-after-free in wintun.dll — native faults that
// end the whole host process with nothing written anywhere:
//
//   - ReadPacket handed gVisor a slice that pointed straight into wintun's
//     receive ring and released it only when gVisor dropped the packet. gVisor
//     holds packets for as long as it likes (TCP reassembly, the GRO queue), so
//     a release could arrive after WintunEndSession had freed the ring. Holding
//     them also pins ring space: wintun reclaims it in order, so one packet
//     parked in a reassembly queue stalls everything behind it once the ring is
//     full. The packet is now copied out and released at once — the same thing
//     wireguard-go does, and for the same reasons.
//
//   - Nothing stopped ReadPacket or Wait from running while Close ended the
//     session. The reader is now woken, and every ring access is waited for,
//     before WintunEndSession runs.

import (
	go_errors "errors"
	"os"
	"time"

	"golang.org/x/sys/windows"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

// readWaitSlice bounds a single wait so that a wake-up lost to the auto-reset
// event costs a fraction of a second rather than a goroutine for good.
const readWaitSlice = 250 // ms

// enter registers an in-flight ring access, or refuses once Close has begun.
//
// A counter rather than a sync.WaitGroup on purpose: a WaitGroup panics when Add
// takes it up from zero while Wait is running — exactly what a reader arriving
// as Close drains would do, and a panic is the outcome this file exists to
// prevent. With sequentially consistent atomics an entrant that increments after
// the drain has seen zero is guaranteed to see ending and back out.
func (t *WindowsTun) enter() bool {
	t.inflight.Add(1)
	if t.ending.Load() {
		t.inflight.Add(-1)
		return false
	}
	return true
}

func (t *WindowsTun) leave() { t.inflight.Add(-1) }

// drainTimeout bounds Close. Every ring call is non-blocking and Wait is woken
// below, so this is only reached if something is badly wrong — and then ending
// the session late is better than never returning from XrayStop.
const drainTimeout = 2 * time.Second

// drainBeforeEnd stops new ring access, wakes a reader parked in Wait, and
// waits for everything in flight to leave before the session can be ended.
func (t *WindowsTun) drainBeforeEnd() {
	t.ending.Store(true)
	if t.readWait != 0 {
		_ = windows.SetEvent(t.readWait)
	}
	deadline := time.Now().Add(drainTimeout)
	for t.inflight.Load() > 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
}

func (t *WindowsTun) forkReadPacket() (byte, *stack.PacketBuffer, error) {
	if !t.enter() {
		return 0, nil, os.ErrClosed
	}
	defer t.leave()

	packet, err := t.session.ReceivePacket()
	if go_errors.Is(err, windows.ERROR_NO_MORE_ITEMS) {
		return 0, nil, ErrQueueEmpty
	}
	if err != nil {
		return 0, nil, err
	}

	// Copied out and released while the session is certainly alive.
	data := make([]byte, len(packet))
	copy(data, packet)
	t.session.ReleaseReceivePacket(packet)

	if len(data) == 0 {
		return 0, nil, ErrQueueEmpty
	}
	version := data[0] >> 4
	return version, stack.NewPacketBuffer(stack.PacketBufferOptions{
		Payload:           buffer.MakeWithData(data),
		IsForwardedPacket: true,
	}), nil
}

func (t *WindowsTun) forkWait() {
	if !t.enter() {
		return
	}
	defer t.leave()

	procyield(1)
	_, _ = windows.WaitForSingleObject(t.readWait, readWaitSlice)
}
