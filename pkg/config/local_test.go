package config_test

import (
	"reflect"
	"testing"

	"github.com/yindia/cluster-vending/pkg/config"
)

// The local run files load: `make run` is the API server, with no AWS
// setting; `make run-controller` is the controller without leader
// election. The controller's file loads with the credentials Secret line
// both commented (IRSA) and uncommented.
func TestLocalRunFilesLoad(t *testing.T) {
	file, err := config.LoadFile("../../hack/config.local.yaml")
	if err != nil {
		t.Fatal(err)
	}
	serve, err := config.LoadServe(nil, lookupIn(nil), file)
	if err != nil {
		t.Fatalf("hack/config.local.yaml: %v", err)
	}
	if !reflect.DeepEqual(serve.NodeGroups, config.DefaultNodeGroups()) {
		t.Errorf("hack/config.local.yaml nodeGroups = %+v, want the defaults it documents", serve.NodeGroups)
	}
	file, err = config.LoadFile("../../hack/controller.local.yaml")
	if err != nil {
		t.Fatal(err)
	}
	controller, err := config.LoadController(nil, lookupIn(nil), file)
	if err != nil {
		t.Fatalf("hack/controller.local.yaml: %v", err)
	}
	if !reflect.DeepEqual(controller.NodeGroups, config.DefaultNodeGroups()) {
		t.Errorf("hack/controller.local.yaml nodeGroups = %+v, want the defaults it documents", controller.NodeGroups)
	}
	// The credentials Secret line is the developer's to uncomment and point
	// at their own Secret: either way the file loads.
	if controller.LeaderElect || controller.AWS.ManagementRoleARN == "" {
		t.Errorf("controller: %+v", controller)
	}
	withSecret, err := config.LoadController(nil,
		lookupIn(map[string]string{config.EnvProviderCredentialsSecret: "crossplane-system/aws-local-credentials"}), file)
	if err != nil || withSecret.AWS.ProviderCredentials == nil {
		t.Errorf("controller with the Secret: %+v (%v)", withSecret.AWS, err)
	}
}
