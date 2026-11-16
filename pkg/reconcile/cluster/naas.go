package cluster

import (
	"github.com/yindia/cluster-vending/pkg/component"
	"github.com/yindia/cluster-vending/pkg/database/model"
)

// An in-flight release move owns its ordered component set until it completes.
// Applying a feature toggle in the middle would shift the saved step numbers,
// skipping an unfinished upgrade or reverting an already-upgraded component.
// Queue the desired feature change behind that move, keeping the previously
// observed selection. A halted move still requires its normal retry/fix.
func naasForPass(desired bool, row *model.Cluster, entries []component.PlatformEntry) bool {
	if !ReleaseMoving(row) {
		return desired
	}
	without := map[string]bool{}
	for _, entry := range component.ForNaaS(entries, false) {
		without[entry.Name] = true
	}
	observed := map[string]bool{}
	for _, item := range storedStatus(row).GetPlatformComponents() {
		observed[item.GetName()] = true
	}
	for _, entry := range entries {
		if !without[entry.Name] && observed[entry.Name] {
			return true
		}
	}
	return false
}
