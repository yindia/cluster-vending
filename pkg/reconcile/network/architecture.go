package network

import (
	"cmp"
	"slices"
	"strings"

	awsec2 "github.com/upbound/provider-aws/v2/apis/namespaced/ec2/v1beta1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/yindia/cluster-vending/pkg/crossplane"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/kube"
)

type architectureObject struct {
	object   client.Object
	resource *cloudv1.NetworkArchitectureResource
	data     map[string]any
}

type architectureIndex struct {
	names map[string]*architectureObject
	ids   map[string]*architectureObject
}

// architecture projects an allowlist of facts, never raw specs, tags, provider
// configuration, credentials, selectors, or an inferred relationship by name.
// The caller already observed network-scoped objects; retain the scope check here
// so even a mixed input cannot link resources from another network or tenant.
func architecture(objects []client.Object, tenantID, networkID string) *cloudv1.NetworkArchitecture {
	out := &cloudv1.NetworkArchitecture{}
	index := architectureIndex{names: map[string]*architectureObject{}, ids: map[string]*architectureObject{}}
	var nodes []*architectureObject
	for _, object := range objects {
		if object.GetNamespace() != kube.TenantNamespace(tenantID) || object.GetLabels()[crossplane.LabelNetwork] != networkID || object.GetName() == "" {
			continue
		}
		kind := architectureKind(object)
		if kind == "" {
			continue
		}
		raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(object)
		if err != nil {
			continue
		}
		fact := func(field string) string {
			return firstArchitectureValue(architectureValue(raw, "status", "atProvider", field), architectureValue(raw, "spec", "forProvider", field))
		}
		resource := &cloudv1.NetworkArchitectureResource{
			Kind: kind, Name: object.GetName(),
			CloudId: firstArchitectureValue(architectureValue(raw, "status", "atProvider", "id"), object.GetAnnotations()["crossplane.io/external-name"]),
			ZoneId:  fact("availabilityZoneId"), CidrBlock: fact("cidrBlock"),
			DestinationCidrBlock: fact("destinationCidrBlock"), DestinationIpv6CidrBlock: fact("destinationIpv6CidrBlock"),
			DestinationPrefixListId: fact("destinationPrefixListId"),
			PublicIp:                architectureValue(raw, "status", "atProvider", "publicIp"),
		}
		if kind == "Subnet" {
			switch object.GetLabels()[crossplane.LabelSubnetRole] {
			case "public":
				resource.SubnetRole = cloudv1.SubnetRole_SUBNET_ROLE_PUBLIC
			case "private":
				resource.SubnetRole = cloudv1.SubnetRole_SUBNET_ROLE_PRIVATE
			}
		}
		node := &architectureObject{object: object, resource: resource, data: raw}
		nodes = append(nodes, node)
		index.names[kind+"/"+object.GetName()] = node
		ids := []string{architectureValue(raw, "status", "atProvider", "id"), architectureValue(raw, "status", "atProvider", "allocationId")}
		// A stale external-name must not alias a different observed cloud ID.
		if ids[0] == "" && ids[1] == "" {
			ids = append(ids, object.GetAnnotations()["crossplane.io/external-name"])
		}
		for _, id := range ids {
			if id == "" {
				continue
			}
			key := kind + "/" + id
			if previous, ok := index.ids[key]; ok && previous != node {
				index.ids[key] = nil
			} else if !ok {
				index.ids[key] = node
			}
		}
		out.Resources = append(out.Resources, resource)
	}
	edges := map[string]*cloudv1.NetworkArchitectureConnection{}
	add := func(source, target *architectureObject, relation, label, evidence string, details ...*cloudv1.NetworkArchitectureConnection) {
		if source == nil || target == nil {
			return
		}
		edge := &cloudv1.NetworkArchitectureConnection{SourceKind: source.resource.Kind, SourceName: source.resource.Name, TargetKind: target.resource.Kind, TargetName: target.resource.Name, Relation: relation, Label: label, Evidence: evidence}
		if len(details) > 0 {
			route := details[0]
			edge.DestinationCidrBlock = route.DestinationCidrBlock
			edge.DestinationIpv6CidrBlock = route.DestinationIpv6CidrBlock
			edge.DestinationPrefixListId = route.DestinationPrefixListId
			edge.RouteState = route.RouteState
			edge.RouteOrigin = route.RouteOrigin
		}
		key := edge.SourceKind + "/" + edge.SourceName + "/" + edge.TargetKind + "/" + edge.TargetName + "/" + relation + "/" + label
		if previous := edges[key]; previous == nil || evidence == "observed" {
			edges[key] = edge
		}
	}
	// Reference-only endpoints have no provider spec, namespace or status. Only
	// allowlisted explicit AWS route IDs can create them, never a named ref.
	references := map[string]*architectureObject{}
	external := func(kind, id string) *architectureObject {
		if !architectureExternalRouteID(kind, id) {
			return nil
		}
		key := kind + "/" + id
		if target, known := index.ids[key]; known {
			return target
		}
		// Do not reuse an owned kind/name as a reference-only endpoint when its
		// observed cloud identity differs from this explicit target ID.
		if index.names[key] != nil {
			return nil
		}
		if references[key] == nil {
			resource := &cloudv1.NetworkArchitectureResource{Kind: kind, Name: id, CloudId: id, ReferenceOnly: true}
			references[key] = &architectureObject{resource: resource}
			out.Resources = append(out.Resources, resource)
		}
		return references[key]
	}
	routeLinks := func(source, table *architectureObject, route *architectureObject, evidence string) {
		section, provider := "spec", "forProvider"
		if evidence == "observed" {
			section, provider = "status", "atProvider"
		}
		values, _, _ := unstructured.NestedMap(route.data, section, provider)
		detail := &cloudv1.NetworkArchitectureConnection{
			DestinationCidrBlock:     architectureValue(values, "destinationCidrBlock"),
			DestinationIpv6CidrBlock: architectureValue(values, "destinationIpv6CidrBlock"),
			DestinationPrefixListId:  architectureValue(values, "destinationPrefixListId"),
		}
		if evidence == "observed" {
			detail.RouteState = architectureValue(values, "state")
			detail.RouteOrigin = architectureValue(values, "origin")
		}
		label := firstArchitectureValue(detail.DestinationCidrBlock, detail.DestinationIpv6CidrBlock, detail.DestinationPrefixListId)
		// Without a destination this is an incomplete observation, not a route path.
		if label == "" {
			return
		}
		add(source, index.resolve(route, "natGatewayId", "NATGateway", evidence), "forwards_to", label, evidence, detail)
		if architectureValue(values, "gatewayId") != "local" {
			add(source, index.resolve(route, "gatewayId", "InternetGateway", evidence), "forwards_to", label, evidence, detail)
		}
		if architectureValue(values, "gatewayId") == "local" && table != nil {
			add(source, index.resolve(table, "vpcId", "VPC", evidence), "forwards_to", label, evidence, detail)
		}
		for _, target := range []struct{ field, kind string }{
			{"vpcEndpointId", "VPCEndpoint"}, {"vpcPeeringConnectionId", "VPCPeeringConnection"}, {"transitGatewayId", "TransitGateway"},
			// Some provider observations encode gateway endpoints in gatewayId.
			{"gatewayId", "VPCEndpoint"},
		} {
			_, hasReference, _ := unstructured.NestedMap(values, target.field+"Ref")
			// gatewayIdRef is specifically an InternetGateway reference, not an
			// endpoint reference, even if an endpoint has the same Kubernetes name.
			if target.field == "gatewayId" && evidence == "configured" && hasReference {
				continue
			}
			resolved := index.resolve(route, target.field, target.kind, evidence)
			if resolved == nil && (evidence == "observed" || !hasReference) {
				resolved = external(target.kind, architectureValue(values, target.field))
			}
			add(source, resolved, "forwards_to", label, evidence, detail)
		}
	}
	for _, node := range nodes {
		link := func(field, kind, relation, label string) {
			for _, evidence := range []string{"configured", "observed"} {
				add(node, index.resolve(node, field, kind, evidence), relation, label, evidence)
			}
		}
		switch node.resource.Kind {
		case "Subnet", "InternetGateway", "VPCEndpoint":
			link("vpcId", "VPC", "contained_in", "VPC")
		case "NATGateway":
			link("vpcId", "VPC", "contained_in", "VPC")
			link("subnetId", "Subnet", "placed_in", "NAT placement")
			link("allocationId", "EIP", "uses_address", "Egress address")
		case "RouteTable":
			link("vpcId", "VPC", "contained_in", "VPC")
			routes, _, _ := unstructured.NestedSlice(node.data, "status", "atProvider", "route")
			for _, entry := range routes {
				values, ok := entry.(map[string]any)
				if !ok {
					continue
				}
				// RouteTable's inline schema uses different destination field names.
				values["destinationCidrBlock"] = values["cidrBlock"]
				values["destinationIpv6CidrBlock"] = values["ipv6CidrBlock"]
				route := &architectureObject{object: node.object, data: map[string]any{"status": map[string]any{"atProvider": values}}}
				routeLinks(node, node, route, "observed")
			}
		case "Route":
			link("routeTableId", "RouteTable", "contained_in", "Route table")
			for _, evidence := range []string{"configured", "observed"} {
				routeLinks(node, index.resolve(node, "routeTableId", "RouteTable", evidence), node, evidence)
			}
		case "RouteTableAssociation":
			link("routeTableId", "RouteTable", "associated_with", "Route table")
			link("subnetId", "Subnet", "associated_with", "Subnet")
			link("gatewayId", "InternetGateway", "associated_with", "Gateway")
			for _, evidence := range []string{"configured", "observed"} {
				// Never combine an observed endpoint with a configured endpoint to
				// manufacture a confirmed path across an association in transition.
				table := index.resolve(node, "routeTableId", "RouteTable", evidence)
				add(index.resolve(node, "subnetId", "Subnet", evidence), table, "routes_via", "Route table association", evidence)
			}
		}
	}
	for _, edge := range edges {
		out.Connections = append(out.Connections, edge)
	}
	slices.SortFunc(out.Resources, func(a, b *cloudv1.NetworkArchitectureResource) int {
		return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Name, b.Name))
	})
	slices.SortFunc(out.Connections, func(a, b *cloudv1.NetworkArchitectureConnection) int {
		return cmp.Or(cmp.Compare(a.SourceKind, b.SourceKind), cmp.Compare(a.SourceName, b.SourceName), cmp.Compare(a.TargetKind, b.TargetKind), cmp.Compare(a.TargetName, b.TargetName), cmp.Compare(a.Relation, b.Relation), cmp.Compare(a.Label, b.Label), cmp.Compare(a.Evidence, b.Evidence))
	})
	return out
}

