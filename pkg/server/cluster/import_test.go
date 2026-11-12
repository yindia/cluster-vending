package cluster

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect/v2"
	protectionv1beta1 "github.com/crossplane/crossplane/apis/v2/protection/v1beta1"
	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	awsiam "github.com/upbound/provider-aws/v2/apis/namespaced/iam/v1beta1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	awscloud "github.com/yindia/cluster-vending/pkg/cloud/aws"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/server/service"
)

type fakeClusterDiscoverer struct{ cluster *awscloud.Cluster }

func (f fakeClusterDiscoverer) DiscoverCluster(context.Context, awscloud.Access, string, string, string) (*awscloud.Cluster, error) {
	return f.cluster, nil
}

const (
	tfIssuer   = "https://oidc.eks.us-east-1.amazonaws.com/id/ABCDEF"
	tfNodeRole = "arn:aws:iam::444455556666:role/tf-node"
)

// terraformCluster is an EKS cluster as a Terraform module builds it:
// secrets encryption with a KMS key, an IAM OIDC provider (IRSA), two node
// groups, access entries of its own, an extra security group, a log group
// with a retention, extra policies on its roles and an add-on the contract
// has no name for. Its subnets are the world's network's.
func terraformCluster() *awscloud.Cluster {
	policy := func(name string) string { return "arn:aws:iam::aws:policy/" + name }
	group := func(name string) awscloud.NodeGroup {
		return awscloud.NodeGroup{Name: name, Status: "ACTIVE", Version: "1.31", NodeRoleARN: tfNodeRole,
			InstanceTypes: []string{"m6i.large"}, SubnetIDs: []string{"subnet-a", "subnet-b"}, CapacityType: "ON_DEMAND",
			AMIType: "AL2023_x86_64_STANDARD", DiskSizeGB: 50, MinSize: 2, MaxSize: 6, DesiredSize: 3,
			Labels: map[string]string{"pool": name}, Taints: []awscloud.NodeTaint{{Key: "dedicated", Value: "x", Effect: "NO_SCHEDULE"}},
			Tags: map[string]string{"terraform": "true"}}
	}
	return &awscloud.Cluster{
		Name: "tf", ARN: "arn:aws:eks:us-east-1:444455556666:cluster/tf", Version: "1.31", Status: "ACTIVE",
		SubnetIDs: []string{"subnet-a", "subnet-b"}, SecurityGroupIDs: []string{"sg-0123456789abcdef0"},
		EndpointPrivateAccess: true, EndpointPublicAccess: true, PublicAccessCIDRs: []string{"203.0.113.0/24"},
		LogTypes: []string{"api", "audit"}, AuthenticationMode: "API_AND_CONFIG_MAP", SupportType: "STANDARD",
		ServiceCIDR: "172.20.0.0/16", IPFamily: "ipv4", EncryptionKeyARN: existingKey, OIDCIssuer: tfIssuer,
		Tags: map[string]string{"terraform": "true"},
		Role: awscloud.ClusterRole{Role: awscloud.Role{Name: "tf-cluster", ARN: "arn:aws:iam::444455556666:role/tf-cluster",
			AssumeRolePolicy: `{"Statement":[{"Action":["sts:AssumeRole","sts:TagSession"]}]}`},
			PolicyARNs: []string{policy("AmazonEKSClusterPolicy"), policy("AmazonEKSVPCResourceController")}},
		NodeRole: awscloud.ClusterRole{Role: awscloud.Role{Name: "tf-node", ARN: tfNodeRole, AssumeRolePolicy: `{"Statement":[]}`},
			PolicyARNs: []string{policy("AmazonEC2ContainerRegistryReadOnly"), policy("AmazonEKSWorkerNodePolicy"),
				policy("AmazonEKS_CNI_Policy"), policy("AmazonSSMManagedInstanceCore")}},
		NodeGroups: []awscloud.NodeGroup{group("default-ng"), group("gpu")},
		Addons: []awscloud.Addon{{Name: "adot", Version: "v0.1.0-eksbuild.1"}, {Name: "coredns", Version: "v1.11.4-eksbuild.2"},
			{Name: "kube-proxy", Version: "v1.31.2-eksbuild.3"}, {Name: "vpc-cni", Version: "v1.19.0-eksbuild.1", ConfigurationValues: `{"env":{}}`}},
		AccessEntries: []awscloud.AccessEntry{
			{PrincipalARN: devRole, Type: "STANDARD", Policies: []awscloud.AccessPolicy{{PolicyARN: viewPolicy, Scope: "namespace", Namespaces: []string{"team-a"}}}},
			{PrincipalARN: tfNodeRole, Type: "EC2_LINUX"},
		},
		PodIdentityAssociations: []awscloud.PodIdentityAssociation{{ID: "a-0123456789abcdefg", Namespace: "apps", ServiceAccount: "api"}},
		IdentityProviderConfigs: []awscloud.IdentityProviderConfig{{Name: "okta", IssuerURL: "https://example.okta.com", ClientID: "kubernetes"}},
		OIDCProviderARN:         "arn:aws:iam::444455556666:oidc-provider/oidc.eks.us-east-1.amazonaws.com/id/ABCDEF",
		LogGroup:                &awscloud.LogGroup{Name: "/aws/eks/tf/cluster", RetentionDays: 30},
		SecurityGroups: []awscloud.ClusterSecurityGroup{{ID: "sg-0123456789abcdef0", Name: "tf-cluster", Description: "EKS control plane",
			VPCID: "vpc-1", Rules: []awscloud.SecurityGroupRule{
				{ID: "sgr-in", Protocol: "tcp", FromPort: ptr.To(443), ToPort: ptr.To(443), ReferencedGroupID: "sg-nodes"},
				{ID: "sgr-out", Egress: true, Protocol: "-1", CIDRIPv4: "0.0.0.0/0"}}}},
	}
}

