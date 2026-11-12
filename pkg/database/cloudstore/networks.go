package cloudstore

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sort"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/convert"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/kube"
	"github.com/yindia/cluster-vending/pkg/owner"
)

type networks struct{ store }

// Stored states of a network the store itself looks at.
const (
	networkDeleting = "NETWORK_STATE_DELETING"
	networkImported = "NETWORK_STATE_IMPORTED"
	// networkBlocked is the stored state of an import that was blocked
	// (FAILED is otherwise observed, never stored).
	networkBlocked = "NETWORK_STATE_FAILED"
)

// lenient reads stored and incoming JSON: fields this build does not
// know are skipped, not errors.
var lenient = protojson.UnmarshalOptions{DiscardUnknown: true}

// decode reads a contract document; an empty one is the empty message.
func decode(raw []byte, m proto.Message) error {
	if len(raw) == 0 {
		return nil
	}
	return lenient.Unmarshal(raw, m)
}

// networkKeys are the claims of a network: its create request ID, and the
// cloud network it was imported from per account and region. A network
// without them claims nothing.
func networkKeys(n *model.Network) []string {
	var keys []string
	if n.CreateRequestID != nil {
		keys = append(keys, "network-request/"+*n.CreateRequestID)
	}
	if n.ExternalID != nil {
		keys = append(keys, fmt.Sprintf("network-external/%s/%s/%s", n.CloudAccountID, n.Region, *n.ExternalID))
	}
	return keys
}

// networkImport is spec.importFrom of a network row: nil for a network
// the platform created. A row is an import when its input is an import
// request, and stays one once its object says so (stored: an update of
// the row rewrites its input as a network, without what was imported).
// The takeover is read from the stored state: an imported network that is
// neither IMPORTED nor blocked was taken over, and stays taken over.
func networkImport(n *model.Network, stored *cloudv1alpha1.NetworkImport) *cloudv1alpha1.NetworkImport {
	out := stored.DeepCopy()
	req := &cloudv1.ImportNetworkRequest{}
	if err := decode(n.Input, req); err == nil && req.GetAws().GetVpcId() != "" {
		out = &cloudv1alpha1.NetworkImport{
			AWS: &cloudv1alpha1.AWSImportNetwork{VPCID: req.GetAws().GetVpcId(), SubnetIDs: req.GetAws().GetSubnetIds()}}
	}
	if out == nil {
		return nil
	}
	takenOver := n.State != networkImported && n.State != networkBlocked || stored != nil && stored.TakeOver
	out.Takeover = cloudv1alpha1.Takeover{TakeOver: takenOver, AcknowledgeNotImported: takenOver}
	return out
}

// fill writes the row onto obj: spec, labels and annotations, everything
// but the status. accountName is the CloudAccount object of the row's
// account.
func (r networks) fill(n *model.Network, accountName string, obj *cloudv1alpha1.Network) error {
	p := &cloudv1.NetworkSpec{}
	if err := decode(n.Spec, p); err != nil {
		return fmt.Errorf("%w: network spec: %w", repository.ErrInvalid, err)
	}
	importFrom := networkImport(n, obj.Spec.ImportFrom)
	spec, remainder := convert.NetworkSpecToAPI(p, accountName, importFrom != nil)
	spec.DisplayName, spec.Tags, spec.ImportFrom = n.DisplayName, cloudv1alpha1.Tags(n.Labels), importFrom
	if spec.Region == "" {
		spec.Region = n.Region
	}
	obj.Spec = spec
	label(obj, map[string]string{LabelID: n.ID, LabelTenant: n.TenantID, LabelAccountID: n.CloudAccountID})
	rest, err := remainderJSON(remainder != nil, remainder)
	if err != nil {
		return err
	}
	annotate(obj, map[string]string{AnnotationInput: string(n.Input), AnnotationRemainder: rest})
	m := resourceMeta(&n.Resource)
	m.Provider, m.CreateRequestID, m.ExternalID = string(n.Provider), n.CreateRequestID, n.ExternalID
	return setMeta(obj, m)
}

// remainderJSON is the remainder annotation: "" for none.
func remainderJSON(has bool, remainder proto.Message) (string, error) {
	if !has {
		return "", nil
	}
	raw, err := protojson.Marshal(remainder)
	return string(raw), err
}

