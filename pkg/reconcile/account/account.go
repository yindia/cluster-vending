// Package account makes the management cluster match a tenant's stored
// cloud account: the tenant namespace and the account's ProviderConfig,
// through which Crossplane reaches the account. The TenantService handlers
// call it today; a CloudAccount controller calls the same entry points. It
// knows no RPC library: failures a caller can act on are faults.
package account

import (
	"context"
	"log/slog"
	"strings"

	"google.golang.org/protobuf/types/known/timestamppb"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/yindia/cluster-vending/pkg/crossplane"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/crossplane/managed"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/fault"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/kube"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// Condition types of CloudAccountStatus.
const (
	// ConditionBootstrapped: the namespace and the account's ProviderConfig
	// are applied.
	ConditionBootstrapped = "Bootstrapped"
	// ConditionAccessVerified: the controller's access check passed: the
	// tenant role works in every region the account uses and allows the
	// platform's calls (VerifyAccess).
	ConditionAccessVerified = "AccessVerified"
)

// namespaceKind is the kind of the tenant namespace among an account's
// recorded objects.
const namespaceKind = "Namespace"

// Reconciler bootstraps cloud accounts in the management cluster.
type Reconciler struct{ deps reconcile.Deps }

// New returns the Reconciler.
func New(d reconcile.Deps) *Reconciler { return &Reconciler{deps: d} }

// Reconcile applies what the stored account needs in the management
// cluster (Apply). It is safe to repeat. An account that does not exist is
// an error (repository.ErrNotFound): unlike a network or cluster, an
// account is not removed by reconciling, so a missing one is the caller's
// mistake.
func (r *Reconciler) Reconcile(ctx context.Context, tenantID, cloudAccountID string) error {
	a, err := r.deps.Store.Tenants().GetCloudAccountByID(ctx, tenantID, cloudAccountID)
	if err != nil {
		return err
	}
	if a.Paused != nil {
		// Paused by an operator (AdminService.PauseReconcile): its
		// ProviderConfig, credentials copy and bindings are left as they
		// are until the resume.
		return nil
	}
	return r.Apply(ctx, a)
}

// objects are what an AWS account needs in the management cluster, in the
// order they are applied: the tenant namespace, then, with a credentials
// Secret (local runs), the account's copy of it, then the account's
// ProviderConfig (it assumes the tenant role, a.Identity, with the
// providers' IRSA credentials or the keys in that copy).
func objects(a *model.CloudAccount, keys *corev1.Secret, direct bool) []client.Object {
	objs := []client.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: a.Namespace, Labels: crossplane.Labels(a.TenantID)}},
	}
	var ref *crossplaneaws.CredentialsSecret
	if keys != nil {
		objs = append(objs, keys)
		ref = &crossplaneaws.CredentialsSecret{Namespace: keys.Namespace, Name: keys.Name, Key: credentialsKey(keys),
			Direct: direct}
	}
	return append(objs, crossplaneaws.ProviderConfig(a.Namespace, a.ProviderConfigName, a.TenantID, a.Identity, ref))
}

// credentialsKey is the one key of an account's credentials copy.
func credentialsKey(s *corev1.Secret) string {
	for k := range s.Data {
		return k
	}
	return ""
}

// CredentialsCopyName is the name of an account's copy of the providers'
// credentials Secret, in the tenant namespace next to its ProviderConfig.
func CredentialsCopyName(a *model.CloudAccount) string { return a.ProviderConfigName + "-credentials" }

// credentialsCopy is the account's copy of the providers' credentials
// Secret (Deps.ProviderCredentials), in the tenant namespace: a namespaced
// ProviderConfig reads its Secret from its own namespace, whatever
// namespace its secretRef names. Only the configured key is copied; the
// keys pass through, never into anything the platform logs. Nil without a
// credentials Secret.
func (r *Reconciler) credentialsCopy(ctx context.Context, a *model.CloudAccount) (*corev1.Secret, error) {
	ref := r.deps.ProviderCredentials
	if ref == nil {
		return nil, nil
	}
	source, err := readCredentialsSecret(ctx, r.deps)
	if err != nil {
		return nil, err
	}
	return &corev1.Secret{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{Name: CredentialsCopyName(a), Namespace: a.Namespace, Labels: crossplane.Labels(a.TenantID)},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{ref.Key: source.Data[ref.Key]},
	}, nil
}

