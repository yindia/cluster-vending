//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"

	"github.com/yindia/cluster-vending/pkg/cli"
)

// Labels of everything the run creates: they become AWS tags on every
// resource, which is how the leak check finds what was left.
const labelRun = "cv-e2e"

// userNodeGroup is the Karpenter node group scenario 4 adds and removes.
const userNodeGroup = "t3"

func (e *env) manifest(kind, name string, labels map[string]string, spec map[string]any) (*cli.Manifest, error) {
	raw, err := json.Marshal(spec)
	if err != nil {
		return nil, err
	}
	return &cli.Manifest{
		APIVersion: cli.APIVersion, Kind: kind, Spec: raw,
		Metadata: cli.Metadata{Name: name, Tenant: e.cfg.Tenant, Labels: labels},
	}, nil
}

func (e *env) runLabels() map[string]string { return map[string]string{labelRun: e.runID} }

// networkManifest is a small network: two zones, one NAT gateway.
func (e *env) networkManifest(name string) (*cloudv1.Network, error) {
	m, err := e.manifest(cli.KindNetwork, name, e.runLabels(), map[string]any{
		"tier":        "TIER_2",
		"region":      e.cfg.Region,
		"bootstrapId": e.st.accountID,
		"layout": map[string]any{
			"cidrBlock":             e.cfg.CIDR,
			"natGatewayMode":        "NAT_GATEWAY_MODE_SINGLE",
			"availabilityZoneCount": 2,
		},
	})
	if err != nil {
		return nil, err
	}
	return m.Network()
}

// clusterManifest is a manual-upgrade cluster on the configured channel, at
// the minor the upgrade scenario starts from.
func (e *env) clusterManifest(name, networkID string) (*cloudv1.Cluster, error) {
	m, err := e.manifest(cli.KindCluster, name, e.runLabels(), map[string]any{
		"region":  e.cfg.Region,
		"network": map[string]any{"networkId": networkID},
		"controlPlane": map[string]any{
			"kubernetesVersion": e.cfg.K8sFrom,
			"endpointAccess":    map[string]any{"publicAccessCidrs": []string{"0.0.0.0/0"}},
			"secretsEncryption": map[string]any{"enabled": true},
		},
		"releaseChannel": e.cfg.Channel,
		"upgradeMode":    "UPGRADE_MODE_MANUAL",
		"aws": map[string]any{
			"irsa":                   true,
			"controlPlaneLogGroup":   map[string]any{"enabled": true, "retentionDays": 7},
			"enabledClusterLogTypes": []string{"AWS_CLUSTER_LOG_TYPE_API"},
			"upgradePolicy":          map[string]any{"supportType": "AWS_SUPPORT_TYPE_STANDARD"},
		},
	})
	if err != nil {
		return nil, err
	}
	return m.Cluster()
}

// userNodeGroupSpec is the t3 Karpenter group of scenario 4.
func userNodeGroupSpec() *cloudv1.NodeGroupSpec {
	cpu, mem := int32(8), int32(32)
	return &cloudv1.NodeGroupSpec{
		Name:             userNodeGroup,
		InstanceFamilies: []string{"t3"},
		Architectures:    []cloudv1.Architecture{cloudv1.Architecture_ARCHITECTURE_AMD64},
		CapacityTypes:    []cloudv1.CapacityType{cloudv1.CapacityType_CAPACITY_TYPE_ON_DEMAND},
		Labels:           map[string]string{"cv-e2e": "t3"},
		Limits:           &cloudv1.NodeGroupLimits{Cpu: &cpu, MemoryGib: &mem},
	}
}

// networkImportRequest is the read-only import of the existing VPC.
func (e *env) networkImportRequest() (*cloudv1.ImportNetworkRequest, error) {
	m, err := e.manifest(cli.KindNetworkImport, e.runID+"-imp", nil, map[string]any{
		"region":      e.cfg.Region,
		"bootstrapId": e.st.accountID,
		"aws":         map[string]any{"vpcId": e.cfg.ImportVPC, "subnetIds": e.cfg.ImportSubnets},
	})
	if err != nil {
		return nil, err
	}
	req, err := m.NetworkImport()
	if err != nil {
		return nil, err
	}
	req.TenantId, req.RequestId = e.cfg.Tenant, newRequestID()
	return req, nil
}

// clusterImportRequest is the read-only import of the existing EKS cluster.
func (e *env) clusterImportRequest(networkID string) (*cloudv1.ImportClusterRequest, error) {
	m, err := e.manifest(cli.KindClusterImport, e.cfg.ImportCluster, nil, map[string]any{
		"networkId": networkID,
		"aws":       map[string]any{"defaultNodeGroupName": e.cfg.ImportNodeGroup},
	})
	if err != nil {
		return nil, err
	}
	req, err := m.ClusterImport()
	if err != nil {
		return nil, fmt.Errorf("cluster import manifest: %w", err)
	}
	req.TenantId, req.RequestId = e.cfg.Tenant, newRequestID()
	return req, nil
}
