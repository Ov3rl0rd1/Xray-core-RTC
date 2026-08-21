//go:build cgo

package main

// The preamble of a file using //export may contain declarations only, never
// definitions -- an include is fine, a function body is not.

/*
#include <stdlib.h>
*/
import "C"

import (
	"fmt"
	"runtime/debug"
	"unsafe"

	"github.com/xtls/xray-core/common/errors"
)

// Every function returning char* returns memory owned by the caller, which
// must release it with XrayFree. Every function returning int returns 0 on
// success and -1 on failure, with the detail available from XrayLastError.

// guard turns a panic inside the core into an error the caller can read.
//
// This is not defensive tidiness, it is the difference between a diagnosable
// failure and none at all. The library is loaded into the host's process, so a
// panic that unwinds out of a C call reaches the runtime and aborts: the whole
// application dies, no managed exception is ever raised, none of the host's
// crash handlers fire, and the only trace left is a tombstone the user cannot
// retrieve. Recovering here turns that into a -1 with a stack in
// XrayLastError, which the host already surfaces and puts in its bug reports.
//
// Deferred at the top of every exported function:
//
//	func XrayThing() (ret C.int) {
//	    defer guard("XrayThing", &ret)()
//	    ...
//	}
//
// The doubled call is deliberate: guard returns the closure that will run at
// return time, so the name and the result pointer are captured now.
//
// What it does not cover: runtime fatals -- concurrent map writes, deadlock
// detection, out of memory -- which are not panics and are unrecoverable by
// design, and panics on goroutines the core started itself, which never pass
// through here. Those still abort. The signature to look for in that case is a
// process death with no crash.log entry.
func guard(name string, ret *C.int) func() {
	return func() {
		r := recover()
		if r == nil {
			return
		}
		if ret != nil {
			*ret = -1
		}
		setLastError(errors.New(fmt.Sprintf("panic in %s: %v\n%s", name, r, debug.Stack())))
	}
}

// guardStr is guard for the entry points returning a string. A panic yields an
// empty string rather than a null pointer, so the caller's unmarshal path is
// unchanged and nothing has to special-case it.
func guardStr(name string, ret **C.char) func() {
	return func() {
		r := recover()
		if r == nil {
			return
		}
		if ret != nil {
			*ret = C.CString("")
		}
		setLastError(errors.New(fmt.Sprintf("panic in %s: %v\n%s", name, r, debug.Stack())))
	}
}

//export XrayStart
func XrayStart(configJSON *C.char) (ret C.int) {
	defer guard("XrayStart", &ret)()

	if err := startInstance(C.GoString(configJSON)); err != nil {
		setLastError(err)
		return -1
	}
	setLastError(nil)
	return 0
}

//export XrayStop
func XrayStop() (ret C.int) {
	defer guard("XrayStop", &ret)()

	if err := stopInstance(); err != nil {
		setLastError(err)
		return -1
	}
	setLastError(nil)
	return 0
}

// XrayIsRunning returns 1 when an instance is running, 0 when not, and -1 if
// the call itself failed -- a caller that only tests for 1 treats the last as
// "not running", which is the safe reading.
//
//export XrayIsRunning
func XrayIsRunning() (ret C.int) {
	defer guard("XrayIsRunning", &ret)()

	if isRunning() {
		return 1
	}
	return 0
}

//export XrayTest
func XrayTest(configJSON *C.char) (ret C.int) {
	defer guard("XrayTest", &ret)()

	if err := testConfig(C.GoString(configJSON)); err != nil {
		setLastError(err)
		return -1
	}
	setLastError(nil)
	return 0
}

//export XraySetAssetPath
func XraySetAssetPath(path *C.char) (ret C.int) {
	defer guard("XraySetAssetPath", &ret)()

	if err := setAssetPath(C.GoString(path)); err != nil {
		setLastError(err)
		return -1
	}
	setLastError(nil)
	return 0
}

// XrayResetConnections closes pooled transport sessions so the next dial builds
// fresh ones. Returns the number closed, or -1 on failure.
//
// Call it on a network handover. Every session established over the previous
// link is dead the moment it goes away, but nothing in the stack is told so --
// QUIC sits on the dead path until an idle timeout measured in minutes, which
// the user experiences as a VPN that is connected and carries nothing until
// they toggle it. This is cheap by comparison with the alternative: the running
// instance and the host's TUN are left standing, and only what the handover
// actually invalidated is thrown away.
//
//export XrayResetConnections
func XrayResetConnections() (ret C.int) {
	defer guard("XrayResetConnections", &ret)()

	setLastError(nil)
	return C.int(resetConnections())
}

// XrayForceGc returns freed memory to the operating system.
//
// Go holds released pages rather than handing them back, which is right for a
// server and wrong for a library inside a mobile app that stays resident for
// weeks: RSS grows, and a large process is what the OOM killer reaches for
// first. Asynchronous, because it forces a stop-the-world collection and the
// host's low-memory callback must not block.
//
//export XrayForceGc
func XrayForceGc() {
	// No result to write to, so a bare recover: a best-effort memory hint must
	// never be the thing that kills the process.
	defer func() {
		if r := recover(); r != nil {
			setLastError(errors.New(fmt.Sprintf("panic in XrayForceGc: %v", r)))
		}
	}()

	forceGc()
}

// XraySleep pauses background housekeeping while the device is idle. The core
// keeps carrying traffic; what stops is the periodic tidying of structures
// nothing is touching, which on Android is a timer the kernel services every
// second and a reason the device never settles into deep sleep. Pair with
// XrayWake. Idempotent.
//
//export XraySleep
func XraySleep() (ret C.int) {
	defer guard("XraySleep", &ret)()

	sleep()
	setLastError(nil)
	return 0
}

// XrayWake resumes background housekeeping. Idempotent, and safe without a
// matching XraySleep.
//
//export XrayWake
func XrayWake() (ret C.int) {
	defer guard("XrayWake", &ret)()

	wake()
	setLastError(nil)
	return 0
}

// XrayIsPaused reports whether housekeeping is paused: 1 paused, 0 running, -1
// on failure. Diagnostics -- it lets the host prove its Doze wiring reaches the
// core instead of assuming it does.
//
//export XrayIsPaused
func XrayIsPaused() (ret C.int) {
	defer guard("XrayIsPaused", &ret)()

	if isPaused() {
		return 1
	}
	return 0
}

//export XrayVersion
func XrayVersion() (ret *C.char) {
	defer guardStr("XrayVersion", &ret)()

	return C.CString(version())
}

//export XrayLastError
func XrayLastError() (ret *C.char) {
	defer guardStr("XrayLastError", &ret)()

	return C.CString(takeLastError())
}

//export XrayFree
func XrayFree(s *C.char) {
	// Bare recover: freeing must never be what kills the process, and there is
	// no way to report from here that would not itself allocate.
	defer func() { _ = recover() }()

	C.free(unsafe.Pointer(s))
}
