// Package tariff is the fork's per-user policy and usage store: who is on
// which plan, how much they have spent against their quotas, how many devices
// they have connected, and what should happen when any of that runs out.
//
// It is the source of truth the data plane consults, and the thing an external
// panel drives over gRPC (see app/tariff/command). Two properties shape the
// whole design:
//
//   - It owns its own counters. Reading Xray's stats counters instead would be
//     less code, but a panel calling QueryStats with reset=true — which is how
//     most of them collect traffic — would wipe the numbers a monthly quota is
//     enforced from, and nobody would find out until the month was over.
//
//   - It keeps enforcing when the panel is down. Policies and spent bytes are
//     held in memory and snapshotted to disk, so a restart does not hand every
//     user a fresh allowance and an unreachable panel does not mean an
//     unmetered server. The panel pushes changes; it is never in the path of a
//     packet.
//
// # Configuration
//
// There is no JSON config key. The state file's path comes from the
// XRAY_TARIFF_STATE environment variable, and everything else — plans, quotas,
// the server allowance — is pushed over gRPC and persisted. That is both what a
// VPN service actually wants (the panel owns this, not a file on the host) and
// what keeps this feature out of upstream's config structs entirely.
package tariff

import (
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xtls/xray-core/app/dispatcher"
	"github.com/xtls/xray-core/common/errors"
	"github.com/xtls/xray-core/common/shaper"
)

// StatePathEnv names the environment variable holding the state file's path.
// Empty or unset disables persistence.
const StatePathEnv = "XRAY_TARIFF_STATE"

const (
	// defaultFlushInterval is how often a changed state is written to disk. A
	// hard kill therefore loses at most this much accounting, which for a
	// monthly quota is noise.
	defaultFlushInterval = 10 * time.Second

	// defaultThrottleBPS is where ACTION_THROTTLE lands when a quota does not
	// name a rate: 1 Mbit/s. Slow, but a user who has run out of allowance can
	// still load a page and see the message telling them why.
	defaultThrottleBPS = 1_000_000 / 8

	// sweepInterval is how often windows are rolled, expiries checked and
	// stale devices dropped.
	sweepInterval = 10 * time.Second

	// deviceRetention is how long a device is remembered after its last
	// connection closes. It keeps a client that reconnects every few seconds
	// from producing an event storm, and lets the API still report who was
	// recently on. Only connected devices count towards max_devices.
	deviceRetention = 5 * time.Minute
)

// Manager holds every user's policy and usage.
type Manager struct {
	// now is the manager's sense of time, fixed at construction. Tests in this
	// package replace it to drive month boundaries and expiry dates without
	// waiting for them; nothing else touches it, so it needs no
	// synchronisation.
	now func() time.Time

	cfg atomic.Pointer[Config]

	mu    sync.RWMutex
	users map[string]*userState

	serverMu     sync.Mutex
	serverQuota  *ServerQuota
	serverBucket atomic.Pointer[quotaBucket]

	events *bus

	dirty     atomic.Bool
	lastFlush atomic.Int64 // unix nanos
	nextSweep atomic.Int64 // unix nanos
	sweeping  atomic.Bool
	flushMu   sync.Mutex
}

// New returns a Manager. A nil cfg uses the defaults.
func New(cfg *Config) *Manager {
	m := &Manager{
		now:    time.Now,
		users:  make(map[string]*userState),
		events: newBus(),
	}
	if cfg == nil {
		cfg = &Config{}
	}
	if cfg.GetStatePath() == "" {
		cfg.StatePath = os.Getenv(StatePathEnv)
	}
	m.cfg.Store(cfg)
	m.nextSweep.Store(m.now().Add(sweepInterval).UnixNano())
	return m
}

var (
	defaultOnce sync.Once
	defaultMgr  *Manager
)

// Default returns the process-wide Manager, creating it on first use and
// loading any persisted state.
//
// Process-wide rather than per-Xray-instance, matching the shaper: a user's
// plan and the bytes they have spent are properties of the user, not of which
// core instance is serving them, and it keeps this feature reachable without
// threading a new feature through upstream's dispatcher.
func Default() *Manager {
	defaultOnce.Do(func() {
		defaultMgr = New(nil)
		if err := defaultMgr.Load(); err != nil {
			errors.LogWarningInner(nil, err, "tariff: could not load state")
		}
		defaultMgr.Install()
	})
	return defaultMgr
}

// Install wires the manager into the dispatcher, so that it supplies per-user
// limits to the shaper and meters every connection's traffic.
func (m *Manager) Install() {
	dispatcher.SetLimitsResolver(m.Limits)
	dispatcher.SetUsageTracker(func(email string, level uint32, inboundTag, device string) dispatcher.UsageMeter {
		return m.Attach(email, level, inboundTag, device)
	})
}

