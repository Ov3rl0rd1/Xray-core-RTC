package main

// Changes to a running instance that must not cost a restart.
//
// A restart is not free on a desktop VPN: the core owns the TUN, so stopping it
// removes the adapter, every application on the machine sees its network vanish,
// and every connection — the game session included — is dropped. Changing a
// split-tunnel rule, or moving from one protocol to the next when the first
// stops working, should not do that. Each function here replaces one piece of
// the running instance and leaves the rest standing.

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/xtls/xray-core/app/router"
	"github.com/xtls/xray-core/common/errors"
	core "github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/features/outbound"
	"github.com/xtls/xray-core/features/routing"
	"github.com/xtls/xray-core/infra/conf"
	"github.com/xtls/xray-core/proxy/tun"
)

func running() (*core.Instance, error) {
	if instance == nil {
		return nil, errors.New("xray is not running")
	}
	return instance, nil
}

// reloadRouting replaces the routing rules (and balancers) with those in a
// "routing" config object. Live connections keep the route they were given;
// every new one is matched against the new rules.
func reloadRouting(routingJSON string) error {
	mu.Lock()
	defer mu.Unlock()

	inst, err := running()
	if err != nil {
		return err
	}
	var rc conf.RouterConfig
	if err := json.Unmarshal([]byte(routingJSON), &rc); err != nil {
		return errors.New("invalid routing config").Base(err)
	}
	cfg, err := rc.Build()
	if err != nil {
		return err
	}
	r, ok := inst.GetFeature(routing.RouterType()).(*router.Router)
	if !ok {
		return errors.New("router does not support reloading")
	}
	return r.ReloadRules(cfg, false)
}

// replaceOutbound swaps the outbound with the given config's tag for one built
// from it, then closes the old one. Connections it was carrying end; nothing
// else does.
func replaceOutbound(outboundJSON string) error {
	mu.Lock()
	defer mu.Unlock()

	inst, err := running()
	if err != nil {
		return err
	}
	var oc conf.OutboundDetourConfig
	if err := json.Unmarshal([]byte(outboundJSON), &oc); err != nil {
		return errors.New("invalid outbound config").Base(err)
	}
	if oc.Tag == "" {
		return errors.New("outbound needs a tag to replace")
	}
	built, err := oc.Build()
	if err != nil {
		return err
	}
	raw, err := core.CreateObject(inst, built)
	if err != nil {
		return err
	}
	handler, ok := raw.(outbound.Handler)
	if !ok {
		return errors.New("not an outbound handler")
	}

	ohm := inst.GetFeature(outbound.ManagerType()).(outbound.Manager)
	old := ohm.GetHandler(oc.Tag)
	if old != nil {
		if err := ohm.RemoveHandler(context.Background(), oc.Tag); err != nil {
			return err
		}
	}
	if err := ohm.AddHandler(context.Background(), handler); err != nil {
		// Put the old one back rather than leave the tag dangling: every rule
		// pointing at it would drop its traffic.
		if old != nil {
			_ = ohm.AddHandler(context.Background(), old)
		}
		return err
	}
	if old != nil {
		_ = old.Close()
	}
	return nil
}

// connectionsJSON lists the connections the TUN is carrying.
func connectionsJSON() string {
	return tun.SnapshotJSON()
}

// closeConnections closes the connections whose ids are listed, comma separated,
// or every connection for "*". Returns how many were closed.
func closeConnections(spec string) int {
	spec = strings.TrimSpace(spec)
	if spec == "*" {
		return tun.CloseConnections(nil, true)
	}
	var ids []uint64
	for _, part := range strings.Split(spec, ",") {
		if id, err := strconv.ParseUint(strings.TrimSpace(part), 10, 64); err == nil {
			ids = append(ids, id)
		}
	}
	return tun.CloseConnections(ids, false)
}