func importWorld(t *testing.T, found *awscloud.Cluster) world {
	t.Helper()
	return newWorld(t, interceptor.Funcs{}, nil, withVersions, func(d *service.Deps) {
		d.AWSClusterDiscovery = fakeClusterDiscoverer{cluster: found}
	})
}

func importTF(t *testing.T, w world) *cloudv1.Cluster {
	t.Helper()
	res, err := w.svc.ImportCluster(t.Context(), &cloudv1.ImportClusterRequest{TenantId: worldTenant, Name: "tf", NetworkId: w.row.NetworkID,
		Provider: &cloudv1.ImportClusterRequest_Aws{Aws: &cloudv1.AwsImportCluster{DefaultNodeGroupName: "default-ng"}}})
	if err != nil {
		t.Fatalf("ImportCluster: %v", err)
	}
	// The call stored the import; a pass of the loop creates its objects.
	id := res.GetCluster().GetId()
	reconcile(t, w, id)
	// The imported EKS cluster is ACTIVE, as its provider reports once it
	// observed it (a blocked import has no object).
	eks := &awseks.Cluster{}
	key := client.ObjectKey{Namespace: worldNS, Name: crossplaneaws.ClusterPrefix(worldTenant, id, "tf")}
	if err := w.kube.Get(t.Context(), key, eks); err == nil {
		eks.Status.AtProvider.Status = ptr.To("ACTIVE")
		if err := w.kube.Update(t.Context(), eks); err != nil {
			t.Fatal(err)
		}
	}
	return live(t, w, id)
}

// update stores an update of a cluster and, when it was stored, runs the
// pass of the loop that applies it.
func update(t *testing.T, w world, req *cloudv1.UpdateClusterRequest) (*cloudv1.UpdateClusterResponse, error) {
	t.Helper()
	res, err := w.svc.UpdateCluster(t.Context(), req)
	if err == nil && !req.GetValidateOnly() {
		reconcile(t, w, req.GetCluster().GetId())
	}
	return res, err
}