// CheckCredentialsSecret reports, as a FailedPrecondition fault, a
// credentials Secret (Deps.ProviderCredentials) that does not exist or
// lacks its key: every ProviderConfig would point the providers at it and
// fail. Only the key's presence is looked at; the keys themselves are
// never read into anything the platform keeps or logs. Nil when no Secret
// is set.
func CheckCredentialsSecret(ctx context.Context, d reconcile.Deps) error {
	if d.ProviderCredentials == nil {
		return nil
	}
	_, err := readCredentialsSecret(ctx, d)
	return err
}

// readCredentialsSecret reads the providers' credentials Secret and checks
// it has its key (CheckCredentialsSecret).
func readCredentialsSecret(ctx context.Context, d reconcile.Deps) (*corev1.Secret, error) {
	ref := d.ProviderCredentials
	secret := &corev1.Secret{}
	err := d.Reader().Get(ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, secret)
	switch {
	case apierrors.IsNotFound(err):
		return nil, fault.Newf(fault.FailedPrecondition,
			"the provider credentials Secret %s/%s does not exist; create it (see docs/controller.md) or unset --provider-credentials-secret",
			ref.Namespace, ref.Name)
	case err != nil:
		return nil, fault.Newf(fault.Unavailable, "cannot read the provider credentials Secret %s/%s: %v", ref.Namespace, ref.Name, err)
	}
	if _, ok := secret.Data[ref.Key]; !ok {
		return nil, fault.Newf(fault.FailedPrecondition,
			"the provider credentials Secret %s/%s has no key %q (an AWS shared credentials file)", ref.Namespace, ref.Name, ref.Key)
	}
	return secret, nil
}

// owner is the account as the owner of its own objects.
func owner(a *model.CloudAccount) reconcile.Owner {
	return reconcile.Owner{TenantID: a.TenantID, CloudAccountID: a.ID, Kind: model.OwnerCloudAccount, ID: a.ID}
}

// Apply applies the tenant namespace, with a credentials Secret the
// account's copy of it, and the account's ProviderConfig, from the stored
// account, and records them as the account's (the copy is deleted with the
// account). The
// providers and their runtime config are the platform's, installed
// beforehand: without them the ProviderConfig kind does not exist and
// Apply fails with a FailedPrecondition fault. In the cluster, the tenant
// namespace also binds the controller to the kubeconfig reader role
// (bindKubeconfigReader). Every step is idempotent.
func (r *Reconciler) Apply(ctx context.Context, a *model.CloudAccount) error {
	keys, err := r.credentialsCopy(ctx, a)
	if err != nil {
		return err
	}
	objs := objects(a, keys, r.deps.ProviderCredentials != nil && r.deps.ProviderCredentials.Direct)
	for _, obj := range objs {
		err := kube.Apply(ctx, r.deps.Kube, obj)
		switch {
		case err == nil:
		case meta.IsNoMatchError(err):
			return fault.New(fault.FailedPrecondition,
				"the Crossplane AWS provider is not installed in the management cluster (no aws.m.upbound.io ProviderConfig kind)")
		default:
			r.logger().ErrorContext(ctx, "bootstrap apply failed", "tenant_id", a.TenantID, "error", err)
			return fault.New(fault.Internal, "could not configure the management cluster")
		}
	}
	if err := r.deps.Record(ctx, owner(a), objs); err != nil {
		r.logger().ErrorContext(ctx, "record bootstrap resources", "tenant_id", a.TenantID, "error", err)
		return fault.New(fault.Internal, "could not record the bootstrap's resources")
	}
	return r.bindKubeconfigReader(ctx, a)
}

