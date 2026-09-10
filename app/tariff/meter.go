package tariff

import (
	"sync/atomic"
	"time"

	"github.com/xtls/xray-core/common/shaper"
)

// Meter is one connection's handle into the store. The dispatcher takes one
// when a connection is set up and calls Count for every byte that crosses it.
//
// Everything the write path needs is resolved here, once: the user's counters,
// the counter for the inbound the connection came in on, the quota buckets
// that traffic through that inbound counts towards, and the device it belongs
// to. Counting a write is then a handful of atomic adds against pointers the
// meter already holds — no map lookups, no locks and no clock reads.
type Meter struct {
	m      *Manager
	u      *userState
	inb    *inboundUsage
	dev    *deviceState // nil when the connection was refused on the device cap
	quotas []*quotaBucket

	// denied is fixed when the meter is created and never changes, so it needs
	// no synchronisation. User-level blocking is separate and dynamic: it is
	// read from the user's published enforcement on every write, so a quota
	// that runs out mid-download stops that download.
	denied bool

	released atomic.Bool
}

// Attach registers a connection and returns its meter. It never returns nil:
// a refused connection gets a meter that reports Blocked, because the caller
// stores it in an interface and a typed nil there would not compare equal to
// nil.
func (m *Manager) Attach(email string, level uint32, inboundTag, device string) *Meter {
	now := m.now()
	m.maybeSweep(now)

	u, created := m.userFor(email, level, now)
	if created {
		// Publish enforcement before the shaper asks for it, so the very first
		// connection of a user the manager has just learned about is already
		// shaped by their plan rather than by the default.
		m.recompute(u)
	}

	mt := &Meter{m: m, u: u}

	u.mu.Lock()
	p := u.policy
	if p == nil {
		p = m.defaultPolicy()
	}

	dev := u.devices[device]
	limitHit := false
	if dev == nil {
		if max := p.GetMaxDevices(); max > 0 && u.activeDevicesLocked() >= max {
			limitHit = true
			mt.denied = p.GetDeviceLimitAction() == Action_ACTION_BLOCK
		}
		if !mt.denied {
			dev = &deviceState{key: device, firstSeen: now.UnixNano()}
			dev.lastSeen.Store(now.UnixNano())
			u.devices[device] = dev
		}
	}
	if dev != nil {
		dev.conns++
		dev.lastSeen.Store(now.UnixNano())
		mt.dev = dev
	}

	iu := u.inbounds[inboundTag]
	if iu == nil {
		iu = &inboundUsage{tag: inboundTag}
		u.inbounds[inboundTag] = iu
	}
	mt.inb = iu

	for _, b := range u.quotas {
		if b.matches(inboundTag) {
			mt.quotas = append(mt.quotas, b)
		}
	}

	u.conns++
	firstConn := u.conns == 1
	newDevice := dev != nil && dev.conns == 1
	u.lastSeen = now
	u.mu.Unlock()

	if firstConn {
		m.events.publish(&Event{
			UnixNano: now.UnixNano(), Kind: EventKind_EVENT_USER_CONNECTED,
			Email: email, InboundTag: inboundTag, Device: device,
		})
	}
	if newDevice {
		m.events.publish(&Event{
			UnixNano: now.UnixNano(), Kind: EventKind_EVENT_DEVICE_ADDED,
			Email: email, InboundTag: inboundTag, Device: device,
			Value: uint64(m.deviceCount(u)),
		})
	}
	if limitHit {
		m.events.publish(&Event{
			UnixNano: now.UnixNano(), Kind: EventKind_EVENT_DEVICE_LIMIT_HIT,
			Email: email, InboundTag: inboundTag, Device: device,
			Value: uint64(m.deviceCount(u)), Limit: uint64(p.GetMaxDevices()),
			Detail: deviceLimitDetail(mt.denied),
		})
	}
	return mt
}

func deviceLimitDetail(denied bool) string {
	if denied {
		return "refused"
	}
	return "allowed"
}

func (m *Manager) deviceCount(u *userState) uint32 {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.activeDevicesLocked()
}

// Count records n bytes moved in one direction.
//
// This is the write path. It does no locking, no allocation and no clock
// reads; the only work beyond the adds is comparing each quota against its
// limit, and the only time it does anything more is the single write on which
// a quota crosses a threshold.
func (mt *Meter) Count(dir shaper.Direction, n int64) {
	if mt == nil || n <= 0 {
		return
	}
	v := uint64(n)
	if dir == shaper.Up {
		mt.u.up.Add(v)
		mt.inb.up.Add(v)
	} else {
		mt.u.down.Add(v)
		mt.inb.down.Add(v)
	}

	for _, q := range mt.quotas {
		warned, exceeded := q.add(v)
		switch {
		case exceeded:
			mt.m.quotaExceeded(mt.u, q)
		case warned:
			mt.m.quotaWarned(mt.u, q)
		}
	}
	if b := mt.m.serverBucket.Load(); b != nil {
		warned, exceeded := b.add(v)
		switch {
		case exceeded:
			mt.m.serverQuotaExceeded(b)
		case warned:
			mt.m.serverQuotaWarned(b)
		}
	}
	mt.m.markDirty()
}

// Blocked reports whether this connection's traffic must be refused: either
// the connection itself was turned away on the device cap, or the user is
// disabled, expired, or over a quota whose action is to block.
//
// It is read on every write rather than once, so that a quota running out
// during a transfer stops that transfer instead of waiting for a reconnect.
func (mt *Meter) Blocked() bool {
	if mt == nil {
		return false
	}
	return mt.denied || mt.u.eff.Load().blocked
}