// reconcile runs one pass of the background loop for the cluster: a call
// only stores what was asked for, this applies it.
func reconcile(t *testing.T, w world, id string) {
	t.Helper()
	if err := w.svc.Reconcile(t.Context(), worldTenant, id); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
}

// live is the cluster as GetClusterStatus returns it: read live.
func live(t *testing.T, w world, id string) *cloudv1.Cluster {
	t.Helper()
	res, err := w.svc.GetClusterStatus(t.Context(), &cloudv1.GetClusterStatusRequest{TenantId: worldTenant, ClusterId: id})
	if err != nil {
		t.Fatalf("GetClusterStatus: %v", err)
	}
	return res.GetCluster()
}

func linesBy(c *cloudv1.Cluster, d cloudv1.Disposition) map[string]*cloudv1.ImportedResource {
	out := map[string]*cloudv1.ImportedResource{}
	for _, l := range c.GetStatus().GetImport().GetResources() {
		if l.GetDisposition() == d {
			out[l.GetKind()+"/"+l.GetExternalName()+l.GetName()] = l
		}
	}
	return out
}

// objectsOf are the management cluster's objects of a cluster, as
// unstructured, by kind and name.
func objectsOf(t *testing.T, w world, id string) map[string]*unstructured.Unstructured {
	t.Helper()
	out := map[string]*unstructured.Unstructured{}
	lists := append(crossplaneaws.ClusterLists(), &protectionv1beta1.UsageList{})
	for _, list := range lists {
		gvks, _, err := w.kube.Scheme().ObjectKinds(list)
		if err != nil {
			t.Fatal(err)
		}
		ul := &unstructured.UnstructuredList{}
		ul.SetGroupVersionKind(gvks[0])
		if err := w.kube.List(t.Context(), ul, client.InNamespace(worldNS), client.MatchingLabels{"cluster-vending.io/cluster": id}); err != nil {
			t.Fatal(err)
		}
		for i := range ul.Items {
			out[ul.Items[i].GetKind()+"/"+ul.Items[i].GetName()] = &ul.Items[i]
		}
	}
	return out
}

func policies(u *unstructured.Unstructured) []string {
	p, _, _ := unstructured.NestedStringSlice(u.Object, "spec", "managementPolicies")
	return p
}

