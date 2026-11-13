package tun

// fork: a socket is pinned one address family at a time, and a family that
// cannot be pinned is sealed.
//
// Upstream sets every option it has for the socket's network and combines the
// errors; the dialer logs the result at Info and uses the socket anyway. On a
// user's Windows machine IPV6_UNICAST_IF answered WSAEINVAL for the physical
// adapter (IPv6 unticked on it), so the IPv6 half of every socket was left
// following the route table — and the only IPv6 route was the TUN's. Anything
// the rules sent direct to an IPv6 address came straight back in, and each turn
// opened a new socket: one datagram became 16 292 sockets in 80 seconds, the
// whole of Windows' ephemeral port range, and for five minutes nothing on the
// machine could open a connection.
//
// So: each family is pinned on its own. One the physical interface will not
// take is pinned to loopback instead, from where it reaches nothing remote, and
// the dial that needed it fails where it would have looped. The other family
// stays on the physical interface — a dual-stack UDP socket still carries the
// IPv4 connection to the node.

import (
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/xtls/xray-core/common/errors"
)

const (
	familyIPv4 = "IPv4"
	familyIPv6 = "IPv6"

	// wholeSocket is the one target of a platform that pins a socket as a whole.
	wholeSocket = ""
)

// pinTarget is one part of a socket the platform can pin by itself: an address
// family on Windows, where a dual-stack socket has two, and the whole socket
// elsewhere.
type pinTarget struct {
	family string
	pin    func(iface *net.Interface) error
}

type pinOutcome struct {
	refused []string // sealed to loopback: the interface would not take them
	open    []string // pinned nowhere: they follow the route table
	cause   error    // what the interface answered, the first time
}

// pinEach pins every target to iface, or to loopback when iface is nil or will
// not take it.
func pinEach(targets []pinTarget, iface, loopback *net.Interface) pinOutcome {
	var out pinOutcome
	for _, target := range targets {
		if iface != nil {
			err := target.pin(iface)
			if err == nil {
				continue
			}
			if out.cause == nil {
				out.cause = err
			}
		}
		if loopback != nil {
			err := target.pin(loopback)
			if err == nil {
				out.refused = append(out.refused, target.family)
				continue
			}
			if out.cause == nil {
				out.cause = err
			}
		}
		out.open = append(out.open, target.family)
	}
	return out
}

// usable reports whether at least one of count targets ended up on the
// physical interface and none was left to the route table.
func (o pinOutcome) usable(count int) bool {
	return len(o.open) == 0 && count > len(o.refused)
}

// verdict is what the dial is told. Only a refusal of the family the dial needs
// is an error: a socket with its IPv6 sealed serves an IPv4 destination as
// well as it ever did.
func (o pinOutcome) verdict(network, address string, iface *net.Interface) error {
	if len(o.open) > 0 {
		where := "loopback"
		if iface != nil {
			where = iface.Name + " nor to loopback"
		}
		return errors.New("[tun] ", familyList(o.open), " for ", address,
			" could be pinned neither to ", where, " and follows the route table").Base(o.cause)
	}
	if len(o.refused) == 0 {
		return nil
	}
	if iface == nil {
		return errors.New("[tun] no physical interface; refusing ", address, " rather than looping it into the TUN")
	}
	if need := familyOf(address); need != "" && !slices.Contains(o.refused, need) && !slices.Contains(o.refused, wholeSocket) {
		return nil
	}
	return errors.New("[tun] ", familyList(o.refused), " cannot be pinned to ", iface.Name,
		"; refusing ", address, " rather than looping it into the TUN").Base(o.cause)
}

// familyOf names the family a dial to address needs, or "" when the address
// does not say. For the dialer's UDP sockets the address is the local wildcard,
// which it picks by the destination's family.
func familyOf(address string) string {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return ""
	}
	if a := ap.Addr(); a.Is4() || a.Is4In6() {
		return familyIPv4
	}
	return familyIPv6
}

func familyList(families []string) string {
	named := make([]string, 0, len(families))
	for _, f := range families {
		if f == wholeSocket {
			f = "the socket"
		}
		named = append(named, f)
	}
	return strings.Join(named, " and ")
}

// bindSaid keeps a refusal to one line per interface and family. The field log
// had 66 000 of them, at a level nobody collects.
type bindSaid struct {
	mu   sync.Mutex
	seen map[bindSaidKey]struct{}
}

type bindSaidKey struct {
	index  int
	family string
}

func (s *bindSaid) first(index int, family string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := bindSaidKey{index, family}
	if _, ok := s.seen[key]; ok {
		return false
	}
	if s.seen == nil {
		s.seen = map[bindSaidKey]struct{}{}
	}
	s.seen[key] = struct{}{}
	return true
}

func (s *bindSaid) reset() {
	s.mu.Lock()
	s.seen = nil
	s.mu.Unlock()
}

var loopbackFound atomic.Pointer[net.Interface]

// loopbackInterface is looked up once: it is asked for on every socket now, and
// listing the adapters is far from free on Windows.
func loopbackInterface() *net.Interface {
	if lo := loopbackFound.Load(); lo != nil {
		return lo
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	for i := range ifaces {
		if ifaces[i].Flags&net.FlagLoopback != 0 {
			loopbackFound.Store(&ifaces[i])
			return &ifaces[i]
		}
	}
	return nil
}
