package cluster

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	awscwl "github.com/upbound/provider-aws/v2/apis/namespaced/cloudwatchlogs/v1beta1"
	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	awsiam "github.com/upbound/provider-aws/v2/apis/namespaced/iam/v1beta1"
	awskms "github.com/upbound/provider-aws/v2/apis/namespaced/kms/v1beta1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corecluster "github.com/yindia/cluster-vending/pkg/cluster"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

const (
	existingKey = "arn:aws:kms:us-east-1:444455556666:key/1234abcd-12ab-34cd-56ef-1234567890ab"
	viewPolicy  = "arn:aws:eks::aws:cluster-access-policy/AmazonEKSViewPolicy"
	devRole     = "arn:aws:iam::444455556666:role/dev"
)

// newCluster turns a world's cluster into one that was just stored and
// never applied, with change applied to its spec.
func newCluster(t *testing.T, w world, change func(*cloudv1.ClusterSpec)) {
	t.Helper()
	ctx := t.Context()
	prefix := crossplaneaws.ClusterPrefix(worldTenant, w.row.ID, w.row.Name)
	if err := w.kube.Delete(ctx, &awseks.Cluster{ObjectMeta: objectMeta(prefix)}); err != nil {
		t.Fatal(err)
	}
	if err := w.store.ManagedResources().DeleteByOwner(ctx, model.OwnerCluster, w.row.ID); err != nil {
		t.Fatal(err)
	}
	row := w.cluster(t)
	spec, err := specOf(row)
	if err != nil {
		t.Fatal(err)
	}
	change(spec)
	if err := corecluster.ResolveOptions(spec); err != nil {
		t.Fatal(err)
	}
	if row.Spec, err = encode(spec); err != nil {
		t.Fatal(err)
	}
	row.State, row.Generation, row.AppliedGeneration = stateProvisioning, 1, 0
	if err := w.store.Clusters().Update(ctx, row); err != nil {
		t.Fatal(err)
	}
}

// markReady sets Ready and Synced on one object, as its provider would.
func markReady(t *testing.T, w world, obj client.Object, name string) {
	t.Helper()
	gvks, _, err := w.kube.Scheme().ObjectKinds(obj)
	if err != nil {
		t.Fatal(err)
	}
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvks[0])
	if err := w.kube.Get(t.Context(), client.ObjectKey{Namespace: worldNS, Name: name}, u); err != nil {
		t.Fatal(err)
	}
	conds := []any{map[string]any{"type": "Ready", "status": "True"},
		map[string]any{"type": "Synced", "status": "True", "observedGeneration": u.GetGeneration()}}
	if err := unstructured.SetNestedSlice(u.Object, conds, "status", "conditions"); err != nil {
		t.Fatal(err)
	}
	if err := w.kube.Update(t.Context(), u); err != nil {
		t.Fatal(err)
	}
}