// forProviderWithoutTags is an object's spec.forProvider as JSON, without
// its tags: what must not change at a takeover.
func forProviderWithoutTags(t *testing.T, u *unstructured.Unstructured) string {
	t.Helper()
	fp, _, _ := unstructured.NestedMap(u.Object, "spec", "forProvider")
	delete(fp, "tags")
	raw, err := json.Marshal(fp)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestImportClusterPlan(t *testing.T) {
	w := importWorld(t, terraformCluster())
	c := importTF(t, w)
	st := c.GetStatus()
	if st.GetState() != cloudv1.ClusterState_CLUSTER_STATE_IMPORTED || st.GetImport().GetPhase() != cloudv1.ImportPhase_IMPORT_PHASE_PLANNED ||
		len(linesBy(c, cloudv1.Disposition_DISPOSITION_BLOCKING)) != 0 {
		t.Fatalf("state %s (%s), phase %s, blocking %v", st.GetState(), st.GetMessage(), st.GetImport().GetPhase(),
			linesBy(c, cloudv1.Disposition_DISPOSITION_BLOCKING))
	}
	// The spec says what the cluster is.
	spec := c.GetSpec()
	pool, aws := spec.GetDefaultNodePool(), spec.GetAws()
	if spec.GetControlPlane().GetKubernetesVersion() != "1.31" || spec.GetControlPlane().GetSecretsEncryption().GetKmsKeyArn() != existingKey ||
		spec.GetControlPlane().GetEndpointAccess().GetPublicAccessCidrs()[0] != "203.0.113.0/24" ||
		spec.GetNetwork().GetServiceCidr() != "172.20.0.0/16" || len(spec.GetNetwork().GetZoneIds()) != 2 ||
		aws.GetAdditionalSecurityGroupIds()[0] != "sg-0123456789abcdef0" ||
		aws.GetAccessConfig().GetAuthenticationMode() != cloudv1.AwsAuthenticationMode_AWS_AUTHENTICATION_MODE_API_AND_CONFIG_MAP ||
		len(aws.GetEnabledClusterLogTypes()) != 2 || pool.GetInstanceTypes()[0] != "m6i.large" || pool.GetDiskSizeGb() != 50 ||
		pool.GetScalingConfig().GetMaxSize() != 6 || pool.GetTaints()[0].GetEffect() != cloudv1.TaintEffect_TAINT_EFFECT_NO_SCHEDULE ||
		aws.GetIrsa() || aws.GetControlPlaneLogGroup() != nil {
		t.Errorf("derived spec = %v", spec)
	}
	if entries := spec.GetAccess().GetAccessEntries(); len(entries) != 1 || entries[0].GetPrincipalArn() != devRole ||
		entries[0].GetPolicies()[0].GetScope() != cloudv1.AccessPolicyScope_ACCESS_POLICY_SCOPE_NAMESPACE {
		t.Errorf("derived access entries = %v, want the dev role's (the node role's is EKS's)", entries)
	}

	prefix := crossplaneaws.ClusterPrefix(worldTenant, c.GetId(), "tf")
	carried := linesBy(c, cloudv1.Disposition_DISPOSITION_CARRIED_OVER)
	for _, want := range []string{
		"Key/1234abcd-12ab-34cd-56ef-1234567890ab" + prefix + "-secrets-key",
		"OpenIDConnectProvider/arn:aws:iam::444455556666:oidc-provider/oidc.eks.us-east-1.amazonaws.com/id/ABCDEF" + prefix + "-oidc",
		"Group//aws/eks/tf/cluster" + prefix + "-logs",
		"Addon/tf:adot" + prefix + "-addon-adot",
		"PodIdentityAssociation/a-0123456789abcdefg" + prefix + "-identity-a-0123456789abcdefg",
	} {
		if carried[want] == nil {
			t.Errorf("no CARRIED_OVER line %s in %v", want, keys(carried))
		}
	}
	extraPolicies, idps, groups, rules := 0, 0, 0, 0
	for k, l := range carried {
		switch l.GetKind() {
		case "SecurityGroup":
			groups++
			if l.GetExternalName() != "sg-0123456789abcdef0" || l.GetDetail() != "" {
				t.Errorf("security group line = %v, want it owned in full (only the cluster uses it)", l)
			}
		case "SecurityGroupIngressRule", "SecurityGroupEgressRule":
			rules++
		}
		if strings.HasPrefix(k, "RolePolicyAttachment/") {
			extraPolicies++
		}
		if l.GetKind() == "IdentityProviderConfig" && l.GetExternalName() == "okta" {
			idps++
		}
	}
	if groups != 1 || rules != 2 {
		t.Errorf("carried over: %d security groups, %d rules; want the control plane's group with its 2 rules", groups, rules)
	}
	if extraPolicies != 2 || idps != 1 || !strings.Contains(carried["Key/1234abcd-12ab-34cd-56ef-1234567890ab"+prefix+"-secrets-key"].GetDetail(), "without Delete") {
		t.Errorf("carried over: %d extra role policies (want 2), %d identity providers (want 1); lines %v", extraPolicies, idps, keys(carried))
	}
	left := linesBy(c, cloudv1.Disposition_DISPOSITION_NOT_IMPORTED)
	if left["NodeGroup/gpu"] == nil || left["AccessEntry/tf:"+tfNodeRole] == nil || len(left) != 2 {
		t.Errorf("NOT_IMPORTED = %v, want the second node group and the node role's access entry", keys(left))
	}
	// The takeover only adds tags to what exists, and creates what the
	// platform adds.
	for k, l := range linesBy(c, cloudv1.Disposition_DISPOSITION_WILL_UPDATE) {
		if !strings.HasPrefix(l.GetDetail(), "adds ") || strings.Contains(l.GetDetail(), "->") {
			t.Errorf("unexpected change planned for %s: %s", k, l.GetDetail())
		}
	}
	created := linesBy(c, cloudv1.Disposition_DISPOSITION_WILL_CREATE)
	for _, want := range []string{"ClusterAuth/" + prefix + "-auth", "Addon/" + prefix + "-addon-eks-pod-identity-agent",
		"Addon/" + prefix + "-addon-aws-ebs-csi-driver"} {
		if created[want] == nil {
			t.Errorf("no WILL_CREATE line %s in %v", want, keys(created))
		}
	}
	// Nothing is listed as unchanged that the takeover touches: the roles
	// and the access entry get the platform's tags, add-ons its conflict
	// resolution.
	updated := linesBy(c, cloudv1.Disposition_DISPOSITION_WILL_UPDATE)
	entry := accessEntryName(t, w, c.GetId())
	for _, want := range []string{"Role/tf-cluster" + prefix + "-cluster-role", "Role/tf-node" + prefix + "-node-role",
		"AccessEntry/tf:" + devRole + entry, "Addon/tf:vpc-cni" + prefix + "-addon-vpc-cni"} {
		if updated[want] == nil {
			t.Errorf("no WILL_UPDATE line %s in %v", want, keys(updated))
		}
	}
	if l := updated["Addon/tf:vpc-cni"+prefix+"-addon-vpc-cni"]; !strings.Contains(l.GetDetail(), "OVERWRITE") {
		t.Errorf("add-on line = %q, want it to say conflicts are overwritten", l.GetDetail())
	}

	// Observe mode: every object only reads, has its cloud identity, and
	// nothing the platform adds exists yet. No cloud write can happen.
	objects := objectsOf(t, w, c.GetId())
	if len(objects) != len(st.GetManagedResources()) || len(objects) < 20 {
		t.Fatalf("%d objects, %d recorded", len(objects), len(st.GetManagedResources()))
	}
	for name, u := range objects {
		if got := policies(u); !slices.Equal(got, []string{"Observe", "LateInitialize"}) {
			t.Errorf("%s managementPolicies = %v, want Observe and LateInitialize only", name, got)
		}
		if u.GetAnnotations()[crossplaneaws.ExternalNameAnnotation] == "" {
			t.Errorf("%s has no external name", name)
		}
		if u.GetKind() == "ClusterAuth" || u.GetKind() == "Usage" {
			t.Errorf("%s exists before the takeover", name)
		}
	}
	for name, want := range map[string]string{
		"Cluster/" + prefix: "tf", "NodeGroup/" + prefix + "-default": "default-ng",
		"Role/" + prefix + "-cluster-role": "tf-cluster", "Role/" + prefix + "-node-role": "tf-node",
		"RolePolicyAttachment/" + prefix + "-node-cni":    "tf-node/arn:aws:iam::aws:policy/AmazonEKS_CNI_Policy",
		"Addon/" + prefix + "-addon-vpc-cni":              "tf:vpc-cni",
		"AccessEntry/" + accessEntryName(t, w, c.GetId()): "tf:" + devRole,
	} {
		if u := objects[name]; u == nil || u.GetAnnotations()[crossplaneaws.ExternalNameAnnotation] != want {
			t.Errorf("%s external name = %v, want %s", name, u.GetAnnotations(), want)
		}
	}
	role := &awsiam.Role{}
	if err := w.kube.Get(t.Context(), client.ObjectKey{Namespace: worldNS, Name: prefix + "-cluster-role"}, role); err != nil ||
		!strings.Contains(*role.Spec.ForProvider.AssumeRolePolicy, "sts:TagSession") {
		t.Errorf("the cluster role's trust policy was not kept: %v, %v", role.Spec.ForProvider.AssumeRolePolicy, err)
	}
	if n := len(objectsOfKind(objects, "NodeGroup")); n != 1 {
		t.Errorf("%d node group objects, want only the adopted one", n)
	}

	// Read-only until taken over; the loop leaves it alone.
	imported := &cloudv1.Cluster{Id: c.GetId(), DisplayName: "x"}
	req := updateRequest(w, nil, "display_name")
	req.Cluster = imported
	if _, err := update(t, w, req); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("update of an imported cluster = %v, want FAILED_PRECONDITION", err)
	}
	if err := w.svc.Reconcile(t.Context(), worldTenant, c.GetId()); err != nil {
		t.Fatal(err)
	}
	if after := objectsOf(t, w, c.GetId()); len(after) != len(objects) {
		t.Errorf("the loop changed an imported cluster: %d -> %d objects", len(objects), len(after))
	}
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func objectsOfKind(objects map[string]*unstructured.Unstructured, kind string) []*unstructured.Unstructured {
	var out []*unstructured.Unstructured
	for _, u := range objects {
		if u.GetKind() == kind {
			out = append(out, u)
		}
	}
	return out
}

// accessEntryName is the name of the cluster's one "-access-" entry object.
func accessEntryName(t *testing.T, w world, id string) string {
	t.Helper()
	for _, u := range objectsOfKind(objectsOf(t, w, id), "AccessEntry") {
		if strings.Contains(u.GetName(), "-access-") {
			return u.GetName()
		}
	}
	t.Fatal("no access entry object")
	return ""
}

func TestTakeOverCluster(t *testing.T) {
	w := importWorld(t, terraformCluster())
	ctx := t.Context()
	c := importTF(t, w)
	prefix := crossplaneaws.ClusterPrefix(worldTenant, c.GetId(), "tf")
	before := objectsOf(t, w, c.GetId())
	req := &cloudv1.TakeOverClusterRequest{TenantId: worldTenant, ClusterId: c.GetId()}

	if _, err := w.svc.TakeOverCluster(ctx, req); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("takeover without acknowledgement = %v, want FAILED_PRECONDITION", err)
	}
	req.AcknowledgeNotImported = true
	// The network must be owned first.
	network, err := w.store.Networks().Get(ctx, worldTenant, w.row.NetworkID)
	if err != nil {
		t.Fatal(err)
	}
	owned := network.State
	network.State = "NETWORK_STATE_IMPORTED"
	if err := w.store.Networks().Update(ctx, network); err != nil {
		t.Fatal(err)
	}
	if _, err := w.svc.TakeOverCluster(ctx, req); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("takeover in a network that is only imported = %v, want FAILED_PRECONDITION", err)
	}
	network.State = owned
	if err := w.store.Networks().Update(ctx, network); err != nil {
		t.Fatal(err)
	}

	out, err := w.svc.TakeOverCluster(ctx, req)
	if err != nil {
		t.Fatalf("TakeOverCluster: %v", err)
	}
	if st := out.GetCluster().GetStatus(); st.GetImport().GetPhase() != cloudv1.ImportPhase_IMPORT_PHASE_CONVERGING ||
		st.GetState() == cloudv1.ClusterState_CLUSTER_STATE_IMPORTED {
		t.Errorf("after takeover: %s / %s", st.GetState(), st.GetImport().GetPhase())
	}
	// The call stored the takeover and owned nothing; the pass does.
	for name, u := range objectsOf(t, w, c.GetId()) {
		if got := policies(u); len(got) == 0 || got[0] != "Observe" {
			t.Fatalf("%s managementPolicies right after the call = %v: the call must not own", name, got)
		}
	}
	reconcile(t, w, c.GetId())
	after := objectsOf(t, w, c.GetId())
	// No diff: what existed keeps its settings and its cloud identity, is
	// fully managed now, and no node group was added or replaced.
	for name, was := range before {
		now := after[name]
		if now == nil {
			t.Errorf("%s is gone after the takeover", name)
			continue
		}
		if a, b := forProviderWithoutTags(t, was), forProviderWithoutTags(t, now); a != b {
			t.Errorf("%s changed at the takeover:\n%s\n%s", name, a, b)
		}
		if was.GetAnnotations()[crossplaneaws.ExternalNameAnnotation] != now.GetAnnotations()[crossplaneaws.ExternalNameAnnotation] {
			t.Errorf("%s lost its external name: %v", name, now.GetAnnotations())
		}
		got := policies(now)
		switch {
		case name == "Key/"+prefix+"-secrets-key":
			if slices.Contains(got, "Delete") || slices.Contains(got, "*") || !slices.Contains(got, "Update") {
				t.Errorf("the KMS key's managementPolicies = %v, want everything but Delete", got)
			}
		case len(got) != 0 && !slices.Equal(got, []string{"*"}):
			t.Errorf("%s managementPolicies = %v, want full management", name, got)
		}
	}
	if n := len(objectsOfKind(after, "NodeGroup")); n != 1 {
		t.Errorf("%d node groups after the takeover, want the adopted one only", n)
	}
	// What the plan listed as WILL_CREATE exists now.
	for _, name := range []string{"ClusterAuth/" + prefix + "-auth", "Addon/" + prefix + "-addon-eks-pod-identity-agent"} {
		if after[name] == nil {
			t.Errorf("%s was not created at the takeover", name)
		}
	}
	if len(objectsOfKind(after, "Usage")) == 0 {
		t.Error("no Usages after the takeover")
	}
	// Carried-over objects are kept by the render that follows.
	records, err := w.store.ManagedResources().ListByOwner(ctx, model.OwnerCluster, c.GetId())
	if err != nil {
		t.Fatal(err)
	}
	carried := 0
	for _, r := range records {
		if r.CarriedOver {
			carried++
			if after[r.Kind+"/"+r.Name] == nil {
				t.Errorf("carried-over %s %s was pruned", r.Kind, r.Name)
			}
		}
	}
	if carried != 11 {
		t.Errorf("%d carried-over records, want 11 (2 role policies, OIDC provider, log group, key, identity provider, add-on, pod identity, security group and its 2 rules)", carried)
	}
	if _, err := w.svc.TakeOverCluster(ctx, req); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("second takeover = %v, want FAILED_PRECONDITION", err)
	}

	// Everything Ready: the cluster is RUNNING and the import CONVERGED.
	for _, u := range objectsOf(t, w, c.GetId()) {
		conds := []any{map[string]any{"type": "Ready", "status": "True"},
			map[string]any{"type": "Synced", "status": "True", "observedGeneration": u.GetGeneration()}}
		if err := unstructured.SetNestedSlice(u.Object, conds, "status", "conditions"); err != nil {
			t.Fatal(err)
		}
		if u.GetKind() == "Cluster" {
			_ = unstructured.SetNestedField(u.Object, "ACTIVE", "status", "atProvider", "status")
		}
		if err := w.kube.Update(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	for range 3 {
		if err := w.svc.Reconcile(ctx, worldTenant, c.GetId()); err != nil {
			t.Fatal(err)
		}
	}
	got, err := w.svc.GetClusterStatus(ctx, &cloudv1.GetClusterStatusRequest{TenantId: worldTenant, ClusterId: c.GetId()})
	if err != nil {
		t.Fatal(err)
	}
	if st := got.GetCluster().GetStatus(); st.GetState() != cloudv1.ClusterState_CLUSTER_STATE_RUNNING ||
		st.GetImport().GetPhase() != cloudv1.ImportPhase_IMPORT_PHASE_CONVERGED {
		t.Errorf("after converging: %s (%s) / %s", st.GetState(), st.GetMessage(), st.GetImport().GetPhase())
	}
}

// A security group other things use too is carried over without Delete;
// one that cannot be read is NOT_IMPORTED. Neither blocks the import.
func TestImportClusterSecurityGroups(t *testing.T) {
	found := terraformCluster()
	found.SecurityGroups[0].Shared = true
	found.SecurityGroups = append(found.SecurityGroups, awscloud.ClusterSecurityGroup{ID: "sg-0ffffffffffffffff", Unreadable: "access denied"})
	w := importWorld(t, found)
	c := importTF(t, w)
	if c.GetStatus().GetState() != cloudv1.ClusterState_CLUSTER_STATE_IMPORTED {
		t.Fatalf("state %s (%s)", c.GetStatus().GetState(), c.GetStatus().GetMessage())
	}
	shared := 0
	for _, l := range linesBy(c, cloudv1.Disposition_DISPOSITION_CARRIED_OVER) {
		if strings.HasPrefix(l.GetKind(), "SecurityGroup") {
			shared++
			if !strings.Contains(l.GetDetail(), "without Delete") {
				t.Errorf("%s %s of a shared group: %q, want it owned without Delete", l.GetKind(), l.GetExternalName(), l.GetDetail())
			}
		}
	}
	if left := linesBy(c, cloudv1.Disposition_DISPOSITION_NOT_IMPORTED)["SecurityGroup/sg-0ffffffffffffffff"]; shared != 3 ||
		left == nil || !strings.Contains(left.GetDetail(), "access denied") {
		t.Errorf("%d shared lines (want the group and 2 rules), unreadable group line %v", shared, left)
	}
	if _, err := w.svc.TakeOverCluster(t.Context(), &cloudv1.TakeOverClusterRequest{TenantId: worldTenant, ClusterId: c.GetId(),
		AcknowledgeNotImported: true}); err != nil {
		t.Fatal(err)
	}
	reconcile(t, w, c.GetId())
	for name, u := range objectsOf(t, w, c.GetId()) {
		if strings.HasPrefix(u.GetKind(), "SecurityGroup") && (slices.Contains(policies(u), "Delete") || slices.Contains(policies(u), "*")) {
			t.Errorf("%s of a shared group can be deleted: %v", name, policies(u))
		}
	}
}

func TestImportClusterBlocked(t *testing.T) {
	for name, tt := range map[string]struct {
		change func(*awscloud.Cluster)
		want   string
	}{
		"no such node group": {func(c *awscloud.Cluster) { c.NodeGroups = c.NodeGroups[1:] }, "no node group"},
		"subnet outside the network": {func(c *awscloud.Cluster) { c.SubnetIDs = []string{"subnet-a", "subnet-zzz"} },
			"not a private subnet of network"},
		"node group in other subnets": {func(c *awscloud.Cluster) { c.NodeGroups[0].SubnetIDs = []string{"subnet-a"} }, "EKS cannot change subnetIds"},
		"not active":                  {func(c *awscloud.Cluster) { c.Status = "UPDATING" }, "must be ACTIVE"},
		"old AMI type":                {func(c *awscloud.Cluster) { c.NodeGroups[0].AMIType = "AL2_x86_64" }, "AMI type AL2_x86_64"},
		"role not readable":           {func(c *awscloud.Cluster) { c.Role.Unreadable = "access denied" }, "role cannot be read"},
	} {
		t.Run(name, func(t *testing.T) {
			found := terraformCluster()
			tt.change(found)
			w := importWorld(t, found)
			c := importTF(t, w)
			st := c.GetStatus()
			if st.GetState() != cloudv1.ClusterState_CLUSTER_STATE_FAILED || st.GetImport().GetPhase() != cloudv1.ImportPhase_IMPORT_PHASE_BLOCKED ||
				!strings.Contains(st.GetMessage(), tt.want) {
				t.Errorf("state %s, phase %s, message %q; want FAILED / BLOCKED / %q", st.GetState(), st.GetImport().GetPhase(), st.GetMessage(), tt.want)
			}
			if n := len(objectsOf(t, w, c.GetId())); n != 0 {
				t.Errorf("%d objects created for a blocked import", n)
			}
			if _, err := w.svc.TakeOverCluster(t.Context(), &cloudv1.TakeOverClusterRequest{TenantId: worldTenant, ClusterId: c.GetId(),
				AcknowledgeNotImported: true}); connect.CodeOf(err) != connect.CodeFailedPrecondition {
				t.Errorf("takeover of a blocked import = %v, want FAILED_PRECONDITION", err)
			}
		})
	}
}
