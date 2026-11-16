package config_test

import (
	"path/filepath"
	"testing"

	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
)

func TestPhysicalEKSReservationsHaveGlobalRBAC(t *testing.T) {
	if cloudstore.EKSNameReservationNamespace != "kube-system" {
		t.Fatal("coordination namespace changed; requires explicit migration")
	}
	controller := clusterRole(t, controllerRole)
	for _, verb := range []string{"get", "list", "create", "update", "patch", "delete"} {
		if !allows(controller, "", "configmaps", verb) {
			t.Errorf("controller ClusterRole cannot %s global EKS reservation ConfigMaps", verb)
		}
	}
	server := clusterRole(t, filepath.Join(serverBase, "role.yaml"))
	for _, verb := range []string{"get", "create", "delete"} {
		if !allows(server, "", "configmaps", verb) {
			t.Errorf("server ClusterRole cannot %s global EKS reservation ConfigMaps", verb)
		}
	}
}
