package cluster

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/yindia/cluster-vending/pkg/crossplane"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/storetest"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/kube"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

func topologyRow() *model.Cluster {
	return &model.Cluster{Resource: model.Resource{Base: model.Base{ID: "cluster-id"}}, TenantID: "acme", NetworkID: "network-id", Provider: model.ProviderAWS}
}

func topologyObject(kind, name string, spec, status map[string]any) *unstructured.Unstructured {
	api := "eks.aws.m.upbound.io/v1beta1"
	switch kind {
	case "Subnet", "SecurityGroup", "LaunchTemplate":
		api = "ec2.aws.m.upbound.io/v1beta1"
	case "Release", "ProviderConfig":
		api = "helm.m.crossplane.io/v1beta1"
	}
	obj := &unstructured.Unstructured{Object: map[string]any{"apiVersion": api, "kind": kind, "spec": map[string]any{"forProvider": spec}, "status": map[string]any{"atProvider": status}}}
	obj.SetName(name)
	obj.SetNamespace(kube.TenantNamespace("acme"))
	obj.SetLabels(crossplane.ClusterLabels("acme", "cluster-id"))
	if kind == "Subnet" {
		obj.SetLabels(crossplane.NetworkLabels("acme", "network-id"))
	}
	return obj
}

func topologyRef(name string) map[string]any { return map[string]any{"name": name} }

func topologyEdge(t *testing.T, got *cloudv1.ClusterArchitecture, sk, sn, tk, tn, relation, evidence string) {
	t.Helper()
	if !slices.ContainsFunc(got.Connections, func(e *cloudv1.NetworkArchitectureConnection) bool {
		return e.SourceKind == sk && e.SourceName == sn && e.TargetKind == tk && e.TargetName == tn && e.Relation == relation && e.Evidence == evidence
	}) {
		t.Fatalf("missing %s/%s -> %s/%s %s %s: %v", sk, sn, tk, tn, relation, evidence, got.Connections)
	}
}

func TestClusterArchitectureAWSRelationships(t *testing.T) {
	root := topologyObject("Cluster", "control", nil, map[string]any{"id": "physical-cluster", "vpcConfig": map[string]any{"subnetIds": []any{"subnet-01234567"}, "clusterSecurityGroupId": "sg-01234567"}})
	group := topologyObject("NodeGroup", "unrelated-name", map[string]any{"clusterNameRef": topologyRef("control"), "subnetIdRefs": []any{topologyRef("private")}, "launchTemplate": map[string]any{"id": "lt-01234567"}}, map[string]any{"clusterName": "physical-cluster", "subnetIds": []any{"subnet-01234567"}, "launchTemplate": map[string]any{"id": "lt-01234567"}})
	addon := topologyObject("Addon", "anything", map[string]any{"clusterNameRef": topologyRef("control"), "addonName": "vpc-cni"}, map[string]any{"clusterName": "physical-cluster", "addonName": "vpc-cni"})
	subnet := topologyObject("Subnet", "private", map[string]any{"cidrBlock": "10.0.1.0/24", "availabilityZoneId": "use1-az1"}, map[string]any{"id": "subnet-01234567"})
	labels := subnet.GetLabels()
	labels[crossplane.LabelSubnetRole] = "private"
	subnet.SetLabels(labels)
	sg := topologyObject("SecurityGroup", "security", nil, map[string]any{"id": "sg-01234567"})
	template := topologyObject("LaunchTemplate", "nodes", map[string]any{"vpcSecurityGroupIdRefs": []any{topologyRef("security")}}, map[string]any{"id": "lt-01234567", "vpcSecurityGroupIds": []any{"sg-01234567"}})
	unused := topologyObject("Subnet", "unused", nil, map[string]any{"id": "subnet-abcdef01"})
	objects := []*unstructured.Unstructured{root, group, addon, subnet, sg, template, unused}
	got := clusterArchitecture(topologyRow(), objects)
	for _, edge := range []struct{ sk, sn, tk, tn, relation string }{
		{"Cluster", "control", "NodeGroup", "unrelated-name", "has_node_group"},
		{"Cluster", "control", "Addon", "anything", "has_addon"},
		{"Cluster", "control", "Subnet", "private", "uses_subnet"},
		{"Cluster", "control", "SecurityGroup", "security", "uses_security_group"},
		{"NodeGroup", "unrelated-name", "Subnet", "private", "uses_subnet"},
		{"NodeGroup", "unrelated-name", "LaunchTemplate", "nodes", "uses_launch_template"},
		{"LaunchTemplate", "nodes", "SecurityGroup", "security", "uses_security_group"},
	} {
		topologyEdge(t, got, edge.sk, edge.sn, edge.tk, edge.tn, edge.relation, "observed")
	}
	if len(got.Resources) != 6 {
		t.Fatalf("unreferenced network subnet included: %v", got.Resources)
	}
	for _, r := range got.Resources {
		if r.ReferenceOnly {
			t.Fatalf("owned resource treated as external: %v", r)
		}
		if r.Kind == "Subnet" && (r.ZoneId != "use1-az1" || r.CidrBlock != "10.0.1.0/24" || r.SubnetRole != cloudv1.SubnetRole_SUBNET_ROLE_PRIVATE) {
			t.Fatalf("subnet facts lost: %v", r)
		}
	}
	slices.Reverse(objects)
	if !proto.Equal(got, clusterArchitecture(topologyRow(), objects)) {
		t.Fatal("topology depends on inventory order")
	}
}

