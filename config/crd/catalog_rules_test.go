package crd_test

import (
	"fmt"
	"strings"
	"testing"
)

// PROP-01 4.5: the CEL rules of the catalog kinds, at apply time.

func definitionDoc(name, spec string) []byte {
	return []byte("apiVersion: cloud.cluster-vending.io/v1alpha1\nkind: ComponentDefinition\nmetadata:\n  name: " + name + "\nspec:\n" +
		"  provider: aws\n  description: d\n  namespace: x\n" + spec +
		"  install:\n    helm: {repository: 'https://charts.example.com', chart: x}\n")
}

func TestDefinitionSequenceBounds(t *testing.T) {
	v := newValidator(t, "ComponentDefinition")
	for _, tc := range []struct {
		sequence int
		refused  bool
	}{{0, false}, {1, false}, {10000, false}, {10001, true}, {-1, true}} {
		errs := v.validate(t, definitionDoc("x", fmt.Sprintf("  sequence: %d\n", tc.sequence)))
		if tc.refused != (len(errs) > 0) {
			t.Errorf("sequence %d: errors %v, want refused %v", tc.sequence, errs.ToAggregate(), tc.refused)
		}
	}
}

func TestDefinitionInstallTimeoutBounds(t *testing.T) {
	v := newValidator(t, "ComponentDefinition")
	for _, tc := range []struct {
		timeout string
		refused bool
	}{{"1m", false}, {"15m", false}, {"60m", false}, {"1h", false}, {"59s", true}, {"61m", true}, {"2h", true}, {"0s", true}} {
		doc := []byte("apiVersion: cloud.cluster-vending.io/v1alpha1\nkind: ComponentDefinition\nmetadata:\n  name: x\nspec:\n" +
			"  provider: aws\n  description: d\n  namespace: x\n  install:\n    timeout: " + tc.timeout +
			"\n    helm: {repository: 'https://charts.example.com', chart: x}\n")
		errs := v.validate(t, doc)
		if tc.refused != (len(errs) > 0) {
			t.Errorf("timeout %s: errors %v, want refused %v", tc.timeout, errs.ToAggregate(), tc.refused)
		}
	}
}

// The CRD's role rules: the NodeProvisioner is not disabled, a role is a
// Platform definition's, and the name karpenter is no rule of its own.
func TestDefinitionRoleRules(t *testing.T) {
	v := newValidator(t, "ComponentDefinition")
	for _, tc := range []struct {
		name, spec string
		refused    bool
	}{
		{"nodes", "  management: Platform\n  role: NodeProvisioner\n", false},
		{"nodes", "  management: Platform\n  role: NodeProvisioner\n  disabled: true\n", true},
		{"nodes", "  management: Platform\n  role: NodeProvisioner\n  disabled: false\n", false},
		{"tenancy", "  management: Platform\n  role: NamespaceTenancy\n  disabled: true\n", false},
		{"nodes", "  role: NodeProvisioner\n", true}, // management defaults to Tenant
		{"nodes", "  management: Platform\n  role: Gateway\n", true},
		{"karpenter", "  sequence: 100\n", false},
		{"other", "  sequence: 0\n", false},
	} {
		errs := v.validate(t, definitionDoc(tc.name, tc.spec))
		if tc.refused != (len(errs) > 0) {
			t.Errorf("%s %q: errors %v, want refused %v", tc.name, strings.TrimSpace(tc.spec), errs.ToAggregate(), tc.refused)
		}
	}
}

