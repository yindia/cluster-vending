package config_test

import (
	"bytes"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

// The chart's alerts are the rule groups of config/prometheus/rules.yaml:
// `make helm-sync` copies them.
func TestChartAlertsAreACopyOfConfig(t *testing.T) {
	var rule struct {
		Spec map[string]any `json:"spec"`
	}
	raw, err := os.ReadFile("prometheus/rules.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(raw, &rule); err != nil {
		t.Fatal(err)
	}
	var copied map[string]any
	raw, err = os.ReadFile(chartDir + "/files/prometheus-rules.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(raw, &copied); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rule.Spec, copied) {
		t.Error("charts/cluster-vending/files/prometheus-rules.yaml differs from config/prometheus/rules.yaml: run make helm-sync")
	}
	// Every alert names its runbook.
	groups, _ := copied["groups"].([]any)
	for _, g := range groups {
		rules, _ := g.(map[string]any)["rules"].([]any)
		for _, r := range rules {
			annotations, _ := r.(map[string]any)["annotations"].(map[string]any)
			if url, _ := annotations["runbook_url"].(string); !strings.Contains(url, "docs/architecture.md#") {
				t.Errorf("alert %v has no runbook_url", r.(map[string]any)["alert"])
			}
		}
	}
}

// kindsNamed are the names of the rendered objects of kind.
func kindsNamed(out []byte, kind string) []string {
	var names []string
	for _, doc := range bytes.Split(out, []byte("\n---\n")) {
		var meta struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		}
		if yaml.Unmarshal(doc, &meta) == nil && meta.Kind == kind {
			names = append(names, meta.Metadata.Name)
		}
	}
	slices.Sort(names)
	return names
}

// Metrics are on by default: the controller serves them, the live reader
// on a port of its own that only networkPolicy.metricsFrom reaches; the
// Prometheus Operator objects are opt-in.
func TestChartMetrics(t *testing.T) {
	r := renderChart(t)
	controller := r.deployment(t, "cluster-vending-controller").Spec.Template.Spec.Containers[0]
	if !slices.Contains(controller.Args, "--metrics-addr=:8080") {
		t.Errorf("controller args %v: metrics are off", controller.Args)
	}
	live := r.deployment(t, "cluster-vending-live").Spec.Template.Spec.Containers[0]
	env := map[string]string{}
	for _, e := range live.Env {
		env[e.Name] = e.Value
	}
	if env["CV_LIVE_METRICS_ADDR"] != ":8091" || !slices.ContainsFunc(live.Ports, func(p corev1.ContainerPort) bool { return p.Name == "metrics" && p.ContainerPort == 8091 }) {
		t.Errorf("live reader: metrics listener %q, ports %+v", env["CV_LIVE_METRICS_ADDR"], live.Ports)
	}
	policy := r.networkPolicies["cluster-vending-live"]
	if policy == nil || len(policy.Spec.Ingress) != 2 {
		t.Fatalf("live reader policy: %+v", policy)
	}
	metrics := policy.Spec.Ingress[1]
	if len(metrics.Ports) != 1 || metrics.Ports[0].Port.IntValue() != 8091 || len(metrics.From) != 1 ||
		metrics.From[0].NamespaceSelector == nil || metrics.From[0].NamespaceSelector.MatchLabels["metrics"] != "enabled" {
		t.Errorf("the metrics port is reachable by %+v on %+v, want metrics=enabled namespaces on 8091 only", metrics.From, metrics.Ports)
	}

	out, err := helmTemplate(t, "-f", chartAuthValues)
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	if got := append(kindsNamed(out, "ServiceMonitor"), kindsNamed(out, "PrometheusRule")...); len(got) != 0 {
		t.Errorf("Prometheus Operator objects by default: %v", got)
	}
	out, err = helmTemplate(t, "-f", chartAuthValues, "--set", "metrics.serviceMonitor.enabled=true",
		"--set", "metrics.prometheusRule.enabled=true", "--set", "metrics.serviceMonitor.labels.release=prom")
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	if got, want := kindsNamed(out, "ServiceMonitor"), []string{"cluster-vending-controller", "cluster-vending-live", "cluster-vending-server"}; !slices.Equal(got, want) {
		t.Errorf("ServiceMonitors %v, want %v", got, want)
	}
	if got := kindsNamed(out, "PrometheusRule"); !slices.Equal(got, []string{"cluster-vending-alerts"}) {
		t.Errorf("PrometheusRules %v", got)
	}
	if !bytes.Contains(out, []byte("release: prom")) || !bytes.Contains(out, []byte("alert: ClusterVendingControllerDown")) {
		t.Error("the ServiceMonitor labels or the alerts are not rendered")
	}
}
