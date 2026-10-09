package tun

import (
	"testing"

	"github.com/xtls/xray-core/transport/internet"
)

func controllers() int {
	internet.ControllersLock.Lock()
	defer internet.ControllersLock.Unlock()
	return len(internet.Controllers)
}

// An embedded core is started and stopped many times in one process; the
// dialer controller must be registered once, not once per start.
func TestBindingRegistersOneControllerForTheProcess(t *testing.T) {
	before := controllers()
	for i := 0; i < 5; i++ {
		u := &InterfaceUpdater{tunIndex: -1}
		bindOutbounds(u)
		unbindOutbounds(u)
	}
	if got := controllers() - before; got > 1 {
		t.Fatalf("%d controllers registered by 5 starts", got)
	}
	if activeBind.Load() != nil {
		t.Fatal("binding still active after unbind")
	}
}

func TestBindFDIsANoOpWithoutATun(t *testing.T) {
	if activeBind.Load() != nil {
		t.Skip("a binding is active")
	}
	if !BindFD(0) {
		t.Fatal("BindFD refused with no TUN running")
	}
}

func TestBindingHooksHearStartAndStop(t *testing.T) {
	var seen []bool
	OnOutboundBinding(func(active bool) { seen = append(seen, active) })
	u := &InterfaceUpdater{tunIndex: -1}
	bindOutbounds(u)
	unbindOutbounds(u)
	unbindOutbounds(u) // a second stop must not notify again
	if len(seen) != 2 || !seen[0] || seen[1] {
		t.Fatalf("hooks saw %v", seen)
	}
}
