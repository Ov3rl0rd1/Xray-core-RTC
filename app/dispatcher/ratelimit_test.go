package dispatcher

import (
	"testing"

	"github.com/xtls/xray-core/common/shaper"
)

// A deployment that has not configured anything must not be shaped. This is
// the regression that matters: the level table is a guess, every user arrives
// at level 0, and a guess applied to a whole fleet reads as "the VPN got slow"
// with nothing in the logs to say why.
func TestTierLimitsOffByDefault(t *testing.T) {
	defer restoreTiers(tiersEnabled)
	tiersEnabled = false

	for _, level := range []uint32{0, 1, 2, 3, 99} {
		got := TierLimits(shaper.User{Email: "u", Level: level})
		if !got.Unlimited() {
			t.Errorf("level %d: shaped with tiers off: up=%d down=%d", level, got.UplinkBPS, got.DownlinkBPS)
		}
	}
}

// Switched on, the table is the table — including for a level nobody defined,
// which must fall back to the ordinary plan rather than to unlimited.
func TestTierLimitsOnWhenEnabled(t *testing.T) {
	defer restoreTiers(tiersEnabled)
	tiersEnabled = true

	if got := TierLimits(shaper.User{Email: "u", Level: 0}); got.DownlinkBPS != tiers[0].DownlinkBPS {
		t.Errorf("level 0: got %d, want %d", got.DownlinkBPS, tiers[0].DownlinkBPS)
	}
	if got := TierLimits(shaper.User{Email: "u", Level: 99}); got.DownlinkBPS != defaultTier.DownlinkBPS {
		t.Errorf("unknown level: got %d, want the default tier %d", got.DownlinkBPS, defaultTier.DownlinkBPS)
	}
	if got := TierLimits(shaper.User{Email: "u", Level: 3}); !got.Unlimited() {
		t.Error("level 3 is the unshaped tier and must stay unshaped")
	}
}

func TestTiersOnParsing(t *testing.T) {
	off := []string{"", "  ", "0", "false", "FALSE", "off", "no"}
	on := []string{"1", "true", "TRUE", "on", "yes", "anything"}

	for _, v := range off {
		if tiersOn(v) {
			t.Errorf("%q should leave the table off", v)
		}
	}
	for _, v := range on {
		if !tiersOn(v) {
			t.Errorf("%q should switch the table on", v)
		}
	}
}

func restoreTiers(v bool) { tiersEnabled = v }