func TestClusterArchitectureConfiguredObservedDriftAndForeignRefs(t *testing.T) {
	a := topologyObject("Cluster", "a", nil, map[string]any{"id": "physical-a"})
	b := topologyObject("Cluster", "b", nil, map[string]any{"id": "physical-b"})
	group := topologyObject("NodeGroup", "ng", map[string]any{"clusterNameRef": topologyRef("a")}, map[string]any{"clusterName": "physical-b"})
	got := clusterArchitecture(topologyRow(), []*unstructured.Unstructured{a, b, group})
	topologyEdge(t, got, "Cluster", "a", "NodeGroup", "ng", "has_node_group", "configured")
	topologyEdge(t, got, "Cluster", "b", "NodeGroup", "ng", "has_node_group", "observed")
	for _, ref := range []map[string]any{topologyRef("absent"), {"name": "a", "namespace": "tenant-other"}, {"namespace": "tenant-other"}} {
		foreign := topologyObject("NodeGroup", "ng", map[string]any{"clusterNameRef": ref, "clusterName": "physical-a", "subnetIdRefs": []any{map[string]any{"name": "private", "namespace": "tenant-other"}}, "subnetIds": []any{"subnet-01234567"}}, nil)
		if result := clusterArchitecture(topologyRow(), []*unstructured.Unstructured{a, foreign}); len(result.Connections) != 0 {
			t.Fatalf("foreign/dangling named ref fell back to scalar: %v", result)
		}
	}
	a.SetAnnotations(map[string]string{"crossplane.io/external-name": "physical-old"})
	group = topologyObject("NodeGroup", "ng", nil, map[string]any{"clusterName": "physical-old"})
	if result := clusterArchitecture(topologyRow(), []*unstructured.Unstructured{a, group}); len(result.Connections) != 0 {
		t.Fatal("stale external name aliased observed cluster identity")
	}
}