// row is the row an object holds.
func (r networks) row(obj *cloudv1alpha1.Network) (*model.Network, error) {
	remainder := &cloudv1.NetworkSpec{}
	if err := decode([]byte(obj.Annotations[AnnotationRemainder]), remainder); err != nil {
		return nil, fmt.Errorf("network %s: stored spec remainder: %w", obj.Name, err)
	}
	spec, err := protojson.Marshal(convert.NetworkSpecFromAPI(obj.Spec, obj.Labels[LabelAccountID], remainder))
	if err != nil {
		return nil, err
	}
	m := metaOf(obj)
	n := &model.Network{
		Resource: model.Resource{Base: model.Base{ID: obj.Labels[LabelID]}, DisplayName: obj.Spec.DisplayName,
			Labels: model.Labels(obj.Spec.Tags), Spec: spec, Status: contractStatus(obj.Status.ContractStatus)},
		TenantID: obj.Labels[LabelTenant], Name: obj.Name, Input: model.JSON(obj.Annotations[AnnotationInput]),
		CloudAccountID: obj.Labels[LabelAccountID], Provider: model.Provider(m.Provider), Region: obj.Spec.Region,
		ExternalID: m.ExternalID, CreateRequestID: m.CreateRequestID,
	}
	m.into(&n.Resource)
	return n, nil
}

// contractStatus is the status document an object holds; nil for none.
func contractStatus(raw *runtime.RawExtension) model.JSON {
	if raw == nil || len(raw.Raw) == 0 {
		return nil
	}
	return model.JSON(raw.Raw)
}

// keepStatus is the contract status to store for raw: nil for none, and
// the stored document when it already says the same (the API server
// stores JSON by meaning; writing it again would be a change for nothing).
func keepStatus(stored *runtime.RawExtension, raw model.JSON) *runtime.RawExtension {
	if len(raw) == 0 {
		return nil
	}
	if stored != nil && sameJSON(stored.Raw, raw) {
		return stored
	}
	return &runtime.RawExtension{Raw: []byte(raw)}
}

// setStatus writes a row's status onto obj.Status: the contract status
// verbatim, and the typed fields read from it. The inventory stays.
func (r networks) setStatus(obj *cloudv1alpha1.Network, raw model.JSON, state string) {
	p := &cloudv1.NetworkStatus{}
	_ = decode(raw, p) // a status this build cannot read is still kept verbatim
	st := &obj.Status
	st.ContractStatus = keepStatus(st.ContractStatus, raw)
	convert.NetworkStatusInto(p, state, metav1.NewTime(now()), st)
	st.ObservedGeneration = obj.Generation
	st.Layout = obj.Spec.Layout.DeepCopy()
	if st.Layout == nil {
		// Imported: the layout read from the cloud, when the kind can hold
		// it.
		remainder := &cloudv1.NetworkSpec{}
		if decode([]byte(obj.Annotations[AnnotationRemainder]), remainder) == nil {
			st.Layout = showableLayout(remainder.GetLayout())
		}
	}
}

// showableLayout is an imported network's layout for status.layout, or
// nil when the kind's rules for a layout would refuse it (the status
// write would fail as a whole): a range outside /16 to /24, no private
// subnet, or two subnets of one range. The contract spec has it anyway.
func showableLayout(p *cloudv1.NetworkLayout) *cloudv1alpha1.NetworkLayout {
	prefix, err := netip.ParsePrefix(p.GetCidrBlock())
	if err != nil || !prefix.Addr().Is4() || prefix.Bits() < 16 || prefix.Bits() > 24 || p.GetAvailabilityZoneCount() != 0 {
		return nil
	}
	seen, private := map[string]bool{}, false
	for _, s := range p.GetSubnets() {
		sub, err := netip.ParsePrefix(s.GetCidrBlock())
		if err != nil || !sub.Addr().Is4() || seen[s.GetCidrBlock()] || s.GetRole() == cloudv1.SubnetRole_SUBNET_ROLE_UNSPECIFIED {
			return nil
		}
		seen[s.GetCidrBlock()] = true
		private = private || s.GetRole() == cloudv1.SubnetRole_SUBNET_ROLE_PRIVATE
	}
	if !private || p.GetNatGatewayMode() == cloudv1.NatGatewayMode_NAT_GATEWAY_MODE_UNSPECIFIED || len(p.GetSubnets()) > 32 {
		return nil
	}
	return convert.LayoutToAPI(p)
}

// find is the network of the tenant with the row ID.
func (r networks) find(ctx context.Context, tenantID, id string) (*cloudv1alpha1.Network, error) {
	if !labelValue(tenantID) {
		return nil, repository.ErrNotFound
	}
	return byID[*cloudv1alpha1.Network](ctx, r.store, &cloudv1alpha1.NetworkList{}, kube.TenantNamespace(tenantID), id)
}

