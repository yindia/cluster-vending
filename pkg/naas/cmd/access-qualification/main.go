// access-qualification emits a fresh runtime-test binding request. It never
// applies resources, reads Kubernetes Secrets, or asserts that tests passed.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/yindia/cluster-vending/pkg/kube"
	"github.com/yindia/cluster-vending/pkg/naas"
)

func main() {
	managementContext := flag.String("management-context", "", "Explicit management kubeconfig context")
	workloadContext := flag.String("workload-context", "", "Explicit workload qualification-operator context")
	managementConfig := flag.String("management-kubeconfig", "", "Management kubeconfig path (default loading rules when empty)")
	workloadConfig := flag.String("workload-kubeconfig", "", "Workload kubeconfig path (default loading rules when empty)")
	namespace := flag.String("namespace", "", "Management tenant namespace")
	allocation := flag.String("allocation", "", "Management SubTenant UUID")
	flag.Parse()
	if *managementContext == "" || *workloadContext == "" || *namespace == "" || *allocation == "" || flag.NArg() != 0 {
		flag.Usage()
		os.Exit(2)
	}
	if err := emitRequest(*managementContext, *workloadContext, *managementConfig, *workloadConfig, *namespace, *allocation); err != nil {
		// Do not print raw REST errors: they can include authentication output.
		fmt.Fprintln(os.Stderr, "Qualification request unavailable. Verify current parent, OIDC association, Capsule bootstrap and namespace readiness with your approved operator tooling.")
		os.Exit(1)
	}
}

func emitRequest(managementContext, workloadContext, managementConfig, workloadConfig, namespace, allocation string) error {
	scheme, err := kube.Scheme()
	if err != nil {
		return err
	}
	newClient := func(contextName, path string) (client.Client, error) {
		rules := clientcmd.NewDefaultClientConfigLoadingRules()
		if path != "" {
			rules.ExplicitPath = path
		}
		config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{CurrentContext: contextName}).ClientConfig()
		if err != nil {
			return nil, err
		}
		config.Timeout = 15 * time.Second
		return client.New(config, client.Options{Scheme: scheme})
	}
	management, err := newClient(managementContext, managementConfig)
	if err != nil {
		return err
	}
	workload, err := newClient(workloadContext, workloadConfig)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	request, err := naas.CurrentAccessQualificationRequest(ctx, management, workload, namespace, allocation)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(request)
}
