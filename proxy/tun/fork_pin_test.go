package tun

import (
	go_errors "errors"
	"net"
	"slices"
	"syscall"
	"testing"

	"github.com/xtls/xray-core/transport/internet"
)

var (
	testPhysical = &net.Interface{Index: 4, Name: "Ethernet"}
	testLoopback = &net.Interface{Index: 1, Name: "Loopback"}
)

// family returns a target that records where it was pinned and refuses the
// interfaces listed in deny.
func family(name string, pinned map[string]int, deny ...int) pinTarget {
	return pinTarget{family: name, pin: func(iface *net.Interface) error {
		if slices.Contains(deny, iface.Index) {
			return syscall.EINVAL
		}
		pinned[name] = iface.Index
		return nil
	}}
}

// What a user's machine did: IPV6_UNICAST_IF on the physical adapter answered
// WSAEINVAL, the socket was left unpinned, and its IPv6 followed the only IPv6
// route there was — into the TUN. The half that cannot be pinned must be sealed;
// the half that can must stay on the physical interface.
func TestAFamilyTheInterfaceWillNotTakeIsPinnedToLoopback(t *testing.T) {
	pinned := map[string]int{}
	out := pinEach([]pinTarget{
		family("IPv6", pinned, testPhysical.Index),
		family("IPv4", pinned),
	}, testPhysical, testLoopback)

	if pinned["IPv6"] != testLoopback.Index {
		t.Fatalf("IPv6 pinned to %d, want loopback", pinned["IPv6"])
	}
	if pinned["IPv4"] != testPhysical.Index {
		t.Fatalf("IPv4 pinned to %d, want the physical interface", pinned["IPv4"])
	}
	if !slices.Equal(out.refused, []string{"IPv6"}) || len(out.open) != 0 {
		t.Fatalf("refused %v, open %v", out.refused, out.open)
	}
	if out.cause == nil {
		t.Fatal("the reason the interface gave is lost")
	}
}

func TestASocketThatPinsCleanlyIsLeftAlone(t *testing.T) {
	pinned := map[string]int{}
	out := pinEach([]pinTarget{family("IPv6", pinned), family("IPv4", pinned)}, testPhysical, testLoopback)
	if len(out.refused) != 0 || len(out.open) != 0 || out.cause != nil {
		t.Fatalf("clean pin reported refused %v, open %v, cause %v", out.refused, out.open, out.cause)
	}
	if pinned["IPv6"] != testPhysical.Index || pinned["IPv4"] != testPhysical.Index {
		t.Fatalf("pinned %v", pinned)
	}
}

func TestWithNoPhysicalInterfaceEveryFamilyIsRefused(t *testing.T) {
	pinned := map[string]int{}
	out := pinEach([]pinTarget{family("IPv6", pinned), family("IPv4", pinned)}, nil, testLoopback)
	if !slices.Equal(out.refused, []string{"IPv6", "IPv4"}) {
		t.Fatalf("refused %v", out.refused)
	}
	if pinned["IPv6"] != testLoopback.Index || pinned["IPv4"] != testLoopback.Index {
		t.Fatalf("pinned %v", pinned)
	}
}

// Neither the interface nor loopback took it: the socket follows the route
// table, and that has to be said rather than passed over.
func TestAFamilyThatCanBePinnedNowhereIsReportedOpen(t *testing.T) {
	pinned := map[string]int{}
	out := pinEach([]pinTarget{
		family("IPv6", pinned, testPhysical.Index, testLoopback.Index),
		family("IPv4", pinned),
	}, testPhysical, testLoopback)
	if !slices.Equal(out.open, []string{"IPv6"}) || len(out.refused) != 0 {
		t.Fatalf("open %v, refused %v", out.open, out.refused)
	}

	out = pinEach([]pinTarget{family("IPv6", pinned, testPhysical.Index)}, testPhysical, nil)
	if !slices.Equal(out.open, []string{"IPv6"}) {
		t.Fatalf("without a loopback interface: open %v", out.open)
	}
}

// A dual-stack UDP socket with its IPv6 sealed still serves an IPv4 node, so
// that is not an error; a dial that needs the sealed family is.
func TestARefusalFailsOnlyTheDialThatNeedsThatFamily(t *testing.T) {
	sealed6 := pinOutcome{refused: []string{"IPv6"}, cause: syscall.EINVAL}
	for _, c := range []struct {
		network, address string
		fails            bool
	}{
		{"tcp6", "[2600:1f18:4622:109::1]:36651", true},
		{"udp6", "[::]:0", true},     // the dialer's wildcard for an IPv6 destination
		{"udp6", "0.0.0.0:0", false}, // and for an IPv4 one, on the same dual-stack socket
		{"tcp6", "[::ffff:1.2.3.4]:443", false},
		{"udp6", "", true}, // nothing says which family is needed
	} {
		err := sealed6.verdict(c.network, c.address, testPhysical)
		if (err != nil) != c.fails {
			t.Errorf("%s %q with IPv6 sealed: err = %v, want failure %v", c.network, c.address, err, c.fails)
		}
	}

	if err := (pinOutcome{}).verdict("tcp4", "1.2.3.4:443", testPhysical); err != nil {
		t.Errorf("a clean pin is an error: %v", err)
	}
	if err := (pinOutcome{open: []string{"IPv4"}, cause: syscall.EINVAL}).verdict("udp6", "[::]:0", testPhysical); err == nil {
		t.Error("an unpinned family is not an error")
	}
	if err := (pinOutcome{refused: []string{"IPv6", "IPv4"}}).verdict("tcp4", "1.2.3.4:443", nil); err == nil {
		t.Error("no physical interface is not an error")
	}
}

// One line per interface and family, not one per socket: the field log had
// 66 000 of them at a level nobody collects.
func TestARefusalIsWarnedAboutOncePerInterface(t *testing.T) {
	var said bindSaid
	if !said.first(4, "IPv6") {
		t.Fatal("first refusal not announced")
	}
	if said.first(4, "IPv6") {
		t.Fatal("announced twice")
	}
	if !said.first(4, "IPv4") || !said.first(7, "IPv6") {
		t.Fatal("another family or interface was swallowed")
	}
	said.reset()
	if !said.first(4, "IPv6") {
		t.Fatal("a new TUN does not announce again")
	}
}

// Since v26.11 the dialer cancels a dial whose controller fails, and on Windows
// retries a dual-stack UDP socket as a single-family one when the failure is a
// *BindError (#7173, #7214). A refusal has to be one, or the retry that serves an
// IPv4 destination from a machine without IPv6 never happens.
func TestARefusalIsABindErrorTheDialerUnderstands(t *testing.T) {
	for _, out := range []pinOutcome{
		{refused: []string{"IPv6"}, cause: syscall.EINVAL},
		{open: []string{"IPv6"}, cause: syscall.EINVAL},
		{refused: []string{wholeSocket}},
	} {
		iface := testPhysical
		if slices.Contains(out.refused, wholeSocket) {
			iface = nil
		}
		err := out.verdict("udp6", "[2001:db8::1]:443", iface)
		var bind *internet.BindError
		if !go_errors.As(err, &bind) {
			t.Errorf("%+v: %v is not a BindError", out, err)
		}
	}
}
