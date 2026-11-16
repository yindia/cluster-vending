package cluster

import (
	"cmp"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/yindia/cluster-vending/pkg/crossplane"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

type clusterArchitectureNode struct {
	object   *unstructured.Unstructured
	resource *cloudv1.NetworkArchitectureResource
}

type clusterArchitectureIndex struct {
	names         map[string]*clusterArchitectureNode
	ids           map[string]*clusterArchitectureNode
	physicalNames map[string]*clusterArchitectureNode
}

// clusterArchitecture is an AWS adapter over an allowlisted, provider-neutral
// output. Edges follow explicit live references, not resource naming conventions
// or membership inferred from the desired Cluster spec.
func clusterArchitecture(row *model.Cluster, objects []*unstructured.Unstructured) *cloudv1.ClusterArchitecture {
	out := &cloudv1.ClusterArchitecture{}
	if row.Provider != model.ProviderAWS {
		return out
	}
	index := clusterArchitectureIndex{names: map[string]*clusterArchitectureNode{}, ids: map[string]*clusterArchitectureNode{}, physicalNames: map[string]*clusterArchitectureNode{}}
	var nodes []*clusterArchitectureNode
	for _, object := range objects {
		if !clusterArchitectureScope(row, object) {
			continue
		}
		fact := func(field string) string {
			value := clusterArchitectureValue(object.Object, "status", "atProvider", field)
			if value == "" {
				value = clusterArchitectureValue(object.Object, "spec", "forProvider", field)
			}
			return value
		}
		id := clusterArchitectureValue(object.Object, "status", "atProvider", "id")
		if id == "" {
			id = object.GetAnnotations()["crossplane.io/external-name"]
		}
		resource := &cloudv1.NetworkArchitectureResource{Kind: object.GetKind(), Name: object.GetName(), CloudId: id}
		if object.GetKind() == "Subnet" {
			resource.ZoneId = fact("availabilityZoneId")
			resource.CidrBlock = fact("cidrBlock")
			switch object.GetLabels()[crossplane.LabelSubnetRole] {
			case "public":
				resource.SubnetRole = cloudv1.SubnetRole_SUBNET_ROLE_PUBLIC
			case "private":
				resource.SubnetRole = cloudv1.SubnetRole_SUBNET_ROLE_PRIVATE
			}
		}
		node := &clusterArchitectureNode{object: object, resource: resource}
		nodes = append(nodes, node)
		index.names[resource.Kind+"/"+resource.Name] = node
		// Prefer observed identity; never alias an outdated external-name to a
		// different actual cloud resource. Ambiguous IDs cannot resolve an edge.
		if id != "" {
			key := resource.Kind + "/" + id
			if _, exists := index.ids[key]; exists {
				index.ids[key] = nil
			} else {
				index.ids[key] = node
			}
		}
		if resource.Kind == "LaunchTemplate" {
			// NodeGroup can specify a physical template name instead of an ID. Match
			// only the explicit provider name, never the Kubernetes resource name.
			name := fact("name")
			if name != "" {
				key := resource.Kind + "/" + name
				if _, exists := index.physicalNames[key]; exists {
					index.physicalNames[key] = nil
				} else {
					index.physicalNames[key] = node
				}
			}
		}
		// Connection plumbing is used only as binding evidence, never exposed:
		// no Secret reference names or provider credentials leave this projection.
		if resource.Kind != "ClusterAuth" && resource.Kind != "ProviderConfig" {
			out.Resources = append(out.Resources, resource)
		}
	}
	edges := map[string]*cloudv1.NetworkArchitectureConnection{}
	add := func(source, target *clusterArchitectureNode, relation, label, evidence string) {
		if source == nil || target == nil {
			return
		}
		key := source.resource.Kind + "/" + source.resource.Name + "/" + target.resource.Kind + "/" + target.resource.Name + "/" + relation + "/" + label
		if previous := edges[key]; previous != nil && previous.Evidence == "observed" {
			return
		}
		edges[key] = &cloudv1.NetworkArchitectureConnection{SourceKind: source.resource.Kind, SourceName: source.resource.Name, TargetKind: target.resource.Kind, TargetName: target.resource.Name, Relation: relation, Label: label, Evidence: evidence}
	}
	references := map[string]*clusterArchitectureNode{}
	byID := func(kind, id string) *clusterArchitectureNode {
		if node, known := index.ids[kind+"/"+id]; known {
			return node
		}
		if !clusterArchitectureReferenceID(kind, id) || index.names[kind+"/"+id] != nil {
			return nil
		}
		key := kind + "/" + id
		if references[key] == nil {
			resource := &cloudv1.NetworkArchitectureResource{Kind: kind, Name: id, CloudId: id, ReferenceOnly: true}
			references[key] = &clusterArchitectureNode{resource: resource}
			out.Resources = append(out.Resources, resource)
		}
		return references[key]
	}
	// Refs take precedence over resolved scalar IDs for configured intent. An
	// invalid/foreign named ref must not silently become a raw-ID connection.
	plural := func(source *clusterArchitectureNode, values map[string]any, refField, idField, kind, relation, label, evidence string) {
		if evidence == "configured" {
			refs, _, _ := unstructured.NestedSlice(values, refField)
			if len(refs) > 0 {
				for _, raw := range refs {
					if ref, ok := raw.(map[string]any); ok {
						add(source, index.named(source, kind, ref), relation, label, evidence)
					}
				}
				return
			}
		}
		ids, _, _ := unstructured.NestedStringSlice(values, idField)
		for _, id := range ids {
			add(source, byID(kind, id), relation, label, evidence)
		}
	}
	for _, node := range nodes {
		kind := node.resource.Kind
		for _, evidence := range []string{"configured", "observed"} {
			section, provider := "spec", "forProvider"
			if evidence == "observed" {
				section, provider = "status", "atProvider"
			}
			values, _, _ := unstructured.NestedMap(node.object.Object, section, provider)
			switch kind {
			case "NodeGroup", "Addon":
				parent := index.cluster(node, values, evidence)
				relation, label := "has_node_group", "Node group"
				if kind == "Addon" {
					relation, label = "has_addon", "Managed add-on"
				}
				add(parent, node, relation, label, evidence)
				if kind == "NodeGroup" {
					plural(node, values, "subnetIdRefs", "subnetIds", "Subnet", "uses_subnet", "Node group subnet", evidence)
					access, _, _ := unstructured.NestedMap(values, "remoteAccess")
					plural(node, access, "sourceSecurityGroupIdRefs", "sourceSecurityGroupIds", "SecurityGroup", "uses_security_group", "Remote access source", evidence)
					template, _, _ := unstructured.NestedMap(values, "launchTemplate")
					var target *clusterArchitectureNode
					if id := clusterArchitectureValue(template, "id"); id != "" {
						target = byID("LaunchTemplate", id)
					} else {
						target = index.physicalNames["LaunchTemplate/"+clusterArchitectureValue(template, "name")]
					}
					add(node, target, "uses_launch_template", "Launch template", evidence)
				}
			case "Cluster":
				vpc, _, _ := unstructured.NestedMap(values, "vpcConfig")
				plural(node, vpc, "subnetIdRefs", "subnetIds", "Subnet", "uses_subnet", "Control plane subnet", evidence)
				plural(node, vpc, "securityGroupIdRefs", "securityGroupIds", "SecurityGroup", "uses_security_group", "Control plane security group", evidence)
				if evidence == "observed" {
					add(node, byID("SecurityGroup", clusterArchitectureValue(vpc, "clusterSecurityGroupId")), "uses_security_group", "Cluster security group", evidence)
				}
			case "LaunchTemplate":
				plural(node, values, "vpcSecurityGroupIdRefs", "vpcSecurityGroupIds", "SecurityGroup", "uses_security_group", "Node security group", evidence)
				interfaces, _, _ := unstructured.NestedSlice(values, "networkInterfaces")
				for _, raw := range interfaces {
					iface, ok := raw.(map[string]any)
					if !ok {
						continue
					}
					plural(node, iface, "securityGroupRefs", "securityGroups", "SecurityGroup", "uses_security_group", "Network interface security group", evidence)
					var subnet *clusterArchitectureNode
					if ref, exists, _ := unstructured.NestedMap(iface, "subnetIdRef"); exists && evidence == "configured" {
						subnet = index.named(node, "Subnet", ref)
					} else {
						subnet = byID("Subnet", clusterArchitectureValue(iface, "subnetId"))
					}
					add(node, subnet, "uses_subnet", "Network interface subnet", evidence)
				}
			}
		}
		if kind == "Release" {
			add(index.componentCluster(node), node, "has_component", "Helm component", "configured")
		}
	}
	for _, edge := range edges {
		out.Connections = append(out.Connections, edge)
	}
	// Network inventory can include subnets this cluster never references. Only
	// include those with an explicit edge, not every available network subnet.
	out.Resources = slices.DeleteFunc(out.Resources, func(resource *cloudv1.NetworkArchitectureResource) bool {
		if resource.Kind != "Subnet" {
			return false
		}
		return !slices.ContainsFunc(out.Connections, func(edge *cloudv1.NetworkArchitectureConnection) bool {
			return edge.TargetKind == resource.Kind && edge.TargetName == resource.Name
		})
	})
	slices.SortFunc(out.Resources, func(a, b *cloudv1.NetworkArchitectureResource) int {
		return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Name, b.Name))
	})
	slices.SortFunc(out.Connections, func(a, b *cloudv1.NetworkArchitectureConnection) int {
		return cmp.Or(cmp.Compare(a.SourceKind, b.SourceKind), cmp.Compare(a.SourceName, b.SourceName), cmp.Compare(a.TargetKind, b.TargetKind), cmp.Compare(a.TargetName, b.TargetName), cmp.Compare(a.Relation, b.Relation), cmp.Compare(a.Label, b.Label), cmp.Compare(a.Evidence, b.Evidence))
	})
	return out
}

