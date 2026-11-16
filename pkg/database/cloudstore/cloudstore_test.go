package cloudstore_test

import (
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	"github.com/yindia/cluster-vending/pkg/database/storetest"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

const namespace = "tenant-acme"

// newClient is a fake API server that knows the kinds and their status
// subresources, as the real one does.
func newClient(t *testing.T) client.Client { return storetest.NewClient(t) }

func newStore(t *testing.T) repository.Store { return cloudstore.New(newClient(t)) }

func document(t *testing.T, m proto.Message) model.JSON {
	t.Helper()
	raw, err := protojson.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return model.JSON(raw)
}

// networkSpec is a contract spec with everything a network can say, for
// the row's account and region.
func networkSpec(n *model.Network) *cloudv1.NetworkSpec {
	return &cloudv1.NetworkSpec{Tier: cloudv1.Tier_TIER_0, Region: n.Region, BootstrapId: n.CloudAccountID,
		Layout: &cloudv1.NetworkLayout{CidrBlock: "10.0.0.0/16", NatGatewayMode: cloudv1.NatGatewayMode_NAT_GATEWAY_MODE_PER_ZONE,
			Subnets: []*cloudv1.Subnet{
				{Role: cloudv1.SubnetRole_SUBNET_ROLE_PUBLIC, CidrBlock: "10.0.224.0/22", ZoneId: "use1-az1"},
				{Role: cloudv1.SubnetRole_SUBNET_ROLE_PRIVATE, CidrBlock: "10.0.0.0/19", ZoneId: "use1-az1"},
			}},
		FlowLogs: &cloudv1.NetworkFlowLogs{Enabled: ptr.To(true), TrafficType: cloudv1.FlowLogTrafficType_FLOW_LOG_TRAFFIC_TYPE_REJECT,
			RetentionDays: 90, AggregationIntervalSeconds: 60}}
}

// clusterSpec is a contract spec with a setting of every part of a
// cluster, for the row's network and region.
func clusterSpec(c *model.Cluster) *cloudv1.ClusterSpec {
	return &cloudv1.ClusterSpec{Region: c.Region,
		Network: &cloudv1.ClusterNetwork{NetworkId: c.NetworkID, ZoneIds: []string{"use1-az1", "use1-az2"}, ServiceCidr: "172.20.0.0/16"},
		ControlPlane: &cloudv1.ControlPlaneSpec{KubernetesVersion: "1.33",
			EndpointAccess:    &cloudv1.EndpointAccess{PublicAccessCidrs: []string{"203.0.113.0/24"}, EndpointPrivateAccess: ptr.To(false)},
			SecretsEncryption: &cloudv1.SecretsEncryption{Enabled: true}},
		DefaultNodePool: &cloudv1.NodePoolSpec{InstanceTypes: []string{"m6i.large"}, CapacityType: cloudv1.CapacityType_CAPACITY_TYPE_SPOT,
			ScalingConfig: &cloudv1.ScalingConfig{MinSize: 1, MaxSize: 3, DesiredSize: ptr.To(uint32(2))}, DiskSizeGb: 50,
			Labels: map[string]string{"pool": "default"},
			Taints: []*cloudv1.Taint{{Key: "dedicated", Value: "x", Effect: cloudv1.TaintEffect_TAINT_EFFECT_NO_SCHEDULE}}},
		Access: &cloudv1.AccessSpec{AdminPrincipals: []string{"arn:aws:iam::444455556666:role/admin"},
			AccessEntries: []*cloudv1.AccessEntry{{PrincipalArn: "arn:aws:iam::444455556666:role/dev",
				Type: cloudv1.AccessEntryType_ACCESS_ENTRY_TYPE_STANDARD, Policies: []*cloudv1.AccessPolicy{{
					PolicyArn: "arn:aws:eks::aws:cluster-access-policy/AmazonEKSViewPolicy",
					Scope:     cloudv1.AccessPolicyScope_ACCESS_POLICY_SCOPE_NAMESPACE, Namespaces: []string{"dev"}}}}}},
		ReleaseChannel: cloudv1.ReleaseChannel_RELEASE_CHANNEL_BETA,
		Provider: &cloudv1.ClusterSpec_Aws{Aws: &cloudv1.AwsClusterSpec{
			AccessConfig:           &cloudv1.AwsAccessConfig{AuthenticationMode: cloudv1.AwsAuthenticationMode_AWS_AUTHENTICATION_MODE_API},
			EnabledClusterLogTypes: []cloudv1.AwsClusterLogType{cloudv1.AwsClusterLogType_AWS_CLUSTER_LOG_TYPE_AUDIT},
			UpgradePolicy:          &cloudv1.AwsUpgradePolicy{SupportType: cloudv1.AwsSupportType_AWS_SUPPORT_TYPE_STANDARD},
			DefaultNodePool:        &cloudv1.AwsNodePoolSpec{AmiType: cloudv1.AwsAmiType_AWS_AMI_TYPE_AL2023_ARM_64_STANDARD},
			Addons:                 []*cloudv1.AwsAddon{{Name: "coredns", Version: "v1.11.4-eksbuild.2"}},
			ControlPlaneLogGroup:   &cloudv1.AwsControlPlaneLogGroup{Enabled: ptr.To(true), RetentionDays: 90},
			Irsa:                   ptr.To(true), AdditionalSecurityGroupIds: []string{"sg-0123456789abcdef0"},
		}}}
}

// The store on the cloud.cluster-vending.io kinds behaves as a store must.
func TestStore(t *testing.T) {
	storetest.Run(t, storetest.Backend{
		New:         newStore,
		NetworkSpec: func(n *model.Network) model.JSON { return document(t, networkSpec(n)) },
		ClusterSpec: func(c *model.Cluster) model.JSON { return document(t, clusterSpec(c)) },
	})
}

// world is a tenant with an account and a network in a store.
type world struct {
	k       client.Client
	s       repository.Store
	account string
	network *model.Network
}

func newWorld(t *testing.T) world {
	t.Helper()
	k := newClient(t)
	s := cloudstore.New(k)
	storetest.MustTenant(t, s, "acme")
	n := &model.Network{TenantID: "acme", Name: "shared", Provider: "aws", Region: "us-east-1", CloudAccountID: storetest.MustAccount(t, s, "acme"),
		Resource: model.Resource{DisplayName: "Shared", State: "NETWORK_STATE_PROVISIONING", Labels: model.Labels{"team": "net"}}}
	n.Spec = document(t, networkSpec(n))
	if err := s.Networks().Create(t.Context(), n); err != nil {
		t.Fatal(err)
	}
	return world{k: k, s: s, account: n.CloudAccountID, network: n}
}

func (w world) cluster(t *testing.T, name string) *model.Cluster {
	t.Helper()
	c := &model.Cluster{TenantID: "acme", Name: name, NetworkID: w.network.ID, Provider: "aws", Region: "us-east-1",
		Resource: model.Resource{State: "CLUSTER_STATE_PROVISIONING"}}
	c.Spec = document(t, clusterSpec(c))
	if err := w.s.Clusters().Create(t.Context(), c); err != nil {
		t.Fatal(err)
	}
	return c
}

// A row is an object of its kind in the tenant's namespace: named after
// the contract name, found by its row ID, with a typed spec, and owned
// along the chain account, network, cluster.
func TestRowsAreObjectsOfTheKinds(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	c := w.cluster(t, "prod")

	account := &cloudv1alpha1.CloudAccount{}
	if err := w.k.Get(ctx, client.ObjectKey{Namespace: namespace, Name: "aws-444455556666"}, account); err != nil {
		t.Fatal(err)
	}
	if account.Spec.AWS.AccountID != "444455556666" || account.Labels[cloudstore.LabelID] != w.account {
		t.Errorf("account = %+v, labels %v", account.Spec, account.Labels)
	}
	network := &cloudv1alpha1.Network{}
	if err := w.k.Get(ctx, client.ObjectKey{Namespace: namespace, Name: "shared"}, network); err != nil {
		t.Fatal(err)
	}
	spec := network.Spec
	if spec.AccountRef.Name != account.Name || spec.Region != "us-east-1" || spec.DisplayName != "Shared" || spec.Tags["team"] != "net" ||
		spec.Layout == nil || spec.Layout.NATGatewayMode != cloudv1alpha1.NATGatewayModePerZone || len(spec.Layout.Subnets) != 2 ||
		spec.FlowLogs.TrafficType != cloudv1alpha1.FlowLogTrafficTypeReject || spec.ImportFrom != nil {
		t.Errorf("network spec = %+v", spec)
	}
	if network.Labels[cloudstore.LabelID] != w.network.ID || network.Labels[cloudv1alpha1.LabelAccount] != account.Name ||
		!metav1.IsControlledBy(network, account) {
		t.Errorf("network labels %v, owners %v: want its row ID and its account as owner", network.Labels, network.OwnerReferences)
	}
	if network.Status.Phase != cloudv1alpha1.NetworkPhaseProvisioning || network.Status.Layout == nil {
		t.Errorf("network status = %+v, want the phase of its state and the layout", network.Status)
	}
	cluster := &cloudv1alpha1.Cluster{}
	if err := w.k.Get(ctx, client.ObjectKey{Namespace: namespace, Name: "prod"}, cluster); err != nil {
		t.Fatal(err)
	}
	if cs := cluster.Spec; cs.Network.NetworkRef.Name != "shared" ||
		cs.ReleaseChannel != cloudv1alpha1.ReleaseChannelBeta || cs.DefaultNodePool.Scaling.MaxSize != 3 || cs.AWS == nil {
		t.Errorf("cluster spec = %+v", cs)
	}
	if !metav1.IsControlledBy(cluster, network) || cluster.Labels[cloudstore.LabelNetworkID] != w.network.ID ||
		cluster.Labels[cloudv1alpha1.LabelNetwork] != "shared" || c.CloudAccountID != w.account {
		t.Errorf("cluster labels %v, owners %v, account %q", cluster.Labels, cluster.OwnerReferences, c.CloudAccountID)
	}
}

// The typed spec is the desired state: what someone edits there is what
// the next read of the row returns.
func TestAnEditOfTheTypedSpecIsRead(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	c := w.cluster(t, "prod")
	obj := &cloudv1alpha1.Cluster{}
	if err := w.k.Get(ctx, client.ObjectKey{Namespace: namespace, Name: "prod"}, obj); err != nil {
		t.Fatal(err)
	}
	obj.Spec.DefaultNodePool.Scaling.MaxSize, obj.Spec.Tags = 9, cloudv1alpha1.Tags{"cost": "x"}
	if err := w.k.Update(ctx, obj); err != nil {
		t.Fatal(err)
	}
	row, err := w.s.Clusters().Get(ctx, "acme", c.ID)
	if err != nil {
		t.Fatal(err)
	}
	spec := &cloudv1.ClusterSpec{}
	if err := protojson.Unmarshal(row.Spec, spec); err != nil {
		t.Fatal(err)
	}
	if spec.GetDefaultNodePool().GetScalingConfig().GetMaxSize() != 9 ||
		row.Labels["cost"] != "x" || row.ETag != c.ETag {
		t.Errorf("row after the edit: max size %d, labels %v", spec.GetDefaultNodePool().GetScalingConfig().GetMaxSize(), row.Labels)
	}
}

// An imported network says so in its spec, keeps the layout read from the
// cloud outside the typed spec (it is not one the platform builds), and
// gives both back; a takeover shows as spec.importFrom.takeOver.
func TestImportedNetwork(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	vpc := "vpc-0123456789abcdef0"
	request := &cloudv1.ImportNetworkRequest{TenantId: "acme", Name: "legacy", Region: "us-east-1", BootstrapId: w.account,
		Provider: &cloudv1.ImportNetworkRequest_Aws{Aws: &cloudv1.AwsImportNetwork{VpcId: vpc, SubnetIds: []string{"subnet-0123456789abcdef0"}}}}
	// A layout the kind's own rules would refuse: no private subnet.
	discovered := &cloudv1.NetworkSpec{Tier: cloudv1.Tier_TIER_0, Region: "us-east-1", BootstrapId: w.account, Layout: &cloudv1.NetworkLayout{CidrBlock: "172.31.0.0/16",
		NatGatewayMode: cloudv1.NatGatewayMode_NAT_GATEWAY_MODE_SINGLE,
		Subnets:        []*cloudv1.Subnet{{Role: cloudv1.SubnetRole_SUBNET_ROLE_PUBLIC, CidrBlock: "172.31.0.0/20", ZoneId: "use1-az1"}}}}
	plan := &cloudv1.NetworkStatus{Import: &cloudv1.ImportStatus{Phase: cloudv1.ImportPhase_IMPORT_PHASE_PLANNED,
		Resources: []*cloudv1.ImportedResource{{Kind: "VPC", ExternalName: vpc, Disposition: cloudv1.Disposition_DISPOSITION_MATCHES}}}}
	n := &model.Network{TenantID: "acme", Name: "legacy", Provider: "aws", Region: "us-east-1", CloudAccountID: w.account, ExternalID: &vpc,
		Input: document(t, request), Resource: model.Resource{State: "NETWORK_STATE_IMPORTED", Spec: document(t, discovered), Status: document(t, plan)}}
	if err := w.s.Networks().Create(ctx, n); err != nil {
		t.Fatal(err)
	}

	obj := &cloudv1alpha1.Network{}
	if err := w.k.Get(ctx, client.ObjectKey{Namespace: namespace, Name: "legacy"}, obj); err != nil {
		t.Fatal(err)
	}
	imp := obj.Spec.ImportFrom
	if obj.Spec.Layout != nil || imp == nil || imp.AWS.VPCID != vpc || len(imp.AWS.SubnetIDs) != 1 || imp.TakeOver {
		t.Fatalf("spec of an imported network = %+v", obj.Spec)
	}
	if obj.Status.Phase != cloudv1alpha1.NetworkPhaseImported || obj.Status.ImportPlan == nil ||
		obj.Status.ImportPlan.Phase != cloudv1alpha1.ImportPhasePlanned || obj.Status.Layout != nil {
		t.Errorf("status = %+v, want the plan, and no layout the kind cannot hold", obj.Status)
	}
	row, err := w.s.Networks().Get(ctx, "acme", n.ID)
	if err != nil {
		t.Fatal(err)
	}
	spec, status := &cloudv1.NetworkSpec{}, &cloudv1.NetworkStatus{}
	if err := errors.Join(protojson.Unmarshal(row.Spec, spec), protojson.Unmarshal(row.Status, status)); err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(spec, discovered) || !proto.Equal(status, plan) || row.ExternalID == nil || *row.ExternalID != vpc || len(row.Input) == 0 {
		t.Errorf("row of the imported network: spec %v, status %v, external ID %v", spec, status, row.ExternalID)
	}

	row.State = "NETWORK_STATE_PROVISIONING" // taken over
	if err := w.s.Networks().Update(ctx, row); err != nil {
		t.Fatal(err)
	}
	if err := w.k.Get(ctx, client.ObjectKey{Namespace: namespace, Name: "legacy"}, obj); err != nil {
		t.Fatal(err)
	}
	if !obj.Spec.ImportFrom.TakeOver || obj.Status.Phase != cloudv1alpha1.NetworkPhaseProvisioning {
		t.Errorf("after the takeover: importFrom %+v, phase %s", obj.Spec.ImportFrom, obj.Status.Phase)
	}

	// An update of the row rewrites its input as a network, without the
	// import request: the network is still an imported one.
	row.Input, row.Labels = document(t, &cloudv1.Network{Name: "legacy", Labels: map[string]string{"team": "net"}}), model.Labels{"team": "net"}
	if err := w.s.Networks().Update(ctx, row); err != nil {
		t.Fatal(err)
	}
	if err := w.k.Get(ctx, client.ObjectKey{Namespace: namespace, Name: "legacy"}, obj); err != nil {
		t.Fatal(err)
	}
	again, err := w.s.Networks().Get(ctx, "acme", n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := protojson.Unmarshal(again.Spec, spec); err != nil {
		t.Fatal(err)
	}
	if imp := obj.Spec.ImportFrom; imp == nil || imp.AWS.VPCID != vpc || !imp.TakeOver || obj.Spec.Layout != nil ||
		obj.Spec.Tags["team"] != "net" || !proto.Equal(spec, discovered) {
		t.Errorf("after an update: spec %+v, row spec %v; want it still imported, with its layout", obj.Spec, spec)
	}
}

// A status is kept verbatim and read into the typed status; the records
// of the owner's objects, which share the status, survive it, and so does
// the status a record change.
func TestStatusAndInventoryShareTheStatus(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	record := model.ManagedResource{TenantID: "acme", CloudAccountID: w.account, OwnerKind: model.OwnerNetwork, OwnerID: w.network.ID,
		APIVersion: "ec2.aws.m.upbound.io/v1beta1", Kind: "VPC", Namespace: namespace, Name: "shared-vpc"}
	if err := w.s.ManagedResources().Put(ctx, []model.ManagedResource{record}); err != nil {
		t.Fatal(err)
	}
	status := document(t, &cloudv1.NetworkStatus{State: cloudv1.NetworkState_NETWORK_STATE_READY,
		Conditions: []*cloudv1.Condition{{Type: "Ready", Status: cloudv1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "AllResourcesReady"}},
		Resources:  &cloudv1.NetworkResources{CloudNetworkId: "vpc-1", PrivateSubnetIds: []string{"subnet-1"}}, ClusterIds: []string{"prod"}})
	if err := w.s.Networks().SetStatus(ctx, "acme", w.network.ID, w.network.ETag, status); err != nil {
		t.Fatal(err)
	}
	rows, err := w.s.ManagedResources().ListByOwner(ctx, model.OwnerNetwork, w.network.ID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("%d records after a status write (%v)", len(rows), err)
	}
	if err := w.s.ManagedResources().SetExternalName(ctx, rows[0].ID, "vpc-1"); err != nil {
		t.Fatal(err)
	}

	obj := &cloudv1alpha1.Network{}
	if err := w.k.Get(ctx, client.ObjectKey{Namespace: namespace, Name: "shared"}, obj); err != nil {
		t.Fatal(err)
	}
	st := obj.Status
	if st.Phase != cloudv1alpha1.NetworkPhaseReady || len(st.Conditions) != 1 || st.Conditions[0].Status != metav1.ConditionTrue ||
		st.Resources.CloudNetworkID != "vpc-1" || len(st.Clusters) != 1 || st.ContractStatus == nil {
		t.Errorf("typed status = %+v", st)
	}
	if len(st.Inventory) != 1 || st.Inventory[0].ExternalName != "vpc-1" || st.Inventory[0].Kind != "VPC" || st.Inventory[0].RecordedAt == nil {
		t.Errorf("inventory = %+v", st.Inventory)
	}
	row, err := w.s.Networks().Get(ctx, "acme", w.network.ID)
	// By meaning: the API server stores a document, not its bytes.
	got, want := &cloudv1.NetworkStatus{}, &cloudv1.NetworkStatus{}
	if err := errors.Join(err, protojson.Unmarshal(row.Status, got), protojson.Unmarshal(status, want)); err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(got, want) || row.ETag != w.network.ETag {
		t.Errorf("row status %s, want the one written and the etag unchanged", row.Status)
	}
	// The same status again writes nothing.
	before := obj.ResourceVersion
	if err := w.s.Networks().SetStatus(ctx, "acme", w.network.ID, w.network.ETag, status); err != nil {
		t.Fatal(err)
	}
	if err := w.k.Get(ctx, client.ObjectKey{Namespace: namespace, Name: "shared"}, obj); err != nil || obj.ResourceVersion != before {
		t.Errorf("an unchanged status was written again (%v)", err)
	}
}

// A request for an access check is an annotation of the account; the
// controller's last check is read from the account's status.
func TestAccountAccess(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	at := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)

	if err := w.s.Tenants().RequestAccessCheck(ctx, "acme", w.account, at, []string{"eu-west-1", "ap-south-1"}); err != nil {
		t.Fatal(err)
	}
	if err := w.s.Tenants().RequestAccessCheck(ctx, "acme", "00000000-0000-0000-0000-000000000000", at, nil); !errors.Is(err, repository.ErrNotFound) {
		t.Errorf("request for an unknown account: %v, want not found", err)
	}
	list := &cloudv1alpha1.CloudAccountList{}
	if err := w.k.List(ctx, list, client.InNamespace(namespace)); err != nil || len(list.Items) != 1 {
		t.Fatalf("%d accounts (%v)", len(list.Items), err)
	}
	obj := &list.Items[0]
	checked := metav1.NewTime(at.Add(time.Minute))
	obj.Status.Access = &cloudv1alpha1.AccountAccess{Verified: false, CheckedAt: &checked, AttemptedAt: &checked,
		PrincipalARN: "arn:aws:sts::444455556666:assumed-role/r/s", Reason: "DeniedBySCP", Message: "us-west-1: denied",
		Regions: []cloudv1alpha1.AccessResult{{Region: "us-west-1", Reason: "DeniedBySCP", Message: "denied"}},
		Checks:  []cloudv1alpha1.AccessResult{{Action: "ec2:DescribeVpcs", Region: "us-east-1", OK: true}}}
	if err := w.k.Status().Update(ctx, obj); err != nil {
		t.Fatal(err)
	}

	row, err := w.s.Tenants().GetCloudAccountByID(ctx, "acme", w.account)
	if err != nil {
		t.Fatal(err)
	}
	if !row.AccessRequestedAt.Equal(at) {
		t.Errorf("requested at %v, want %v", row.AccessRequestedAt, at)
	}
	a := row.Access
	if a == nil || a.Verified || !a.CheckedAt.Equal(checked.Time) || a.Reason != "DeniedBySCP" || a.PrincipalARN == "" ||
		len(a.Regions) != 1 || a.Regions[0].Region != "us-west-1" || len(a.Checks) != 1 || !a.Checks[0].OK {
		t.Errorf("access = %+v", a)
	}
	if want := []string{"eu-west-1", "ap-south-1"}; !slices.Equal(row.AccessRequestedRegions, want) {
		t.Errorf("requested regions %v, want %v", row.AccessRequestedRegions, want)
	}

	// A request without regions keeps the ones asked for before.
	if err := w.s.Tenants().RequestAccessCheck(ctx, "acme", w.account, at.Add(time.Hour), nil); err != nil {
		t.Fatal(err)
	}
	if row, err = w.s.Tenants().GetCloudAccountByID(ctx, "acme", w.account); err != nil {
		t.Fatal(err)
	}
	if len(row.AccessRequestedRegions) != 2 || !row.AccessRequestedAt.Equal(at.Add(time.Hour)) {
		t.Errorf("after a request without regions: regions %v, at %v", row.AccessRequestedRegions, row.AccessRequestedAt)
	}
}

// An owner is not deleted while it has dependents, and a delete never
// takes dependents with it.
func TestOwnersWithDependentsStay(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	c := w.cluster(t, "prod")
	storetest.RequireErr(t, w.s.Networks().Delete(ctx, "acme", w.network.ID), repository.ErrInUse)
	storetest.RequireErr(t, w.s.Tenants().Delete(ctx, "acme"), repository.ErrInUse)

	// A finalizer of the kind does not keep a row the repository deletes.
	obj := &cloudv1alpha1.Cluster{}
	if err := w.k.Get(ctx, client.ObjectKey{Namespace: namespace, Name: "prod"}, obj); err != nil {
		t.Fatal(err)
	}
	obj.Finalizers = []string{cloudv1alpha1.ClusterFinalizer}
	if err := w.k.Update(ctx, obj); err != nil {
		t.Fatal(err)
	}
	if err := w.s.Clusters().Delete(ctx, "acme", c.ID); err != nil {
		t.Fatal(err)
	}
	if err := w.k.Get(ctx, client.ObjectKey{Namespace: namespace, Name: "prod"}, obj); client.IgnoreNotFound(err) != nil || err == nil {
		t.Errorf("the cluster object after the delete: %v, want it gone", err)
	}
	if err := w.s.Networks().Delete(ctx, "acme", w.network.ID); err != nil {
		t.Fatal(err)
	}
	if err := w.s.Tenants().Delete(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	// The namespace stays, nobody's tenant; the tenant can be made again.
	ns := &corev1.Namespace{}
	if err := w.k.Get(ctx, client.ObjectKey{Name: namespace}, ns); err != nil || ns.Labels[cloudstore.LabelTenant] != "" {
		t.Errorf("namespace after the tenant's delete: labels %v (%v)", ns.Labels, err)
	}
	accounts := &cloudv1alpha1.CloudAccountList{}
	if err := w.k.List(ctx, accounts); err != nil || len(accounts.Items) != 0 {
		t.Errorf("%d accounts left (%v)", len(accounts.Items), err)
	}
	storetest.MustTenant(t, w.s, "acme")
}

// A namespace without an ownership marker is never adopted by registration.
func TestTenantRejectsUnmarkedNamespace(t *testing.T) {
	k := newClient(t)
	ctx := t.Context()
	original := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace, Labels: map[string]string{"x": "y"}}}
	if err := k.Create(ctx, original); err != nil {
		t.Fatal(err)
	}
	s := cloudstore.New(k)
	if _, err := s.Tenants().Get(ctx, "acme"); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("unmarked namespace was a tenant: %v", err)
	}
	storetest.RequireErr(t, s.Tenants().Create(ctx, &model.Tenant{ID: "acme"}), repository.ErrConflict)
	ns := &corev1.Namespace{}
	if err := k.Get(ctx, client.ObjectKey{Name: namespace}, ns); err != nil || ns.Labels["x"] != "y" || ns.Labels[cloudstore.LabelTenant] != "" || ns.ResourceVersion != original.ResourceVersion {
		t.Fatalf("collision changed namespace: %#v (%v)", ns, err)
	}
	if err := k.Get(ctx, client.ObjectKey{Name: "acme"}, &cloudv1alpha1.Tenant{}); !apierrors.IsNotFound(err) {
		t.Fatalf("failed create left registration: %v", err)
	}
}

