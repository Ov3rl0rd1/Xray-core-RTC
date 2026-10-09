package main

import (
	"strings"
	"testing"

	"github.com/xtls/xray-core/features/outbound"
	"github.com/xtls/xray-core/features/routing"
)

const liveConfig = `{
  "log": {"loglevel": "none"},
  "inbounds": [{"tag": "socks-in", "listen": "127.0.0.1", "port": 0, "protocol": "socks",
                "settings": {"auth": "noauth"}}],
  "outbounds": [{"tag": "proxy", "protocol": "freedom"},
                {"tag": "direct", "protocol": "freedom"},
                {"tag": "block", "protocol": "blackhole"}],
  "routing": {"rules": [{"type": "field", "network": "tcp,udp", "outboundTag": "proxy"}]}
}`

func TestLiveChangesNeedARunningInstance(t *testing.T) {
	stopInstance()
	if err := reloadRouting(`{"rules": []}`); err == nil {
		t.Fatal("reload succeeded with nothing running")
	}
	if err := replaceOutbound(`{"tag": "proxy", "protocol": "freedom"}`); err == nil {
		t.Fatal("replace succeeded with nothing running")
	}
}

func TestReloadRoutingReplacesTheRules(t *testing.T) {
	if err := startInstance(liveConfig); err != nil {
		t.Fatal(err)
	}
	defer stopInstance()

	err := reloadRouting(`{"rules": [
	  {"type": "field", "ruleTag": "games", "process": ["game.exe"], "outboundTag": "proxy"},
	  {"type": "field", "network": "tcp,udp", "outboundTag": "direct"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	rules := instance.GetFeature(routing.RouterType()).(routing.Router).ListRule()
	if len(rules) != 2 || rules[0].GetRuleTag() != "games" || rules[1].GetOutboundTag() != "direct" {
		t.Fatalf("rules after reload: %d", len(rules))
	}

	// A config the core rejects must leave the running rules alone.
	if err := reloadRouting(`{"rules": [{"type": "field", "outboundTag": "proxy", "ip": ["not-an-ip"]}]}`); err == nil {
		t.Fatal("invalid routing accepted")
	}
	if n := len(instance.GetFeature(routing.RouterType()).(routing.Router).ListRule()); n != 2 {
		t.Fatalf("a rejected reload changed the rules (%d)", n)
	}
}

func TestReplaceOutboundSwapsTheHandler(t *testing.T) {
	if err := startInstance(liveConfig); err != nil {
		t.Fatal(err)
	}
	defer stopInstance()

	ohm := instance.GetFeature(outbound.ManagerType()).(outbound.Manager)
	before := ohm.GetHandler("proxy")

	if err := replaceOutbound(`{"tag": "proxy", "protocol": "blackhole"}`); err != nil {
		t.Fatal(err)
	}
	after := ohm.GetHandler("proxy")
	if after == nil || after == before {
		t.Fatal("proxy handler was not replaced")
	}
	if ohm.GetHandler("direct") == nil {
		t.Fatal("an unrelated outbound went missing")
	}

	if err := replaceOutbound(`{"protocol": "freedom"}`); err == nil || !strings.Contains(err.Error(), "tag") {
		t.Fatalf("untagged outbound: %v", err)
	}
	if err := replaceOutbound(`{"tag": "proxy", "protocol": "no-such-protocol"}`); err == nil {
		t.Fatal("unknown protocol accepted")
	}
	if ohm.GetHandler("proxy") != after {
		t.Fatal("a failed replace disturbed the running outbound")
	}
}

func TestCloseConnectionsParsesIds(t *testing.T) {
	if n := closeConnections("not,ids"); n != 0 {
		t.Fatalf("closed %d", n)
	}
	if n := closeConnections("*"); n < 0 {
		t.Fatal("negative count")
	}
	if got := connectionsJSON(); !strings.HasPrefix(got, "[") {
		t.Fatalf("connections JSON: %q", got)
	}
}