func (index clusterArchitectureIndex) named(source *clusterArchitectureNode, kind string, ref map[string]any) *clusterArchitectureNode {
	namespace := clusterArchitectureValue(ref, "namespace")
	if namespace != "" && namespace != source.object.GetNamespace() {
		return nil
	}
	return index.names[kind+"/"+clusterArchitectureValue(ref, "name")]
}

func (index clusterArchitectureIndex) cluster(source *clusterArchitectureNode, values map[string]any, evidence string) *clusterArchitectureNode {
	if ref, exists, _ := unstructured.NestedMap(values, "clusterNameRef"); exists && evidence == "configured" {
		return index.named(source, "Cluster", ref)
	}
	return index.ids["Cluster/"+clusterArchitectureValue(values, "clusterName")]
}

// Helm Release ownership labels only authorize inclusion in the snapshot. The
// actual configured destination must be bound via the owned ProviderConfig's
// Secret reference to the connection Secret output of an owned ClusterAuth,
// whose explicit cluster reference identifies the root. Secret payloads are
// never read; a deployed Helm status alone cannot establish this binding.
func (index clusterArchitectureIndex) componentCluster(release *clusterArchitectureNode) *clusterArchitectureNode {
	ref, _, _ := unstructured.NestedMap(release.object.Object, "spec", "providerConfigRef")
	if clusterArchitectureValue(ref, "kind") != "ProviderConfig" {
		return nil
	}
	pc := index.named(release, "ProviderConfig", ref)
	if pc == nil {
		return nil
	}
	credentials, _, _ := unstructured.NestedMap(pc.object.Object, "spec", "credentials")
	if clusterArchitectureValue(credentials, "source") != "Secret" {
		return nil
	}
	secret, _, _ := unstructured.NestedMap(credentials, "secretRef")
	if clusterArchitectureValue(secret, "namespace") != release.object.GetNamespace() || clusterArchitectureValue(secret, "key") != "kubeconfig" || clusterArchitectureValue(secret, "name") == "" {
		return nil
	}
	var parent *clusterArchitectureNode
	for _, auth := range index.names {
		if auth.resource.Kind != "ClusterAuth" {
			continue
		}
		output := clusterArchitectureValue(auth.object.Object, "spec", "writeConnectionSecretToRef", "name")
		if output != clusterArchitectureValue(secret, "name") {
			continue
		}
		values, _, _ := unstructured.NestedMap(auth.object.Object, "spec", "forProvider")
		candidate := index.cluster(auth, values, "configured")
		if candidate == nil {
			return nil
		}
		if parent != nil && parent != candidate {
			return nil
		}
		parent = candidate
	}
	return parent
}

func clusterArchitectureValue(values map[string]any, path ...string) string {
	value, _, _ := unstructured.NestedString(values, path...)
	return value
}

func clusterArchitectureReferenceID(kind, id string) bool {
	prefix := map[string]string{"Subnet": "subnet-", "SecurityGroup": "sg-", "LaunchTemplate": "lt-"}[kind]
	if prefix == "" {
		return false
	}
	suffix, ok := strings.CutPrefix(id, prefix)
	if !ok || len(suffix) != 8 && len(suffix) != 17 {
		return false
	}
	for _, char := range suffix {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}
