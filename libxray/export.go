//go:build cgo

package main

// The preamble of a file using //export may contain declarations only, never
// definitions -- an include is fine, a function body is not.

/*
#include <stdlib.h>
*/
import "C"

import "unsafe"

// Every function returning char* returns memory owned by the caller, which
// must release it with XrayFree. Every function returning int returns 0 on
// success and -1 on failure, with the detail available from XrayLastError.

//export XrayStart
func XrayStart(configJSON *C.char) C.int {
	if err := startInstance(C.GoString(configJSON)); err != nil {
		setLastError(err)
		return -1
	}
	setLastError(nil)
	return 0
}

//export XrayStop
func XrayStop() C.int {
	if err := stopInstance(); err != nil {
		setLastError(err)
		return -1
	}
	setLastError(nil)
	return 0
}

//export XrayIsRunning
func XrayIsRunning() C.int {
	if isRunning() {
		return 1
	}
	return 0
}

//export XrayTest
func XrayTest(configJSON *C.char) C.int {
	if err := testConfig(C.GoString(configJSON)); err != nil {
		setLastError(err)
		return -1
	}
	setLastError(nil)
	return 0
}

//export XraySetAssetPath
func XraySetAssetPath(path *C.char) C.int {
	if err := setAssetPath(C.GoString(path)); err != nil {
		setLastError(err)
		return -1
	}
	setLastError(nil)
	return 0
}

//export XrayVersion
func XrayVersion() *C.char {
	return C.CString(version())
}

//export XrayLastError
func XrayLastError() *C.char {
	return C.CString(takeLastError())
}

//export XrayFree
func XrayFree(s *C.char) {
	C.free(unsafe.Pointer(s))
}