func TestClusterArchitectureReferenceOnlyAndPrivacy(t *testing.T) {
	root := topologyObject("Cluster", "control", map[string]any{"vpcConfig": map[string]any{"subnetIds": []any{"subnet-0123456789abcdef0"}, "securityGroupIds": []any{"sg-0123456789abcdef0", "DO-NOT-EXPOSE"}}, "tags": map[string]any{"private": "DO-NOT-EXPOSE"}}, nil)
	root.SetAnnotations(map[string]string{"private": "DO-NOT-EXPOSE"})
	group := topologyObject("NodeGroup", "nodes", map[string]any{"clusterNameRef": topologyRef("control"), "launchTemplate": map[string]any{"id": "lt-0123456789abcdef0"}, "remoteAccess": map[string]any{"sourceSecurityGroupIds": []any{"sg-abcdef01234567890"}, "ec2SshKey": "DO-NOT-EXPOSE"}}, nil)
	got := clusterArchitecture(topologyRow(), []*unstructured.Unstructured{root, group})
	if len(got.Resources) != 6 {
		t.Fatalf("explicit references missing or invalid reference included: %v", got.Resources)
	}
	for _, r := range got.Resources {
		if r.Kind == "Cluster" || r.Kind == "NodeGroup" {
			continue
		}
		if !r.ReferenceOnly || r.Name != r.CloudId || r.CidrBlock != "" || r.ZoneId != "" {
			t.Fatalf("reference presented as observed owned resource: %v", r)
		}
	}
	raw, err := protojson.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "DO-NOT-EXPOSE") {
		t.Fatalf("raw spec/annotations leaked: %s", raw)
	}
}

func topologyComponentBinding() []*unstructured.Unstructured {
	root := topologyObject("Cluster", "control", nil, map[string]any{"id": "physical"})
	auth := topologyObject("ClusterAuth", "auth", map[string]any{"clusterNameRef": topologyRef("control")}, nil)
	auth.Object["spec"].(map[string]any)["writeConnectionSecretToRef"] = map[string]any{"name": "private-kubeconfig"}
	pc := topologyObject("ProviderConfig", "helm", nil, nil)
	pc.Object["spec"] = map[string]any{"credentials": map[string]any{"source": "Secret", "secretRef": map[string]any{"name": "private-kubeconfig", "namespace": "tenant-acme", "key": "kubeconfig"}}}
	release := topologyObject("Release", "component", map[string]any{"values": map[string]any{"token": "DO-NOT-EXPOSE"}}, map[string]any{"state": "deployed"})
	release.Object["spec"].(map[string]any)["providerConfigRef"] = map[string]any{"kind": "ProviderConfig", "name": "helm"}
	return []*unstructured.Unstructured{root, auth, pc, release}
}

func TestClusterArchitectureComponentBinding(t *testing.T) {
	objects := topologyComponentBinding()
	got := clusterArchitecture(topologyRow(), objects)
	topologyEdge(t, got, "Cluster", "control", "Release", "component", "has_component", "configured")
	if len(got.Resources) != 2 {
		t.Fatalf("private plumbing exposed: %v", got.Resources)
	}
	raw, err := protojson.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"private-kubeconfig", "ProviderConfig", "ClusterAuth", "DO-NOT-EXPOSE"} {
		if strings.Contains(string(raw), value) {
			t.Fatalf("credential plumbing leaked: %s", raw)
		}
	}
	for _, tt := range []struct {
		name   string
		change func([]*unstructured.Unstructured)
	}{
		{"wrong credentials", func(o []*unstructured.Unstructured) {
			_ = unstructured.SetNestedField(o[2].Object, "wrong-secret", "spec", "credentials", "secretRef", "name")
		}},
		{"foreign secret namespace", func(o []*unstructured.Unstructured) {
			_ = unstructured.SetNestedField(o[2].Object, "tenant-other", "spec", "credentials", "secretRef", "namespace")
		}},
		{"foreign ProviderConfig", func(o []*unstructured.Unstructured) {
			_ = unstructured.SetNestedField(o[3].Object, "tenant-other", "spec", "providerConfigRef", "namespace")
		}},
		{"foreign auth cluster", func(o []*unstructured.Unstructured) {
			_ = unstructured.SetNestedField(o[1].Object, "missing", "spec", "forProvider", "clusterNameRef", "name")
		}},
		{"labels alone", func(o []*unstructured.Unstructured) {
			unstructured.RemoveNestedField(o[3].Object, "spec", "providerConfigRef")
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			values := topologyComponentBinding()
			tt.change(values)
			result := clusterArchitecture(topologyRow(), values)
			if len(result.Connections) != 0 {
				t.Fatalf("component binding fabricated: %v", result)
			}
		})
	}
}

