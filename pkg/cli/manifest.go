package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"sigs.k8s.io/yaml"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// APIVersion is the apiVersion of every manifest.
const APIVersion = "cluster-vending.io/v1"

// Manifest kinds.
const (
	KindCloudAccount  = "CloudAccount"
	KindNetwork       = "Network"
	KindNetworkImport = "NetworkImport"
	KindCluster       = "Cluster"
	KindClusterImport = "ClusterImport"
)

// Manifest is the Kubernetes-style input of create, update and import:
//
//	apiVersion: cluster-vending.io/v1
//	kind: Cluster
//	metadata:
//	  name: prod            # the resource name (CloudAccount: account name)
//	  tenant: acme          # or --tenant
//	  displayName: Prod     # optional
//	  labels: {env: prod}   # filtering, and the cloud tags of every resource
//	spec: {...}             # the resource spec, protojson field names
//
// metadata.id is the server-assigned id: read on update (or the argument),
// refused on create. status, when present (e.g. edited from a get), is
// ignored.
type Manifest struct {
	APIVersion string          `json:"apiVersion"`
	Kind       string          `json:"kind"`
	Metadata   Metadata        `json:"metadata"`
	Spec       json.RawMessage `json:"spec"`
	Status     json.RawMessage `json:"status,omitempty"`
}

// Metadata is a manifest's metadata.
type Metadata struct {
	Name        string            `json:"name"`
	ID          string            `json:"id,omitempty"`
	Tenant      string            `json:"tenant,omitempty"`
	DisplayName string            `json:"displayName,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
}

// LoadManifest reads a YAML or JSON manifest of kind from path. Unknown
// fields, at any level, are errors.
func LoadManifest(path, kind string) (*Manifest, error) {
	if path == "" {
		return nil, fmt.Errorf("-f is required: a %s manifest (apiVersion: %s, kind: %s)", kind, APIVersion, kind)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("-f: %w", err)
	}
	js, err := yaml.YAMLToJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("-f %s: %w", path, err)
	}
	dec := json.NewDecoder(bytes.NewReader(js))
	dec.DisallowUnknownFields()
	m := &Manifest{}
	if err := dec.Decode(m); err != nil {
		return nil, fmt.Errorf("-f %s: %w", path, err)
	}
	switch {
	case m.APIVersion != APIVersion:
		return nil, fmt.Errorf("-f %s: apiVersion must be %s, got %q", path, APIVersion, m.APIVersion)
	case m.Kind != kind:
		return nil, fmt.Errorf("-f %s: this command takes kind %s, got %q", path, kind, m.Kind)
	case m.Metadata.Name == "":
		return nil, fmt.Errorf("-f %s: metadata.name is required", path)
	}
	return m, nil
}

// tenant is the manifest's tenant, or flag; both set must agree.
func (m *Manifest) tenant(flag string) (string, error) {
	switch t := m.Metadata.Tenant; {
	case t == "" && flag == "":
		return "", errors.New("the tenant is required: metadata.tenant or --tenant")
	case t != "" && flag != "" && t != flag:
		return "", fmt.Errorf("metadata.tenant %q and --tenant %q differ", t, flag)
	case t != "":
		return t, nil
	default:
		return flag, nil
	}
}

// spec is the manifest's spec as a map.
func (m *Manifest) spec() (map[string]any, error) {
	spec := map[string]any{}
	if len(m.Spec) > 0 && string(m.Spec) != "null" {
		if err := json.Unmarshal(m.Spec, &spec); err != nil {
			return nil, fmt.Errorf("spec: %w", err)
		}
	}
	return spec, nil
}

// decode unmarshals fields (protojson names) into msg.
func (m *Manifest) decode(fields map[string]any, msg proto.Message) error {
	js, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	if err := protojson.Unmarshal(js, msg); err != nil {
		if removedNodeGroupField(err) {
			return fmt.Errorf("%s %s: %w: the platform owns a cluster's node groups (its system group and Karpenter platform pool); "+
				"remove defaultNodePool, aws.defaultNodePool and nodeGroups from the manifest", m.Kind, m.Metadata.Name, err)
		}
		return fmt.Errorf("%s %s: %w", m.Kind, m.Metadata.Name, err)
	}
	return nil
}

// removedNodeGroupField reports whether a decode failed on a node group
// field the cluster spec had before the platform owned its node groups.
func removedNodeGroupField(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, `unknown field "defaultNodePool"`) || strings.Contains(msg, `unknown field "nodeGroups"`)
}

// Network is the manifest as a cloud.v1 Network.
func (m *Manifest) Network() (*cloudv1.Network, error) {
	spec, err := m.spec()
	if err != nil {
		return nil, err
	}
	n := &cloudv1.Network{}
	err = m.decode(map[string]any{"id": m.Metadata.ID, "name": m.Metadata.Name, "displayName": m.Metadata.DisplayName,
		"labels": m.Metadata.Labels, "spec": spec}, n)
	return n, err
}

// Cluster is the manifest as a cloud.v1 Cluster.
func (m *Manifest) Cluster() (*cloudv1.Cluster, error) {
	spec, err := m.spec()
	if err != nil {
		return nil, err
	}
	c := &cloudv1.Cluster{}
	err = m.decode(map[string]any{"id": m.Metadata.ID, "name": m.Metadata.Name, "displayName": m.Metadata.DisplayName,
		"labels": m.Metadata.Labels, "spec": spec}, c)
	return c, err
}

// NetworkImport is the manifest as an ImportNetworkRequest: spec holds
// bootstrapId, region and aws: {vpcId, subnetIds}. An imported network
// takes no labels until it is taken over.
func (m *Manifest) NetworkImport() (*cloudv1.ImportNetworkRequest, error) {
	if len(m.Metadata.Labels) > 0 {
		return nil, errors.New("metadata.labels cannot be set on import; update the network after the takeover")
	}
	spec, err := m.spec()
	if err != nil {
		return nil, err
	}
	spec["name"] = m.Metadata.Name
	if m.Metadata.DisplayName != "" {
		spec["displayName"] = m.Metadata.DisplayName
	}
	req := &cloudv1.ImportNetworkRequest{}
	return req, m.decode(spec, req)
}

// ClusterImport is the manifest as an ImportClusterRequest: metadata.name
// is the EKS cluster's name, spec holds networkId and
// aws: {defaultNodeGroupName}. An imported cluster takes no labels until
// it is taken over.
func (m *Manifest) ClusterImport() (*cloudv1.ImportClusterRequest, error) {
	if len(m.Metadata.Labels) > 0 {
		return nil, errors.New("metadata.labels cannot be set on import; update the cluster after the takeover")
	}
	spec, err := m.spec()
	if err != nil {
		return nil, err
	}
	spec["name"] = m.Metadata.Name
	if m.Metadata.DisplayName != "" {
		spec["displayName"] = m.Metadata.DisplayName
	}
	req := &cloudv1.ImportClusterRequest{}
	return req, m.decode(spec, req)
}

// CloudAccount is the manifest as a CloudAccount: metadata.name is the
// account name, spec holds aws: {accountId, role}.
func (m *Manifest) CloudAccount() (*cloudv1.CloudAccount, error) {
	if len(m.Metadata.Labels) > 0 {
		return nil, errors.New("metadata.labels do not apply to a CloudAccount")
	}
	spec, err := m.spec()
	if err != nil {
		return nil, err
	}
	spec["accountName"] = m.Metadata.Name
	a := &cloudv1.CloudAccount{}
	return a, m.decode(spec, a)
}
