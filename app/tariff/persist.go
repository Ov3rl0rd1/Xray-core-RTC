package tariff

import (
	"os"
	"path/filepath"
	"time"

	"github.com/xtls/xray-core/common/errors"
	"google.golang.org/protobuf/encoding/protojson"
)

// stateVersion is bumped when the on-disk shape changes incompatibly. A file
// from a future version is refused rather than half-read: starting with an
// empty quota ledger is a bandwidth bill, and it should be a loud failure.
const stateVersion = 1

// Load reads the state file, if one is configured. A missing file is not an
// error — that is simply a server that has not run before.
func (m *Manager) Load() error {
	path := m.cfg.Load().GetStatePath()
	if path == "" {
		return nil
	}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return errors.New("tariff: reading ", path).Base(err)
	}

	var st State
	if err := protojson.Unmarshal(raw, &st); err != nil {
		return errors.New("tariff: parsing ", path).Base(err)
	}
	if st.GetVersion() > stateVersion {
		return errors.New("tariff: ", path, " was written by a newer version (",
			st.GetVersion(), " > ", stateVersion, "); refusing to start from a partial ledger")
	}
	m.restore(&st)
	return nil
}

// restore rebuilds the manager from a decoded state file.
func (m *Manager) restore(st *State) {
	now := m.now()

	if cfg := st.GetConfig(); cfg != nil {
		// The path the file was found at wins over the one recorded in it: the
		// operator moved the file, and the file should not argue.
		path := m.cfg.Load().GetStatePath()
		cfg.StatePath = path
		m.cfg.Store(cfg)
	}
	if sq := st.GetServerQuota(); sq != nil {
		m.serverMu.Lock()
		m.serverQuota = sq
		if sq.GetLimitBytes() > 0 {
			b := newQuotaBucket(quotaFromServer(sq), now)
			restoreBucket(b, st.GetServerUsed(), st.GetServerWindowStarted(),
				st.GetServerExceeded(), st.GetServerWarned(), now)
			m.serverBucket.Store(b)
		}
		m.serverMu.Unlock()
	}

	for email, us := range st.GetUsers() {
		u := newUserState(email, 0, now)
		u.up.Store(us.GetUplink())
		u.down.Store(us.GetDownlink())
		for _, iu := range us.GetInbounds() {
			c := &inboundUsage{tag: iu.GetTag()}
			c.up.Store(iu.GetUplink())
			c.down.Store(iu.GetDownlink())
			u.inbounds[iu.GetTag()] = c
		}
		if p := us.GetPolicy(); p != nil {
			u.setPolicyLocked(p, now)
			// Re-attach the spent bytes to the buckets setPolicyLocked just
			// built, matching on terms. A quota whose terms changed while the
			// server was down is a different allowance and starts clean.
			for _, spent := range us.GetQuotas() {
				for _, b := range u.quotas {
					if sameTerms(b.spec, spent.GetSpec()) {
						restoreBucket(b, spent.GetUsed(), spent.GetWindowStarted(),
							spent.GetExceeded(), spent.GetWarned(), now)
						break
					}
				}
			}
		}
		if ls := us.GetLastSeen(); ls > 0 {
			u.lastSeen = time.Unix(ls, 0)
		}
		m.mu.Lock()
		m.users[email] = u
		m.mu.Unlock()
	}

	m.recomputeAll()
}

// restoreBucket puts a persisted counter back, unless the window it was
// counted in has since turned over — in which case the allowance is genuinely
// fresh and the bytes are dropped.
func restoreBucket(b *quotaBucket, used uint64, windowStarted int64, exceeded, warned bool, now time.Time) {
	start, _ := windowBounds(b.spec.GetWindow(), now)
	if !start.IsZero() && windowStarted != start.Unix() {
		return
	}
	b.used.Store(used)
	b.exceeded.Store(exceeded)
	b.warned.Store(warned)
	if windowStarted > 0 {
		b.windowStart.Store(windowStarted)
	}
}

// snapshot renders the manager's persistable state.
func (m *Manager) snapshot() *State {
	st := &State{
		Version: stateVersion,
		SavedAt: m.now().Unix(),
		Config:  m.cfg.Load(),
		Users:   make(map[string]*UserState),
	}

	m.serverMu.Lock()
	st.ServerQuota = m.serverQuota
	m.serverMu.Unlock()
	if b := m.serverBucket.Load(); b != nil {
		st.ServerUsed = b.used.Load()
		st.ServerWindowStarted = b.windowStart.Load()
		st.ServerExceeded = b.exceeded.Load()
		st.ServerWarned = b.warned.Load()
	}

	m.mu.RLock()
	users := make([]*userState, 0, len(m.users))
	for _, u := range m.users {
		users = append(users, u)
	}
	m.mu.RUnlock()

	for _, u := range users {
		us := &UserState{
			Uplink:   u.up.Load(),
			Downlink: u.down.Load(),
		}
		u.mu.Lock()
		us.Policy = u.policy
		us.LastSeen = u.lastSeen.Unix()
		for _, iu := range u.inbounds {
			us.Inbounds = append(us.Inbounds, &InboundUsage{
				Tag: iu.tag, Uplink: iu.up.Load(), Downlink: iu.down.Load(),
			})
		}
		for _, q := range u.quotas {
			us.Quotas = append(us.Quotas, &QuotaSpent{
				Spec:          q.spec,
				Used:          q.used.Load(),
				WindowStarted: q.windowStart.Load(),
				Exceeded:      q.exceeded.Load(),
				Warned:        q.warned.Load(),
			})
		}
		u.mu.Unlock()
		st.Users[u.email] = us
	}
	return st
}

// Flush writes the state file. It is safe to call concurrently; only one write
// happens at a time.
func (m *Manager) Flush() error {
	path := m.cfg.Load().GetStatePath()
	if path == "" {
		return nil
	}

	m.flushMu.Lock()
	defer m.flushMu.Unlock()

	raw, err := protojson.MarshalOptions{Multiline: true, Indent: "  "}.Marshal(m.snapshot())
	if err != nil {
		return errors.New("tariff: encoding state").Base(err)
	}

	// Write to a sibling and rename over the target. A half-written ledger is
	// worse than no ledger, and rename within a directory is atomic on every
	// filesystem this runs on.
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return errors.New("tariff: creating ", dir).Base(err)
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*")
	if err != nil {
		return errors.New("tariff: creating a temporary file in ", dir).Base(err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename has succeeded

	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return errors.New("tariff: writing ", tmpName).Base(err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return errors.New("tariff: syncing ", tmpName).Base(err)
	}
	if err := tmp.Close(); err != nil {
		return errors.New("tariff: closing ", tmpName).Base(err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return errors.New("tariff: renaming over ", path).Base(err)
	}
	m.dirty.Store(false)
	m.lastFlush.Store(m.now().UnixNano())
	return nil
}

// maybeFlush writes the state file if anything has changed and enough time has
// passed since the last write.
func (m *Manager) maybeFlush(now time.Time) {
	if !m.dirty.Load() || m.cfg.Load().GetStatePath() == "" {
		return
	}
	if now.UnixNano()-m.lastFlush.Load() < int64(m.flushInterval()) {
		return
	}
	if err := m.Flush(); err != nil {
		errors.LogWarningInner(nil, err, "tariff: could not write state")
	}
}
