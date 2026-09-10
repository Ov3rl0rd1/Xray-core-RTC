package tariff

import (
	"sort"
	"time"

	"github.com/xtls/xray-core/common/errors"
)

// Usage returns everything the manager knows about what one user has spent, or
// nil if it has never seen them.
func (m *Manager) Usage(email string) *Usage {
	m.mu.RLock()
	u := m.users[email]
	m.mu.RUnlock()
	if u == nil {
		return nil
	}
	return m.usageOf(u, m.now())
}

// AllUsage returns the same for every user the manager holds, sorted by email
// so a panel diffing two polls sees a stable order.
func (m *Manager) AllUsage() []*Usage {
	m.mu.RLock()
	users := make([]*userState, 0, len(m.users))
	for _, u := range m.users {
		users = append(users, u)
	}
	m.mu.RUnlock()

	now := m.now()
	out := make([]*Usage, 0, len(users))
	for _, u := range users {
		out = append(out, m.usageOf(u, now))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Email < out[j].Email })
	return out
}

func (m *Manager) usageOf(u *userState, now time.Time) *Usage {
	eff := u.eff.Load()
	usage := &Usage{
		Email:                u.email,
		Uplink:               u.up.Load(),
		Downlink:             u.down.Load(),
		EffectiveUplinkBps:   uint64(max64(eff.limits.UplinkBPS, 0)),
		EffectiveDownlinkBps: uint64(max64(eff.limits.DownlinkBPS, 0)),
		Blocked:              eff.blocked,
		BlockedReason:        eff.reason,
	}

	u.mu.Lock()
	usage.Connections = uint32(u.conns)
	for _, iu := range u.inbounds {
		usage.Inbounds = append(usage.Inbounds, &InboundUsage{
			Tag: iu.tag, Uplink: iu.up.Load(), Downlink: iu.down.Load(),
		})
	}
	for _, d := range u.devices {
		usage.Devices = append(usage.Devices, &Device{
			Key:         d.key,
			FirstSeen:   d.firstSeen / int64(time.Second),
			LastSeen:    d.lastSeen.Load() / int64(time.Second),
			Connections: uint32(d.conns),
		})
	}
	for _, q := range u.quotas {
		usage.Quotas = append(usage.Quotas, q.state(now))
	}
	u.mu.Unlock()

	sort.Slice(usage.Inbounds, func(i, j int) bool { return usage.Inbounds[i].Tag < usage.Inbounds[j].Tag })
	sort.Slice(usage.Devices, func(i, j int) bool { return usage.Devices[i].Key < usage.Devices[j].Key })
	return usage
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// AddUsage adds to a user's counters without any traffic having crossed the
// server.
//
// It exists for reconciliation: the panel is the ultimate source of truth for
// what a subscription has spent, and after a state file is lost, a server is
// rebuilt, or a user is moved between servers, this is how the month-to-date
// figure gets restored. An empty inboundTag adds to the user's total without
// attributing it to any inbound.
//
// The bytes count towards every quota they would have counted towards had they
// really been carried, so restoring a baseline can immediately exhaust a quota
// — which is the point.
func (m *Manager) AddUsage(email, inboundTag string, up, down uint64) error {
	if email == "" {
		return errors.New("tariff: email must not be empty")
	}
	now := m.now()
	u, created := m.userFor(email, 0, now)
	if created {
		m.recompute(u)
	}

	u.up.Add(up)
	u.down.Add(down)
	if inboundTag != "" {
		u.inbound(inboundTag).add(up, down)
	}

	total := up + down
	var exceeded []*quotaBucket
	u.mu.Lock()
	buckets := append([]*quotaBucket(nil), u.quotas...)
	u.mu.Unlock()
	for _, q := range buckets {
		if !q.matches(inboundTag) {
			continue
		}
		if _, hit := q.add(total); hit {
			exceeded = append(exceeded, q)
		}
	}
	for _, q := range exceeded {
		m.quotaExceeded(u, q)
	}
	if b := m.serverBucket.Load(); b != nil {
		if _, hit := b.add(total); hit {
			m.serverQuotaExceeded(b)
		}
	}
	m.markDirty()
	return nil
}

// ResetUsage clears a user's spent bytes and re-arms every quota they hold.
//
// This is how a subscription that renews on its own billing day is served:
// leave the quotas on a LIFETIME window and call this on the day, so the
// billing calendar stays in the billing system rather than being reimplemented
// here.
func (m *Manager) ResetUsage(email string) error {
	m.mu.RLock()
	u := m.users[email]
	m.mu.RUnlock()
	if u == nil {
		return errors.New("tariff: no state for ", email)
	}

	u.up.Store(0)
	u.down.Store(0)

	now := m.now()
	u.mu.Lock()
	for _, iu := range u.inbounds {
		iu.up.Store(0)
		iu.down.Store(0)
	}
	quotas := append([]*quotaBucket(nil), u.quotas...)
	u.mu.Unlock()

	for _, q := range quotas {
		q.reset(now)
		m.events.publish(&Event{
			UnixNano: now.UnixNano(), Kind: EventKind_EVENT_QUOTA_RESET,
			Email: email, InboundTag: q.spec.GetInboundTag(),
			Limit: q.spec.GetLimitBytes(),
		})
	}
	m.recompute(u)
	m.markDirty()
	return nil
}

// ResetServerUsage clears the server-wide allowance's counter.
func (m *Manager) ResetServerUsage() {
	b := m.serverBucket.Load()
	if b == nil {
		return
	}
	now := m.now()
	b.reset(now)
	m.events.publish(&Event{
		UnixNano: now.UnixNano(), Kind: EventKind_EVENT_SERVER_QUOTA_RESET,
		Limit: b.spec.GetLimitBytes(),
	})
	m.recomputeAll()
	m.markDirty()
}

func (iu *inboundUsage) add(up, down uint64) {
	iu.up.Add(up)
	iu.down.Add(down)
}
