package shaper

import "time"

// Defaults applied to a Limits with the corresponding field left at zero.
const (
	// DefaultOvercommit is how much of a user's aggregate cap one device may
	// hold while other devices of the same user are also active.
	//
	// The device cap is not there to divide the tariff — the user's own bucket
	// already does that, and it does it evenly because every flow claims the
	// same chunk size. The device cap is there so that one device cannot sit
	// on the whole bucket while another is starved. 1.5 means two active
	// devices are each capped at 75% of the tariff (so both must be served),
	// while a device left alone still reaches 100% of it.
	DefaultOvercommit = 1.5

	// DefaultBoostRefill is how long a fully spent speed boost takes to earn
	// itself back. An hour is long enough that the boost cannot be farmed by
	// reconnecting, and short enough that a user who actually was idle gets
	// their fast first minute back.
	DefaultBoostRefill = time.Hour

	// DefaultPriorityShare is the fraction of a user's rate lent to short
	// flows so they can skip the queue. Lent, not given: see Limits.
	DefaultPriorityShare = 0.10
)

// Limits is the traffic policy the shaper enforces for one user. Zero values
// mean "use the default" for every field except the two rates, where zero
// means unlimited — so the zero Limits shapes nothing at all, which is the
// right behaviour for a user nobody has assigned a tariff to.
//
// A Limits is a plain value and is safe to copy. Handing a new one to
// [Shaper.SetLimits] takes effect on live connections without reconnecting.
type Limits struct {
	// UplinkBPS and DownlinkBPS cap the user's aggregate throughput in bytes
	// per second — across every device, every connection and every protocol,
	// because the shaper sits at the dispatcher, which all of them cross.
	// Zero means unlimited and switches shaping off for that direction
	// entirely, wrappers included.
	//
	// Uplink is client to internet, downlink is internet to client. They are
	// independent budgets; a user on 10 MB/s can do 10 MB/s in each direction
	// at once.
	UplinkBPS   int64
	DownlinkBPS int64

	// Overcommit is how much of the cap a single device may hold when several
	// are active, as a multiple of its equal share. See DefaultOvercommit.
	// Values below 1 are raised to 1 (a device may always use its own share).
	Overcommit float64

	// BoostBytes is the speed-boost budget in bytes: how much traffic a user
	// may move at BoostCeilBPS before the tariff starts applying. Set it to
	// the number of bytes a good connection moves in the window you want to
	// feel unlimited — 60 s at 25 MB/s is 1.5 GB.
	//
	// The boost is genuinely extra capacity: bytes spent from it are not
	// debited against the tariff, so the user is not starved afterwards to pay
	// for it. Zero disables the boost.
	//
	// A budget rather than a timer is deliberate. A 60-second timer measures
	// how long ago the user connected, which a client can reset by
	// reconnecting; a budget measures how much fast traffic they have actually
	// been given, which they cannot. It also degrades better: a user who only
	// checks mail never spends the budget and is fast every time, while a user
	// who saturates the link exhausts it in the promised minute.
	BoostBytes int64

	// BoostRefill is how long a fully spent boost takes to refill. See
	// DefaultBoostRefill.
	BoostRefill time.Duration

	// BoostCeilBPS caps throughput while boost credit is being spent. Zero
	// means "as fast as the path allows", which is what a speed test should
	// see; set it if one user saturating the server's uplink is a problem.
	BoostCeilBPS int64

	// PriorityShare is the fraction of the user's rate lent to flows that have
	// not yet moved InteractiveBytes, so that a DNS lookup or a TLS handshake
	// is not stuck behind a download's queue. Zero uses
	// DefaultPriorityShare; a negative value disables the allowance.
	//
	// Lent, not given: bytes taken from the allowance are still debited
	// against the tariff bucket, they just do not wait for it. The bucket goes
	// into deficit and the next bulk chunk pays it back, so the long-run
	// average stays exactly at the cap while short flows stay responsive.
	PriorityShare float64

	// InteractiveBytes is how much a flow may move before it stops counting as
	// short and loses access to the priority allowance. Zero uses
	// DefaultInteractiveBytes.
	InteractiveBytes int64
}

// DefaultInteractiveBytes is how much traffic a flow may move while still
// counting as short. 128 KiB comfortably covers a DNS exchange, a TLS
// handshake, an API call or a message, and is far below anything worth calling
// a download.
const DefaultInteractiveBytes = 128 * 1024

// normalize returns l with every zero field replaced by its default and every
// nonsensical value clamped. It is called once when a direction is built, so
// the hot path never has to check.
func (l Limits) normalize() Limits {
	if l.Overcommit < 1 {
		l.Overcommit = DefaultOvercommit
	}
	if l.BoostRefill <= 0 {
		l.BoostRefill = DefaultBoostRefill
	}
	if l.PriorityShare == 0 {
		l.PriorityShare = DefaultPriorityShare
	}
	if l.InteractiveBytes <= 0 {
		l.InteractiveBytes = DefaultInteractiveBytes
	}
	if l.UplinkBPS < 0 {
		l.UplinkBPS = 0
	}
	if l.DownlinkBPS < 0 {
		l.DownlinkBPS = 0
	}
	if l.BoostBytes < 0 {
		l.BoostBytes = 0
	}
	if l.BoostCeilBPS < 0 {
		l.BoostCeilBPS = 0
	}
	return l
}

// Unlimited reports whether l shapes nothing in either direction, in which
// case callers should skip wrapping altogether.
func (l Limits) Unlimited() bool {
	return l.UplinkBPS <= 0 && l.DownlinkBPS <= 0
}

// bps returns the cap for one direction.
func (l Limits) bps(dir Direction) int64 {
	if dir == Up {
		return l.UplinkBPS
	}
	return l.DownlinkBPS
}