func (index architectureIndex) resolve(source *architectureObject, field, kind, evidence string) *architectureObject {
	if evidence == "observed" {
		return index.ids[kind+"/"+architectureValue(source.data, "status", "atProvider", field)]
	}
	// A named reference is authoritative for configured intent. Never fall back
	// to a different resolved ID when that reference is foreign or dangling.
	if ref, exists, _ := unstructured.NestedMap(source.data, "spec", "forProvider", field+"Ref"); exists {
		name := architectureValue(ref, "name")
		if name == "" {
			return nil
		}
		namespace := architectureValue(ref, "namespace")
		if namespace != "" && namespace != source.object.GetNamespace() {
			return nil
		}
		return index.names[kind+"/"+name]
	}
	return index.ids[kind+"/"+architectureValue(source.data, "spec", "forProvider", field)]
}

func architectureValue(data map[string]any, path ...string) string {
	value, _, _ := unstructured.NestedString(data, path...)
	return value
}

func firstArchitectureValue(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func architectureKind(object client.Object) string {
	switch object.(type) {
	case *awsec2.VPC:
		return "VPC"
	case *awsec2.Subnet:
		return "Subnet"
	case *awsec2.NATGateway:
		return "NATGateway"
	case *awsec2.InternetGateway:
		return "InternetGateway"
	case *awsec2.RouteTable:
		return "RouteTable"
	case *awsec2.Route:
		return "Route"
	case *awsec2.RouteTableAssociation:
		return "RouteTableAssociation"
	case *awsec2.EIP:
		return "EIP"
	case *awsec2.VPCEndpoint:
		return "VPCEndpoint"
	case *awsec2.VPCPeeringConnection:
		return "VPCPeeringConnection"
	case *awsec2.TransitGateway:
		return "TransitGateway"
	default:
		return ""
	}
}

// Keep placeholders bounded to real ID forms from explicit allowlisted fields;
// arbitrary strings, names, selectors, URLs and ARNs are not reference nodes.
func architectureExternalRouteID(kind, id string) bool {
	prefix := map[string]string{"VPCEndpoint": "vpce-", "VPCPeeringConnection": "pcx-", "TransitGateway": "tgw-"}[kind]
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
