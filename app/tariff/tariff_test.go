package tariff

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/xtls/xray-core/common/shaper"
)

// clock drives the manager's sense of time so that month boundaries, expiry
// dates and device retention can be tested without waiting for them.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock(t time.Time) *clock { return &clock{t: t} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// midMonth is a time comfortably inside a month, so that advancing by a few
// days in a test does not accidentally cross a boundary.
var midMonth = time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

func newTestManager(t *testing.T, cfg *Config) (*Manager, *clock) {
	t.Helper()
	c := newClock(midMonth)
	m := New(cfg)
	m.now = c.Now
	return m, c
}

const (
	kb = 1024
	mb = 1024 * kb
	gb = 1024 * mb
)

// --- window arithmetic -----------------------------------------------------

func TestWindowBoundsAlignToTheCalendar(t *testing.T) {
	at := time.Date(2026, 6, 17, 13, 45, 30, 0, time.UTC) // a Wednesday

	cases := []struct {
		w                  Window
		wantStart, wantEnd time.Time
	}{
		{Window_WINDOW_DAY,
			time.Date(2026, 6, 17, 0, 0, 0, 0, time.UTC),
			time.Date(2026, 6, 18, 0, 0, 0, 0, time.UTC)},
		{Window_WINDOW_WEEK, // ISO weeks start Monday
			time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC),
			time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC)},
		{Window_WINDOW_MONTH,
			time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
			time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)},
	}
	for _, c := range cases {
		start, end := windowBounds(c.w, at)
		if !start.Equal(c.wantStart) || !end.Equal(c.wantEnd) {
			t.Errorf("%v: got [%v, %v), want [%v, %v)", c.w, start, end, c.wantStart, c.wantEnd)
		}
	}

	if start, end := windowBounds(Window_WINDOW_LIFETIME, at); !start.IsZero() || !end.IsZero() {
		t.Errorf("lifetime window has bounds [%v, %v), want none", start, end)
	}
}

func TestWeekBoundsOnSunday(t *testing.T) {
	// Go puts Sunday at weekday 0, which is the off-by-one this guards.
	sunday := time.Date(2026, 6, 21, 6, 0, 0, 0, time.UTC)
	start, _ := windowBounds(Window_WINDOW_WEEK, sunday)
	want := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
	if !start.Equal(want) {
		t.Fatalf("week containing Sunday started %v, want %v", start, want)
	}
}

func TestWindowBoundsUseUTCNotLocalTime(t *testing.T) {
	// Same instant, two zones: the month must not depend on the host's.
	zone := time.FixedZone("UTC+13", 13*3600)
	at := time.Date(2026, 6, 30, 23, 0, 0, 0, time.UTC)
	a, _ := windowBounds(Window_WINDOW_MONTH, at)
	b, _ := windowBounds(Window_WINDOW_MONTH, at.In(zone))
	if !a.Equal(b) {
		t.Fatalf("month depends on the zone: %v vs %v", a, b)
	}
}

// --- quota buckets ---------------------------------------------------------

func TestQuotaWarnsOnceThenExceedsOnce(t *testing.T) {
	b := newQuotaBucket(&Quota{LimitBytes: 1000, WarnPercent: 90}, midMonth)

	if warned, exceeded := b.add(500); warned || exceeded {
		t.Fatalf("at 50%%: warned=%v exceeded=%v, want neither", warned, exceeded)
	}
	if warned, _ := b.add(400); !warned {
		t.Fatal("at 90% the quota should have warned")
	}
	if warned, _ := b.add(10); warned {
		t.Fatal("the warning should only fire once per window")
	}
	if _, exceeded := b.add(100); !exceeded {
		t.Fatal("past the limit the quota should have reported exceeded")
	}
	if _, exceeded := b.add(100); exceeded {
		t.Fatal("exceeded should only fire once per window")
	}
}

