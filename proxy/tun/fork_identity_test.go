package tun

import (
	"crypto/md5"
	"reflect"
	"testing"
)

// Identity 0 must stay upstream's md5(name): an existing installation then keeps
// its adapter GUID, and the network profile Windows attached to it.
func TestFirstIdentityIsUpstreamsGUID(t *testing.T) {
	if adapterGUIDBytes("Horus", 0) != md5.Sum([]byte("Horus")) {
		t.Fatal("identity 0 is not md5(name)")
	}
}

func TestIdentitiesAreDistinctAndStable(t *testing.T) {
	seen := map[[16]byte]int{}
	for i := 0; i < adapterIdentities; i++ {
		g := adapterGUIDBytes("Horus", i)
		if j, dup := seen[g]; dup {
			t.Fatalf("identities %d and %d share a GUID", j, i)
		}
		seen[g] = i
		if adapterGUIDBytes("Horus", i) != g {
			t.Fatalf("identity %d is not deterministic", i)
		}
	}
}

func none(int) bool { return false }

func TestCandidates(t *testing.T) {
	heldSet := func(ids ...int) func(int) bool {
		return func(i int) bool {
			for _, id := range ids {
				if id == i {
					return true
				}
			}
			return false
		}
	}
	cases := []struct {
		name   string
		held   func(int) bool
		failed map[int]bool
		want   []int
	}{
		{"a clean machine always gets identity 0 first", none, nil, []int{0, 1}},
		{"a device still holding 0 is passed over", heldSet(0), nil, []int{1, 2}},
		{"an identity that failed here is not retried", none, map[int]bool{0: true}, []int{1, 2}},
		{"held and failed together", heldSet(1), map[int]bool{0: true}, []int{2, 3}},
		{"one left, then Windows chooses", heldSet(0, 1, 2), nil, []int{3, randomIdentity}},
		{"none left", heldSet(0, 1, 2, 3), nil, []int{randomIdentity}},
	}
	for _, c := range cases {
		if got := adapterCandidates(c.held, c.failed, 2); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// Every lookup is a call into the device manager; stop once there are enough.
func TestCandidatesAskOnlyAsFarAsNeeded(t *testing.T) {
	var asked []int
	adapterCandidates(func(i int) bool { asked = append(asked, i); return false }, nil, 2)
	if !reflect.DeepEqual(asked, []int{0, 1}) {
		t.Fatalf("asked about %v", asked)
	}
}