// account is the CloudAccount object of the row's account, in the
// tenant's namespace: ErrNotFound when there is none, ErrInvalid when it
// is another tenant's.
func (r networks) account(ctx context.Context, n *model.Network) (*cloudv1alpha1.CloudAccount, error) {
	account, err := byID[*cloudv1alpha1.CloudAccount](ctx, r.store, &cloudv1alpha1.CloudAccountList{}, kube.TenantNamespace(n.TenantID), n.CloudAccountID)
	if err == nil {
		return account, nil
	}
	if !errors.Is(err, repository.ErrNotFound) {
		return nil, err
	}
	other, err := byID[*cloudv1alpha1.CloudAccount](ctx, r.store, &cloudv1alpha1.CloudAccountList{}, "", n.CloudAccountID)
	if err != nil {
		return nil, fmt.Errorf("cloud account: %w", err)
	}
	return nil, fmt.Errorf("%w: account %s belongs to tenant %s, network to tenant %s",
		repository.ErrInvalid, other.Name, other.Labels[LabelTenant], n.TenantID)
}

// Create stores a network as a Network object owned by its account. Its
// name must be free in the tenant (ErrAlreadyExists).
func (r networks) Create(ctx context.Context, n *model.Network) error {
	if _, err := r.tenantNamespace(ctx, n.TenantID); err != nil {
		return err
	}
	if err := invalid(n.Provider.Validate()); err != nil {
		return err
	}
	account, err := r.account(ctx, n)
	if err != nil {
		return err
	}
	if err := onlyAWS(n.Provider); err != nil {
		return err
	}
	if err := prepare(&n.Resource); err != nil {
		return err
	}
	obj := &cloudv1alpha1.Network{ObjectMeta: metav1.ObjectMeta{Namespace: account.Namespace, Name: n.Name}}
	if err := r.fill(n, account.Name, obj); err != nil {
		return err
	}
	if err := owner.Set(account, obj, r.c.Scheme()); err != nil {
		return invalid(err)
	}
	keys := networkKeys(n)
	if err := r.claimAll(ctx, obj.Namespace, kindNetwork, n.ID, keys); err != nil {
		return err
	}
	if err := r.insert(ctx, obj, func() { r.setStatus(obj, n.Status, n.State) }); err != nil {
		r.release(ctx, obj.Namespace, keys...)
		return err
	}
	r.created(func(ctx context.Context) error {
		err := r.c.Delete(ctx, obj, client.PropagationPolicy(metav1.DeletePropagationOrphan))
		r.release(ctx, obj.Namespace, keys...)
		return translate(err)
	})
	return nil
}

// insert creates obj and then writes its status (set fills it in): the
// API server stores no status on create. A status that cannot be written
// takes the object back: an imported row without its import plan must not
// exist.
func (s store) insert(ctx context.Context, obj client.Object, set func()) error {
	if err := s.c.Create(ctx, obj); err != nil {
		return translate(err)
	}
	set()
	if err := s.c.Status().Update(ctx, obj); err != nil {
		if derr := s.c.Delete(ctx, obj, client.PropagationPolicy(metav1.DeletePropagationOrphan)); derr != nil {
			return errors.Join(translate(err), fmt.Errorf("take %s back: %w", obj.GetName(), derr))
		}
		return translate(err)
	}
	return nil
}

func (r networks) Get(ctx context.Context, tenantID, id string) (*model.Network, error) {
	obj, err := r.find(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}
	return r.row(obj)
}

func (r networks) GetByCreateRequest(ctx context.Context, tenantID, requestID string) (*model.Network, error) {
	if !labelValue(tenantID) {
		return nil, repository.ErrNotFound
	}
	id, err := r.holder(ctx, kube.TenantNamespace(tenantID), "network-request/"+requestID)
	if err != nil {
		return nil, err
	}
	return r.Get(ctx, tenantID, id)
}