func TestQuotaWithNoLimitIsAPureMeter(t *testing.T) {
	b := newQuotaBucket(&Quota{}, midMonth)
	if warned, exceeded := b.add(1 << 40); warned || exceeded {
		t.Fatalf("a quota with no limit fired: warned=%v exceeded=%v", warned, exceeded)
	}
	if b.isExceeded() {
		t.Fatal("a quota with no limit can never be exceeded")
	}
	if got := b.used.Load(); got != 1<<40 {
		t.Fatalf("used = %d, want the bytes to still be counted", got)
	}
}

func TestQuotaRollsWithTheCalendar(t *testing.T) {
	b := newQuotaBucket(&Quota{LimitBytes: 1000, Window: Window_WINDOW_MONTH}, midMonth)
	b.add(1000)
	if !b.isExceeded() {
		t.Fatal("quota should be exceeded")
	}

	if b.roll(midMonth.AddDate(0, 0, 3)) {
		t.Fatal("the quota rolled without the month changing")
	}
	if !b.roll(midMonth.AddDate(0, 1, 0)) {
		t.Fatal("the quota did not roll into the next month")
	}
	if b.isExceeded() || b.used.Load() != 0 {
		t.Fatalf("after rolling: exceeded=%v used=%d, want a clean window", b.isExceeded(), b.used.Load())
	}
}

func TestLifetimeQuotaNeverRolls(t *testing.T) {
	b := newQuotaBucket(&Quota{LimitBytes: 1000, Window: Window_WINDOW_LIFETIME}, midMonth)
	b.add(1000)
	if b.roll(midMonth.AddDate(1, 0, 0)) {
		t.Fatal("a lifetime quota rolled after a year")
	}
	if !b.isExceeded() {
		t.Fatal("a lifetime quota forgot it was exceeded")
	}
}

// --- policies and enforcement ---------------------------------------------

func TestPolicySetsTheShapersLimits(t *testing.T) {
	m, _ := newTestManager(t, nil)
	if err := m.SetPolicy(&Policy{
		Email: "a@example", UplinkBps: 1 * mb, DownlinkBps: 2 * mb,
	}); err != nil {
		t.Fatal(err)
	}

	l := m.Limits(shaper.User{Email: "a@example"})
	if l.UplinkBPS != 1*mb || l.DownlinkBPS != 2*mb {
		t.Fatalf("limits = %d/%d, want %d/%d", l.UplinkBPS, l.DownlinkBPS, 1*mb, 2*mb)
	}
}

func TestUnknownUserGetsTheDefaultPolicy(t *testing.T) {
	m, _ := newTestManager(t, &Config{
		DefaultPolicy: &Policy{UplinkBps: 7 * mb, DownlinkBps: 7 * mb},
	})
	if got := m.Limits(shaper.User{Email: "nobody@example"}).DownlinkBPS; got != 7*mb {
		t.Fatalf("default limits = %d, want %d", got, 7*mb)
	}
}

func TestDisabledAndExpiredUsersAreBlocked(t *testing.T) {
	m, c := newTestManager(t, nil)

	if err := m.SetPolicy(&Policy{Email: "off@example", Disabled: true}); err != nil {
		t.Fatal(err)
	}
	mt := m.Attach("off@example", 0, "in", "1.2.3.4")
	if !mt.Blocked() || mt.BlockReason() != "disabled" {
		t.Fatalf("disabled user: blocked=%v reason=%q", mt.Blocked(), mt.BlockReason())
	}

	expiry := c.Now().Add(time.Hour)
	if err := m.SetPolicy(&Policy{Email: "soon@example", ExpiresAt: expiry.Unix()}); err != nil {
		t.Fatal(err)
	}
	live := m.Attach("soon@example", 0, "in", "1.2.3.4")
	if live.Blocked() {
		t.Fatal("a user whose subscription has not expired yet was blocked")
	}

	c.Set(expiry.Add(time.Minute))
	m.sweep(c.Now())
	if !live.Blocked() || live.BlockReason() != "expired" {
		t.Fatalf("after expiry: blocked=%v reason=%q", live.Blocked(), live.BlockReason())
	}
}

