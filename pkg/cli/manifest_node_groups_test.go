package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A manifest written for an older contract that still names node groups
// says what replaced them, not only which field is unknown.
func TestClusterManifestWithRemovedNodeGroupFieldsSaysThePlatformOwnsThem(t *testing.T) {
	for name, spec := range map[string]string{
		"defaultNodePool":     "  defaultNodePool: {instanceTypes: [m6i.large]}\n",
		"nodeGroups":          "  nodeGroups: [{name: gpu}]\n",
		"aws.defaultNodePool": "  aws: {defaultNodePool: {amiType: AWS_AMI_TYPE_AL2023_X86_64_STANDARD}}\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "cluster.yaml")
			doc := "apiVersion: " + APIVersion + "\nkind: Cluster\nmetadata: {name: prod, tenant: acme}\nspec:\n  region: us-east-1\n" + spec
			if !strings.Contains(spec, "aws:") {
				doc += "  aws: {}\n"
			}
			if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
				t.Fatal(err)
			}
			m, err := LoadManifest(path, "Cluster")
			if err != nil {
				t.Fatal(err)
			}

			_, err = m.Cluster()

			if err == nil || !strings.Contains(err.Error(), "the platform owns a cluster's node groups") {
				t.Errorf("error = %v", err)
			}
		})
	}
}

// A manifest that still sets naas (removed with sub-tenants) says so.
func TestClusterManifestWithTheRemovedNaaSFieldSaysItWasRemoved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cluster.yaml")
	doc := "apiVersion: " + APIVersion + "\nkind: Cluster\nmetadata: {name: prod, tenant: acme}\nspec:\n  region: us-east-1\n  naas: true\n  aws: {}\n"
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := LoadManifest(path, "Cluster")
	if err != nil {
		t.Fatal(err)
	}

	_, err = m.Cluster()

	if err == nil || !strings.Contains(err.Error(), "naas was removed with sub-tenants") {
		t.Errorf("error = %v", err)
	}
}