func TestClusterArchitectureScopeAndMissingEvidence(t *testing.T) {
	root := topologyObject("Cluster", "same-name", nil, map[string]any{"id": "physical"})
	group := topologyObject("NodeGroup", "same-name-default", nil, nil)
	foreign := topologyObject("Addon", "foreign", map[string]any{"clusterNameRef": topologyRef("same-name")}, nil)
	labels := foreign.GetLabels()
	labels[crossplane.LabelCluster] = "other"
	foreign.SetLabels(labels)
	otherTenant := topologyObject("NodeGroup", "other", nil, map[string]any{"clusterName": "physical"})
	otherTenant.SetNamespace("tenant-other")
	unknown := topologyObject("Role", "unknown", nil, nil)
	got := clusterArchitecture(topologyRow(), []*unstructured.Unstructured{root, group, foreign, otherTenant, unknown})
	if len(got.Resources) != 2 || len(got.Connections) != 0 {
		t.Fatalf("scope/name heuristic violation: %v", got)
	}
	row := topologyRow()
	row.Provider = "future"
	if len(clusterArchitecture(row, []*unstructured.Unstructured{root}).Resources) != 0 {
		t.Fatal("AWS assumed for unknown provider")
	}
}

func TestClusterArchitectureLiveViewBeforePersistence(t *testing.T) {
	store := storetest.NewStore(t)
	storetest.MustTenant(t, store, "acme")
	network := storetest.MustNetwork(t, store, "acme", "network")
	row := storetest.MustCluster(t, store, "acme", "cluster", network)
	k := storetest.NewClient(t)
	deps := reconcile.Deps{Store: store, Kube: k}
	root := topologyObject("Cluster", "control", nil, map[string]any{"id": "physical"})
	root.SetLabels(crossplane.ClusterLabels("acme", row.ID))
	group := topologyObject("NodeGroup", "nodes", map[string]any{"clusterNameRef": topologyRef("control")}, nil)
	group.SetLabels(crossplane.ClusterLabels("acme", row.ID))
	for _, obj := range []*unstructured.Unstructured{root, group} {
		if err := k.Create(t.Context(), obj); err != nil {
			t.Fatal(err)
		}
	}
	if err := deps.Record(t.Context(), reconcile.Owner{TenantID: "acme", Kind: model.OwnerCluster, ID: row.ID}, []client.Object{root, group}); err != nil {
		t.Fatal(err)
	}
	records, err := store.ManagedResources().ListByOwner(t.Context(), model.OwnerCluster, row.ID)
	if err != nil {
		t.Fatal(err)
	}
	before := slices.Clone(row.Status)
	readOnly := &topologyReadOnlyClient{Client: k}
	s := New(reconcile.Deps{Store: store, Kube: readOnly})
	view, err := s.viewOf(t.Context(), row, records, componentsState{})
	if err != nil {
		t.Fatal(err)
	}
	topologyEdge(t, view.GetStatus().GetArchitecture(), "Cluster", "control", "NodeGroup", "nodes", "has_node_group", "configured")
	if readOnly.secretReads != 0 || readOnly.writes != 0 {
		t.Fatal("projection performed sensitive reads or writes")
	}
	after, err := store.Clusters().Get(t.Context(), "acme", row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after.Status) {
		t.Fatal("live view persisted cluster status")
	}
	// Even an otherwise scoped inventory record must not cause a Secret read.
	records = append(records, model.ManagedResource{TenantID: row.TenantID, OwnerKind: model.OwnerCluster, OwnerID: row.ID, APIVersion: "v1", Kind: "Secret", Namespace: kube.TenantNamespace(row.TenantID), Name: "private-kubeconfig"})

	if _, err := s.observeArchitecture(t.Context(), row, records); err != nil {
		t.Fatal(err)
	}
	if readOnly.secretReads != 0 {
		t.Fatal("architecture read a Secret payload")
	}
	// The reader must propagate actual errors, rather than report partial facts.
	readOnly.fail = true
	if _, err := s.observeArchitecture(t.Context(), row, records); err == nil {
		t.Fatal("read error swallowed")
	}
}