// The log group and the encryption key come before the EKS cluster: it is
// applied only once the log group is Ready and the key has an ARN, and
// then encrypts with that key.
func TestLogGroupAndKeyStageBeforeTheCluster(t *testing.T) {
	resumeAfter, stageAfter = 0, 0
	t.Cleanup(func() { resumeAfter, stageAfter = 2*time.Minute, 15*time.Second })
	w := newWorld(t, interceptor.Funcs{}, nil)
	ctx := t.Context()
	prefix := crossplaneaws.ClusterPrefix(worldTenant, w.row.ID, w.row.Name)
	newCluster(t, w, func(s *cloudv1.ClusterSpec) {
		s.GetAws().ControlPlaneLogGroup = &cloudv1.AwsControlPlaneLogGroup{Enabled: ptr.To(true), RetentionDays: 30}
		s.ControlPlane.SecretsEncryption = &cloudv1.SecretsEncryption{Enabled: true}
	})
	reconcile := func() {
		t.Helper()
		if err := w.svc.Reconcile(ctx, worldTenant, w.row.ID); err != nil {
			t.Fatal(err)
		}
	}
	exists := func(obj client.Object, name string) bool {
		t.Helper()
		err := w.kube.Get(ctx, client.ObjectKey{Namespace: worldNS, Name: name}, obj)
		if client.IgnoreNotFound(err) != nil {
			t.Fatal(err)
		}
		return err == nil
	}

	reconcile()
	group, key := &awscwl.Group{}, &awskms.Key{}
	if !exists(group, prefix+"-logs") || !exists(key, prefix+"-secrets-key") || !exists(&awskms.Alias{}, prefix+"-secrets-alias") ||
		!exists(&awsiam.Role{}, prefix+"-cluster-role") {
		t.Fatal("the stage before the cluster (log group, key, alias, roles) was not applied")
	}
	if group.Annotations[crossplaneaws.ExternalNameAnnotation] != "/aws/eks/prod/cluster" || *group.Spec.ForProvider.RetentionInDays != 30 {
		t.Errorf("log group = %v %+v", group.Annotations, group.Spec.ForProvider)
	}
	if exists(&awseks.Cluster{}, prefix) || exists(&awseks.NodeGroup{}, prefix+"-default") {
		t.Fatal("the EKS cluster was applied before its log group and key exist")
	}

	// The log group is Ready, the key has no ARN yet: still held.
	markReady(t, w, &awscwl.Group{}, prefix+"-logs")
	reconcile()
	if exists(&awseks.Cluster{}, prefix) {
		t.Fatal("the EKS cluster was applied before its key has an ARN")
	}
	if !exists(key, prefix+"-secrets-key") { // as it is now: every pass applies it again
		t.Fatal("the key is gone")
	}
	key.Status.AtProvider.Arn = ptr.To(existingKey)
	if err := w.kube.Update(ctx, key); err != nil {
		t.Fatal(err)
	}
	reconcile()
	eks := &awseks.Cluster{}
	if !exists(eks, prefix) || !exists(&awseks.NodeGroup{}, prefix+"-default") {
		t.Fatal("the EKS cluster was not applied once its log group and key exist")
	}
	if enc := eks.Spec.ForProvider.EncryptionConfig; enc == nil || *enc.Provider.KeyArn != existingKey {
		t.Errorf("encryption config = %+v, want the rendered key's ARN", enc)
	}
	if got := w.cluster(t); got.AppliedGeneration != 0 {
		t.Errorf("applied generation %d: add-ons are still held back until EKS is ACTIVE", got.AppliedGeneration)
	}
}

func updateRequest(w world, spec *cloudv1.ClusterSpec, paths ...string) *cloudv1.UpdateClusterRequest {
	return &cloudv1.UpdateClusterRequest{TenantId: worldTenant, Cluster: &cloudv1.Cluster{Id: w.row.ID, Spec: spec},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: paths}}
}

// Encryption can be turned on for an existing cluster (in place), never
// off and never moved to another key.
func TestSecretsEncryptionCannotBeRemoved(t *testing.T) {
	w := appliedWorld(t)
	ctx := t.Context()
	prefix := crossplaneaws.ClusterPrefix(worldTenant, w.row.ID, w.row.Name)
	on := &cloudv1.ClusterSpec{ControlPlane: &cloudv1.ControlPlaneSpec{SecretsEncryption: &cloudv1.SecretsEncryption{KmsKeyArn: existingKey}}}

	plan := proto.CloneOf(updateRequest(w, on, pathSecretsEncryption))
	plan.ValidateOnly = true
	planned, err := w.svc.UpdateCluster(ctx, plan)
	if err != nil {
		t.Fatal(err)
	}
	if got := changesOf(planned.GetPlan(), "Cluster", cloudv1.PlannedAction_PLANNED_ACTION_UPDATE_IN_PLACE); len(got) == 0 ||
		got[0].GetSpecPath() != pathSecretsEncryption || !planned.GetPlan().GetApplicable() {
		t.Fatalf("plan to turn encryption on = %v, want it in place", planned.GetPlan().GetChanges())
	}
	if _, err := w.svc.UpdateCluster(ctx, updateRequest(w, on, pathSecretsEncryption)); err != nil {
		t.Fatalf("turn encryption on: %v", err)
	}
	eks := &awseks.Cluster{}
	if err := w.kube.Get(ctx, client.ObjectKey{Namespace: worldNS, Name: prefix}, eks); err != nil {
		t.Fatal(err)
	}
	if enc := eks.Spec.ForProvider.EncryptionConfig; enc == nil || *enc.Provider.KeyArn != existingKey {
		t.Fatalf("encryption config = %+v", enc)
	}
	// No key is rendered for an existing one.
	if err := w.kube.Get(ctx, client.ObjectKey{Namespace: worldNS, Name: prefix + "-secrets-key"}, &awskms.Key{}); err == nil {
		t.Error("a KMS key was rendered although the spec names an existing one")
	}

	for name, spec := range map[string]*cloudv1.ClusterSpec{
		"off":         {ControlPlane: &cloudv1.ControlPlaneSpec{SecretsEncryption: &cloudv1.SecretsEncryption{}}},
		"another key": {ControlPlane: &cloudv1.ControlPlaneSpec{SecretsEncryption: &cloudv1.SecretsEncryption{KmsKeyArn: strings.Replace(existingKey, "1234abcd", "9999abcd", 1)}}},
		"a new key":   {ControlPlane: &cloudv1.ControlPlaneSpec{SecretsEncryption: &cloudv1.SecretsEncryption{Enabled: true}}},
	} {
		if _, err := w.svc.UpdateCluster(ctx, updateRequest(w, spec, pathSecretsEncryption)); connect.CodeOf(err) != connect.CodeFailedPrecondition {
			t.Errorf("%s: %v, want FAILED_PRECONDITION", name, err)
		}
	}
	off := updateRequest(w, &cloudv1.ClusterSpec{ControlPlane: &cloudv1.ControlPlaneSpec{SecretsEncryption: &cloudv1.SecretsEncryption{}}}, pathSecretsEncryption)
	off.ValidateOnly = true
	planned, err = w.svc.UpdateCluster(ctx, off)
	if err != nil {
		t.Fatal(err)
	}
	rejected := changesOf(planned.GetPlan(), "Cluster", cloudv1.PlannedAction_PLANNED_ACTION_REJECTED_IMMUTABLE)
	if len(rejected) == 0 || rejected[0].GetSpecPath() != pathSecretsEncryption || planned.GetPlan().GetApplicable() {
		t.Errorf("plan to turn encryption off = %v (applicable %t), want REJECTED_IMMUTABLE", planned.GetPlan().GetChanges(),
			planned.GetPlan().GetApplicable())
	}
	if err := w.kube.Get(ctx, client.ObjectKey{Namespace: worldNS, Name: prefix}, eks); err != nil || eks.Spec.ForProvider.EncryptionConfig == nil {
		t.Errorf("the refused updates changed the cluster: %+v, %v", eks.Spec.ForProvider.EncryptionConfig, err)
	}
}