func TestQuotaThrottleLowersTheLimitsAndDropsTheBoost(t *testing.T) {
	m, _ := newTestManager(t, nil)
	if err := m.SetPolicy(&Policy{
		Email: "t@example", UplinkBps: 10 * mb, DownlinkBps: 10 * mb,
		BoostBytes: 1 * gb, BoostCeilBps: 50 * mb,
		Quotas: []*Quota{{
			LimitBytes: 1000, Action: Action_ACTION_THROTTLE, ThrottleBps: 64 * kb,
		}},
	}); err != nil {
		t.Fatal(err)
	}

	mt := m.Attach("t@example", 0, "in", "1.2.3.4")
	if l := m.Limits(shaper.User{Email: "t@example"}); l.DownlinkBPS != 10*mb {
		t.Fatalf("before the quota ran out: %d B/s, want the full plan", l.DownlinkBPS)
	}

	mt.Count(shaper.Down, 1000)

	l := m.Limits(shaper.User{Email: "t@example"})
	if l.DownlinkBPS != 64*kb || l.UplinkBPS != 64*kb {
		t.Fatalf("after the quota ran out: %d/%d B/s, want %d in both directions",
			l.UplinkBPS, l.DownlinkBPS, 64*kb)
	}
	if l.BoostBytes != 0 {
		t.Fatal("a user paying off a quota should not also be handed a speed boost")
	}
	if mt.Blocked() {
		t.Fatal("a throttled user must stay connected")
	}
}

func TestQuotaBlockRefusesTrafficMidTransfer(t *testing.T) {
	m, _ := newTestManager(t, nil)
	if err := m.SetPolicy(&Policy{
		Email:  "b@example",
		Quotas: []*Quota{{LimitBytes: 1000, Action: Action_ACTION_BLOCK}},
	}); err != nil {
		t.Fatal(err)
	}

	mt := m.Attach("b@example", 0, "in", "1.2.3.4")
	mt.Count(shaper.Down, 999)
	if mt.Blocked() {
		t.Fatal("blocked before the quota was spent")
	}
	mt.Count(shaper.Down, 2)
	if !mt.Blocked() {
		t.Fatal("the connection that spent the quota was not stopped")
	}
}

func TestQuotaNotifyChangesNothing(t *testing.T) {
	m, _ := newTestManager(t, nil)
	if err := m.SetPolicy(&Policy{
		Email: "n@example", DownlinkBps: 10 * mb,
		Quotas: []*Quota{{LimitBytes: 1000, Action: Action_ACTION_NOTIFY}},
	}); err != nil {
		t.Fatal(err)
	}
	_, events, _ := m.Subscribe()

	mt := m.Attach("n@example", 0, "in", "1.2.3.4")
	mt.Count(shaper.Down, 2000)

	if mt.Blocked() {
		t.Fatal("ACTION_NOTIFY must not block")
	}
	if got := m.Limits(shaper.User{Email: "n@example"}).DownlinkBPS; got != 10*mb {
		t.Fatalf("ACTION_NOTIFY changed the limits to %d", got)
	}
	if !hasEvent(events, EventKind_EVENT_QUOTA_EXCEEDED) {
		t.Fatal("ACTION_NOTIFY should still announce that the quota ran out")
	}
}