func TestDefinitionSizingShape(t *testing.T) {
	v := newValidator(t, "ComponentDefinition")
	for _, tc := range []struct {
		spec    string
		refused bool
	}{
		{"  sizing:\n    deployments: [{name: a, replicas: replicaCount, requests: resources.requests}]\n    daemonSets: [{name: b, requests: node.resources.requests}]\n", false},
		{"  sizing:\n    deployments: [{name: a, requests: resources.requests}]\n", true},
		{"  sizing:\n    deployments: [{name: a, replicas: 'a..b', requests: resources.requests}]\n", true},
	} {
		errs := v.validate(t, definitionDoc("x", "  management: Platform\n"+tc.spec))
		if tc.refused != (len(errs) > 0) {
			t.Errorf("%q: errors %v, want refused %v", strings.TrimSpace(tc.spec), errs.ToAggregate(), tc.refused)
		}
	}
}

func releaseDoc(serial string) []byte {
	return []byte("apiVersion: cloud.cluster-vending.io/v1alpha1\nkind: Release\nmetadata:\n  name: 2026-11-a\nspec:\n" + serial +
		"  content:\n    renderVersion: 1\n    kubernetes:\n" +
		"    - {minor: '1.35', endOfSupport: '2027-01-01T00:00:00Z'}\n" +
		"    - {minor: '1.36', endOfSupport: '2027-01-01T00:00:00Z'}\n" +
		"    - {minor: '1.37', default: true, endOfSupport: '2027-01-01T00:00:00Z'}\n")
}

func TestReleaseSerialBounds(t *testing.T) {
	v := newValidator(t, "Release")
	for _, tc := range []struct {
		serial  string
		refused bool
	}{{"", false}, {"  serial: 1\n", false}, {"  serial: 12\n", false}, {"  serial: 0\n", true}, {"  serial: -3\n", true}} {
		errs := v.validate(t, releaseDoc(tc.serial))
		if tc.refused != (len(errs) > 0) {
			t.Errorf("%q: errors %v, want refused %v", tc.serial, errs.ToAggregate(), tc.refused)
		}
	}
}

// A serial cannot change once set; one that was never set may be added.
func TestReleaseSerialIsImmutableOnceSet(t *testing.T) {
	v := newValidator(t, "Release")
	for _, tc := range []struct {
		name, from, to string
		refused        bool
	}{
		{"same", "  serial: 5\n", "  serial: 5\n", false},
		{"changed", "  serial: 5\n", "  serial: 6\n", true},
		{"removed", "  serial: 5\n", "", true},
		{"added", "", "  serial: 5\n", false},
		{"never set", "", "", false},
	} {
		errs, err := v.v.ValidateUpdate(t.Context(), releaseDoc(tc.to), releaseDoc(tc.from))
		if err != nil {
			t.Fatal(err)
		}
		if tc.refused != (len(errs) > 0) {
			t.Errorf("%s: errors %v, want refused %v", tc.name, errs.ToAggregate(), tc.refused)
		}
	}
}

func channelDoc(name, spec string) []byte {
	return []byte("apiVersion: cloud.cluster-vending.io/v1alpha1\nkind: ReleaseChannel\nmetadata:\n  name: " + name + "\nspec:\n" + spec)
}

func TestChannelReleaseSoakOnlyForAutomatic(t *testing.T) {
	v := newValidator(t, "ReleaseChannel")
	for _, tc := range []struct {
		name, spec string
		refused    bool
	}{
		{"automatic with soak", "  promotion: Automatic\n  policy: {releaseSoak: 48h}\n", false},
		{"automatic without soak", "  promotion: Automatic\n", false},
		{"manual with soak", "  promotion: Manual\n  release: 2026-11-a\n  policy: {releaseSoak: 48h}\n", true},
		{"pinned with soak", "  promotion: Pinned\n  release: 2026-11-a\n  policy: {releaseSoak: 0s}\n", true},
		{"manual without soak", "  promotion: Manual\n  release: 2026-11-a\n  policy: {soak: 1h}\n", false},
	} {
		errs := v.validate(t, channelDoc("beta", tc.spec))
		if tc.refused != (len(errs) > 0) {
			t.Errorf("%s: errors %v, want refused %v", tc.name, errs.ToAggregate(), tc.refused)
		}
	}
}