// Access entries and their policies are added, changed and removed as
// objects of their own: the EKS cluster is not touched.
func TestAccessEntriesChangeInPlace(t *testing.T) {
	w := appliedWorld(t)
	ctx := t.Context()
	prefix := crossplaneaws.ClusterPrefix(worldTenant, w.row.ID, w.row.Name)
	const path = "spec.access.access_entries"
	entries := func(policies ...*cloudv1.AccessPolicy) *cloudv1.ClusterSpec {
		return &cloudv1.ClusterSpec{Access: &cloudv1.AccessSpec{AccessEntries: []*cloudv1.AccessEntry{{PrincipalArn: devRole, Policies: policies}}}}
	}
	list := func() (entryNames, policyNames []string) {
		t.Helper()
		e, p := &awseks.AccessEntryList{}, &awseks.AccessPolicyAssociationList{}
		if err := w.kube.List(ctx, e, client.InNamespace(worldNS)); err != nil {
			t.Fatal(err)
		}
		if err := w.kube.List(ctx, p, client.InNamespace(worldNS)); err != nil {
			t.Fatal(err)
		}
		for _, i := range e.Items {
			if strings.Contains(i.Name, "-access-") {
				entryNames = append(entryNames, i.Name)
			}
		}
		for _, i := range p.Items {
			if strings.Contains(i.Name, "-access-") {
				policyNames = append(policyNames, i.Name+"="+*i.Spec.ForProvider.AccessScope.Type)
			}
		}
		return entryNames, policyNames
	}
	eksSpec := func() string {
		t.Helper()
		eks := &awseks.Cluster{}
		if err := w.kube.Get(ctx, client.ObjectKey{Namespace: worldNS, Name: prefix}, eks); err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(eks.Spec)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	before := eksSpec()

	// Add an entry with a cluster-wide policy.
	if _, err := w.svc.UpdateCluster(ctx, updateRequest(w, entries(&cloudv1.AccessPolicy{PolicyArn: viewPolicy}), path)); err != nil {
		t.Fatalf("add: %v", err)
	}
	e, p := list()
	if len(e) != 1 || len(p) != 1 || !strings.HasSuffix(p[0], "=cluster") {
		t.Fatalf("after add: entries %v, policies %v", e, p)
	}
	entry, clusterWide := e[0], p[0]

	// Narrow the policy to a namespace: planned as a new association and
	// the old one deleted, nothing on the cluster, applicable.
	scoped := entries(&cloudv1.AccessPolicy{PolicyArn: viewPolicy, Scope: cloudv1.AccessPolicyScope_ACCESS_POLICY_SCOPE_NAMESPACE, Namespaces: []string{"team-a"}})
	plan := updateRequest(w, scoped, path)
	plan.ValidateOnly = true
	planned, err := w.svc.UpdateCluster(ctx, plan)
	if err != nil {
		t.Fatal(err)
	}
	pl := planned.GetPlan()
	if len(changesOf(pl, "AccessPolicyAssociation", cloudv1.PlannedAction_PLANNED_ACTION_CREATE)) != 1 ||
		len(changesOf(pl, "AccessPolicyAssociation", cloudv1.PlannedAction_PLANNED_ACTION_DELETE)) != 1 ||
		len(changesOf(pl, "AccessPolicyAssociation", cloudv1.PlannedAction_PLANNED_ACTION_REJECTED_IMMUTABLE)) != 0 || !pl.GetApplicable() {
		t.Errorf("plan of a scope change = %v (applicable %t), want one association created and one deleted", pl.GetChanges(), pl.GetApplicable())
	}
	for _, c := range pl.GetChanges() {
		if c.GetKind() == "Cluster" || c.GetKind() == "NodeGroup" {
			t.Errorf("an access entry change plans %s %s: %s", c.GetKind(), c.GetName(), c.GetDetail())
		}
	}
	if _, err := w.svc.UpdateCluster(ctx, updateRequest(w, scoped, path)); err != nil {
		t.Fatalf("change: %v", err)
	}
	e, p = list()
	if len(e) != 1 || e[0] != entry || len(p) != 1 || p[0] == clusterWide || !strings.HasSuffix(p[0], "=namespace") {
		t.Errorf("after the scope change: entries %v, policies %v; want the same entry and only the namespaced association", e, p)
	}

	// Remove the entry.
	if _, err := w.svc.UpdateCluster(ctx, updateRequest(w, &cloudv1.ClusterSpec{Access: &cloudv1.AccessSpec{}}, path)); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if e, p = list(); len(e) != 0 || len(p) != 0 {
		t.Errorf("after remove: entries %v, policies %v", e, p)
	}
	if after := eksSpec(); after != before {
		t.Errorf("the EKS cluster's spec changed for access entry changes:\n%s\n%s", before, after)
	}

	// A principal cannot be an admin and an entry.
	dup := &cloudv1.ClusterSpec{Access: &cloudv1.AccessSpec{AdminPrincipals: []string{devRole},
		AccessEntries: []*cloudv1.AccessEntry{{PrincipalArn: devRole}}}}
	if _, err := w.svc.UpdateCluster(ctx, updateRequest(w, dup, path, "spec.access.admin_principals")); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("admin and entry for one principal: %v, want INVALID_ARGUMENT", err)
	}
}