// all lists the networks with labels in namespace, ordered by ID (oldest
// first).
func (r networks) all(ctx context.Context, namespace string, labels map[string]string) ([]model.Network, error) {
	objects, err := items[*cloudv1alpha1.Network](ctx, r.store, &cloudv1alpha1.NetworkList{}, namespace, labels)
	if err != nil {
		return nil, err
	}
	out := make([]model.Network, 0, len(objects))
	for _, obj := range objects {
		if obj.Labels[LabelID] == "" {
			continue // not a row: an object the store did not write
		}
		n, err := r.row(obj)
		if err != nil {
			return nil, err
		}
		out = append(out, *n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (r networks) List(ctx context.Context, tenantID string, p repository.Page) ([]model.Network, error) {
	if p.Limit <= 0 {
		return nil, fmt.Errorf("page limit must be positive, got %d", p.Limit)
	}
	rows, err := r.all(ctx, kube.TenantNamespace(tenantID), map[string]string{LabelTenant: tenantID})
	if err != nil {
		return nil, err
	}
	return page(rows, func(n model.Network) string { return n.ID }, p)
}

// Update writes the row if the stored etag is n's, and gives n a new one.
// The object (spec, annotations) is written first, the status after it
// when it changed; see the package comment for what a failure between the
// two leaves.
func (r networks) Update(ctx context.Context, n *model.Network) error {
	old, updated := n.ETag, n.UpdatedAt
	var obj *cloudv1alpha1.Network
	err := retry(func() error {
		var err error
		if obj, err = r.find(ctx, n.TenantID, n.ID); err != nil {
			return err
		}
		m := metaOf(obj)
		if m.ETag != old {
			return repository.ErrConflict
		}
		n.ETag, n.UpdatedAt, n.CreatedAt = newETag(), now(), m.CreatedAt
		if err := r.fill(n, obj.Spec.AccountRef.Name, obj); err != nil {
			return err
		}
		return r.c.Update(ctx, obj)
	})
	if err != nil {
		n.ETag, n.UpdatedAt = old, updated
		return translate(err)
	}
	return r.writeStatus(ctx, n.TenantID, n.ID, n.ETag, n.Status, obj)
}

// writeStatus stores a row's status while the row still has etag (a row
// changed since keeps what that change stored: nil). obj is the object as
// just read or written, or nil to read it.
func (r networks) writeStatus(ctx context.Context, tenantID, id, etag string, status model.JSON, obj *cloudv1alpha1.Network) error {
	return translate(retry(func() error {
		if obj == nil {
			var err error
			if obj, err = r.find(ctx, tenantID, id); err != nil {
				return err
			}
		}
		current := obj
		obj = nil // read again on a retry
		m := metaOf(current)
		if m.ETag != etag {
			return nil
		}
		before := current.Status.DeepCopy()
		r.setStatus(current, status, m.State)
		if equality.Semantic.DeepEqual(before, &current.Status) {
			return nil
		}
		return r.c.Status().Update(ctx, current)
	}))
}

// SetStatus writes the status only, and only while the row has etag; the
// etag and the update time stay.
func (r networks) SetStatus(ctx context.Context, tenantID, id, etag string, status model.JSON) error {
	return translate(retry(func() error {
		obj, err := r.find(ctx, tenantID, id)
		if err != nil {
			return err
		}
		m := metaOf(obj)
		if m.ETag != etag {
			return repository.ErrConflict
		}
		before := obj.Status.DeepCopy()
		r.setStatus(obj, status, m.State)
		if equality.Semantic.DeepEqual(before, &obj.Status) {
			return nil
		}
		return r.c.Status().Update(ctx, obj)
	}))
}

// MarkBehind puts the applied generation below the generation and assigns
// a new etag, so a write based on the row as it was fails.
func (r networks) MarkBehind(ctx context.Context, tenantID, id string) error {
	return translate(retry(func() error {
		obj, err := r.find(ctx, tenantID, id)
		if err != nil {
			return err
		}
		if err := markBehind(obj); err != nil {
			return err
		}
		return r.c.Update(ctx, obj)
	}))
}

// markBehind edits obj's row annotation: a new etag, and an applied
// generation below the generation.
func markBehind(obj client.Object) error {
	m := metaOf(obj)
	m.ETag = newETag()
	if m.AppliedGeneration >= m.Generation {
		m.AppliedGeneration = m.Generation - 1
	}
	return setMeta(obj, m)
}

// Delete removes the network unless clusters use it. A cluster created in
// the same instant changes the network (clusters.Create), so the delete of
// the network as it was read is refused, or sees the network gone when it
// claims it and takes itself back.
func (r networks) Delete(ctx context.Context, tenantID, id string) error {
	obj, err := r.find(ctx, tenantID, id)
	if err != nil {
		return err
	}
	users, err := items[*cloudv1alpha1.Cluster](ctx, r.store, &cloudv1alpha1.ClusterList{}, obj.Namespace, map[string]string{LabelNetworkID: id})
	if err != nil {
		return err
	}
	if n := len(users); n > 0 {
		return fmt.Errorf("%w: %d clusters run in it", repository.ErrInUse, n)
	}
	row, err := r.row(obj)
	if err != nil {
		return err
	}
	if err := r.remove(ctx, obj); err != nil {
		return err
	}
	r.release(ctx, obj.Namespace, networkKeys(row)...)
	return nil
}

func (r networks) ListPending(ctx context.Context, states []string, renderTarget int64) ([]model.Network, error) {
	rows, err := r.all(ctx, "", nil)
	if err != nil {
		return nil, err
	}
	out := rows[:0]
	for _, n := range rows {
		if isPending(&n.Resource, states, renderTarget) {
			out = append(out, n)
		}
	}
	return out, nil
}
