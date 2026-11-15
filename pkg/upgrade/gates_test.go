package upgrade

import (
	"reflect"
	"testing"
	"time"
)

// ready is a cluster nothing holds back from an upgrade to 1.37.
func ready() Facts {
	return Facts{Mode: Automatic, Target: "1.37", Ready: true, PlatformReady: true,
		DeprecationsScanned: true, DeprecationsTarget: "1.37", WindowOpen: true}
}

func TestCheck(t *testing.T) {
	saturday := time.Date(2026, 11, 21, 2, 0, 0, 0, time.UTC)
	with := func(change func(*Facts)) Facts {
		f := ready()
		change(&f)
		return f
	}
	tests := []struct {
		name  string
		facts Facts
		want  []Gate
	}{
		{name: "nothing holds it back", facts: ready()},
		{name: "manual", facts: with(func(f *Facts) { f.Mode = Manual }),
			want: []Gate{{Code: GateManual, Blocking: true, Message: "manual upgrades: 1.37 is available, not started"}}},
		{name: "empty mode is manual", facts: with(func(f *Facts) { f.Mode = "" }),
			want: []Gate{{Code: GateManual, Blocking: true, Message: "manual upgrades: 1.37 is available, not started"}}},
		{name: "halted", facts: with(func(f *Facts) { f.Halted = true }),
			want: []Gate{{Code: GateHalted, Blocking: true, Message: "the last upgrade failed; it starts again once it is fixed"}}},
		{name: "already upgrading", facts: with(func(f *Facts) { f.Upgrading = true }),
			want: []Gate{{Code: GateUpgrading, Blocking: true, Message: "an upgrade is already running"}}},
		{name: "paused", facts: with(func(f *Facts) { f.Paused = true }),
			want: []Gate{{Code: GatePaused, Blocking: true, Message: "the cluster is paused"}}},
		{name: "frozen", facts: with(func(f *Facts) { f.Frozen, f.FrozenMessage = true, "the platform is frozen: provider upgrade" }),
			want: []Gate{{Code: GateFrozen, Blocking: true, Message: "the platform is frozen: provider upgrade"}}},
		{name: "frozen, no message", facts: with(func(f *Facts) { f.Frozen = true }),
			want: []Gate{{Code: GateFrozen, Blocking: true, Message: "frozen"}}},
		{name: "not ready", facts: with(func(f *Facts) { f.Ready = false }),
			want: []Gate{{Code: GateNotReady, Blocking: true, Message: "the cluster is not Ready"}}},
		{name: "platform not ready", facts: with(func(f *Facts) { f.PlatformReady = false }),
			want: []Gate{{Code: GatePlatform, Blocking: true, Message: "a platform component is not Ready"}}},
		{name: "never scanned", facts: with(func(f *Facts) { f.DeprecationsScanned = false }),
			want: []Gate{{Code: GateDeprecations, Blocking: true, Message: "waiting for the deprecation scan of 1.37"}}},
		{name: "scanned for another minor", facts: with(func(f *Facts) { f.DeprecationsTarget = "1.36" }),
			want: []Gate{{Code: GateDeprecations, Blocking: true, Message: "waiting for the deprecation scan of 1.37"}}},
		{name: "removed APIs in use", facts: with(func(f *Facts) { f.RemovedInUse = 3 }),
			want: []Gate{{Code: GateDeprecations, Blocking: true, Message: "3 objects use APIs removed in 1.37; update them first"}}},
		{name: "one removed API in use", facts: with(func(f *Facts) { f.RemovedInUse = 1 }),
			want: []Gate{{Code: GateDeprecations, Blocking: true, Message: "1 object uses APIs removed in 1.37; update it first"}}},
		{name: "window closed", facts: with(func(f *Facts) { f.WindowOpen, f.NextOpen = false, saturday }),
			want: []Gate{{Code: GateWindow, Blocking: true, Message: "outside the maintenance window; it opens Sat 21 Nov 02:00 UTC"}}},
		{name: "window closed, no next opening", facts: with(func(f *Facts) { f.WindowOpen = false }),
			want: []Gate{{Code: GateWindow, Blocking: true, Message: "outside the maintenance window"}}},
		{name: "every reason, in order", facts: Facts{Mode: Manual, Target: "1.37", Halted: true, Upgrading: true, Frozen: true, Paused: true},
			want: []Gate{
				{Code: GateManual, Blocking: true, Message: "manual upgrades: 1.37 is available, not started"},
				{Code: GateHalted, Blocking: true, Message: "the last upgrade failed; it starts again once it is fixed"},
				{Code: GateUpgrading, Blocking: true, Message: "an upgrade is already running"},
				{Code: GateFrozen, Blocking: true, Message: "frozen"},
				{Code: GatePaused, Blocking: true, Message: "the cluster is paused"},
				{Code: GateNotReady, Blocking: true, Message: "the cluster is not Ready"},
				{Code: GatePlatform, Blocking: true, Message: "a platform component is not Ready"},
				{Code: GateDeprecations, Blocking: true, Message: "waiting for the deprecation scan of 1.37"},
				{Code: GateWindow, Blocking: true, Message: "outside the maintenance window"},
			}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Check(tt.facts)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Check() =\n%+v\nwant\n%+v", got, tt.want)
			}
			if Blocks(got) != (len(tt.want) > 0) {
				t.Fatalf("Blocks() = %v with gates %+v", Blocks(got), got)
			}
		})
	}
}

func TestBlockedIgnoresWarnings(t *testing.T) {
	if Blocks([]Gate{{Code: "WARNING", Message: "only a warning"}}) {
		t.Fatal("a gate that does not block blocks")
	}
	if Blocks(nil) {
		t.Fatal("no gates blocks")
	}
}
