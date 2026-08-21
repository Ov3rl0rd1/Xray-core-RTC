// Package pause lets a host application stop the core's background
// housekeeping while the device is asleep, and start it again on wake.
//
// # Why this exists
//
// A core embedded in a mobile client keeps running when the screen goes off,
// and the housekeeping goroutines it starts do not care that nobody is using
// the tunnel. The hysteria transport alone runs two of them at 1 Hz — one
// reaping idle UDP sessions, one reaping dead QUIC clients — and on Android
// each tick is a timer the kernel has to service, which is exactly what stops
// a device settling into a deep sleep state. Over a night that is tens of
// thousands of wakeups to look at a map that has not changed.
//
// Doze already tells the app when this happens; the app has had no way to tell
// the core. This package is that channel: [Pause] on
// ACTION_DEVICE_IDLE_MODE_CHANGED going idle, [Resume] on leaving it, exposed
// over the C ABI as XraySleep/XrayWake.
//
// # What pausing costs
//
// Reaping is deferred, not skipped: an idle UDP session or a dead QUIC client
// lives until the next resume rather than being collected within the second.
// Nothing is creating new ones while the device is idle, so the backlog does
// not grow, and the first tick after [Resume] clears whatever accumulated.
// Live traffic is untouched — this only governs housekeeping loops, never the
// data path.
//
// # Contract for loops
//
// Take the channel before testing the flag, or a resume landing between the
// two leaves the loop waiting on a channel nobody will close again:
//
//	resumed := pause.Resumed()
//	if pause.IsPaused() {
//	    ticker.Stop()
//	    <-resumed
//	    ticker.Reset(interval)
//	    continue
//	}
//
// Stopping the ticker matters as much as skipping the work. A ticker left
// running still arms a runtime timer on every period, so a loop that merely
// ignores its ticks saves the work and none of the wakeups.
//
// Nothing here is required: a core whose host never calls [Pause] behaves
// exactly as it did before, because the package starts resumed.
package pause

import "sync"

var (
	mu     sync.Mutex
	paused bool

	// Closed while running, replaced with a fresh open channel by Pause.
	// Waiters hold the channel they observed, so a Resume that races the
	// check still releases them.
	resumed = closedChan()
)

func closedChan() chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}

// Pause asks background housekeeping to stop until [Resume]. Idempotent.
func Pause() {
	mu.Lock()
	defer mu.Unlock()

	if paused {
		return
	}
	paused = true
	resumed = make(chan struct{})
}

// Resume releases everything waiting on [Resumed]. Idempotent, and safe to
// call without a matching Pause.
func Resume() {
	mu.Lock()
	defer mu.Unlock()

	if !paused {
		return
	}
	paused = false
	close(resumed)
}

// IsPaused reports the current state.
func IsPaused() bool {
	mu.Lock()
	defer mu.Unlock()

	return paused
}

// Resumed returns a channel that is closed while running, and closed by the
// next [Resume] while paused.
//
// Call it before [IsPaused], never after: the pair is not atomic, and taking
// the channel second means a Resume in between hands back the channel for a
// pause that has not happened yet.
func Resumed() <-chan struct{} {
	mu.Lock()
	defer mu.Unlock()

	return resumed
}
