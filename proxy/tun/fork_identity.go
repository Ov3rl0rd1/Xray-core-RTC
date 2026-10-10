package tun

// fork: which identity a Windows TUN adapter asks for. Kept free of Windows APIs
// so the choice is tested on any OS; fork_open_windows.go does the asking and
// explains why one GUID per name is not enough.

import (
	"crypto/md5"
	"strconv"
)

// adapterIdentities is how many GUIDs one adapter name maps to. Small and fixed
// on purpose: Windows remembers a network profile per adapter GUID, so a random
// GUID per start would leave a new profile behind every time.
const adapterIdentities = 4

// randomIdentity stands for a GUID Windows picks itself — the last resort.
const randomIdentity = -1

// adapterGUIDBytes is identity i of an adapter name, laid out as windows.GUID
// is in memory. Identity 0 is upstream's md5(name), so an installation keeps the
// adapter, and the network profile, Windows already knows it by.
func adapterGUIDBytes(name string, i int) [16]byte {
	if i == 0 {
		return md5.Sum([]byte(name))
	}
	return md5.Sum([]byte(name + "#" + strconv.Itoa(i)))
}

// adapterCandidates lists at most max identities to try, best first: the lowest
// ones that no device still holds and that have not failed in this process,
// then randomIdentity if there is room. held is asked lazily, in order, and only
// until the list is full.
func adapterCandidates(held func(i int) bool, failed map[int]bool, max int) []int {
	out := make([]int, 0, max)
	for i := 0; i < adapterIdentities && len(out) < max; i++ {
		if failed[i] || held(i) {
			continue
		}
		out = append(out, i)
	}
	if len(out) < max {
		out = append(out, randomIdentity)
	}
	return out
}