// --- configuration ---------------------------------------------------------

// Config returns the manager's configuration.
func (m *Manager) Config() *Config { return m.cfg.Load() }

// SetConfig replaces the configuration and re-evaluates every user against it.
func (m *Manager) SetConfig(cfg *Config) {
	if cfg == nil {
		cfg = &Config{}
	}
	m.cfg.Store(cfg)
	m.SetServerQuota(cfg.GetServerQuota())
	m.recomputeAll()
	m.markDirty()
}

func (m *Manager) defaultPolicy() *Policy {
	if p := m.cfg.Load().GetDefaultPolicy(); p != nil {
		return p
	}
	return &Policy{}
}

// baseLimits is the plan a user starts from, before quotas take anything away.
//
// The three-step fallback matters: a user with their own policy gets it, a
// user without one gets the configured default, and a user with neither falls
// back to the dispatcher's level table. That last step is what keeps enabling
// this store from silently unshaping everyone the panel has not pushed yet —
// the store overrides the level table per user, it does not replace it.
func (m *Manager) baseLimits(email string, level uint32, p *Policy) shaper.Limits {
	if p != nil {
		return limitsFrom(p)
	}
	if dp := m.cfg.Load().GetDefaultPolicy(); dp != nil {
		return limitsFrom(dp)
	}
	return dispatcher.TierLimits(shaper.User{Email: email, Level: level})
}

func (m *Manager) throttleBPS(quotaValue uint64) int64 {
	if quotaValue > 0 {
		return int64(quotaValue)
	}
	if v := m.cfg.Load().GetDefaultThrottleBps(); v > 0 {
		return int64(v)
	}
	return defaultThrottleBPS
}

func (m *Manager) flushInterval() time.Duration {
	if s := m.cfg.Load().GetFlushSeconds(); s > 0 {
		return time.Duration(s) * time.Second
	}
	return defaultFlushInterval
}

// --- policies --------------------------------------------------------------

// SetPolicy installs a user's plan, taking effect on their live connections.
func (m *Manager) SetPolicy(p *Policy) error {
	if p.GetEmail() == "" {
		return errors.New("tariff: policy email must not be empty")
	}
	now := m.now()
	u, _ := m.userFor(p.GetEmail(), 0, now)

	u.mu.Lock()
	u.setPolicyLocked(p, now)
	u.mu.Unlock()

	m.recompute(u)
	m.markDirty()
	m.events.publish(&Event{
		UnixNano: now.UnixNano(),
		Kind:     EventKind_EVENT_POLICY_CHANGED,
		Email:    p.GetEmail(),
	})
	return nil
}

// RemovePolicy forgets a user entirely — their plan, their spent bytes and
// their devices. Live connections fall back to the default plan.
func (m *Manager) RemovePolicy(email string) error {
	m.mu.Lock()
	_, ok := m.users[email]
	delete(m.users, email)
	m.mu.Unlock()

	if !ok {
		return errors.New("tariff: no policy for ", email)
	}
	m.markDirty()
	m.events.publish(newEvent(m.now(), EventKind_EVENT_POLICY_REMOVED, email))
	dispatcher.InvalidateUserLimits(email)
	return nil
}

