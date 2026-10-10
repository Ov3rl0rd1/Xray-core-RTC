//go:build windows

package tun

// fork: an adapter identity that can be had right now.
//
// Upstream asks wintun for one GUID per adapter name. wintun turns it into the
// device instance SWD\Wintun\{GUID} and removes that device when the adapter is
// closed. While the removal is still under way — or after Windows has put it
// off, which it does while something else holds the device — a new adapter
// asking for the same GUID gets the same instance; wintun then waits 15 s for an
// interface that never appears and fails with "Failed to setup adapter (problem
// code: 0x1F, ...)". In the field that happened on a reconnect two seconds after
// a disconnect, and every retry after it failed the same way: the instance stays
// taken until the removal goes through, at worst until a reboot.
//
// So a name maps to a few GUIDs (fork_identity.go), each looked up before it is
// asked for: one whose device instance still exists, present or left behind, is
// passed over, and one that failed in this process is not tried again. On a
// healthy machine the closed adapter is gone by the next start and identity 0 is
// used every time, as upstream would.

import (
	"context"
	"encoding/binary"
	"fmt"
	"log"
	"sync"
	"syscall"
	"unsafe"

	"github.com/xtls/xray-core/common/errors"
	"golang.org/x/sys/windows"
	"golang.zx2c4.com/wintun"
)

// adapterAttempts bounds one start: every failed attempt costs wintun's 15 s.
const adapterAttempts = 2

var adapterIdentity struct {
	sync.Mutex
	failed map[int]bool // identities that failed in this process
}

var procLocateDevNode = windows.NewLazySystemDLL("cfgmgr32.dll").NewProc("CM_Locate_DevNodeW")

const cmLocateDevNodePhantom = 0x1 // also find devices that are no longer present

// forkOpenAdapter replaces upstream's open: create the adapter under the first
// identity that is free, then the next.
func forkOpenAdapter(name, desc string) (*wintun.Adapter, error) {
	adapterIdentity.Lock()
	defer adapterIdentity.Unlock()
	if adapterIdentity.failed == nil {
		adapterIdentity.failed = map[int]bool{}
	}

	held := func(i int) bool {
		guid := identityGUID(name, i)
		taken, state := deviceHeld(guid)
		if taken {
			adapterNote("adapter identity %d %s is still held by a device (%s); passing over it", i, guid, state)
		}
		return taken
	}

	var lastErr error
	for _, i := range adapterCandidates(held, adapterIdentity.failed, adapterAttempts) {
		guid := identityGUID(name, i)
		adapter, err := wintun.CreateAdapter(name, desc, guid)
		if err == nil {
			if i != 0 {
				adapterNote("adapter created under %s", describeIdentity(i, guid))
			}
			return adapter, nil
		}
		if i != randomIdentity {
			adapterIdentity.failed[i] = true
		}
		adapterNote("adapter not created under %s: %v", describeIdentity(i, guid), err)
		lastErr = err
	}
	return nil, lastErr
}

// identityGUID is identity i as a GUID, or nil for randomIdentity, which wintun
// takes as "let Windows choose".
func identityGUID(name string, i int) *windows.GUID {
	if i == randomIdentity {
		return nil
	}
	b := adapterGUIDBytes(name, i)
	return &windows.GUID{
		Data1: binary.LittleEndian.Uint32(b[0:4]),
		Data2: binary.LittleEndian.Uint16(b[4:6]),
		Data3: binary.LittleEndian.Uint16(b[6:8]),
		Data4: [8]byte(b[8:16]),
	}
}

func describeIdentity(i int, guid *windows.GUID) string {
	if guid == nil {
		return "a GUID chosen by Windows"
	}
	return fmt.Sprintf("identity %d %s", i, guid)
}

// deviceHeld reports whether wintun's device instance for guid still exists, and
// in what state, for the log.
func deviceHeld(guid *windows.GUID) (bool, string) {
	id, err := windows.UTF16PtrFromString(`SWD\Wintun\` + guid.String())
	if err != nil || procLocateDevNode.Find() != nil {
		return false, ""
	}
	var inst windows.DEVINST
	r, _, _ := syscall.SyscallN(procLocateDevNode.Addr(),
		uintptr(unsafe.Pointer(&inst)), uintptr(unsafe.Pointer(id)), cmLocateDevNodePhantom)
	if windows.CONFIGRET(r) != windows.CR_SUCCESS {
		return false, ""
	}
	var status, problem uint32
	switch {
	case windows.CM_Get_DevNode_Status(&status, &problem, inst, 0) != nil:
		return true, "left behind, not present"
	case status&windows.DN_HAS_PROBLEM != 0:
		return true, fmt.Sprintf("present, problem 0x%X", problem)
	default:
		return true, "present"
	}
}

// note goes to stderr beside wintun's own lines, which is where someone reading
// "Failed to setup adapter" will look, and to the core's log.
func adapterNote(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	log.Print("[tun] " + msg)
	errors.LogWarning(context.Background(), "[tun] ", msg)
}