// TestPerInboundQuotaCountsOnlyItsInbound is the "twenty gigabytes on the
// profile that bypasses the whitelist, twenty terabytes overall" case.
func TestPerInboundQuotaCountsOnlyItsInbound(t *testing.T) {
	m, _ := newTestManager(t, nil)
	if err := m.SetPolicy(&Policy{
		Email: "p@example",
		Quotas: []*Quota{
			{InboundTag: "full-tunnel", LimitBytes: 20 * kb, Action: Action_ACTION_BLOCK},
			{LimitBytes: 20 * mb, Action: Action_ACTION_BLOCK},
		},
	}); err != nil {
		t.Fatal(err)
	}

	split := m.Attach("p@example", 0, "split-tunnel", "1.2.3.4")
	full := m.Attach("p@example", 0, "full-tunnel", "1.2.3.4")

	// Traffic on the unrestricted profile must not spend the restricted one's
	// allowance.
	split.Count(shaper.Down, 1*mb)
	if full.Blocked() || split.Blocked() {
		t.Fatal("split-tunnel traffic spent the full-tunnel allowance")
	}

	full.Count(shaper.Down, 21*kb)
	if !full.Blocked() {
		t.Fatal("the full-tunnel profile went over its allowance and was not blocked")
	}

	usage := m.Usage("p@example")
	if len(usage.GetInbounds()) != 2 {
		t.Fatalf("usage reported %d inbounds, want 2", len(usage.GetInbounds()))
	}
	byTag := map[string]uint64{}
	for _, iu := range usage.GetInbounds() {
		byTag[iu.GetTag()] = iu.GetDownlink()
	}
	if byTag["split-tunnel"] != 1*mb || byTag["full-tunnel"] != 21*kb {
		t.Fatalf("per-inbound usage = %v, want the two profiles counted apart", byTag)
	}
}

func TestServerQuotaAppliesToEveryone(t *testing.T) {
	m, _ := newTestManager(t, nil)
	for _, e := range []string{"a@example", "b@example"} {
		if err := m.SetPolicy(&Policy{Email: e, DownlinkBps: 10 * mb}); err != nil {
			t.Fatal(err)
		}
	}
	m.SetServerQuota(&ServerQuota{
		LimitBytes: 1000, Action: Action_ACTION_THROTTLE, ThrottleBps: 32 * kb,
	})

	a := m.Attach("a@example", 0, "in", "1.1.1.1")
	m.Attach("b@example", 0, "in", "2.2.2.2")
	a.Count(shaper.Down, 1001)

	for _, e := range []string{"a@example", "b@example"} {
		if got := m.Limits(shaper.User{Email: e}).DownlinkBPS; got != 32*kb {
			t.Fatalf("%s: %d B/s after the server allowance ran out, want %d", e, got, 32*kb)
		}
	}
	if st := m.ServerUsage(); st == nil || !st.GetExceeded() {
		t.Fatal("server usage does not report the allowance as exceeded")
	}
}

// --- devices ---------------------------------------------------------------

func TestDeviceLimitNotifiesByDefault(t *testing.T) {
	m, _ := newTestManager(t, nil)
	if err := m.SetPolicy(&Policy{Email: "d@example", MaxDevices: 2}); err != nil {
		t.Fatal(err)
	}
	_, events, _ := m.Subscribe()

	m.Attach("d@example", 0, "in", "1.1.1.1")
	m.Attach("d@example", 0, "in", "2.2.2.2")
	third := m.Attach("d@example", 0, "in", "3.3.3.3")

	if third.Blocked() {
		t.Fatal("the default device-limit action must not lock a user out: a phone " +
			"changing cells would be enough to do it")
	}
	if !hasEvent(events, EventKind_EVENT_DEVICE_LIMIT_HIT) {
		t.Fatal("exceeding the device limit was not announced")
	}
}

func TestDeviceLimitBlocksOnlyTheExtraDevice(t *testing.T) {
	m, _ := newTestManager(t, nil)
	if err := m.SetPolicy(&Policy{
		Email: "d@example", MaxDevices: 2, DeviceLimitAction: Action_ACTION_BLOCK,
	}); err != nil {
		t.Fatal(err)
	}

	first := m.Attach("d@example", 0, "in", "1.1.1.1")
	second := m.Attach("d@example", 0, "in", "2.2.2.2")
	third := m.Attach("d@example", 0, "in", "3.3.3.3")

	if first.Blocked() || second.Blocked() {
		t.Fatal("devices already connected were blocked by a later one arriving")
	}
	if !third.Blocked() {
		t.Fatal("the device past the limit was admitted")
	}

	// A refused device must not count towards the limit either, or the cap
	// would ratchet down every time one retried.
	if got := m.deviceCount(m.users["d@example"]); got != 2 {
		t.Fatalf("device count = %d, want 2 — a refused device was counted", got)
	}
}