// Policy returns a user's plan, or nil if they are on the default.
func (m *Manager) Policy(email string) *Policy {
	m.mu.RLock()
	u := m.users[email]
	m.mu.RUnlock()
	if u == nil {
		return nil
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.policy
}

// Policies returns every plan the manager holds.
func (m *Manager) Policies() []*Policy {
	m.mu.RLock()
	users := make([]*userState, 0, len(m.users))
	for _, u := range m.users {
		users = append(users, u)
	}
	m.mu.RUnlock()

	out := make([]*Policy, 0, len(users))
	for _, u := range users {
		u.mu.Lock()
		p := u.policy
		u.mu.Unlock()
		if p != nil {
			out = append(out, p)
		}
	}
	return out
}

// userFor returns a user's state, creating it if needed, and reports whether
// it had to create it. Callers act on that outside the manager lock:
// recomputing enforcement takes other locks, and doing it here would nest them
// under this one.
func (m *Manager) userFor(email string, level uint32, now time.Time) (*userState, bool) {
	m.mu.RLock()
	u := m.users[email]
	m.mu.RUnlock()
	if u != nil {
		if level > 0 {
			u.level.Store(level)
		}
		return u, false
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if u = m.users[email]; u != nil {
		return u, false
	}
	u = newUserState(email, level, now)
	m.users[email] = u
	return u, true
}

// --- enforcement -----------------------------------------------------------

// Limits implements the shaper's resolver: the caps a user gets right now,
// after their plan, their quotas and their expiry have all had their say.
func (m *Manager) Limits(su shaper.User) shaper.Limits {
	m.mu.RLock()
	u := m.users[su.Email]
	m.mu.RUnlock()
	if u == nil {
		return m.baseLimits(su.Email, su.Level, nil)
	}
	return u.eff.Load().limits
}

// recompute rebuilds one user's enforcement and republishes it to the shaper.
func (m *Manager) recompute(u *userState) {
	now := m.now()

	u.mu.Lock()
	p := u.policy
	eff := &effective{limits: m.baseLimits(u.email, u.level.Load(), p)}
	switch {
	case p.GetDisabled():
		eff.blocked, eff.reason = true, "disabled"
	case p.GetExpiresAt() > 0 && now.Unix() >= p.GetExpiresAt():
		eff.blocked, eff.reason = true, "expired"
	}
	if !eff.blocked {
		for _, q := range u.quotas {
			if q.isExceeded() {
				applyAction(eff, q.spec.GetAction(), m.throttleBPS(q.spec.GetThrottleBps()), quotaReason(q.spec))
			}
		}
	}
	u.mu.Unlock()

	// The server allowance applies on top of everyone's own, and is read
	// outside the user lock because it is shared.
	if !eff.blocked {
		if b := m.serverBucket.Load(); b != nil && b.isExceeded() {
			m.serverMu.Lock()
			sq := m.serverQuota
			m.serverMu.Unlock()
			applyAction(eff, sq.GetAction(), m.throttleBPS(sq.GetThrottleBps()), "server quota")
		}
	}

	u.eff.Store(eff)
	dispatcher.InvalidateUserLimits(u.email)
}

func (m *Manager) recomputeAll() {
	m.mu.RLock()
	users := make([]*userState, 0, len(m.users))
	for _, u := range m.users {
		users = append(users, u)
	}
	m.mu.RUnlock()
	for _, u := range users {
		m.recompute(u)
	}
}

func quotaReason(q *Quota) string {
	if tag := q.GetInboundTag(); tag != "" {
		return "quota:" + tag
	}
	return "quota"
}

// --- server allowance ------------------------------------------------------

// SetServerQuota installs the allowance for the server as a whole, counting
// every user's traffic together. A nil quota removes it.
func (m *Manager) SetServerQuota(sq *ServerQuota) {
	m.serverMu.Lock()
	old := m.serverBucket.Load()
	m.serverQuota = sq
	switch {
	case sq == nil || sq.GetLimitBytes() == 0:
		m.serverBucket.Store(nil)
	case old != nil && sameTerms(old.spec, quotaFromServer(sq)):
		// Terms unchanged: keep the bytes already spent this window.
		old.spec = quotaFromServer(sq)
	default:
		m.serverBucket.Store(newQuotaBucket(quotaFromServer(sq), m.now()))
	}
	m.serverMu.Unlock()

	m.recomputeAll()
	m.markDirty()
}

// ServerQuota returns the server-wide allowance, or nil.
func (m *Manager) ServerQuota() *ServerQuota {
	m.serverMu.Lock()
	defer m.serverMu.Unlock()
	return m.serverQuota
}

// ServerUsage returns the server-wide allowance's state, or nil if there is
// none.
func (m *Manager) ServerUsage() *QuotaState {
	if b := m.serverBucket.Load(); b != nil {
		return b.state(m.now())
	}
	return nil
}

func quotaFromServer(sq *ServerQuota) *Quota {
	return &Quota{
		LimitBytes:  sq.GetLimitBytes(),
		Window:      sq.GetWindow(),
		Action:      sq.GetAction(),
		ThrottleBps: sq.GetThrottleBps(),
		WarnPercent: sq.GetWarnPercent(),
	}
}

// --- events ----------------------------------------------------------------

// Subscribe returns a channel of events and the id that stops it. The channel
// is closed by Unsubscribe.
func (m *Manager) Subscribe() (uint64, <-chan *Event, *atomic.Uint64) {
	id, s := m.events.subscribe()
	return id, s.ch, &s.dropped
}

// Unsubscribe stops an event subscription.
func (m *Manager) Unsubscribe(id uint64) { m.events.unsubscribe(id) }

// Subscribers reports how many event streams are open.
func (m *Manager) Subscribers() int { return m.events.subscribers() }

func (m *Manager) markDirty() {
	// Load before Store: this is called from the write path, and repeatedly
	// storing true into a shared word would bounce its cache line between
	// every core doing I/O. A shared read costs nothing once the line is
	// already there.
	if !m.dirty.Load() {
		m.dirty.Store(true)
	}
}
