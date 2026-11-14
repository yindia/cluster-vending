package upgrade

import (
	"fmt"
	"time"
)

// Mode is a cluster's upgrade mode, as the Cluster kind names it.
type Mode string

const (
	// Manual clusters are never upgraded by the platform; an empty mode
	// is Manual.
	Manual Mode = "Manual"
	// Automatic clusters follow their channel's minor.
	Automatic Mode = "Automatic"
)

// GateCode names why an upgrade may not start.
type GateCode string

// The gates, in the order Check reports them.
const (
	GateManual       GateCode = "MANUAL"
	GateHalted       GateCode = "HALTED"
	GateUpgrading    GateCode = "UPGRADING"
	GatePaused       GateCode = "PAUSED"
	GateHeld         GateCode = "HELD"
	GateNotReady     GateCode = "NOT_READY"
	GatePlatform     GateCode = "PLATFORM_NOT_READY"
	GateDeprecations GateCode = "DEPRECATIONS"
	GateWindow       GateCode = "MAINTENANCE_WINDOW"
)

// Gate is one reason an upgrade may not start now. Message is for the
// cluster's users.
type Gate struct {
	Code     GateCode
	Blocking bool
	Message  string
}

// Facts are what Check needs to know of one cluster and the move NextHop
// chose for it (Target).
type Facts struct {
	Mode Mode
	// Target is the minor the cluster would move to.
	Target string
	// Ready: the cluster is Ready; PlatformReady: every platform
	// component is Ready.
	Ready, PlatformReady bool
	// Paused: the cluster's resources are paused; Held: its upgrades are
	// paused (a ReleaseHold), HeldMessage says why and until when.
	Paused, Held bool
	HeldMessage  string
	// DeprecationsScanned: a deprecation scan finished; DeprecationsTarget
	// is the minor it checked against; RemovedInUse counts the objects it
	// found on APIs the target removes.
	DeprecationsScanned bool
	DeprecationsTarget  string
	RemovedInUse        int
	// WindowOpen: now is inside the cluster's maintenance window (always
	// true without one); NextOpen is when it opens next, zero if unknown.
	WindowOpen bool
	NextOpen   time.Time
	// Upgrading: an upgrade is in progress; Halted: the last one failed
	// and has not been fixed.
	Upgrading, Halted bool
}

// Check returns every reason the cluster may not start its upgrade to
// f.Target now, in a fixed order; none when it may.
func Check(f Facts) []Gate {
	var gates []Gate
	block := func(code GateCode, message string) {
		gates = append(gates, Gate{Code: code, Blocking: true, Message: message})
	}
	if f.Mode != Automatic {
		block(GateManual, fmt.Sprintf("manual upgrades: %s is available, not started", f.Target))
	}
	if f.Halted {
		block(GateHalted, "the last upgrade failed; it starts again once it is fixed")
	}
	if f.Upgrading {
		block(GateUpgrading, "an upgrade is already running")
	}
	if f.Paused {
		block(GatePaused, "the cluster is paused")
	}
	if f.Held {
		message := f.HeldMessage
		if message == "" {
			message = "upgrades paused"
		}
		block(GateHeld, message)
	}
	if !f.Ready {
		block(GateNotReady, "the cluster is not Ready")
	}
	if !f.PlatformReady {
		block(GatePlatform, "a platform component is not Ready")
	}
	if message := deprecations(f); message != "" {
		block(GateDeprecations, message)
	}
	if !f.WindowOpen {
		block(GateWindow, window(f.NextOpen))
	}
	return gates
}

func deprecations(f Facts) string {
	switch {
	case !f.DeprecationsScanned || f.DeprecationsTarget != f.Target:
		return fmt.Sprintf("waiting for the deprecation scan of %s", f.Target)
	case f.RemovedInUse == 1:
		return fmt.Sprintf("1 object uses APIs removed in %s; update it first", f.Target)
	case f.RemovedInUse > 1:
		return fmt.Sprintf("%d objects use APIs removed in %s; update them first", f.RemovedInUse, f.Target)
	}
	return ""
}

func window(next time.Time) string {
	if next.IsZero() {
		return "outside the maintenance window"
	}
	return "outside the maintenance window; it opens " + next.UTC().Format("Mon 2 Jan 15:04") + " UTC"
}

// Blocks reports whether any gate blocks the upgrade.
func Blocks(gates []Gate) bool {
	for _, g := range gates {
		if g.Blocking {
			return true
		}
	}
	return false
}
