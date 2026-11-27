package aws_test

import (
	"slices"
	"strings"
	"testing"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
)

// validEntry is an add-on entry the loader accepts; the cases below break
// one thing each.
const validEntry = `
  - name: vpc-cni
    default: true
`

func TestParseAddonCatalogRefusesBadEntries(t *testing.T) {
	tests := []struct {
		name, yaml, want string
	}{
		{name: "an unknown field", want: "unknown field",
			yaml: "addons:" + validEntry + "    colour: blue\n"},
		{name: "an unknown top-level field", want: "unknown field",
			yaml: "addons:" + validEntry + "extra: 1\n"},
		{name: "a duplicate name", want: `"vpc-cni" twice`,
			yaml: "addons:" + validEntry + validEntry},
		{name: "an empty name", want: "name",
			yaml: "addons:\n  - default: true\n"},
		{name: "a name EKS cannot have", want: "name",
			yaml: "addons:" + validEntry + "  - name: Not_An_Addon\n"},
		{name: "no add-ons", want: "no add-ons", yaml: "addons: []\n"},
		{name: "no default add-on", want: "default", yaml: "addons:\n  - name: vpc-cni\n"},
		{name: "a placement without paths", want: "paths",
			yaml: "addons:" + validEntry + "  - name: coredns\n    placement: {paths: []}\n    schema: {version: v1, source: s}\n"},
		{name: "an empty path", want: "path",
			yaml: "addons:" + validEntry + "  - name: coredns\n    placement: {paths: [\"\"]}\n    schema: {version: v1, source: s}\n"},
		{name: "a path with an empty key", want: "path",
			yaml: "addons:" + validEntry + "  - name: coredns\n    placement: {paths: [a..b]}\n    schema: {version: v1, source: s}\n"},
		{name: "a path twice", want: "path",
			yaml: "addons:" + validEntry + "  - name: coredns\n    placement: {paths: [., .]}\n    schema: {version: v1, source: s}\n"},
		{name: "a placement without its schema source", want: "schema",
			yaml: "addons:" + validEntry + "  - name: coredns\n    placement: {paths: [.]}\n"},
		{name: "a configuration without its schema source", want: "schema",
			yaml: "addons:" + validEntry + "  - name: coredns\n    configuration: {a: b}\n"},
		{name: "an empty configuration", want: "configuration",
			yaml: "addons:" + validEntry + "  - name: coredns\n    configuration: {}\n    schema: {version: v1, source: s}\n"},
		{name: "an unknown hook", want: `hook "nope"`,
			yaml: "addons:" + validEntry + "  - name: x\n    setup: {short: x, namespace: n, serviceAccount: s, hook: nope}\n"},
		{name: "a setup without policies or hook", want: "managedPolicies",
			yaml: "addons:" + validEntry + "  - name: x\n    setup: {short: x, namespace: n, serviceAccount: s}\n"},
		{name: "a short name too long for IAM", want: "short",
			yaml: "addons:" + validEntry + "  - name: x\n    setup: {short: muchtoolong, namespace: n, serviceAccount: s, managedPolicies: [p]}\n"},
		{name: "a short name twice", want: "short",
			yaml: "addons:" + validEntry +
				"  - name: x\n    setup: {short: x, namespace: n, serviceAccount: s, managedPolicies: [p]}\n" +
				"  - name: y\n    setup: {short: x, namespace: n, serviceAccount: s, managedPolicies: [p]}\n"},
		{name: "a setup without its service account", want: "serviceAccount",
			yaml: "addons:" + validEntry + "  - name: x\n    setup: {short: x, namespace: n, managedPolicies: [p]}\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := crossplaneaws.ParseAddonCatalog([]byte(tt.yaml))

			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want one naming %q", err, tt.want)
			}
		})
	}
}

func TestParseAddonCatalogAcceptsTheShippedFile(t *testing.T) {
	c, err := crossplaneaws.ParseAddonCatalog(crossplaneaws.AddonCatalogFile())
	if err != nil {
		t.Fatal(err)
	}

	if got := c.Names(); !slices.Equal(got, allAddons) {
		t.Errorf("names = %v, want %v", got, allAddons)
	}
	if got, want := c.Defaults(), []string{"vpc-cni", "coredns", "kube-proxy", "eks-pod-identity-agent", "aws-ebs-csi-driver"}; !slices.Equal(got, want) {
		t.Errorf("defaults = %v, want %v", got, want)
	}
	if !slices.Equal(crossplaneaws.DefaultAddons, c.Defaults()) || !slices.Equal(crossplaneaws.KnownAddons(), c.Names()) {
		t.Errorf("the package's add-ons are not the shipped file's")
	}
}

// An import cannot read back the buckets of the Mountpoint driver: every
// other known add-on can go into an imported cluster's spec.
func TestImportableAddons(t *testing.T) {
	want := slices.DeleteFunc(slices.Clone(allAddons), func(n string) bool { return n == "aws-mountpoint-s3-csi-driver" })

	if got := crossplaneaws.ImportableAddons(); !slices.Equal(got, want) {
		t.Errorf("importable = %v, want %v", got, want)
	}
}