type topologyReadOnlyClient struct {
	client.Client
	secretReads, writes int
	fail                bool
}

func (c *topologyReadOnlyClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if obj.GetObjectKind().GroupVersionKind().Kind == "Secret" {
		c.secretReads++
		return errors.New("secret read forbidden")
	}
	if c.fail {
		return errors.New("read denied")
	}
	return c.Client.Get(ctx, key, obj, opts...)
}
func (c *topologyReadOnlyClient) Create(context.Context, client.Object, ...client.CreateOption) error {
	c.writes++
	return errors.New("write forbidden")
}
func (c *topologyReadOnlyClient) Update(context.Context, client.Object, ...client.UpdateOption) error {
	c.writes++
	return errors.New("write forbidden")
}
func (c *topologyReadOnlyClient) Patch(context.Context, client.Object, client.Patch, ...client.PatchOption) error {
	c.writes++
	return errors.New("write forbidden")
}
func (c *topologyReadOnlyClient) Delete(context.Context, client.Object, ...client.DeleteOption) error {
	c.writes++
	return errors.New("write forbidden")
}

func TestClusterArchitectureLaunchTemplateExplicitPhysicalName(t *testing.T) {
	template := topologyObject("LaunchTemplate", "different-kubernetes-name", map[string]any{"name": "actual-template"}, map[string]any{"id": "lt-01234567", "name": "actual-template"})
	group := topologyObject("NodeGroup", "nodes", map[string]any{"launchTemplate": map[string]any{"name": "actual-template"}}, map[string]any{"launchTemplate": map[string]any{"name": "actual-template"}})
	got := clusterArchitecture(topologyRow(), []*unstructured.Unstructured{template, group})
	topologyEdge(t, got, "NodeGroup", "nodes", "LaunchTemplate", "different-kubernetes-name", "uses_launch_template", "observed")
	_ = unstructured.SetNestedField(group.Object, "different-kubernetes-name", "spec", "forProvider", "launchTemplate", "name")
	_ = unstructured.SetNestedField(group.Object, "different-kubernetes-name", "status", "atProvider", "launchTemplate", "name")
	if result := clusterArchitecture(topologyRow(), []*unstructured.Unstructured{template, group}); len(result.Connections) != 0 {
		t.Fatal("Kubernetes name inferred as physical template name")
	}
}

func TestClusterArchitectureRejectsForeignInventoryBeforeRead(t *testing.T) {
	row := topologyRow()
	for _, r := range []model.ManagedResource{
		{TenantID: "other", OwnerKind: model.OwnerCluster, OwnerID: row.ID, Namespace: "tenant-other", APIVersion: "eks.aws.m.upbound.io/v1beta1", Kind: "Cluster", Name: "foreign"},
		{TenantID: "acme", OwnerKind: model.OwnerCluster, OwnerID: "other", Namespace: "tenant-acme", APIVersion: "eks.aws.m.upbound.io/v1beta1", Kind: "Cluster", Name: "foreign"},
		{TenantID: "acme", OwnerKind: model.OwnerCluster, OwnerID: row.ID, Namespace: "tenant-acme", APIVersion: "unexpected.example/v1", Kind: "Cluster", Name: "foreign"},
	} {
		// No store/client is available: a foreign inventory row must not trigger
		// any management read, much less expose another tenant's topology.
		result, err := New(reconcile.Deps{}).observeArchitecture(t.Context(), row, []model.ManagedResource{r})
		if err != nil || len(result.Resources) != 0 {
			t.Fatalf("foreign inventory accepted: %v %v", result, err)
		}
	}
}