func TestDeviceFreesItsSlotWhenItDisconnects(t *testing.T) {
	m, _ := newTestManager(t, nil)
	if err := m.SetPolicy(&Policy{
		Email: "d@example", MaxDevices: 1, DeviceLimitAction: Action_ACTION_BLOCK,
	}); err != nil {
		t.Fatal(err)
	}

	first := m.Attach("d@example", 0, "in", "1.1.1.1")
	if blocked := m.Attach("d@example", 0, "in", "2.2.2.2").Blocked(); !blocked {
		t.Fatal("a second device was admitted with max_devices=1")
	}
	first.Release()

	if m.Attach("d@example", 0, "in", "2.2.2.2").Blocked() {
		t.Fatal("the slot was not freed when the first device disconnected")
	}
}

func TestReleaseIsIdempotent(t *testing.T) {
	m, _ := newTestManager(t, nil)
	mt := m.Attach("r@example", 0, "in", "1.1.1.1")
	other := m.Attach("r@example", 0, "in", "1.1.1.1")

	mt.Release()
	mt.Release()

	u := m.users["r@example"]
	u.mu.Lock()
	conns := u.conns
	u.mu.Unlock()
	if conns != 1 {
		t.Fatalf("connections = %d after a doubled Release, want 1", conns)
	}
	other.Release()
}

// --- policy updates --------------------------------------------------------

// TestPolicyUpdateKeepsSpentBytes is the property that stops a panel syncing
// policies on a timer from silently handing everyone a fresh allowance.
func TestPolicyUpdateKeepsSpentBytes(t *testing.T) {
	m, _ := newTestManager(t, nil)
	quota := &Quota{LimitBytes: 10000, Window: Window_WINDOW_MONTH, Action: Action_ACTION_BLOCK}
	policy := &Policy{Email: "k@example", DownlinkBps: 1 * mb, Quotas: []*Quota{quota}}
	if err := m.SetPolicy(policy); err != nil {
		t.Fatal(err)
	}

	mt := m.Attach("k@example", 0, "in", "1.1.1.1")
	mt.Count(shaper.Down, 8000)

	// Push the same quota again with a different rate on the plan.
	if err := m.SetPolicy(&Policy{
		Email: "k@example", DownlinkBps: 2 * mb,
		Quotas: []*Quota{{LimitBytes: 10000, Window: Window_WINDOW_MONTH, Action: Action_ACTION_BLOCK}},
	}); err != nil {
		t.Fatal(err)
	}

	if got := m.Usage("k@example").GetQuotas()[0].GetUsedBytes(); got != 8000 {
		t.Fatalf("spent bytes after a policy push = %d, want the 8000 carried over", got)
	}
	if got := m.Limits(shaper.User{Email: "k@example"}).DownlinkBPS; got != 2*mb {
		t.Fatalf("the new rate did not take effect: %d", got)
	}
}

func TestChangingAQuotasTermsStartsItClean(t *testing.T) {
	m, _ := newTestManager(t, nil)
	if err := m.SetPolicy(&Policy{
		Email:  "c@example",
		Quotas: []*Quota{{LimitBytes: 10000, Window: Window_WINDOW_MONTH}},
	}); err != nil {
		t.Fatal(err)
	}
	m.Attach("c@example", 0, "in", "1.1.1.1").Count(shaper.Down, 8000)

	// A different allowance, not the same one topped up.
	if err := m.SetPolicy(&Policy{
		Email:  "c@example",
		Quotas: []*Quota{{LimitBytes: 50000, Window: Window_WINDOW_MONTH}},
	}); err != nil {
		t.Fatal(err)
	}
	if got := m.Usage("c@example").GetQuotas()[0].GetUsedBytes(); got != 0 {
		t.Fatalf("a quota with new terms started at %d, want 0", got)
	}
}

