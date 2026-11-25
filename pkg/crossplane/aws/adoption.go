package aws

import (
	"encoding/json"
	"strings"
)

// AdoptionAnnotation, on a Cluster object, is what an imported cluster
// keeps of the resources it was imported from: Adoption as JSON, next to
// the import's own keys (its planned objects, what it discovered).
const AdoptionAnnotation = "cloud.cluster-vending.io/adoption"

// legacyDiscoveredKey is the key (reconcile.DiscoveredKey) under which an
// import keeps the spec it derived. A cluster imported before the platform
// owned node groups has its node group there only, in the fields the
// contract had then (spec.default_node_pool, spec.aws.default_node_pool).
const legacyDiscoveredKey = "importDiscovered"

// ParseAdoption reads a stored adoption (JSON; empty: none). An imported
// cluster stored before Adoption.NodeGroup existed gets it from the spec
// its import derived, so it keeps rendering the node group it adopted. A
// value that cannot be read is no adoption.
func ParseAdoption(raw []byte) Adoption {
	var a Adoption
	if len(raw) == 0 || json.Unmarshal(raw, &a) != nil {
		return Adoption{}
	}
	if a.NodeGroup == nil && a.Imported() {
		a.NodeGroup = legacyNodeGroup(raw)
	}
	return a
}

// legacySpec is the part of a stored protojson ClusterSpec that held the
// default node group before the platform owned it.
type legacySpec struct {
	DefaultNodePool *struct {
		InstanceTypes []string `json:"instanceTypes"`
		CapacityType  string   `json:"capacityType"`
		DiskSizeGb    int      `json:"diskSizeGb"`
		ScalingConfig *struct {
			MinSize     int  `json:"minSize"`
			MaxSize     int  `json:"maxSize"`
			DesiredSize *int `json:"desiredSize"`
		} `json:"scalingConfig"`
		Labels map[string]string `json:"labels"`
		Taints []struct {
			Key    string `json:"key"`
			Value  string `json:"value"`
			Effect string `json:"effect"`
		} `json:"taints"`
	} `json:"defaultNodePool"`
	AWS *struct {
		DefaultNodePool *struct {
			AmiType        string `json:"amiType"`
			LaunchTemplate *struct {
				ID      string `json:"id"`
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"launchTemplate"`
		} `json:"defaultNodePool"`
	} `json:"aws"`
}

// legacyNodeGroup is the node group in the import's derived spec, in EKS
// spellings; nil when there is none.
func legacyNodeGroup(raw []byte) *NodePoolPlan {
	var fields map[string]json.RawMessage
	var discovered struct {
		Spec legacySpec `json:"spec"`
	}
	if json.Unmarshal(raw, &fields) != nil || json.Unmarshal(fields[legacyDiscoveredKey], &discovered) != nil {
		return nil
	}
	pool := discovered.Spec.DefaultNodePool
	if pool == nil {
		return nil
	}
	out := &NodePoolPlan{InstanceTypes: pool.InstanceTypes, CapacityType: strings.TrimPrefix(pool.CapacityType, "CAPACITY_TYPE_"),
		DiskSizeGB: pool.DiskSizeGb, Labels: pool.Labels}
	if sc := pool.ScalingConfig; sc != nil {
		out.MinSize, out.MaxSize, out.Desired = sc.MinSize, sc.MaxSize, max(sc.MinSize, 1)
		if sc.DesiredSize != nil {
			out.Desired = *sc.DesiredSize
		}
	}
	for _, t := range pool.Taints {
		out.Taints = append(out.Taints, TaintPlan{Key: t.Key, Value: t.Value, Effect: strings.TrimPrefix(t.Effect, "TAINT_EFFECT_")})
	}
	if aws := discovered.Spec.AWS; aws != nil && aws.DefaultNodePool != nil {
		// AWS_AMI_TYPE_AL2023_X86_64_STANDARD is EKS's AL2023_x86_64_STANDARD.
		out.AMIType = strings.ReplaceAll(strings.TrimPrefix(aws.DefaultNodePool.AmiType, "AWS_AMI_TYPE_"), "X86_64", "x86_64")
		if lt := aws.DefaultNodePool.LaunchTemplate; lt != nil {
			out.LaunchTemplate = &LaunchTemplatePlan{ID: lt.ID, Name: lt.Name, Version: lt.Version}
		}
	}
	if out.AMIType == "" {
		out.AMIType = DefaultSystemAMIType
	}
	if out.CapacityType == "" {
		out.CapacityType = DefaultSystemCapacityType
	}
	return out
}
