package config_test

import (
	"slices"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"sigs.k8s.io/yaml"

	"github.com/yindia/cluster-vending/pkg/config"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// The live reader (PROP-02 5.5): one replica, Recreate, its own service
// account (the name the controller binds in tenant namespaces), a role that
// writes nothing and grants no Secret, reachable only from the API server.
func TestLiveReaderDeployment(t *testing.T) {
	d := deployment(t, "live/deployment.yaml")
	if d.Spec.Replicas == nil || *d.Spec.Replicas != 1 || d.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
		t.Errorf("live reader: replicas %v, strategy %q; want one, Recreate", d.Spec.Replicas, d.Spec.Strategy.Type)
	}
	pod := d.Spec.Template.Spec
	if got := "cluster-vending-" + pod.ServiceAccountName; got != reconcile.LiveReaderServiceAccount {
		t.Errorf("live reader runs as %s, the controller binds %s", got, reconcile.LiveReaderServiceAccount)
	}
	if c := pod.Containers[0]; !slices.Equal(c.Command, []string{"/cluster-vending", "live"}) {
		t.Errorf("command = %v", c.Command)
	}
	_, envs := known(config.GlobalSettings(), config.LiveSettings())
	for _, e := range pod.Containers[0].Env {
		if !slices.Contains(envs, e.Name) {
			t.Errorf("live reader: %s is not a setting of the live command", e.Name)
		}
	}

	role := clusterRole(t, "live/role.yaml")
	for _, verb := range []string{"create", "update", "patch", "delete", "deletecollection", "*"} {
		for _, r := range role.Rules {
			if slices.Contains(r.Verbs, verb) {
				t.Errorf("live role may %s %v", verb, r.Resources)
			}
		}
	}
	for _, verb := range []string{"get", "list", "watch"} {
		if allows(role, "", "secrets", verb) {
			t.Errorf("live role may %s Secrets; it reads only through the tenant namespaces' binding", verb)
		}
	}
	if !allows(role, "cloud.cluster-vending.io", "clusters", "get") || !allows(role, "eks.aws.m.upbound.io", "clusterauths", "get") {
		t.Error("live role cannot read Clusters and ClusterAuths")
	}

	np := &networkingv1.NetworkPolicy{}
	if err := yaml.Unmarshal(documents(t, "live/network_policy.yaml")[0], np); err != nil {
		t.Fatal(err)
	}
	if len(np.Spec.Ingress) != 1 || len(np.Spec.Ingress[0].From) != 1 ||
		np.Spec.Ingress[0].From[0].PodSelector.MatchLabels["app.kubernetes.io/name"] != "cluster-vending-server" {
		t.Errorf("live reader ingress = %+v, want the API server's pods only", np.Spec.Ingress)
	}
	if !slices.Contains(np.Spec.PolicyTypes, networkingv1.PolicyTypeEgress) {
		t.Error("live reader egress is not limited")
	}

	// The server reaches it by its Service, and is never told to read live
	// without authentication.
	env := literals(t, serverBase)
	if env[config.EnvLiveURL] != "http://cluster-vending-live:8090" {
		t.Errorf("server %s = %q", config.EnvLiveURL, env[config.EnvLiveURL])
	}
	if _, set := env[config.EnvLiveAllowWithoutAuth]; set {
		t.Errorf("the deploy base sets %s", config.EnvLiveAllowWithoutAuth)
	}
}