// BlockReason explains a Blocked meter, for the error the client sees in the
// log.
func (mt *Meter) BlockReason() string {
	if mt == nil {
		return ""
	}
	if mt.denied {
		return "device limit"
	}
	return mt.u.eff.Load().reason
}

// Release gives up the connection's device and user references. It is
// idempotent.
func (mt *Meter) Release() {
	if mt == nil || !mt.released.CompareAndSwap(false, true) {
		return
	}
	now := mt.m.now()
	u := mt.u

	u.mu.Lock()
	u.conns--
	if u.conns < 0 {
		u.conns = 0
	}
	last := u.conns == 0
	u.lastSeen = now
	if mt.dev != nil {
		mt.dev.conns--
		if mt.dev.conns < 0 {
			mt.dev.conns = 0
		}
		mt.dev.lastSeen.Store(now.UnixNano())
	}
	u.mu.Unlock()

	if last {
		mt.m.events.publish(&Event{
			UnixNano: now.UnixNano(), Kind: EventKind_EVENT_USER_DISCONNECTED,
			Email: u.email,
		})
	}
}

// --- quota transitions -----------------------------------------------------

func (m *Manager) quotaWarned(u *userState, q *quotaBucket) {
	m.events.publish(&Event{
		UnixNano: m.now().UnixNano(), Kind: EventKind_EVENT_QUOTA_WARNING,
		Email: u.email, InboundTag: q.spec.GetInboundTag(),
		Value: q.used.Load(), Limit: q.spec.GetLimitBytes(),
	})
	m.markDirty()
}

func (m *Manager) quotaExceeded(u *userState, q *quotaBucket) {
	m.events.publish(&Event{
		UnixNano: m.now().UnixNano(), Kind: EventKind_EVENT_QUOTA_EXCEEDED,
		Email: u.email, InboundTag: q.spec.GetInboundTag(),
		Value: q.used.Load(), Limit: q.spec.GetLimitBytes(),
		Detail: q.spec.GetAction().String(),
	})
	// Republish this user's enforcement so the consequence — a throttle, or a
	// block — reaches their live connections on their next write.
	m.recompute(u)
	m.markDirty()
}

func (m *Manager) serverQuotaWarned(b *quotaBucket) {
	m.events.publish(&Event{
		UnixNano: m.now().UnixNano(), Kind: EventKind_EVENT_SERVER_QUOTA_WARNING,
		Value: b.used.Load(), Limit: b.spec.GetLimitBytes(),
	})
	m.markDirty()
}

func (m *Manager) serverQuotaExceeded(b *quotaBucket) {
	m.events.publish(&Event{
		UnixNano: m.now().UnixNano(), Kind: EventKind_EVENT_SERVER_QUOTA_EXCEEDED,
		Value: b.used.Load(), Limit: b.spec.GetLimitBytes(),
		Detail: b.spec.GetAction().String(),
	})
	// The server allowance applies to everyone, so everyone's enforcement has
	// to be rebuilt.
	m.recomputeAll()
	m.markDirty()
}

// --- sweep -----------------------------------------------------------------

// maybeSweep runs the periodic housekeeping if it is due. It runs from Attach
// rather than a goroutine so the manager has no lifecycle to manage and leaks
// nothing if it is dropped; the cost is that a completely idle server does not
// roll its windows until someone connects, which is exactly when it matters.
func (m *Manager) maybeSweep(now time.Time) {
	if now.UnixNano() < m.nextSweep.Load() {
		return
	}
	if !m.sweeping.CompareAndSwap(false, true) {
		return
	}
	defer m.sweeping.Store(false)
	m.nextSweep.Store(now.Add(sweepInterval).UnixNano())
	m.sweep(now)
}

func (m *Manager) sweep(now time.Time) {
	if b := m.serverBucket.Load(); b != nil && b.roll(now) {
		m.events.publish(&Event{
			UnixNano: now.UnixNano(), Kind: EventKind_EVENT_SERVER_QUOTA_RESET,
			Limit: b.spec.GetLimitBytes(),
		})
		m.recomputeAll()
		m.markDirty()
	}

	m.mu.RLock()
	users := make([]*userState, 0, len(m.users))
	for _, u := range m.users {
		users = append(users, u)
	}
	m.mu.RUnlock()

	for _, u := range users {
		m.sweepUser(u, now)
	}
	m.maybeFlush(now)
}

func (m *Manager) sweepUser(u *userState, now time.Time) {
	var reset []*quotaBucket
	var gone []string
	expired := false

	u.mu.Lock()
	for _, q := range u.quotas {
		if q.roll(now) {
			reset = append(reset, q)
		}
	}
	for key, d := range u.devices {
		if d.conns == 0 && now.UnixNano()-d.lastSeen.Load() > int64(deviceRetention) {
			delete(u.devices, key)
			gone = append(gone, key)
		}
	}
	if p := u.policy; p != nil && p.GetExpiresAt() > 0 && now.Unix() >= p.GetExpiresAt() {
		expired = !u.eff.Load().blocked
	}
	u.mu.Unlock()

	for _, key := range gone {
		m.events.publish(&Event{
			UnixNano: now.UnixNano(), Kind: EventKind_EVENT_DEVICE_REMOVED,
			Email: u.email, Device: key,
		})
	}
	for _, q := range reset {
		m.events.publish(&Event{
			UnixNano: now.UnixNano(), Kind: EventKind_EVENT_QUOTA_RESET,
			Email: u.email, InboundTag: q.spec.GetInboundTag(),
			Limit: q.spec.GetLimitBytes(),
		})
	}
	if expired {
		m.events.publish(newEvent(now, EventKind_EVENT_USER_EXPIRED, u.email))
	}
	if len(reset) > 0 || expired {
		m.recompute(u)
		m.markDirty()
	}
}