func objectMeta(name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Namespace: worldNS, Name: name}
}

// IRSA can be turned on for a cluster that exists (the OIDC provider is
// created with the cluster's issuer) and off again (it is deleted).
func TestUpdateIRSA(t *testing.T) {
	w := appliedWorld(t)
	ctx := t.Context()
	prefix := crossplaneaws.ClusterPrefix(worldTenant, w.row.ID, w.row.Name)
	eks := &awseks.Cluster{}
	if err := w.kube.Get(ctx, client.ObjectKey{Namespace: worldNS, Name: prefix}, eks); err != nil {
		t.Fatal(err)
	}
	eks.Status.AtProvider.Identity = []awseks.ClusterIdentityObservation{{Oidc: []awseks.OidcObservation{{Issuer: ptr.To("https://oidc.eks.us-east-1.amazonaws.com/id/ABC")}}}}
	if err := w.kube.Update(ctx, eks); err != nil {
		t.Fatal(err)
	}
	irsa := func(on bool) {
		t.Helper()
		spec := &cloudv1.ClusterSpec{Provider: &cloudv1.ClusterSpec_Aws{Aws: &cloudv1.AwsClusterSpec{Irsa: ptr.To(on)}}}
		if _, err := w.svc.UpdateCluster(ctx, updateRequest(w, spec, "spec.aws.irsa")); err != nil {
			t.Fatalf("irsa %t: %v", on, err)
		}
	}
	key := client.ObjectKey{Namespace: worldNS, Name: prefix + "-oidc"}
	irsa(true)
	provider := &awsiam.OpenIDConnectProvider{}
	if err := w.kube.Get(ctx, key, provider); err != nil || *provider.Spec.ForProvider.URL != "https://oidc.eks.us-east-1.amazonaws.com/id/ABC" {
		t.Fatalf("OIDC provider after turning IRSA on: %+v, %v", provider.Spec.ForProvider, err)
	}
	irsa(false)
	if err := w.kube.Get(ctx, key, provider); client.IgnoreNotFound(err) != nil || err == nil {
		t.Errorf("OIDC provider after turning IRSA off: %v, want it deleted", err)
	}
}