func TestResetUsageClearsEverything(t *testing.T) {
	m, _ := newTestManager(t, nil)
	if err := m.SetPolicy(&Policy{
		Email:  "z@example",
		Quotas: []*Quota{{LimitBytes: 1000, Action: Action_ACTION_BLOCK}},
	}); err != nil {
		t.Fatal(err)
	}
	mt := m.Attach("z@example", 0, "in", "1.1.1.1")
	mt.Count(shaper.Down, 2000)
	if !mt.Blocked() {
		t.Fatal("setup: the user should be blocked")
	}

	if err := m.ResetUsage("z@example"); err != nil {
		t.Fatal(err)
	}
	if mt.Blocked() {
		t.Fatal("the user is still blocked after their usage was reset")
	}
	u := m.Usage("z@example")
	if u.GetDownlink() != 0 || u.GetQuotas()[0].GetUsedBytes() != 0 {
		t.Fatalf("after reset: total=%d quota=%d, want zeroes",
			u.GetDownlink(), u.GetQuotas()[0].GetUsedBytes())
	}
}

func TestAddUsageRestoresABaseline(t *testing.T) {
	m, _ := newTestManager(t, nil)
	if err := m.SetPolicy(&Policy{
		Email:  "r@example",
		Quotas: []*Quota{{LimitBytes: 1000, Action: Action_ACTION_BLOCK}},
	}); err != nil {
		t.Fatal(err)
	}

	// The panel tells the server this subscription has already spent its month.
	if err := m.AddUsage("r@example", "in", 600, 600); err != nil {
		t.Fatal(err)
	}
	if mt := m.Attach("r@example", 0, "in", "1.1.1.1"); !mt.Blocked() {
		t.Fatal("a restored baseline past the limit did not block the user")
	}
}

// --- persistence -----------------------------------------------------------

func TestStateSurvivesARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tariff.json")
	m, c := newTestManager(t, &Config{StatePath: path})

	if err := m.SetPolicy(&Policy{
		Email: "s@example", DownlinkBps: 3 * mb,
		Quotas: []*Quota{{LimitBytes: 10000, Window: Window_WINDOW_MONTH, Action: Action_ACTION_BLOCK}},
	}); err != nil {
		t.Fatal(err)
	}
	m.Attach("s@example", 0, "vless-in", "1.1.1.1").Count(shaper.Down, 7000)
	if err := m.Flush(); err != nil {
		t.Fatal(err)
	}

	// A new process, same file, same month.
	restarted := New(&Config{StatePath: path})
	restarted.now = c.Now
	if err := restarted.Load(); err != nil {
		t.Fatal(err)
	}

	u := restarted.Usage("s@example")
	if u == nil {
		t.Fatal("the user did not survive the restart")
	}
	if u.GetDownlink() != 7000 {
		t.Fatalf("downlink after restart = %d, want 7000", u.GetDownlink())
	}
	if got := u.GetQuotas()[0].GetUsedBytes(); got != 7000 {
		t.Fatalf("quota after restart = %d, want the 7000 already spent", got)
	}
	if got := restarted.Limits(shaper.User{Email: "s@example"}).DownlinkBPS; got != 3*mb {
		t.Fatalf("plan after restart = %d, want %d", got, 3*mb)
	}
	if len(u.GetInbounds()) != 1 || u.GetInbounds()[0].GetTag() != "vless-in" {
		t.Fatalf("per-inbound usage did not survive: %v", u.GetInbounds())
	}
}

