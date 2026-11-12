package config_test

import (
	"testing"

	"github.com/yindia/cluster-vending/pkg/config"
)

// The local run files load: `make run` is the API server on the
// Kubernetes store, without the background loop and without any AWS
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
	if serve.Store != config.StoreKubernetes || serve.BackgroundLoop {
		t.Errorf("serve: store %s, background loop %t; want the Kubernetes store, the controller reconciling", serve.Store, serve.BackgroundLoop)
	}
	file, err = config.LoadFile("../../hack/controller.local.yaml")
	if err != nil {
		t.Fatal(err)
	}
	controller, err := config.LoadController(nil, lookupIn(nil), file)
	if err != nil {
		t.Fatalf("hack/controller.local.yaml: %v", err)
	}
	if controller.LeaderElect || controller.AWS.ManagementAccountID != "111122223333" || controller.AWS.ProviderCredentials != nil {
		t.Errorf("controller: %+v", controller)
	}
	withSecret, err := config.LoadController(nil,
		lookupIn(map[string]string{config.EnvProviderCredentialsSecret: "crossplane-system/aws-local-credentials"}), file)
	if err != nil || withSecret.AWS.ProviderCredentials == nil {
		t.Errorf("controller with the Secret: %+v (%v)", withSecret.AWS, err)
	}
}