// Delete removes what Apply created for the account and reports whether
// it is gone: the account's ProviderConfig is deleted, and the account's
// records are forgotten once it is. The tenant namespace stays: the
// tenant's other accounts and their networks live in it. An account whose
// networks or clusters still have objects is not deleted (a
// FailedPrecondition fault): their managed resources reach the cloud
// through the ProviderConfig. It is safe to repeat.
func (r *Reconciler) Delete(ctx context.Context, a *model.CloudAccount) (bool, error) {
	if a.Paused != nil {
		return false, nil // held until the resume
	}
	records, err := r.deps.Store.ManagedResources().ListByAccount(ctx, a.TenantID, a.ID)
	if err != nil {
		return false, err
	}
	var own []model.ManagedResource
	for _, rec := range records {
		switch {
		case rec.OwnerKind != model.OwnerCloudAccount:
			return false, fault.Newf(fault.FailedPrecondition,
				"account %s still has resources of a %s (%s %s); delete its networks and clusters first",
				a.ID, rec.OwnerKind, rec.Kind, rec.Name)
		case rec.Kind != namespaceKind:
			own = append(own, rec)
		}
	}
	if err := r.deps.DeleteRecorded(ctx, own); err != nil {
		r.logger().ErrorContext(ctx, "delete bootstrap resources", "tenant_id", a.TenantID, "error", err)
		return false, fault.New(fault.Internal, "could not remove the account from the management cluster")
	}
	views, err := r.deps.Observe(ctx, own)
	if err != nil {
		return false, err
	}
	if !reconcile.AllGone(views) {
		return false, nil
	}
	return true, r.deps.Store.ManagedResources().DeleteByOwner(ctx, model.OwnerCloudAccount, a.ID)
}

// View is the API view of a stored account with every object created
// under it (its own, and its networks', clusters' and node pools'), each
// with its live state. It only reads.
func (r *Reconciler) View(ctx context.Context, a *model.CloudAccount) (*cloudv1.CloudAccount, error) {
	records, err := r.deps.Store.ManagedResources().ListByAccount(ctx, a.TenantID, a.ID)
	if err != nil {
		return nil, err
	}
	views, err := r.deps.Observe(ctx, records)
	if err != nil {
		return nil, err
	}
	out := Proto(a)
	out.Status.ManagedResources = views
	return out, nil
}

// Proto is the API view of a stored account, without its objects. Only
// AWS can be bootstrapped so far; the status is what bootstrap applies and
// the controller's last access check. An account whose access was denied
// is FAILED.
func Proto(a *model.CloudAccount) *cloudv1.CloudAccount {
	out := proto(a)
	out.Status.Access = accessProto(a.Access)
	out.Status.Conditions = append(out.Status.Conditions, AccessCondition(a.Access))
	if AccessDenied(a.Access) {
		// Bootstrapped, but the platform cannot act in it.
		out.Status.State = cloudv1.CloudAccountState_CLOUD_ACCOUNT_STATE_FAILED
	}
	if a.Paused != nil {
		out.Status.Pause = reconcile.PauseProto(a.Paused)
		out.Status.Conditions = append(out.Status.Conditions, PausedCondition(a.Paused))
	}
	return out
}

// PausedNote is what a paused account's status says after who paused it.
const PausedNote = "nothing is created, changed or deleted until it is resumed (admin resume)"

// PausedCondition is the Paused condition of a paused account: who
// paused it, since when, why, and what that means.
func PausedCondition(p *model.Pause) *cloudv1.Condition {
	return &cloudv1.Condition{Type: reconcile.ConditionPaused, Status: cloudv1.ConditionStatus_CONDITION_STATUS_TRUE,
		Reason: managed.ReasonPaused, Message: reconcile.PausedBy(p) + ": " + PausedNote}
}

func proto(a *model.CloudAccount) *cloudv1.CloudAccount {
	return &cloudv1.CloudAccount{
		Id:          a.ID,
		TenantId:    a.TenantID,
		AccountName: a.AccountName,
		Provider:    &cloudv1.CloudAccount_Aws{Aws: &cloudv1.AwsAccount{AccountId: a.AccountID, Role: RoleName(a.Identity)}},
		CreateTime:  timestamppb.New(a.CreatedAt),
		UpdateTime:  timestamppb.New(a.UpdatedAt),
		Status: &cloudv1.CloudAccountStatus{
			State:              cloudv1.CloudAccountState_CLOUD_ACCOUNT_STATE_READY,
			Namespace:          a.Namespace,
			ProviderConfigName: a.ProviderConfigName,
			Identity:           a.Identity,
			Conditions: []*cloudv1.Condition{
				{Type: ConditionBootstrapped, Status: cloudv1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "Applied"},
			},
		},
	}
}

// RoleName is the role of an AWS identity, arn:aws:iam::<account>:role/<name>;
// empty for accounts bootstrapped before identities were stored.
func RoleName(identity string) string {
	if i := strings.LastIndex(identity, ":role/"); i >= 0 {
		return identity[i+len(":role/"):]
	}
	return ""
}

func (r *Reconciler) logger() *slog.Logger {
	if r.deps.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return r.deps.Logger
}
