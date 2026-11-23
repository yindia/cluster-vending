// access-qualification qualifies a NaaS cluster for sub-tenant access
// (PROP-05 6.1). Two modes:
//
//   - --run is the naas-qualifier Job's body, in the workload cluster: it runs
//     the required checks against two throwaway sub-tenants and leaves the
//     result for the controller, which records it.
//   - --print emits, from outside, the key a cluster's record must carry and
//     the checks it must hold, for an operator's manual record. It writes
//     nothing and asserts nothing.
//
// It never reads Kubernetes Secrets. The logic is in pkg/naas.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/yindia/cluster-vending/pkg/kube"
	"github.com/yindia/cluster-vending/pkg/naas"
)

func main() {
	run := flag.Bool("run", false, "Run the checks in-cluster (the naas-qualifier Job)")
	key := flag.String("key", "", "With --run: the qualification key, as JSON, given by the controller")
	probeImage := flag.String("probe-image", "", "With --run: the image of the test pods")
	printMode := flag.Bool("print", false, "Print the key and checks a cluster's record must carry")
	managementContext := flag.String("management-context", "", "With --print: explicit management kubeconfig context")
	workloadContext := flag.String("workload-context", "", "With --print: explicit workload qualification-operator context")
	managementConfig := flag.String("management-kubeconfig", "", "With --print: management kubeconfig path (default loading rules when empty)")
	workloadConfig := flag.String("workload-kubeconfig", "", "With --print: workload kubeconfig path (default loading rules when empty)")
	namespace := flag.String("namespace", "", "With --print: management tenant namespace")
	cluster := flag.String("cluster", "", "With --print: the Cluster object's name")
	flag.Parse()
	var err error
	switch {
	case *run == *printMode || flag.NArg() != 0:
		flag.Usage()
		os.Exit(2)
	case *run:
		err = runJob(*key, *probeImage)
	default:
		if *managementContext == "" || *workloadContext == "" || *namespace == "" || *cluster == "" {
			flag.Usage()
			os.Exit(2)
		}
		err = emitRequest(*managementContext, *workloadContext, *managementConfig, *workloadConfig, *namespace, *cluster)
	}
	if err != nil {
		// Do not print raw REST errors: they can include authentication output.
		fmt.Fprintln(os.Stderr, "Qualification unavailable. Verify the current cluster, its OIDC, the Capsule bootstrap and the qualifier's RBAC with your approved operator tooling.")
		os.Exit(1)
	}
}

func runJob(key, probeImage string) error {
	if key == "" || probeImage == "" {
		return fmt.Errorf("--key and --probe-image are required with --run")
	}
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return err
	}
	cfg.Timeout = 30 * time.Second
	scheme, err := kube.Scheme()
	if err != nil {
		return err
	}
	clients, err := naas.ImpersonatingClients(cfg, scheme)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()
	result, err := naas.RunQualifierJob(ctx, &naas.QualifierRunner{Clients: clients, ProbeImage: probeImage}, key, os.Getenv("QUALIFIER_JOB_UID"))
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(result)
}

func emitRequest(managementContext, workloadContext, managementConfig, workloadConfig, namespace, cluster string) error {
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
	request, err := naas.CurrentClusterQualificationRequest(ctx, management, workload, namespace, cluster)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(request)
}
