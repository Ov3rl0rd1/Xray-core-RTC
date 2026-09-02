package hysteria

// Fork-only: pause-aware housekeeping for the hysteria transport, plus the
// handover helper the embedded client needs.
//
// Everything here lives in its own file on purpose. Upstream owns conn.go and
// dialer.go and rewrites them regularly (the v2.12.2 upgrade touched both), so
// keeping this logic out of them means a rebase onto a new upstream has almost
// nothing to reconcile. What stays behind in upstream files is two call sites
// and nothing else:
//
//	conn.go    udpSessionManager.clean → forkTick(...)
//	dialer.go  clientManager.clean     → forkCleanClients(m)
//	dialer.go  Dial                    → forkNotify() after adding a client
//
// # What it is for
//
// A core embedded in a mobile client keeps running when the screen goes off,
// and the housekeeping goroutines it starts do not care that nobody is using
// the tunnel. The hysteria transport alone runs two of them at 1 Hz — one
// reaping idle UDP sessions, one reaping dead QUIC clients. On Android each
// tick is a timer the kernel must service, which is exactly what stops a device
// settling into deep sleep: over a night, tens of thousands of wakeups to look
// at a map that has not changed.
//
// So both loops stop their ticker outright whenever they have nothing to do:
//
//   - the host has paused housekeeping (Doze), until it resumes;
//   - the client pool is empty, until a dial adds one.
//
// Reaping is deferred, never skipped. Nothing creates sessions while the device
// is idle, so no backlog accumulates, and the first tick after a resume clears
// whatever did. Behaviour while running is unchanged, and a host that never
// calls pause.Pause sees exactly the upstream behaviour.

import (
	"time"

	"github.com/xtls/xray-core/common/pause"
)

// forkTick blocks until the next housekeeping tick is due, stopping the ticker
// for the duration of a pause rather than leaving it armed.
//
// Stopping matters as much as skipping the work: a ticker left running still
// arms a runtime timer every period, so a loop that merely ignores its ticks
// saves the work and none of the wakeups.
//
// Used by the UDP session reaper, whose loop is otherwise upstream's.
func forkTick(ticker *time.Ticker, interval time.Duration) {
	for {
		// Taken before the test, not after: a Resume landing in between would
		// otherwise hand back the channel for a pause that has not happened,
		// and the wait below would never be released. See pause.Resumed.
		resumed := pause.Resumed()
		if pause.IsPaused() {
			ticker.Stop()
			<-resumed
			ticker.Reset(interval)
			continue
		}

		<-ticker.C
		return
	}
}

// forkWake is poked when a client is added, so the clean loop can park on an
// empty pool instead of waking once a second to look at a map it knows is
// empty.
//
// A package variable rather than a field on clientManager: the manager is a
// process-wide singleton built once under initmanager, and keeping the channel
// here means upstream's struct definition and its initialiser stay untouched.
// Buffered by one — a send that finds the buffer full means a wakeup is already
// pending, which is all the signal conveys.
var forkWake = make(chan struct{}, 1)

// forkNotify tells a parked clean loop that the pool is no longer empty.
func forkNotify() {
	select {
	case forkWake <- struct{}{}:
	default:
	}
}

func (m *clientManager) empty() bool {
	m.RLock()
	defer m.RUnlock()

	return len(m.m) == 0
}

// forkCleanClients reaps clients whose QUIC session has gone away.
//
// Upstream runs this as an unconditional `for range ticker.C` at 1 Hz, started
// once per process and never stopped — a wakeup every second for the life of
// the process, including after the pool has been emptied and including while
// the device is asleep with nothing dialling at all. It buys nothing either:
// reaping a dead client a minute late is the same as reaping it a second late,
// because dialling checks status before reusing one.
func forkCleanClients(m *clientManager) {
	ticker := time.NewTicker(idleCleanupInterval)
	defer ticker.Stop()

	ticking := true
	idle := func() {
		if ticking {
			ticker.Stop()
			ticking = false
		}
	}
	active := func() {
		if !ticking {
			ticker.Reset(idleCleanupInterval)
			ticking = true
		}
	}

	for {
		// Before the test, not after — see forkTick.
		resumed := pause.Resumed()
		if pause.IsPaused() {
			idle()
			<-resumed
			continue
		}

		if m.empty() {
			idle()
			select {
			case <-forkWake:
			case <-resumed:
				// Only reachable if the host paused and resumed while parked;
				// loop round and re-evaluate.
			}
			continue
		}

		active()
		select {
		case <-ticker.C:
			m.RLock()
			for _, c := range m.m {
				c.clean()
			}
			m.RUnlock()
		case <-forkWake:
		}
	}
}

// CloseAllClients tears down every pooled QUIC session and empties the pool,
// returning how many were closed.
//
// Meant for one thing: a network handover. When a device moves from Wi-Fi to
// mobile, every established session is dead, but nothing in the stack knows it
// — QUIC will sit on the old path until its idle timeout, which on Android is
// minutes, and to the user that is a VPN that is connected and carries nothing
// until they toggle it. Closing the sessions makes the next dial build fresh
// ones on the new path immediately.
//
// The pool is emptied rather than just closed so the entries do not linger as
// work for the clean loop, and so the loop can park when nothing replaces them.
// Safe to call when nothing has ever dialled.
func CloseAllClients() int {
	if manager == nil {
		return 0
	}

	manager.Lock()
	defer manager.Unlock()

	closed := 0
	for key, c := range manager.m {
		c.Lock()
		// A client that never completed a dial has no conn, and close would
		// nil-dereference on it.
		if c.status() != StatusNull {
			c.close()
		}
		c.Unlock()

		delete(manager.m, key)
		closed++
	}

	return closed
}
