//go:build windows

package tun

// fork: the Windows TUN as it has to behave inside a host process that starts
// and stops the core many times over its life (libxray, P/Invoked by a GUI).
//
// Up to v26.10 this file carried the fix for two use-after-free faults in
// wintun.dll: ReadPacket handed out a slice into wintun's receive ring and
// released it only when the stack dropped the packet, and nothing kept a read
// or a write from running while Close ended the session. Upstream's own stack
// (v26.11, #7130) copies each packet out and releases it at once, and takes the
// device's lock around every ring access, so both are gone and so is the fix.
//
// What is left is the wait. ReadPacket parks on wintun's read event between
// packets; Close sets the event before it ends the session, and the reader then
// finds the device closed. But the event is auto-reset and its handle is closed
// by WintunEndSession: a reader that reaches the wait only after that waits on
// a handle that is gone, or on whatever object Windows has given its number to
// since — with INFINITE, possibly for good, keeping its goroutine and a 64 KiB
// buffer per restart. A slice bounds that to a fraction of a second; between
// packets the reader looks at the ring four times a second, which costs nothing.

// readWaitSlice is how long, in milliseconds, one wait for a packet lasts.
const readWaitSlice = 250
