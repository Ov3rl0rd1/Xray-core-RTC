package olcrtc

import (
	"strings"
	"sync"
	"time"
)

// Room selection for the inbound.
//
// The tunnel lives inside somebody else's conference room, and rooms stop
// working for reasons that have nothing to do with this server: a provider
// retires one, stops issuing tokens for it, or it gets blocked. Joining
// exactly one room means any of those ends the tunnel until a human notices.
//
// So the inbound is given an ordered list and works down it. The whole policy
// is here, in the fork's own file, because the library has no opinion about
// which room to be in — it joins the one it is told to.

const (
	// defaultCooldown is how long a room sits out after its first failure.
	// Short enough that a transient provider hiccup costs one attempt, long
	// enough that a room which is genuinely gone is not hammered.
	defaultCooldown = 30 * time.Second

	// maxCooldownFactor caps the doubling, so a room that has been failing all
	// day is still retried every few minutes rather than every few hours. A
	// block can be lifted, and a room nobody ever retries is a room nobody ever
	// discovers is back.
	maxCooldownFactor = 16

	// stableAfter is how long a carrier has to stay up before the room is
	// considered to have worked. Below this the room joined but did not hold,
	// which is a failure however cleanly it was reported.
	stableAfter = 2 * time.Minute
)

// roomState is what the pool has learned about one room.
type roomState struct {
	id string

	failures      int
	lastError     string
	lastAttempt   time.Time
	lastHealthy   time.Time
	cooldownUntil time.Time
	up            bool
}

// roomPool decides which room to use next.
//
// Selection always restarts from the top of the list, so the primary is
// preferred the moment its cooldown lapses: the fallbacks exist to keep the
// service up, not to become the new normal. Every room being in cooldown means
// the pool hands back the one whose cooldown ends soonest rather than giving
// up — a tunnel that stops trying is worse than one that retries too eagerly.
type roomPool struct {
	mu       sync.Mutex
	rooms    []*roomState
	current  int
	cooldown time.Duration
	now      func() time.Time
}

// newRoomPool builds a pool from the primary room and its fallbacks, dropping
// blanks and duplicates so a config that repeats itself does not produce a
// rotation that appears to move but does not.
func newRoomPool(primary string, fallbacks []string, cooldown time.Duration) *roomPool {
	if cooldown <= 0 {
		cooldown = defaultCooldown
	}
	p := &roomPool{cooldown: cooldown, now: time.Now}
	seen := make(map[string]bool)
	for _, id := range append([]string{primary}, fallbacks...) {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		p.rooms = append(p.rooms, &roomState{id: id})
	}
	return p
}

// Len reports how many distinct rooms the pool holds.
func (p *roomPool) Len() int {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.rooms)
}

// next returns the room to try, and whether it differs from the last one
// handed out. An empty pool returns "".
func (p *roomPool) next() (room string, switched bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.rooms) == 0 {
		return "", false
	}

	now := p.now()
	previous := p.current

	// Prefer the first room that is not sitting out, scanning from the top so
	// the primary reclaims its place as soon as it can.
	chosen := -1
	for i, r := range p.rooms {
		if now.After(r.cooldownUntil) {
			chosen = i
			break
		}
	}
	if chosen < 0 {
		// Everything is in cooldown. Take whichever is free soonest instead of
		// refusing to run at all.
		chosen = 0
		for i, r := range p.rooms {
			if r.cooldownUntil.Before(p.rooms[chosen].cooldownUntil) {
				chosen = i
			}
		}
	}

	p.current = chosen
	p.rooms[chosen].lastAttempt = now
	return p.rooms[chosen].id, chosen != previous
}

// succeeded records that a room carried a session, clearing its failure
// history so a room that works again is treated as working again.
func (p *roomPool) succeeded(room string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if r := p.find(room); r != nil {
		r.failures = 0
		r.lastError = ""
		r.cooldownUntil = time.Time{}
		r.lastHealthy = p.now()
		r.up = true
	}
}

// failed records that a room did not work and puts it on a cooldown that
// lengthens with successive failures.
//
// A run that stayed up longer than stableAfter is not counted as a failure of
// the room: the carrier worked and something else ended it, and penalising the
// room for that would rotate away from a room that is fine.
func (p *roomPool) failed(room string, ranFor time.Duration, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	r := p.find(room)
	if r == nil {
		return
	}
	r.up = false
	if err != nil {
		r.lastError = err.Error()
	}
	if ranFor >= stableAfter {
		r.failures = 0
		r.cooldownUntil = time.Time{}
		return
	}
	r.failures++
	factor := 1 << min(r.failures-1, 4) // 1,2,4,8,16
	if factor > maxCooldownFactor {
		factor = maxCooldownFactor
	}
	r.cooldownUntil = p.now().Add(p.cooldown * time.Duration(factor))
}

func (p *roomPool) find(room string) *roomState {
	for _, r := range p.rooms {
		if r.id == room {
			return r
		}
	}
	return nil
}

// snapshot renders the pool for logs and for the management API.
func (p *roomPool) snapshot() []roomState {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]roomState, 0, len(p.rooms))
	for _, r := range p.rooms {
		out = append(out, *r)
	}
	return out
}