// Of creates that race for one value, one is stored: a name, a create
// request ID, an imported VPC.
func TestConcurrentCreatesStoreOne(t *testing.T) {
	key, vpc := "3f2b8c1e-4a5d-4e6f-9a7b-1c2d3e4f5a6b", "vpc-0123456789abcdef0"
	for name, row := range map[string]func(w world, i int) *model.Network{
		"one name": func(w world, _ int) *model.Network {
			return &model.Network{Name: "prod"}
		},
		"one request ID": func(w world, i int) *model.Network {
			return &model.Network{Name: "n" + string(rune('a'+i)), CreateRequestID: &key}
		},
		"one imported VPC": func(w world, i int) *model.Network {
			return &model.Network{Name: "n" + string(rune('a'+i)), ExternalID: &vpc}
		},
	} {
		w := newWorld(t)
		const writers = 8
		errs := make([]error, writers)
		var wg sync.WaitGroup
		for i := range writers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				n := row(w, i)
				n.TenantID, n.Provider, n.Region, n.CloudAccountID = "acme", "aws", "us-east-1", w.account
				n.Resource = model.Resource{State: "NETWORK_STATE_PROVISIONING", Spec: document(t, networkSpec(n))}
				errs[i] = w.s.Networks().Create(t.Context(), n)
			}()
		}
		wg.Wait()
		stored := 0
		for _, err := range errs {
			switch {
			case err == nil:
				stored++
			case !errors.Is(err, repository.ErrAlreadyExists):
				t.Errorf("%s: a losing create failed with %v, want ErrAlreadyExists", name, err)
			}
		}
		rows, err := w.s.Networks().List(t.Context(), "acme", repository.Page{Limit: 50})
		if stored != 1 || err != nil || len(rows) != 2 { // the world's own network and the winner
			t.Errorf("%s: %d creates succeeded, %d networks stored (%v); want 1 and 2", name, stored, len(rows), err)
		}
	}
}