// TestRestartIntoANewMonthStartsTheQuotaClean is the other half: persistence
// must not resurrect an allowance that has legitimately expired.
func TestRestartIntoANewMonthStartsTheQuotaClean(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tariff.json")
	m, _ := newTestManager(t, &Config{StatePath: path})

	if err := m.SetPolicy(&Policy{
		Email:  "s@example",
		Quotas: []*Quota{{LimitBytes: 10000, Window: Window_WINDOW_MONTH, Action: Action_ACTION_BLOCK}},
	}); err != nil {
		t.Fatal(err)
	}
	m.Attach("s@example", 0, "in", "1.1.1.1").Count(shaper.Down, 10001)
	if err := m.Flush(); err != nil {
		t.Fatal(err)
	}

	later := newClock(midMonth.AddDate(0, 1, 0))
	restarted := New(&Config{StatePath: path})
	restarted.now = later.Now
	if err := restarted.Load(); err != nil {
		t.Fatal(err)
	}

	q := restarted.Usage("s@example").GetQuotas()[0]
	if q.GetUsedBytes() != 0 || q.GetExceeded() {
		t.Fatalf("in the new month: used=%d exceeded=%v, want a clean allowance",
			q.GetUsedBytes(), q.GetExceeded())
	}
	if mt := restarted.Attach("s@example", 0, "in", "1.1.1.1"); mt.Blocked() {
		t.Fatal("last month's exhausted quota is still blocking this month")
	}
}

func TestLoadingIsFineWhenThereIsNoFileYet(t *testing.T) {
	m := New(&Config{StatePath: filepath.Join(t.TempDir(), "absent.json")})
	if err := m.Load(); err != nil {
		t.Fatalf("a server that has not run before should load cleanly: %v", err)
	}
}

func TestLoadingRefusesAFileFromTheFuture(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tariff.json")
	writeStateVersion(t, path, stateVersion+1)

	fresh := New(&Config{StatePath: path})
	if err := fresh.Load(); err == nil {
		t.Fatal("a state file from a newer version was accepted; starting from a " +
			"partial ledger must be a loud failure, not a silent one")
	}
}

// --- events ----------------------------------------------------------------

func TestConnectAndDisconnectAreAnnounced(t *testing.T) {
	m, _ := newTestManager(t, nil)
	_, events, _ := m.Subscribe()

	mt := m.Attach("e@example", 0, "in", "1.1.1.1")
	if !hasEvent(events, EventKind_EVENT_USER_CONNECTED) {
		t.Fatal("no connect event")
	}
	mt.Release()
	if !hasEvent(events, EventKind_EVENT_USER_DISCONNECTED) {
		t.Fatal("no disconnect event")
	}
}

// TestASlowSubscriberCannotStallTheWritePath is the property that keeps a
// monitoring problem from becoming an outage.
func TestASlowSubscriberCannotStallTheWritePath(t *testing.T) {
	m, _ := newTestManager(t, nil)
	_, _, dropped := m.Subscribe() // never read

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < eventBuffer*4; i++ {
			m.Attach("e@example", 0, "in", "1.1.1.1").Release()
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("publishing blocked on a subscriber that stopped reading")
	}
	if dropped.Load() == 0 {
		t.Fatal("a subscriber that overflowed was not told it had lost events")
	}
}

func TestUnsubscribeClosesTheChannel(t *testing.T) {
	m, _ := newTestManager(t, nil)
	id, events, _ := m.Subscribe()
	if got := m.Subscribers(); got != 1 {
		t.Fatalf("subscribers = %d, want 1", got)
	}
	m.Unsubscribe(id)
	if _, open := <-events; open {
		t.Fatal("the channel should be closed after unsubscribing")
	}
	if got := m.Subscribers(); got != 0 {
		t.Fatalf("subscribers = %d after unsubscribing, want 0", got)
	}
}

// --- helpers ---------------------------------------------------------------

// hasEvent drains what is queued and reports whether the kind showed up.
func hasEvent(events <-chan *Event, kind EventKind) bool {
	for {
		select {
		case e := <-events:
			if e != nil && e.GetKind() == kind {
				return true
			}
		default:
			return false
		}
	}
}