// Records of one owner made at the same moment are all kept.
func TestConcurrentRecordsAreAllKept(t *testing.T) {
	w := newWorld(t)
	const writers = 8
	var wg sync.WaitGroup
	errs := make([]error, writers)
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = w.s.ManagedResources().Put(t.Context(), []model.ManagedResource{
				{TenantID: "acme", CloudAccountID: w.account, OwnerKind: model.OwnerNetwork, OwnerID: w.network.ID,
					APIVersion: "ec2.aws.m.upbound.io/v1beta1", Kind: "Subnet", Namespace: namespace, Name: "subnet-" + string(rune('a'+i))},
				{TenantID: "acme", CloudAccountID: w.account, OwnerKind: model.OwnerNetwork, OwnerID: w.network.ID,
					APIVersion: "ec2.aws.m.upbound.io/v1beta1", Kind: "VPC", Namespace: namespace, Name: "shared-vpc"},
			})
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Fatal(err)
	}
	rows, err := w.s.ManagedResources().ListByOwner(t.Context(), model.OwnerNetwork, w.network.ID)
	if err != nil || len(rows) != writers+1 {
		t.Errorf("%d records (%v), want %d subnets and the VPC once", len(rows), err, writers)
	}
}

// What a failed function created inside WithinTransaction is taken back.
func TestWithinTransactionUndoesCreates(t *testing.T) {
	k := newClient(t)
	s := cloudstore.New(k)
	boom := errors.New("boom")
	err := s.WithinTransaction(t.Context(), func(tx repository.Store) error {
		storetest.MustTenant(t, tx, "acme")
		storetest.MustAccount(t, tx, "acme")
		return boom
	})
	storetest.RequireErr(t, err, boom)
	if _, err := s.Tenants().Get(t.Context(), "acme"); !errors.Is(err, repository.ErrNotFound) {
		t.Errorf("the tenant after the failed function: %v", err)
	}
	accounts := &cloudv1alpha1.CloudAccountList{}
	if err := k.List(t.Context(), accounts); err != nil || len(accounts.Items) != 0 {
		t.Errorf("%d accounts left (%v)", len(accounts.Items), err)
	}
}
